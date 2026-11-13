package mediahealth

import (
	"context"
	"errors"
	"image"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"
)

// WallCapture judges what the wall actually shows (feral-file/ffos-user#392,
// "L1 as play evidence"): a fixed time after an HTML work is on screen it
// captures the artwork from the kiosk — the real GPU, size and timing, which
// the nightly headless render only approximates — classifies the frame with
// the L1 classifier, and feeds the verdict into the play run as a StageFrame
// outcome. A rendered frame is the play's evidence for an HTML work (an
// iframe's `ready` is not); a blank or known-bad frame is a counted play
// failure.
//
// Off by default (mediaHealth.wallCapture.enabled): it screenshots the live
// kiosk. One capture at a time, at most once per mount, only while the panel
// is awake, only for HTML works (isSoftwareRecord), and a source whose last
// frame was ok is left alone for WallRecheckOK. The frame never leaves the
// device; only its variance and hash are kept.
type WallCapture struct {
	cfg WallCaptureConfig

	mu sync.Mutex
	// mount is the work on screen: the item at the player's index while
	// renderStatus is 2 (committed and on screen). A different source or
	// index, or leaving 2, is a new mount.
	mount wallMount
	// lastPollAt is when Observe last ran: the poll before a mount is first
	// seen is where that mount began (wallMount.startedAt).
	lastPollAt time.Time
}

type wallMount struct {
	seq      uint64
	source   string
	index    int
	readyAt  time.Time
	captured bool
	// startedAt is the last status poll that showed the previous item on
	// screen (or nothing): the mount began after it — through every
	// pending poll before it was first seen ready, since the kiosk's
	// document navigations happen while the player reports pending (PR
	// #450 review, F18). Bounds which navigations belong to this mount
	// (FrameEvidence.MountedAt).
	startedAt time.Time
	// pending marks a placeholder: polls that showed nothing ready since
	// the previous mount. It keeps startedAt through them — even a zero
	// one, on a fresh daemon with no poll before the first (PR #450
	// review, F18 second round): a zero start bounds nothing, which is
	// right, since every navigation seen so far is this mount's.
	pending bool
}

// WallFrame is one capture of the artwork on the wall, with the player's own
// UI over it as masks (image coordinates).
type WallFrame struct {
	Image   image.Image
	Masks   []image.Rectangle
	Clipped bool
}

// WallCapturer captures the artwork on the wall (the screenshot package's
// CaptureArtwork, adapted in main). ErrFrameUnavailable (wrapped) means
// there was nothing to judge: the capturer was busy, the artwork was
// covered by an overlay, the kiosk could not be reached.
type WallCapturer interface {
	CaptureWall(ctx context.Context) (*WallFrame, error)
}

// ErrFrameUnavailable marks a capture that is not evidence.
var ErrFrameUnavailable = errors.New("media health: wall frame unavailable")

// Wall-capture defaults.
const (
	// DefaultWallSettle is how long a work has been on screen before the
	// capture. The kiosk renders on its GPU, so it needs less than the
	// nightly headless render (DefaultSettle).
	DefaultWallSettle = 15 * time.Second
	// WallRecheckOK spaces captures of a source whose last frame rendered:
	// a screenshot of the live wall is not free, and an ok frame needs no
	// corroboration. A source with a failing frame is captured every play,
	// which is how a second play corroborates the first.
	WallRecheckOK = 24 * time.Hour
	// DefaultWallMinUnmasked is the least share of the frame outside the
	// player's UI that still says something.
	DefaultWallMinUnmasked = 0.5
	// defaultWallTick is how often the runner checks the pending mount.
	defaultWallTick = time.Second
	// wallCaptureTimeout bounds one capture.
	wallCaptureTimeout = 15 * time.Second
)

// WallCaptureConfig wires the runner.
type WallCaptureConfig struct {
	Store    *Store
	Capturer WallCapturer
	// Deliver folds a frame outcome into the play run (the Service's
	// ObserveWallFrame).
	Deliver func(Outcome)
	// Awake reports the panel showing the wall (not asleep); nil = awake.
	Awake func() bool
	// Settle is DefaultWallSettle when 0.
	Settle time.Duration
	// KnownBad and BlankVarianceThreshold are the L1 classifier's.
	KnownBad               []Fingerprint
	BlankVarianceThreshold float64
	MinUnmasked            float64
	Logger                 *zap.Logger
	Now                    func() time.Time
	Tick                   time.Duration
}

