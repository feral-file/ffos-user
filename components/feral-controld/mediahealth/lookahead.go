package mediahealth

import (
	"context"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Lookahead probes the items coming up on the wall and, when the wall is
// quiet, sweeps records that are due for another look.
//
// Fed by the status poller's playback observer (the same seam the
// automatic playback cache uses): every poll hands it the player's item
// list and index. It performs no I/O on the poller goroutine — it only
// retains the snapshot and wakes its worker. The worker:
//
//  1. Walks the playlist from index+1 (wrapping) and probes the first
//     Window sources that have no record or are due (Record.Due), one at a
//     time, with Gap between requests. The hint for a newly broken item
//     rides the next push (refresher, cutover, re-cast).
//  2. With nothing ahead to probe, takes one due record not on the wall
//     from the store's recheck index, under a token bucket of
//     SweepPerHour, so a 100k-record store never becomes 100k probes a
//     day and a fixed source still heals without being cast again.
//
// Admit, when set, is consulted before each request (the offline cache's
// resource gate, read-only); a refusal just waits for the next tick.
type Lookahead struct {
	Prober *Prober
	Store  *Store
	Logger *zap.Logger
	// Window is how many upcoming items are probed ahead (DefaultWindow
	// when 0).
	Window int
	// Gap is the pause between requests (DefaultGap when 0).
	Gap time.Duration
	// SweepPerHour bounds background re-probes of records not on the wall
	// (DefaultSweepPerHour when 0; negative disables the sweep).
	SweepPerHour int
	// Admit, when non-nil, gates each request on the device's resources.
	Admit func() bool
	// Now is the clock (time.Now when nil).
	Now func() time.Time

	mu      sync.Mutex
	sources []string
	index   int
	wake    chan struct{}
	once    sync.Once

	// held parks sources the worker just probed until the time recorded,
	// whatever the store says. A probe that writes no record (a source
	// guard or URL-length refusal: a policy objection, deliberately never
	// a verdict) leaves the record due forever; without this hold the
	// worker would re-probe that one source every tick and nothing behind
	// it — later window items, the current item's recheck, the sweep —
	// would ever run (PR #438 review, round 2, F10). probeHold matches the
	// store's shortest recheck, so a recorded source is never held longer
	// than its own record already holds it. Pruned on use.
	held map[string]time.Time

	sweepTokens float64
	sweepLast   time.Time
}

const (
	DefaultWindow       = 8
	DefaultGap          = time.Second
	DefaultSweepPerHour = 20
	// idleTick is how often the worker looks for due work with no wake.
	idleTick = time.Minute
	// probeHold is how long a just-probed source is left alone by the
	// worker regardless of its record (see Lookahead.held).
	probeHold = RetryTransient
	// maxObservedItems bounds the retained snapshot: a hostile 4 MiB cast
	// could otherwise park a huge slice here on every poll.
	maxObservedItems = 4096
)

// Observe retains the current playlist snapshot. items may be nil when the
// player is not displaying a playlist (the lookahead then only sweeps).
func (l *Lookahead) Observe(sources []string, index int) {
	if len(sources) > maxObservedItems || index < 0 || index >= len(sources) {
		sources, index = nil, 0
	}
	l.mu.Lock()
	changed := !slices.Equal(l.sources, sources) || l.index != index
	if changed {
		l.sources = slices.Clone(sources)
		l.index = index
	}
	l.mu.Unlock()
	if changed {
		l.kick()
	}
}

func (l *Lookahead) kick() {
	l.init()
	select {
	case l.wake <- struct{}{}:
	default:
	}
}

func (l *Lookahead) init() {
	l.once.Do(func() { l.wake = make(chan struct{}, 1) })
}

// Run is the worker loop; it returns when ctx is done.
func (l *Lookahead) Run(ctx context.Context) {
	l.init()
	logger := l.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	gap := l.Gap
	if gap <= 0 {
		gap = DefaultGap
	}
	ticker := time.NewTicker(idleTick)
	defer ticker.Stop()
	for {
		worked := l.step(ctx, logger)
		var wait <-chan time.Time
		if worked {
			wait = time.After(gap)
		}
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		case <-ticker.C:
		case <-wait:
		}
	}
}

