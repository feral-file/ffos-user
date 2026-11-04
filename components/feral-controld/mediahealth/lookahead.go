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

	sweepTokens float64
	sweepLast   time.Time
}

const (
	DefaultWindow       = 8
	DefaultGap          = time.Second
	DefaultSweepPerHour = 20
	// idleTick is how often the worker looks for due work with no wake.
	idleTick = time.Minute
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
	if src := l.nextAhead(now()); src != "" {
		res := l.Prober.ProbeAndRecord(ctx, src)
		logger.Debug("media health: lookahead probe", zap.String("source", res.Source), zap.String("verdict", string(res.Verdict)))
		return true
	}
	if src := l.nextDue(now()); src != "" {
		res := l.Prober.ProbeAndRecord(ctx, src)
		logger.Debug("media health: sweep probe", zap.String("source", res.Source), zap.String("verdict", string(res.Verdict)))
		return true
	}
	return false
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
	candidates := make([]string, 0, window)
	for step := 1; step < len(sources) && len(candidates) < window; step++ {
		src := sources[(index+step)%len(sources)]
		if src == "" || IsDataURI(src) {
			continue
		}
		candidates = append(candidates, src)
	}
	if len(candidates) == 0 {
		return ""
	}
	records, err := l.Store.GetMany(candidates)
	if err != nil {
		return ""
	}
	for _, src := range candidates {
		if records[src].Due(now) {
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
	onWall := make(map[string]struct{}, len(l.sources))
	for _, s := range l.sources {
		onWall[s] = struct{}{}
	}
	l.mu.Unlock()

	due, err := l.Store.Due(now, 8)
	if err != nil {
		return ""
	}
	for _, src := range due {
		if _, playing := onWall[src]; playing || IsDataURI(src) {
			continue
		}
		l.mu.Lock()
		l.sweepTokens--
		l.mu.Unlock()
		return src
	}
	return ""
}
