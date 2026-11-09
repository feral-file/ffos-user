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
//
// What the pass does NOT do any more is probe a whole 256-item playlist
// at cast with 16 workers: that burst looked like a scrape to a CDN and a
// 429 there was the fleet's own doing. The cast pass now covers the first
// Items sources in play order with a small pool, paced per host by the
// shared HostLimiter, and leaves the rest to the lookahead, which walks
// the whole wall at one request every Gap. Items beyond the pass are
// unknown at cast (no hint — fail-open) and get their hint on the next
// push. The all-dead rejection keeps its reach: while every source seen so
// far is dead the pass continues, batch by batch, up to the budget, so a
// cast of 200 dead items is still refused and a cast of 24 dead items in
// front of a live one still plays.
type Prober struct {
	Probe  *Probe
	Store  *Store
	Logger *zap.Logger
	// Hosts paces requests per origin and keeps a 429/503 host cool; shared
	// with the Lookahead through this Prober. nil = no per-host pacing
	// (tests, the bench).
	Hosts *HostLimiter

	// Concurrency is the worker-pool size (DefaultConcurrency when 0).
	Concurrency int
	// Items is how many sources, in play order, the pass probes before
	// handing the rest to the lookahead (DefaultPassItems when 0). Only
	// network work counts: data: URIs and sources answered from the store
	// are outside it.
	Items int
	// MaxAttempts caps distinct dialing wire URLs per pass
	// (DefaultMaxAttempts when 0). Sources past it are inconclusive. The
	// all-dead extension stops here.
	MaxAttempts int
	// PhaseCeiling bounds the whole pass (DefaultPhaseCeiling when 0).
	PhaseCeiling time.Duration
	// BackgroundItemTimeout is the per-item deadline for probes outside a
	// cast (the lookahead, the wall walk, the sweep), via ProbeAndRecord
	// (DefaultBackgroundItemTimeout when 0). The cast pass keeps the
	// Probe's short ItemTimeout because it sits on the cast's latency;
	// background probes sit on nothing, and a short deadline there only
	// turned slow answers into timeouts: arweave.net answers some
	// transactions with its 404 page after 7–27 s, so a 5 s probe recorded
	// "no answer" and never saw the status (office FF1, "Infinite
	// Descent").
	BackgroundItemTimeout time.Duration
	// Now is the clock (time.Now when nil).
	Now func() time.Time
	// Online reports the device's applied internet verdict (the mediator's
	// connectivity check, which itself needs DNS and an HTTP answer). With
	// lastAnswered it decides ProbeResult.NetworkHealthy: an unreachable
	// result counts toward a source's streak only while the device
	// demonstrably reaches other origins. nil reads as not online.
	Online func() bool

	// answeredMu guards the network evidence: when and from which host the
	// last END-TO-END answer came (see noteNetwork).
	answeredMu   sync.Mutex
	answeredAt   time.Time
	answeredHost string
}

// networkWindow is how recent another origin's answer must be for an
// unreachable result to count against the failing source.
const networkWindow = time.Hour

