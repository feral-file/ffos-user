package mediahealth

import (
	"context"
	"errors"
	"net/url"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Prober runs the cast-time L0 pass over a playlist's sources: the one
// probe pass with two consumers — the cast admission verdict (every item
// dead ⇒ sourceUnreachable, the former standalone preflight of #304) and
// the observation store. It replaces offlinecache.SourceProber.
//
// Shape kept from the preflight, because the same hostile-input reasoning
// applies: a fixed worker pool over an index feed (never a goroutine per
// item — the unauthenticated hub accepts a 4 MiB playlist with no item
// cap), wire-identical sources probed once, a hard per-pass budget of
// distinct wire URLs, a per-item deadline covering guard DNS plus the
// request, and a phase ceiling on the whole pass. What changed: there is
// NO early exit on the first alive source any more. The preflight stopped
// there because it only asked "is this cast all dead?"; this pass also
// records every verdict, so every source inside the budget is probed. The
// latency that early exit saved is bought back by the store: a source with
// a conclusive record is answered from it without a request, so a playlist
// seen before costs no network at cast.
type Prober struct {
	Probe  *Probe
	Store  *Store
	Logger *zap.Logger

	// Concurrency is the worker-pool size (DefaultConcurrency when 0).
	Concurrency int
	// MaxAttempts caps distinct dialing wire URLs per pass
	// (DefaultMaxAttempts when 0). Sources past it are inconclusive.
	MaxAttempts int
	// PhaseCeiling bounds the whole pass (DefaultPhaseCeiling when 0).
	PhaseCeiling time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
}

const (
	// DefaultConcurrency matches the offline cache's measured sweet spot
	// against the same uplink (classifyConcurrency: past 16 the uplink is
	// the limit).
	DefaultConcurrency = 16
	// DefaultMaxAttempts comfortably covers real playlists (the dynamic
	// resolution path caps at 255) while keeping a hostile cast's worst
	// case a burst, not a stream.
	DefaultMaxAttempts = 256
	// DefaultPhaseCeiling bounds the pass's share of the cast request's
	// time. It does not guarantee the whole command answers inside the
	// hub's 30 s write deadline (DP-1 resolution before it is unbounded by
	// anything here); it guarantees a black-holing origin cannot hang the
	// command.
	DefaultPhaseCeiling = 10 * time.Second
)

// Preflight probes sources and returns one result per source, in input
// order. It never fails as a whole. Sources with a conclusive stored
// verdict (broken inside its recheck window, or ok inside its window) are
// answered from the store (FromStore) without a request; data: URIs are
// settled synchronously outside the attempt budget; everything else is
// probed and its result written to the store.
func (p *Prober) Preflight(ctx context.Context, sources []string) []ProbeResult {
	results := make([]ProbeResult, len(sources))
	if len(sources) == 0 {
		return results
	}
	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	ceiling := p.PhaseCeiling
	if ceiling <= 0 {
		ceiling = DefaultPhaseCeiling
	}
	maxAttempts := p.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = DefaultMaxAttempts
	}
	ctx, cancel := context.WithTimeout(ctx, ceiling)
	defer cancel()

	// One read transaction for every stored verdict the pass can reuse.
	var stored map[string]*Record
	if p.Store != nil {
		var err error
		stored, err = p.Store.GetMany(sources)
		if err != nil {
			logger.Warn("media health: stored verdicts unavailable for the cast pass; probing everything", zap.Error(err))
			stored = nil
		}
	}
	at := now()

	// Settle data: URIs and stored verdicts per RAW source first, then
	// dedup the network work by wire key and budget it. The order matters:
	// a stored verdict belongs to the exact source (two fragment variants
	// of one URL are two items with two records — one may have played ok
	// while the other is broken), so a sibling's verdict must never be
	// copied onto it; only the request is shared (PR #438 review, round
	// 2, F9). Capacity hints are capped at the budget so a playlist
	// repeating one tiny URL cannot size an allocation.
	settled := make([]bool, len(sources))
	// writes collects every record the pass settles outside the worker
	// pool (malformed data: URIs, fanned-out sibling verdicts) for ONE
	// store transaction after the barrier; see Store.ApplyProbes.
	var writes []ProbeWrite
	firstIndex := make(map[string]int, min(len(sources), maxAttempts))
	keyOf := make([]string, len(sources))
	uniqueIndices := make([]int, 0, min(len(sources), maxAttempts))
	for i, source := range sources {
		if IsDataURI(source) {
			results[i] = probeDataURI(source)
			settled[i] = true
			if results[i].Verdict == VerdictDead {
				// A provably malformed data: URI is as conclusive as a 404
				// and can never heal; it needs a record and a hint like
				// any other broken source (PR #438 review, round 3, F13).
				// Well-formed inline sources stay unrecorded.
				writes = append(writes, ProbeWrite{Source: source, Result: results[i]})
			}
			continue
		}
		if rec := stored[source]; rec != nil {
			if fromStoreVerdict(rec, at) != "" {
				results[i] = storedResult(rec, at)
				settled[i] = true
				continue
			}
			if !rec.Due(at) {
				// Under a transient retry hold (a 429 minutes ago): the
				// cast must not re-dial an origin asking to be left
				// alone, and nothing conclusive is known — inconclusive,
				// fail-open, no request.
				results[i] = ProbeResult{Source: redactForLog(source), Verdict: VerdictInconclusive, FromStore: true, Err: errRetryHold}
				settled[i] = true
				continue
			}
		}
		key := wireKey(source)
		keyOf[i] = key
		if _, seen := firstIndex[key]; seen {
			continue
		}
		if len(uniqueIndices) == maxAttempts {
			results[i] = ProbeResult{
				Source:  redactForLog(source),
				Verdict: VerdictInconclusive,
				Err:     errBudgetExhausted,
			}
			settled[i] = true
			continue
		}
		firstIndex[key] = i
		uniqueIndices = append(uniqueIndices, i)
	}

	workers := p.Concurrency
	if workers <= 0 {
		workers = DefaultConcurrency
	}
	if len(uniqueIndices) < workers {
		workers = len(uniqueIndices)
	}
	indices := make(chan int)
	// probed marks the unique positions whose request actually ran (not
	// cut off by the ceiling), so a shared verdict is recorded for the
	// siblings only when it is a real verdict.
	probed := make([]bool, len(sources))
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Go(func() {
			for i := range indices {
				if ctx.Err() != nil {
					// Ceiling already hit: never probed, so never dead.
					results[i] = ProbeResult{
						Source:  redactForLog(sources[i]),
						Verdict: VerdictInconclusive,
						Err:     ctx.Err(),
					}
					continue
				}
				results[i] = p.Probe.ProbeOne(ctx, sources[i])
				probed[i] = true
				p.record(sources[i], results[i], logger)
			}
		})
	}
	for _, i := range uniqueIndices {
		select {
		case indices <- i:
		case <-ctx.Done():
			results[i] = ProbeResult{
				Source:  redactForLog(sources[i]),
				Verdict: VerdictInconclusive,
				Err:     ctx.Err(),
			}
		}
	}
	close(indices)
	wg.Wait()

	// Fan each probed verdict back out to the wire-identical siblings that
	// had no verdict of their own, after the barrier so every read sees a
	// settled result — and record it under every sibling's own key, so a
	// freshly probed variant carries its own record (and hint) too.
	for i := range sources {
		if settled[i] {
			continue
		}
		first := firstIndex[keyOf[i]]
		if first == i {
			continue
		}
		results[i] = results[first]
		results[i].Source = redactForLog(sources[i])
		if probed[first] && sources[i] != sources[first] {
			writes = append(writes, ProbeWrite{Source: sources[i], Result: results[i]})
		}
	}
	if len(writes) > 0 && p.Store != nil {
		if err := p.Store.ApplyProbes(writes); err != nil && !errors.Is(err, ErrLowDisk) {
			logger.Warn("media health: recording settled verdicts failed", zap.Int("count", len(writes)), zap.Error(err))
		}
	}
	return results
}

