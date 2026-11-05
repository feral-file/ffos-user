package mediahealth

// L1: the nightly render probe (feral-file/ffos-user#392, docs/media-health.md
// §6). An experiment, off by default (mediaHealth.l1.enabled). While the
// panel sleeps it renders software-class sources in the offline cache's
// headless Chromium, captures the viewport after a settle, and classifies
// the frame: a known-bad fingerprint (an error page that paints fine), a
// blank frame (confirmed by a second, longer render), or a real render.
// Its verdicts are stored under the record's `l1` sub-record and shown to
// the app as `suspect`; they are NEVER a hint and NEVER count toward cast
// rejection — a blank frame is weaker evidence than a byte-level failure,
// and a false blank hides real art, the worst outcome here.
//
// The port keeps ff-indexer-v2's classifier contract (internal/media/probe,
// internal/media/phash): the same pHash algorithm (goimagehash's
// PerceptionHash — the bit order is part of the persisted contract), the
// same per-channel variance blank signal and default threshold, the same
// verdict ladder (fingerprint first, then blank), and the same debounce (a
// blank concludes only on a second consecutive blank, rendered with the
// longer confirmation settle).

import (
	"context"
	"errors"
	"fmt"
	"image"
	"math/bits"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/corona10/goimagehash"
	"go.uber.org/zap"
)

// Frame is one rendered viewport.
type Frame struct {
	Image image.Image
	// MainStatus is the HTTP status of the main document, 0 when the
	// renderer could not observe it.
	MainStatus int
}

// Renderer renders wireURL in the headless browser, waits settle, and
// captures the viewport. It is implemented by the offline cache (which owns
// the browser and its source guard) and injected here, so this package
// stays free of browser code. ErrRendererUnavailable (wrapped) means the
// browser could not be reached at all: no evidence, retried next night.
type Renderer interface {
	Render(ctx context.Context, wireURL string, settle time.Duration) (*Frame, error)
}

// ErrRendererUnavailable: the headless browser could not be started or
// dialed. Not evidence about the source.
var ErrRendererUnavailable = errors.New("media health: renderer unavailable")

// RenderVerdict is an L1 verdict.
type RenderVerdict string

const (
	// RenderOK: a real, non-uniform frame that matched no fingerprint.
	RenderOK RenderVerdict = "ok"
	// RenderBlank: a near-uniform frame. Concludes (StateSuspect) only on a
	// second consecutive blank.
	RenderBlank RenderVerdict = "blank"
	// RenderKnownBad: the frame matched a known-bad fingerprint. Concludes
	// immediately.
	RenderKnownBad RenderVerdict = "known_bad"
	// RenderStalled: no frame to judge — navigation failed, the main
	// document was not 2xx, the capture timed out, or the browser was
	// unavailable. Never evidence; retried after RecheckL1Stalled.
	RenderStalled RenderVerdict = "stalled"
)

// L1Verdict is the record's `l1` sub-record: the last render and when the
// next one is due. It never feeds the recheck index (Record.dueAt) — the
// L0 lookahead and sweep must not pick L1 work up — so candidates are
// found by Store.L1Candidates instead.
type L1Verdict struct {
	Verdict  RenderVerdict `json:"verdict"`
	PHash    string        `json:"phash,omitempty"`
	Variance float64       `json:"variance,omitempty"`
	// Label is the matched fingerprint's label on a known_bad verdict.
	Label string    `json:"label,omitempty"`
	At    time.Time `json:"at"`
	Next  time.Time `json:"next"`
	// ConsecutiveBlank counts blank renders in a row; the debounce
	// concludes at 2 and any non-blank frame resets it.
	ConsecutiveBlank int `json:"consecutiveBlank,omitempty"`
	// MainStatus is the main document's HTTP status on the last render.
	MainStatus int    `json:"mainStatus,omitempty"`
	LastError  string `json:"lastError,omitempty"`
}