// NewWallCapture builds the runner with defaults filled in.
func NewWallCapture(cfg WallCaptureConfig) *WallCapture {
	if cfg.Settle <= 0 {
		cfg.Settle = DefaultWallSettle
	}
	if cfg.BlankVarianceThreshold <= 0 || cfg.BlankVarianceThreshold >= MaxVariance {
		cfg.BlankVarianceThreshold = DefaultBlankVarianceThreshold
	}
	if cfg.MinUnmasked <= 0 || cfg.MinUnmasked > 1 {
		cfg.MinUnmasked = DefaultWallMinUnmasked
	}
	if cfg.Logger == nil {
		cfg.Logger = zap.NewNop()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Tick <= 0 {
		cfg.Tick = defaultWallTick
	}
	if cfg.Awake == nil {
		cfg.Awake = func() bool { return true }
	}
	return &WallCapture{cfg: cfg}
}

// Observe takes one player status poll: the wall's sources, the index on
// screen and its render status (2 = committed and on screen). nil-safe.
func (w *WallCapture) Observe(sources []string, index int, renderStatus *int) {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	now := w.cfg.Now()
	prevPoll := w.lastPollAt
	w.lastPollAt = now
	ready := renderStatus != nil && *renderStatus == 2 && index >= 0 && index < len(sources)
	if !ready {
		// The next mount begins after the last poll that still showed the
		// previous one: the first pending poll fixes startedAt, later
		// pending polls keep it.
		startedAt := prevPoll
		if w.mount.pending {
			startedAt = w.mount.startedAt
		}
		w.mount = wallMount{seq: w.mount.seq + 1, startedAt: startedAt, pending: true}
		return
	}
	src := sources[index]
	if w.mount.readyAt.IsZero() || w.mount.source != src || w.mount.index != index {
		startedAt := prevPoll
		if w.mount.pending {
			startedAt = w.mount.startedAt // seen pending first: the mount began before those polls
		}
		w.mount = wallMount{seq: w.mount.seq + 1, source: src, index: index, readyAt: now, startedAt: startedAt}
	}
}

// Run captures each due mount until ctx ends.
func (w *WallCapture) Run(ctx context.Context) {
	t := time.NewTicker(w.cfg.Tick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			w.Step(ctx)
		}
	}
}

// Step captures the mount on screen if it is due; it reports whether it
// captured. Exposed for tests.
func (w *WallCapture) Step(ctx context.Context) bool {
	w.mu.Lock()
	m := w.mount
	w.mu.Unlock()
	now := w.cfg.Now()
	if m.source == "" || m.captured || now.Sub(m.readyAt) < w.cfg.Settle || IsDataURI(m.source) || !w.cfg.Awake() {
		return false
	}
	// Whatever happens next, this mount is done: one capture per mount.
	if !w.claim(m.seq) {
		return false
	}
	rec, err := w.cfg.Store.Get(m.source)
	if err != nil || rec == nil || !isSoftwareRecord(rec) {
		return false
	}
	if rec.Wall != nil && rec.Wall.Verdict == RenderOK && rec.State == StateOK && now.Sub(rec.Wall.At) < WallRecheckOK {
		return false
	}

	capCtx, cancel := context.WithTimeout(ctx, wallCaptureTimeout)
	defer cancel()
	frame, err := w.cfg.Capturer.CaptureWall(capCtx)
	if err != nil {
		w.cfg.Logger.Debug("media health: wall capture skipped", zap.String("source", redactForLog(m.source)), zap.Error(err))
		return false
	}
	// The wall may have moved on while the capture ran: a frame of the
	// next work must not be filed under this one.
	w.mu.Lock()
	moved := w.mount.seq != m.seq
	w.mu.Unlock()
	if moved {
		w.cfg.Logger.Debug("media health: wall changed during the capture; frame dropped", zap.String("source", redactForLog(m.source)))
		return false
	}
	if UnmaskedFraction(frame.Image, frame.Masks) < w.cfg.MinUnmasked {
		w.cfg.Logger.Debug("media health: wall frame mostly covered by the player's UI; not judged", zap.String("source", redactForLog(m.source)))
		return false
	}
	known := append(slices.Clone(w.cfg.KnownBad), w.cfg.Store.LearnedFingerprints()...)
	c, err := ClassifyMasked(frame.Image, frame.Masks, known, w.cfg.BlankVarianceThreshold)
	if err != nil {
		w.cfg.Logger.Warn("media health: classifying a wall frame failed", zap.Error(err))
		return false
	}
	o := Outcome{Source: m.source, Kind: OutcomeFail, Stage: StageFrame, Code: string(c.Verdict),
		frame: &FrameEvidence{PHash: c.PHash, Variance: c.Variance, Label: c.Label, Clipped: frame.Clipped, MountedAt: m.startedAt}}
	if c.Verdict == RenderOK {
		o.Kind = OutcomeOK
	}
	w.cfg.Logger.Info("media health: wall frame classified",
		zap.String("source", redactForLog(m.source)), zap.String("verdict", string(c.Verdict)),
		zap.Float64("variance", c.Variance), zap.Bool("clipped", frame.Clipped), zap.Int("masks", len(frame.Masks)))
	w.cfg.Deliver(o)
	return true
}

// claim marks mount seq captured; false when the mount changed or was
// already claimed.
func (w *WallCapture) claim(seq uint64) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.mount.seq != seq || w.mount.captured {
		return false
	}
	w.mount.captured = true
	return true
}
