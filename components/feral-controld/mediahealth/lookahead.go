package mediahealth

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"
)

// Lookahead keeps the wall judged: it walks the playlist from the player's
// position and probes whatever is due, then, with the wall quiet, sweeps
// records that are not on the wall.
//
// Fed by the status poller's playback observer (the same seam the
// automatic playback cache uses): every poll hands it the player's item
// list and index. It performs no I/O on the poller goroutine — it only
// retains the snapshot and wakes its worker. The worker runs ONE rule:
//
//   - Starting after the current item, walk the wall in play order and
//     probe the first source that is due (Record.Due: no record, an
//     expired verdict, or a passed retry hold). The next items come first
//     because they are first in the walk; the rest of the wall follows,
//     and the current item comes last — it is already on screen, but a
//     one-item playlist would otherwise never get its recheck. A cursor
//     remembers where the walk stopped, so each step reads one chunk of
//     records, not the whole wall, and resets when the playlist or the
//     index changes so the next items always come first again. There is
//     no cap on the wall: the refresher re-resolves a playlist with no
//     item limit, so the wall holds everything the query returns, and a
//     wall the lookahead refused to see played on unjudged (#392).
//   - With nothing due on the wall, take one due record NOT on the wall
//     from the store's recheck index, under a token bucket of
//     SweepPerHour, so a 100k-record store never becomes 100k probes a
//     day and a fixed source still heals without being cast again.
//
// Pace: the next probe starts as soon as the previous one finishes — no
// gap. Politeness is the Prober's shared HostLimiter alone: at most two
// requests in flight per host (this worker is one at a time), and a host
// answering 429/503 is paused for its Retry-After; a source on a paused
// host is stepped over, not held. Against one CDN this is a few requests
// a second at typical answer times, and the cooldown is the backstop.
//
// Admit, when set, is consulted before each request (the offline cache's
// resource gate, read-only); a refusal just waits for the next tick.
type Lookahead struct {
	Prober *Prober
	Store  *Store
	Logger *zap.Logger
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
	// wallSet is the sources on the wall, for the sweep's exclusion;
	// rebuilt (never mutated) when the snapshot changes, so a reader may
	// hold the map outside mu.
	wallSet map[string]struct{}
	// cursor is the walk position (0 = right after the current item) the
	// next step resumes from.
	cursor int
	wake   chan struct{}
	once   sync.Once

	// held parks sources the worker just probed until the time recorded,
	// but only when the probe wrote no record (ProbeResult.Recorded false:
	// a source guard or URL-length refusal, a write the store refused).
	// Such a source stays due forever, and without this hold the worker
	// would re-pick it every step and nothing behind it would ever run
	// (PR #438 review, F10/F21). A recorded answer is governed by its
	// record alone (F19). Pruned on use.
	held map[string]time.Time

	sweepTokens float64
	sweepLast   time.Time
}

const (
	DefaultSweepPerHour = 20
	// idleTick is how often the worker looks for due work with no wake.
	idleTick = time.Minute
	// probeHold is how long an unrecorded probe keeps its source out of
	// the walk (see Lookahead.held).
	probeHold = RetryTransient
	// walkChunk is how many wall positions one store read covers while
	// the walk looks for the next due source.
	walkChunk = 64
)