// ProbeAndRecord probes one source outside a cast pass (the lookahead and
// the background sweep) and writes the result.
func (p *Prober) ProbeAndRecord(ctx context.Context, source string) ProbeResult {
	res := p.Probe.ProbeOne(ctx, source)
	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	p.record(source, res, logger)
	return res
}

func (p *Prober) record(source string, res ProbeResult, logger *zap.Logger) {
	if p.Store == nil {
		return
	}
	if _, err := p.Store.ApplyProbe(source, res); err != nil && !errors.Is(err, ErrLowDisk) {
		logger.Warn("media health: recording probe result failed",
			zap.String("source", res.Source), zap.Error(err))
	}
}

// fromStoreVerdict says which verdict a stored record answers for the pass
// without a request, or "" when the record is due for a look.
func fromStoreVerdict(rec *Record, now time.Time) Verdict {
	if rec == nil || now.After(rec.RecheckAfter) || rec.RecheckAfter.Equal(now) {
		return ""
	}
	switch rec.State {
	case StateBroken:
		return VerdictDead
	case StateOK:
		return VerdictAlive
	}
	return ""
}

func storedResult(rec *Record, now time.Time) ProbeResult {
	return ProbeResult{
		Source:    redactForLog(rec.Source),
		Verdict:   fromStoreVerdict(rec, now),
		Status:    rec.HTTPStatus,
		Reason:    rec.Reason,
		Detail:    rec.LastError,
		WireURL:   rec.WireURL,
		FromStore: true,
	}
}

// wireKey is the dedup key for one source: the URL as it would appear on
// the wire. Fragments never leave the client, so keying on the raw string
// would let `#unique-N` suffixes defeat the dedup and turn one target into
// N probes.
func wireKey(source string) string {
	u, err := url.Parse(source)
	if err != nil {
		return source
	}
	u.Fragment = ""
	return u.String()
}

type budgetError struct{}

func (budgetError) Error() string { return "media health: per-cast probe budget exhausted" }

var errBudgetExhausted error = budgetError{}

type retryHoldError struct{}

func (retryHoldError) Error() string { return "media health: source under a transient retry hold" }

var errRetryHold error = retryHoldError{}