// L1 recheck policy (own constants: an L1 verdict is weaker than L0's).
const (
	// RecheckL1OK: a frame that rendered is looked at again after this.
	RecheckL1OK = 7 * 24 * time.Hour
	// RecheckL1Suspect: a suspect record is re-rendered after this, so a
	// fixed page or a transient blank heals within a day.
	RecheckL1Suspect = 24 * time.Hour
	// RecheckL1Stalled: no evidence; try again the next night.
	RecheckL1Stalled = 24 * time.Hour

	// DefaultBlankVarianceThreshold is the indexer's
	// render_probe.blank_variance_threshold default.
	DefaultBlankVarianceThreshold = 0.001
	// MaxVariance bounds Variance (half the samples at 0, half at 1); a
	// threshold at or above it classifies every frame blank.
	MaxVariance = 0.25
	// DefaultSettle / DefaultConfirmSettle mirror the indexer's
	// render_probe.settle_ms / confirm_settle_ms.
	DefaultSettle        = 15 * time.Second
	DefaultConfirmSettle = 30 * time.Second
	// DefaultMaxPerNight bounds renders per local calendar day.
	DefaultMaxPerNight = 200
	// DefaultL1Tick is how often the runner re-checks its gate.
	DefaultL1Tick = time.Minute
	// defaultGateWatch is how often an in-flight render re-checks the gate
	// so a wake (or a cast) cancels it rather than letting it finish.
	defaultGateWatch = 2 * time.Second
)

// Fingerprint identifies a known-bad render (a gateway error page, a
// placeholder) by its pHash: every frame within MaxDistance of it is
// known_bad immediately.
type Fingerprint struct {
	Hash        uint64
	MaxDistance int
	Label       string
}

// ParseFingerprint converts an operator-supplied hex pHash (with or without
// 0x) into a Fingerprint, rejecting values that cannot be a 64-bit hash so
// a config typo is logged instead of silently never matching.
func ParseFingerprint(phashHex string, maxDistance int, label string) (Fingerprint, error) {
	cleaned := strings.TrimPrefix(strings.TrimSpace(strings.ToLower(phashHex)), "0x")
	if cleaned == "" || len(cleaned) > 16 {
		return Fingerprint{}, fmt.Errorf("fingerprint %q: phash must be 1-16 hex digits, got %q", label, phashHex)
	}
	hash, err := strconv.ParseUint(cleaned, 16, 64)
	if err != nil {
		return Fingerprint{}, fmt.Errorf("fingerprint %q: invalid phash hex %q: %w", label, phashHex, err)
	}
	if maxDistance < 0 || maxDistance > 64 {
		return Fingerprint{}, fmt.Errorf("fingerprint %q: maxDistance must be 0-64, got %d", label, maxDistance)
	}
	return Fingerprint{Hash: hash, MaxDistance: maxDistance, Label: label}, nil
}

// PHash returns the 64-bit DCT perceptual hash of img (goimagehash's
// PerceptionHash: 32x32 grayscale DCT, top-left 8x8, median threshold).
func PHash(img image.Image) (uint64, error) {
	h, err := goimagehash.PerceptionHash(img)
	if err != nil {
		return 0, fmt.Errorf("perception hash: %w", err)
	}
	return h.GetHash(), nil
}

// HashDistance is the Hamming distance between two pHashes.
func HashDistance(a, b uint64) int { return bits.OnesCount64(a ^ b) }

// Variance returns the image's largest per-channel population variance,
// normalized to the [0,1] channel range (0 = perfectly uniform). The max
// across R, G and B rather than luma: vivid art varying in hue but not
// brightness is not blank, and a frame is blank only when every channel is
// flat. Samples every 4th pixel in each dimension.
func Variance(img image.Image) float64 {
	bounds := img.Bounds()
	if bounds.Empty() {
		return 0
	}
	const stride = 4
	var sum, sumSq [3]float64
	var n int
	for y := bounds.Min.Y; y < bounds.Max.Y; y += stride {
		for x := bounds.Min.X; x < bounds.Max.X; x += stride {
			r, g, b, _ := img.At(x, y).RGBA()
			channels := [3]float64{float64(r) / 65535.0, float64(g) / 65535.0, float64(b) / 65535.0}
			for i, v := range channels {
				sum[i] += v
				sumSq[i] += v * v
			}
			n++
		}
	}
	if n == 0 {
		return 0
	}
	var maxVar float64
	for i := range sum {
		mean := sum[i] / float64(n)
		if v := sumSq[i]/float64(n) - mean*mean; v > maxVar {
			maxVar = v
		}
	}
	return maxVar
}

// Classification is the pure verdict over one frame.
type Classification struct {
	Verdict  RenderVerdict
	PHash    uint64
	Variance float64
	Label    string
}

