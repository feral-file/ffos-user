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

	// Dedup by wire key; settle data: URIs and stored verdicts inline;
	// budget the rest. Capacity hints are capped at the budget so a
	// playlist repeating one tiny URL cannot size an allocation.
	firstIndex := make(map[string]int, min(len(sources), maxAttempts))
	keyOf := make([]string, len(sources))
	uniqueIndices := make([]int, 0, min(len(sources), maxAttempts))
	for i, source := range sources {
		key := wireKey(source)
		keyOf[i] = key
		if _, seen := firstIndex[key]; seen {
			continue
		}
		if IsDataURI(source) {
			firstIndex[key] = i
			results[i] = probeDataURI(source)
			continue
		}
		if rec := stored[source]; rec != nil && fromStoreVerdict(rec, at) != "" {
			firstIndex[key] = i
			results[i] = storedResult(rec, at)
			continue
		}
		if len(uniqueIndices) == maxAttempts {
			results[i] = ProbeResult{
				Source:  redactForLog(source),
				Verdict: VerdictInconclusive,
				Err:     errBudgetExhausted,
			}
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

	// Fan the unique verdicts back out to the duplicate positions, after the
	// barrier so every read sees a settled result.
	for i := range sources {
		if first, probed := firstIndex[keyOf[i]]; probed && first != i {
			results[i] = results[first]
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