// Observe retains the current playlist snapshot. items may be nil when the
// player is not displaying a playlist (the lookahead then only sweeps).
func (l *Lookahead) Observe(sources []string, index int) {
	if index < 0 || index >= len(sources) {
		sources, index = nil, 0
	}
	l.mu.Lock()
	changed := !slices.Equal(l.sources, sources) || l.index != index
	if changed {
		l.sources = slices.Clone(sources)
		l.index = index
		l.cursor = 0
		set := make(map[string]struct{}, len(sources))
		for _, s := range sources {
			if s != "" {
				set[s] = struct{}{}
			}
		}
		l.wallSet = set
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
	ticker := time.NewTicker(idleTick)
	defer ticker.Stop()
	for {
		if ctx.Err() != nil {
			return
		}
		if l.step(ctx, logger) {
			// A request was made; look for the next one right away.
			continue
		}
		select {
		case <-ctx.Done():
			return
		case <-l.wake:
		case <-ticker.C:
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
	if src := l.nextOnWall(at); src != "" {
		l.probe(ctx, src, at, "wall", logger)
		return true
	}
	if src := l.nextDue(at); src != "" {
		l.probe(ctx, src, at, "sweep", logger)
		return true
	}
	return false
}

// probe runs one request and holds the source only when the result did not
// change the store (ProbeResult.Recorded); a source skipped for a host
// cooldown is held only until the cooldown ends.
func (l *Lookahead) probe(ctx context.Context, src string, at time.Time, kind string, logger *zap.Logger) {
	// Held while the request runs, so nothing else picks it meanwhile.
	l.hold(src, at.Add(probeHold))
	res := l.Prober.ProbeAndRecord(ctx, src)
	var cooled *hostCooldownError
	switch {
	case res.Recorded:
		l.release(src)
	case errors.As(res.Err, &cooled):
		l.hold(src, cooled.until)
	}
	logger.Debug("media health: "+kind+" probe", zap.String("source", res.Source), zap.String("verdict", string(res.Verdict)))
}

func (l *Lookahead) hold(src string, until time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.held == nil {
		l.held = make(map[string]time.Time)
	}
	l.held[src] = until
}

func (l *Lookahead) release(src string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.held, src)
}

// pruneHeld drops expired holds. The map is bounded by the probe rate
// times probeHold, so pruning on every step keeps it small.
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

// walkSource is the wall source at walk position pos: the items after the
// current one in play order, wrapping, with the current item last.
func walkSource(sources []string, index, pos int) string {
	return sources[(index+1+pos)%len(sources)]
}

// nextOnWall returns the next due source on the wall from the cursor,
// scanning the whole wall at most once (wrapping), or "" when nothing on
// the wall is due. The cursor is left just past the source returned, so
// the following step continues from there; it is reset to the start when
// a full scan found nothing.
func (l *Lookahead) nextOnWall(now time.Time) string {
	l.mu.Lock()
	sources := l.sources
	index := l.index
	start := l.cursor
	l.mu.Unlock()
	n := len(sources)
	if n == 0 {
		return ""
	}
	if start < 0 || start >= n {
		start = 0
	}
	for scanned := 0; scanned < n; {
		from := (start + scanned) % n
		chunk := min(walkChunk, n-scanned)
		candidates := make([]string, 0, chunk)
		positions := make([]int, 0, chunk)
		for i := 0; i < chunk; i++ {
			pos := (from + i) % n
			// data: items are walked too: a due one is settled
			// synchronously with no network (ProbeOne decodes it), and its
			// record then keeps it off the walk until its recheck.
			src := walkSource(sources, index, pos)
			if src == "" {
				continue
			}
			candidates = append(candidates, src)
			positions = append(positions, pos)
		}
		scanned += chunk
		if len(candidates) == 0 {
			continue
		}
		records, err := l.Store.GetMany(candidates)
		if err != nil {
			return ""
		}
		for i, src := range candidates {
			if records[src].Due(now) && !l.isHeld(src) && !l.Prober.cooled(src) {
				l.mu.Lock()
				if len(l.sources) == n {
					l.cursor = (positions[i] + 1) % n
				}
				l.mu.Unlock()
				return src
			}
		}
	}
	l.mu.Lock()
	l.cursor = 0
	l.mu.Unlock()
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
	// Excluded INSIDE the query, before its limit, so neither can fill the
	// result and shadow an eligible off-wall row (PR #438 review, F8): the
	// whole wall (the walk above owns it) and the sources the worker holds.
	// The wall set is a shared read-only snapshot, never copied per call;
	// the held map is small (bounded by the probe rate times probeHold), so
	// it is cloned.
	wall := l.wallSet
	held := maps.Clone(l.held)
	l.mu.Unlock()
	skip := func(src string) bool {
		if _, onWall := wall[src]; onWall {
			return true
		}
		_, isHeld := held[src]
		return isHeld
	}

	due, err := l.Store.Due(now, 1, skip)
	if err != nil || len(due) == 0 {
		return ""
	}
	// A due data: record (a malformed URI the cast pass recorded) is
	// re-settled through the same path: ProbeOne answers it synchronously
	// with no network, and the broken backoff spaces the rechecks out.
	l.mu.Lock()
	l.sweepTokens--
	l.mu.Unlock()
	return due[0]
}