// Classify applies the verdict ladder: known-bad fingerprint first (an error
// page is usually not blank, and the specific label is the better answer),
// then blank, else ok.
func Classify(img image.Image, fingerprints []Fingerprint, blankVarianceThreshold float64) (Classification, error) {
	hash, err := PHash(img)
	if err != nil {
		return Classification{}, err
	}
	c := Classification{Verdict: RenderOK, PHash: hash, Variance: Variance(img)}
	for _, fp := range fingerprints {
		if HashDistance(hash, fp.Hash) <= fp.MaxDistance {
			c.Verdict = RenderKnownBad
			c.Label = fp.Label
			return c, nil
		}
	}
	if c.Variance < blankVarianceThreshold {
		c.Verdict = RenderBlank
	}
	return c, nil
}

// RenderResult is what one render contributes to the record.
type RenderResult struct {
	Verdict    RenderVerdict
	PHash      uint64
	Variance   float64
	Label      string
	MainStatus int
	Err        string
}

// L1Config configures the nightly runner.
type L1Config struct {
	Store    *Store
	Renderer Renderer
	// Asleep reports whether the panel is asleep (the schedule wants it
	// and the player confirmed). The runner renders only while it is true
	// and cancels an in-flight render when it turns false.
	Asleep func() bool
	// Admit, when set, is the device resource gate (the offline cache's,
	// read-only); a refusal just waits for the next tick.
	Admit  func() bool
	Logger *zap.Logger
	Now    func() time.Time

	MaxPerNight            int
	BlankVarianceThreshold float64
	KnownBad               []Fingerprint
	Settle                 time.Duration
	ConfirmSettle          time.Duration
	Tick                   time.Duration
	// GateWatch is how often an in-flight render re-checks Asleep
	// (defaultGateWatch when 0).
	GateWatch time.Duration
}

// L1 is the nightly render runner. One render at a time; the per-night cap
// counts renders per local calendar day.
type L1 struct {
	cfg    L1Config
	logger *zap.Logger
	now    func() time.Time

	nightDay   string
	nightCount int
}

// NewL1 builds the runner. Store, Renderer and Asleep are required.
func NewL1(cfg L1Config) *L1 {
	if cfg.MaxPerNight <= 0 {
		cfg.MaxPerNight = DefaultMaxPerNight
	}
	if cfg.BlankVarianceThreshold <= 0 || cfg.BlankVarianceThreshold >= MaxVariance {
		cfg.BlankVarianceThreshold = DefaultBlankVarianceThreshold
	}
	if cfg.Settle <= 0 {
		cfg.Settle = DefaultSettle
	}
	if cfg.ConfirmSettle <= 0 {
		cfg.ConfirmSettle = DefaultConfirmSettle
	}
	if cfg.Tick <= 0 {
		cfg.Tick = DefaultL1Tick
	}
	if cfg.GateWatch <= 0 {
		cfg.GateWatch = defaultGateWatch
	}
	l := &L1{cfg: cfg, logger: cfg.Logger, now: cfg.Now}
	if l.logger == nil {
		l.logger = zap.NewNop()
	}
	if l.now == nil {
		l.now = time.Now
	}
	return l
}