const (
	// DefaultConcurrency is the pool size. Four is enough to keep a mixed
	// playlist's pass inside the ceiling (one slow origin blocks one
	// worker), and together with the per-host cap it means an origin
	// serving the whole playlist sees DefaultHostInFlight connections,
	// not the pool.
	DefaultConcurrency = 4
	// DefaultPassItems is how far into the playlist the cast pass looks.
	// 24 items at 2 per host and ~300 ms each is a few seconds; it covers
	// the opening of any real wall, and the lookahead reaches the rest
	// before playback does, one request after another.
	DefaultPassItems = 24
	// DefaultMaxAttempts bounds the all-dead extension (the dynamic
	// resolution path caps at 255) while keeping a hostile cast's worst
	// case bounded.
	DefaultMaxAttempts = 256
	// DefaultPhaseCeiling bounds the pass's share of the cast request's
	// time. It does not guarantee the whole command answers inside the
	// hub's 30 s write deadline (DP-1 resolution before it is unbounded by
	// anything here); it guarantees a black-holing origin cannot hang the
	// command.
	DefaultPhaseCeiling = 10 * time.Second
	// DefaultBackgroundItemTimeout: long enough for a slow gateway to
	// answer, short enough that one black hole holds the one-at-a-time
	// lookahead for seconds, not minutes. The guarded transport's own
	// 10 s TLS-handshake bound still applies inside it.
	DefaultBackgroundItemTimeout = 30 * time.Second
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
			switch {
			case results[i].Verdict == VerdictDead:
				// A provably malformed data: URI, or a payload that is not
				// what it declares, is as conclusive as a 404 and can never
				// heal; it needs a record and a hint like any other broken
				// source (PR #438 review, round 3, F13).
				writes = append(writes, ProbeWrite{Source: source, Result: results[i]})
			case probeWritesRecord(results[i]) && stored[source].Due(at):
				// A checked payload (ok) or one that would not decode (a
				// hold): recorded when due, not on every cast — the bytes
				// cannot change under the same URI.
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

	// The network work runs in batches of Items. The first batch always
	// runs; a later one runs only while every source seen so far — the
	// settled ones included — is dead, because that is the only state in
	// which probing further can change the cast's fate (allSourcesDead
	// needs every item dead; one alive, inline or inconclusive item admits
	// the cast, and the lookahead will look at the rest). probed marks the
	// unique positions whose request actually ran (not cut off by the
	// ceiling), so a shared verdict is recorded for the siblings only when
	// it is a real verdict.
	probed := make([]bool, len(sources))
	batch := p.Items
	if batch <= 0 {
		batch = DefaultPassItems
	}
	rejectable := true
	for i := range sources {
		if settled[i] && results[i].Verdict != VerdictDead {
			rejectable = false
			break
		}
	}
	for start := 0; start < len(uniqueIndices); start += batch {
		end := min(start+batch, len(uniqueIndices))
		if start > 0 && !rejectable {
			for _, i := range uniqueIndices[start:] {
				results[i] = ProbeResult{
					Source:  redactForLog(sources[i]),
					Verdict: VerdictInconclusive,
					Err:     errBeyondPass,
				}
			}
			break
		}
		p.runPool(ctx, sources, uniqueIndices[start:end], results, probed, logger)
		for _, i := range uniqueIndices[start:end] {
			if results[i].Verdict != VerdictDead {
				rejectable = false
			}
		}
	}

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

// runPool probes the unique positions in indices with a fixed worker pool
// and writes each result (and its record) in place.
func (p *Prober) runPool(ctx context.Context, sources []string, indices []int, results []ProbeResult, probed []bool, logger *zap.Logger) {
	workers := p.Concurrency
	if workers <= 0 {
		workers = DefaultConcurrency
	}
	if len(indices) < workers {
		workers = len(indices)
	}
	feed := make(chan int)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Go(func() {
			for i := range feed {
				if ctx.Err() != nil {
					// Ceiling already hit: never probed, so never dead.
					results[i] = ProbeResult{
						Source:  redactForLog(sources[i]),
						Verdict: VerdictInconclusive,
						Err:     ctx.Err(),
					}
					continue
				}
				results[i], probed[i] = p.probeOne(ctx, sources[i], 0, logger)
			}
		})
	}
	for _, i := range indices {
		select {
		case feed <- i:
		case <-ctx.Done():
			results[i] = ProbeResult{
				Source:  redactForLog(sources[i]),
				Verdict: VerdictInconclusive,
				Err:     ctx.Err(),
			}
		}
	}
	close(feed)
	wg.Wait()
}

// ProbeAndRecord probes one source outside a cast pass (the lookahead and
// the background sweep) and writes the result.
func (p *Prober) ProbeAndRecord(ctx context.Context, source string) ProbeResult {
	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	timeout := p.BackgroundItemTimeout
	if timeout <= 0 {
		timeout = DefaultBackgroundItemTimeout
	}
	res, _ := p.probeOne(ctx, source, timeout, logger)
	return res
}

// probeOne is the one network path of both consumers: the host pacing,
// the probe, the host cooldown, the record. probed is false when no
// request was made (a cooled host, a cut-short wait for a slot) — such a
// result is inconclusive without a Reason, so Store.ApplyProbe writes
// nothing and the source stays unknown rather than held for an hour.
func (p *Prober) probeOne(ctx context.Context, source string, timeout time.Duration, logger *zap.Logger) (res ProbeResult, probed bool) {
	host := p.wireHost(source)
	if p.Hosts != nil {
		if until, ok := p.Hosts.Cooled(host); ok {
			return ProbeResult{
				Source:  redactForLog(source),
				Verdict: VerdictInconclusive,
				Err:     &hostCooldownError{until: until},
			}, false
		}
		if err := p.Hosts.Acquire(ctx, host); err != nil {
			return ProbeResult{
				Source:  redactForLog(source),
				Verdict: VerdictInconclusive,
				Err:     err,
			}, false
		}
		defer p.Hosts.Release(host)
	}
	res = p.Probe.ProbeOneWithin(ctx, source, timeout)
	if timeout == 0 && res.Verdict == VerdictInconclusive && fetchErrorKind(res.Err) == FetchErrTimeout {
		// The cast pass's SHORT item deadline (Probe.ItemTimeout, 5 s)
		// expired: it bounds the cast's latency, not the source. Recording
		// it held a slow-but-healthy origin for an hour, counted toward
		// its unreachable streak and showed the app `no_answer` while a
		// 30 s background probe would have answered in seconds. Unrecorded,
		// the item stays due and the lookahead's background probe
		// (BackgroundItemTimeout) is the real attempt.
		return ProbeResult{Source: redactForLog(source), Verdict: VerdictInconclusive, Err: res.Err}, false
	}
	if ctx.Err() != nil && res.Verdict == VerdictInconclusive {
		// The CALLER's context ended — the cast pass's phase ceiling, or
		// shutdown — not this source's own ItemTimeout. Running out of our
		// own budget says nothing about the source: recording it as a
		// transient failure put a one-hour hold on every item still in
		// flight when the pass ended (four items on the office FF1), and
		// because they then had a record the lookahead's walk for
		// never-probed sources skipped them too. Unrecorded, the item
		// stays unknown and the lookahead reaches it within seconds.
		return ProbeResult{Source: redactForLog(source), Verdict: VerdictInconclusive, Err: ctx.Err()}, false
	}
	p.noteNetwork(&res)
	if p.Hosts != nil && coolsHost(res.Status) && res.Verdict == VerdictInconclusive {
		// Keyed by the host that actually answered (post-redirect), which
		// is the one asking for the pause; the pre-dial check above keys
		// by the pre-redirect host, so a 429 at the end of a redirect
		// chain cools the answering host and not the source's — accepted,
		// the chain is the uncommon case.
		p.Hosts.Cooldown(hostOf(res.WireURL), res.RetryAfter)
		logger.Info("media health: origin asked for a pause; host cooled",
			zap.String("host", hostOf(res.WireURL)),
			zap.Int("status", res.Status),
			zap.Duration("retry_after", res.RetryAfter))
	}
	res.Recorded = p.record(source, res, logger)
	return res, true
}

// noteNetwork keeps the network-health evidence and marks an unreachable
// result NetworkHealthy when the device demonstrably reaches OTHER origins:
// controld's internet verdict is online, or a DIFFERENT host gave an
// end-to-end answer inside networkWindow.
//
// Three rules keep a failure from vouching for itself (PR #438 review,
// F22): the verdict is read from the evidence BEFORE this result is added
// to it; evidence from the failing host itself never counts (repeated
// failures from one origin cannot corroborate each other); and only an
// end-to-end answer is evidence — a 2xx–4xx, or a definitive NXDOMAIN from
// the resolver. A 5xx is not: a captive portal or a broken proxy answers
// 502 for every host, and counting those would let every work on the wall
// corroborate every other one into broken.
func (p *Prober) noteNetwork(res *ProbeResult) {
	now := time.Now
	if p.Now != nil {
		now = p.Now
	}
	at := now()
	host := hostOf(res.WireURL)
	p.answeredMu.Lock()
	defer p.answeredMu.Unlock()
	if res.Unreachable != "" {
		switch {
		case p.Online != nil && p.Online():
			res.NetworkHealthy = true
		case !p.answeredAt.IsZero() && at.Sub(p.answeredAt) <= networkWindow && p.answeredHost != host:
			res.NetworkHealthy = true
		}
	}
	endToEnd := (res.Status >= 200 && res.Status < 500) || (res.Verdict == VerdictDead && res.Reason == ReasonDNS)
	if endToEnd {
		p.answeredAt, p.answeredHost = at, host
	}
}

// wireHost is the hostname the probe would dial for source, the key the
// HostLimiter paces by.
func (p *Prober) wireHost(source string) string {
	if IsDataURI(source) {
		return ""
	}
	if p.Probe == nil {
		return hostOf(source)
	}
	wireURL, _ := p.Probe.wire(source)
	return hostOf(wireURL)
}

// cooled reports whether source's host is under a cooldown (the lookahead
// steps over such sources without holding them).
func (p *Prober) cooled(source string) bool {
	if p.Hosts == nil {
		return false
	}
	_, ok := p.Hosts.Cooled(p.wireHost(source))
	return ok
}

// record writes res and reports whether the store actually changed.
func (p *Prober) record(source string, res ProbeResult, logger *zap.Logger) bool {
	if p.Store == nil || !probeWritesRecord(res) {
		return false
	}
	if _, err := p.Store.ApplyProbe(source, res); err != nil {
		if !errors.Is(err, ErrLowDisk) {
			logger.Warn("media health: recording probe result failed",
				zap.String("source", res.Source), zap.Error(err))
		}
		return false
	}
	return true
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

type beyondPassError struct{}

func (beyondPassError) Error() string {
	return "media health: beyond the cast pass; left to the lookahead"
}

var errBeyondPass error = beyondPassError{}

// hostCooldownError marks a source not probed because its host asked for
// a pause. No Reason rides with it, so no record is written.
type hostCooldownError struct{ until time.Time }

func (e *hostCooldownError) Error() string {
	return "media health: host under a cooldown until " + e.until.UTC().Format(time.RFC3339)
}

type retryHoldError struct{}

func (retryHoldError) Error() string { return "media health: source under a transient retry hold" }

var errRetryHold error = retryHoldError{}