// step does at most one request and reports whether it did one.
func (l *Lookahead) step(ctx context.Context, logger *zap.Logger) bool {
	if l.Prober == nil || l.Store == nil {
		return false
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	if l.Admit != nil && !l.Admit() {
		return false
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	at := now()
	l.pruneHeld(at)
	if src := l.nextAhead(at); src != "" {
		l.hold(src, at)
		res := l.Prober.ProbeAndRecord(ctx, src)
		logger.Debug("media health: lookahead probe", zap.String("source", res.Source), zap.String("verdict", string(res.Verdict)))
		return true
	}
	if src := l.nextDue(at); src != "" {
		l.hold(src, at)
		res := l.Prober.ProbeAndRecord(ctx, src)
		logger.Debug("media health: sweep probe", zap.String("source", res.Source), zap.String("verdict", string(res.Verdict)))
		return true
	}
	return false
}

func (l *Lookahead) hold(src string, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = make(map[string]time.Time)
	}
	l.held[src] = now.Add(probeHold)
}

// pruneHeld drops expired holds. The map is bounded by the probe rate
// (one per Gap) times probeHold, so pruning on every step keeps it small.
func (l *Lookahead) pruneHeld(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	for src, until := range l.held {
		if !now.Before(until) {
			delete(l.held, src)
		}
	}
}

// isHeld reports whether src is under an active hold (after pruneHeld).
func (l *Lookahead) isHeld(src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	_, ok := l.held[src]
	return ok
}

// nextAhead returns the first upcoming source that wants a look, or "".
func (l *Lookahead) nextAhead(now time.Time) string {
	l.mu.Lock()
	sources := l.sources
	index := l.index
	l.mu.Unlock()
	if len(sources) == 0 {
		return ""
	}
	window := l.Window
	if window <= 0 {
		window = DefaultWindow
	}
	candidates := make([]string, 0, window+1)
	for step := 1; step < len(sources) && len(candidates) < window; step++ {
		src := sources[(index+step)%len(sources)]
		if src == "" || IsDataURI(src) {
			continue
		}
		candidates = append(candidates, src)
	}
	// The current item last: it is already on screen, so nothing ahead
	// depends on it, but a one-item playlist (or one parked on its current
	// item) would otherwise never get its scheduled recheck.
	if cur := sources[index]; cur != "" && !IsDataURI(cur) {
		candidates = append(candidates, cur)
	}
	if len(candidates) == 0 {
		return ""
	}
	records, err := l.Store.GetMany(candidates)
	if err != nil {
		return ""
	}
	for _, src := range candidates {
		if records[src].Due(now) && !l.isHeld(src) {
			return src
		}
	}
	return ""
}

// nextDue takes one due record not on the wall, under the sweep budget.
func (l *Lookahead) nextDue(now time.Time) string {
	perHour := l.SweepPerHour
	if perHour == 0 {
		perHour = DefaultSweepPerHour
	}
	if perHour < 0 {
		return ""
	}
	window := l.Window
	if window <= 0 {
		window = DefaultWindow
	}
	l.mu.Lock()
	if l.sweepLast.IsZero() {
		l.sweepTokens = float64(perHour)
	} else {
		l.sweepTokens += now.Sub(l.sweepLast).Hours() * float64(perHour)
		if l.sweepTokens > float64(perHour) {
			l.sweepTokens = float64(perHour)
		}
	}
	l.sweepLast = now
	if l.sweepTokens < 1 {
		l.mu.Unlock()
		return ""
	}
	// Excluded inside the query: the window the walk above owns (the
	// current item and the next Window), and held sources. NOT the rest
	// of the wall: a due record further ahead on the wall is reached by
	// nothing else until playback approaches it, so an expired verdict
	// there would read `broken` to the app for hours without a hint
	// behind it (#392 follow-up); the sweep rechecks it under its budget.
	exclude := make(map[string]struct{}, len(l.held)+window+1)
	for _, s := range l.windowSources(window) {
		exclude[s] = struct{}{}
	}
	for s := range l.held {
		exclude[s] = struct{}{}
	}
	l.mu.Unlock()

	due, err := l.Store.Due(now, 8, exclude)
	if err != nil {
		return ""
	}
	// A due data: record (a malformed URI the cast pass recorded) is
	// re-settled through the same path: ProbeOne answers it synchronously
	// with no network, and the broken backoff spaces the rechecks out.
	// Skipping it here would let a handful of them shadow the rows behind.
	if len(due) > 0 {
		l.mu.Lock()
		l.sweepTokens--
		l.mu.Unlock()
		return due[0]
	}
	return ""
}

// windowSources is the current item and the next window items, the
// sources nextAhead owns. Caller holds l.mu.
func (l *Lookahead) windowSources(window int) []string {
	if len(l.sources) == 0 {
		return nil
	}
	out := make([]string, 0, window+1)
	out = append(out, l.sources[l.index])
	for step := 1; step < len(l.sources) && step <= window; step++ {
		out = append(out, l.sources[(l.index+step)%len(l.sources)])
	}
	return out
}