// Run is the worker loop; it returns when ctx is done. Every tick it
// renders at most one candidate while the gate holds.
func (l *L1) Run(ctx context.Context) {
	ticker := time.NewTicker(l.cfg.Tick)
	defer ticker.Stop()
	for {
		// Keep rendering back to back while the gate holds and candidates
		// remain; otherwise wait a tick.
		for l.Step(ctx) {
			if ctx.Err() != nil {
				return
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (l *L1) gate() bool {
	if l.cfg.Asleep == nil || !l.cfg.Asleep() {
		return false
	}
	if l.cfg.Admit != nil && !l.cfg.Admit() {
		return false
	}
	return true
}

// Step renders one due candidate if the gate holds and the nightly cap is
// not spent; it reports whether it rendered.
func (l *L1) Step(ctx context.Context) bool {
	if l.cfg.Store == nil || l.cfg.Renderer == nil || !l.gate() {
		return false
	}
	now := l.now()
	day := now.Local().Format("2006-01-02")
	if day != l.nightDay {
		l.nightDay, l.nightCount = day, 0
	}
	if l.nightCount >= l.cfg.MaxPerNight {
		return false
	}
	cands, err := l.cfg.Store.L1Candidates(now, 1)
	if err != nil {
		l.logger.Warn("media health: l1 candidate scan failed", zap.Error(err))
		return false
	}
	if len(cands) == 0 {
		return false
	}
	rec, err := l.cfg.Store.Get(cands[0])
	if err != nil || rec == nil {
		return false
	}
	l.nightCount++
	l.renderAndRecord(ctx, rec)
	return true
}

func (l *L1) renderAndRecord(ctx context.Context, rec *Record) {
	settle := l.cfg.Settle
	confirm := rec.L1 != nil && rec.L1.Verdict == RenderBlank && rec.L1.ConsecutiveBlank == 1
	if confirm {
		settle = l.cfg.ConfirmSettle
	}
	wire := rec.WireURL
	if wire == "" {
		wire = rec.Source
	}

	// Cancel on wake: the gate is re-checked while the render runs, so a
	// panel that wakes (or a cast that interrupts the night) stops the
	// headless browser's work instead of letting it finish beside the wall.
	renderCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	watchDone := make(chan struct{})
	var gateClosed atomic.Bool
	go func() {
		defer close(watchDone)
		t := time.NewTicker(l.cfg.GateWatch)
		defer t.Stop()
		for {
			select {
			case <-renderCtx.Done():
				return
			case <-t.C:
				if !l.gate() {
					gateClosed.Store(true)
					cancel()
					return
				}
			}
		}
	}()
	frame, err := l.cfg.Renderer.Render(renderCtx, wire, settle)
	cancel()
	<-watchDone

	res := RenderResult{}
	switch {
	case err != nil:
		if gateClosed.Load() || ctx.Err() != nil {
			// Canceled by the gate (or shutdown): not evidence and not a
			// stalled verdict either; the candidate stays due.
			l.logger.Info("media health: l1 render canceled (panel woke or gate closed)", zap.String("source", redactForLog(rec.Source)))
			return
		}
		res.Verdict = RenderStalled
		res.Err = err.Error()
		if errors.Is(err, ErrRendererUnavailable) {
			l.logger.Warn("media health: l1 renderer unavailable", zap.Error(err))
		}
	case frame == nil || frame.Image == nil:
		res.Verdict = RenderStalled
		res.Err = "no frame"
	case frame.MainStatus != 0 && (frame.MainStatus < 200 || frame.MainStatus >= 300):
		res.Verdict = RenderStalled
		res.MainStatus = frame.MainStatus
		res.Err = fmt.Sprintf("main document HTTP %d", frame.MainStatus)
	default:
		c, cerr := Classify(frame.Image, l.cfg.KnownBad, l.cfg.BlankVarianceThreshold)
		if cerr != nil {
			res.Verdict = RenderStalled
			res.Err = cerr.Error()
			break
		}
		res = RenderResult{Verdict: c.Verdict, PHash: c.PHash, Variance: c.Variance, Label: c.Label, MainStatus: frame.MainStatus}
	}
	if _, err := l.cfg.Store.ApplyRender(rec.Source, res); err != nil && !errors.Is(err, ErrLowDisk) {
		l.logger.Warn("media health: recording l1 verdict failed", zap.Error(err))
	}
	l.logger.Info("media health: l1 render",
		zap.String("source", redactForLog(rec.Source)),
		zap.String("verdict", string(res.Verdict)),
		zap.Bool("confirmation", confirm),
		zap.Float64("variance", res.Variance),
		zap.String("label", res.Label),
		zap.Int("main_status", res.MainStatus),
		zap.String("error", res.Err))
}

// isSoftwareRecord says whether a record is an L1 candidate class: an HTML
// document (by L0's sniffed or declared type) or a source the player
// rendered in an iframe. Images, video and audio are L0's business (their
// container rules), and a browser render of them proves nothing L0 did not.
func isSoftwareRecord(rec *Record) bool {
	if rec == nil || rec.Source == "" || IsDataURI(rec.Source) {
		return false
	}
	if rec.SniffedType == "text/html" || strings.HasPrefix(rec.DeclaredType, "text/html") || strings.HasPrefix(rec.DeclaredType, "application/xhtml") {
		return true
	}
	return rec.MediaType == "iframe"
}
