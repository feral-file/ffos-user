package netlog

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	// eventQueueDepth buffers producer events for the single writer goroutine.
	// Producers NEVER block (they run on D-Bus, relayer, and provisioning-loop
	// goroutines); overflow drops the event and counts it, and the drop count
	// is recorded in-band so a gappy timeline is legible as gappy.
	eventQueueDepth = 256

	// defaultLadderMinInterval rate-limits AUTOMATIC ladder runs: a flapping
	// network must not turn the diagnostic tool into load on itself (the
	// plan's probe-discipline constraint). On-demand runs bypass this — a
	// human asked, and humans are already rate-limited by patience.
	defaultLadderMinInterval = 60 * time.Second

	// defaultStabilityWindow is how long the internet verdict must stay
	// online after an outage before the recorder triggers a self-upload
	// (stage 2a): uploading into a half-healed network would fail and burn
	// the single-flight slot for nothing.
	defaultStabilityWindow = 2 * time.Minute

	// outageCountWindow is the lastOutage.count24h accounting window.
	outageCountWindow = 24 * time.Hour

	// defaultSelfUploadMinInterval bounds how often the stability-window
	// self-upload may fire. Without it, a site flapping every few minutes —
	// exactly the site this feature targets — would push one full log bundle
	// (up to 128 MB of input) per recovery, turning the diagnostic into
	// uplink load and S3 cost. Suppressed uploads are recorded in-band; the
	// controller-initiated uploadLogs command is never subject to this.
	defaultSelfUploadMinInterval = 6 * time.Hour

	// defaultPruneInterval paces the ring's age-based retention sweep. The
	// ring prunes at open and roll, but a device that stays healthy after its
	// last outage does neither for months (the dedupe discipline means zero
	// appends), which would let sealed segments outlive the stated 7-day
	// retention and keep riding support bundles. Hourly is generous against a
	// 7-day window and costs one ReadDir per tick.
	defaultPruneInterval = time.Hour
)

// ladderRunner is the recorder's seam onto the Ladder (fake in tests).
type ladderRunner interface {
	Run(ctx context.Context, trigger string) *LadderResult
}

// RecorderOptions wires a Recorder. Zero-value durations select defaults.
type RecorderOptions struct {
	Ring   *Ring
	Ladder ladderRunner
	Clock  wrapper.Clock
	Logger *zap.Logger

	// SelfUpload, when non-nil, is invoked (on its own goroutine) after an
	// outage closes and the connection stays stable for StabilityWindow.
	// nil = stage-2a egress disabled (no support-logs API key provisioned).
	SelfUpload func()

	// InternetLevel is the cached-read level probe (never a live network
	// probe) backing the edge+level discipline: any consumer of the
	// connectivity_change edge must also reconcile off a level signal or it
	// sticks wrong after a missed edge. nil disables the reconcile loop
	// (tests drive edges directly).
	InternetLevel func(ctx context.Context) (bool, error)

	LadderMinInterval time.Duration
	StabilityWindow   time.Duration

	// stamp overrides the timestamp source in tests.
	stamp stampSource
}

// event is one producer observation queued to the writer goroutine.
type event struct {
	stamp    Stamp
	internet *InternetEvent
	prov     *ProvEvent
	relayer  *relayerObservation
	ladder   *LadderResult // completed on-demand run to record
}

// relayerObservation carries the raw producer signal. The relayer reports a
// close-frame teardown as (false, code) from its read loop and the code-less
// closeConn teardown as (false, 0); state dedupe keeps whichever arrived
// first, which is the code-carrying one on the close-frame path.
type relayerObservation struct {
	connected bool
	closeCode int // 0 = no code in this observation
}

// Recorder is the flight recorder: it dedupes producer streams, writes
// transition records to the ring, opens/closes outage episodes, triggers the
// rate-limited ladder on failure edges, and drives the stage-2 egress
// (stability-window self-upload + lastOutage summary).
//
// Concurrency shape: producers enqueue (non-blocking) from their own
// goroutines; ONE worker goroutine owns all ring writes and all episode
// state transitions. LastOutage() reads a mu-guarded copy. The recorder
// never calls back into any producer — observe-only, by contract.
type Recorder struct {
	ring   *Ring
	ladder ladderRunner
	clock  wrapper.Clock
	logger *zap.Logger
	stamp  stampSource

	selfUpload    func()
	internetLevel func(ctx context.Context) (bool, error)

	ladderMinInterval time.Duration
	stabilityWindow   time.Duration

	events  chan event
	done    chan struct{}
	stopped sync.Once
	wg      sync.WaitGroup

	droppedMu sync.Mutex
	dropped   int64

	// mu guards the episode/summary state shared between the worker and
	// LastOutage()/RunDiagnostics() callers.
	mu               sync.Mutex
	lastInternet     *bool
	lastRelayer      *bool
	outageOpen       bool
	outageStart      time.Time
	outageClass      Class
	lastAutoLadder   time.Time
	haveAutoLadder   bool
	recentOutageEnds []time.Time
	lastOutage       *OutageSummary
	stabilityGen     int
	lastSelfUpload   time.Time
	haveSelfUpload   bool
}

// NewRecorder builds a Recorder over an open ring.
func NewRecorder(opts RecorderOptions) *Recorder {
	if opts.LadderMinInterval <= 0 {
		opts.LadderMinInterval = defaultLadderMinInterval
	}
	if opts.StabilityWindow <= 0 {
		opts.StabilityWindow = defaultStabilityWindow
	}
	if opts.stamp == nil {
		opts.stamp = newStampSource()
	}
	return &Recorder{
		ring:              opts.Ring,
		ladder:            opts.Ladder,
		clock:             opts.Clock,
		logger:            opts.Logger,
		stamp:             opts.stamp,
		selfUpload:        opts.SelfUpload,
		internetLevel:     opts.InternetLevel,
		ladderMinInterval: opts.LadderMinInterval,
		stabilityWindow:   opts.StabilityWindow,
		events:            make(chan event, eventQueueDepth),
		done:              make(chan struct{}),
	}
}

// Start launches the worker (and the level-reconcile loop when wired).
// ctx is the daemon context; cancellation stops both.
func (r *Recorder) Start(ctx context.Context) {
	r.wg.Add(1)
	go r.run(ctx)
	if r.internetLevel != nil {
		r.wg.Add(1)
		go r.levelReconcileLoop(ctx)
	}
}

// Stop ends the recorder and seals the ring. Idempotent; safe after ctx
// cancellation (Close is guarded).
func (r *Recorder) Stop() {
	r.stopped.Do(func() { close(r.done) })
	r.wg.Wait()
	if err := r.ring.Close(); err != nil {
		r.logger.Warn("netlog: ring close failed", zap.Error(err))
	}
}

// --- producer inputs (all non-blocking) ---

// ObserveInternet feeds one internet-reachability verdict (edge or level —
// the recorder dedupes, so callers report what they see, not transitions).
func (r *Recorder) ObserveInternet(connected bool) {
	r.observeInternet(connected, "")
}

func (r *Recorder) observeInternet(connected bool, source string) {
	r.enqueue(event{stamp: r.stamp(), internet: &InternetEvent{Connected: connected, Source: source}})
}

// ObserveProvTransition feeds one provisioning-machine transition.
func (r *Recorder) ObserveProvTransition(from, to, fromReason, reason string) {
	r.enqueue(event{stamp: r.stamp(), prov: &ProvEvent{From: from, To: to, FromReason: fromReason, Reason: reason}})
}

// ObserveRelayer feeds one relayer connection observation (closeCode 0 =
// none seen; see relayerObservation for the two-teardown-paths contract).
func (r *Recorder) ObserveRelayer(connected bool, closeCode int) {
	r.enqueue(event{stamp: r.stamp(), relayer: &relayerObservation{connected: connected, closeCode: closeCode}})
}

// enqueue queues an event without ever blocking a producer; overflow is
// counted and later surfaced as an in-band note record.
func (r *Recorder) enqueue(ev event) {
	select {
	case <-r.done:
		return
	default:
	}
	select {
	case r.events <- ev:
	default:
		r.droppedMu.Lock()
		r.dropped++
		r.droppedMu.Unlock()
	}
}

// --- on-demand diagnostics (stage 2c) ---

// RunDiagnostics executes one ladder run on demand (runNetworkDiagnostics),
// records it, and returns it. It bypasses the automatic rate limit but shares
// the Ladder's one-run-at-a-time serialization. When the run classifies an
// open outage more specifically than "unknown", the episode adopts the class.
func (r *Recorder) RunDiagnostics(ctx context.Context) (*LadderResult, error) {
	if r.ladder == nil {
		return nil, fmt.Errorf("netlog: no ladder wired")
	}
	res := r.ladder.Run(ctx, "on-demand")
	r.adoptClass(res.Class)
	r.enqueue(event{stamp: r.stamp(), ladder: res})
	return res, nil
}

// LastOutage returns the last closed outage summary (nil before the first),
// with Count24h recomputed against the caller's now.
func (r *Recorder) LastOutage() *OutageSummary {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.lastOutage == nil {
		return nil
	}
	out := *r.lastOutage
	out.Count24h = r.countRecentLocked(r.clock.Now())
	return &out
}

// --- worker ---

func (r *Recorder) run(ctx context.Context) {
	defer r.wg.Done()
	r.append(Record{Stamp: r.stamp(), Kind: KindNote, Note: "recorder_start"})
	// Retention must not depend on outage activity (see defaultPruneInterval);
	// the worker owns the tick so pruning shares the single-writer discipline
	// with every other ring mutation.
	pruneTicker := r.clock.NewTicker(defaultPruneInterval)
	defer pruneTicker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case <-pruneTicker.C():
			r.ring.Prune()
		case ev := <-r.events:
			r.handle(ctx, ev)
			r.flushDrops()
		}
	}
}

func (r *Recorder) handle(ctx context.Context, ev event) {
	switch {
	case ev.internet != nil:
		r.handleInternet(ctx, ev)
	case ev.prov != nil:
		r.append(Record{Stamp: ev.stamp, Kind: KindProv, Prov: ev.prov})
	case ev.relayer != nil:
		r.handleRelayer(ev)
	case ev.ladder != nil:
		r.append(Record{Stamp: ev.stamp, Kind: KindLadder, Ladder: ev.ladder})
	}
}

// handleInternet dedupes the verdict stream and drives the episode machine:
// online→offline opens an outage and triggers the (rate-limited) ladder;
// offline→online closes it, seals the segment, and arms the stability window.
func (r *Recorder) handleInternet(ctx context.Context, ev event) {
	connected := ev.internet.Connected

	r.mu.Lock()
	if r.lastInternet != nil && *r.lastInternet == connected {
		r.mu.Unlock()
		return
	}
	first := r.lastInternet == nil
	v := connected
	r.lastInternet = &v
	if first && ev.internet.Source == "" {
		ev.internet.Source = "initial"
	}
	r.mu.Unlock()

	r.append(Record{Stamp: ev.stamp, Kind: KindInternet, Internet: ev.internet})

	if !connected {
		r.openOutage(ctx, ev.stamp)
		return
	}
	if !first {
		r.closeOutage(ev.stamp)
	}
}

func (r *Recorder) openOutage(ctx context.Context, at Stamp) {
	r.mu.Lock()
	if !r.outageOpen {
		r.outageOpen = true
		r.outageStart = at.Wall
		r.outageClass = ClassUnknownProbe
	}
	r.stabilityGen++ // cancel any armed stability window: the network relapsed
	now := r.clock.Now()
	limited := r.haveAutoLadder && now.Sub(r.lastAutoLadder) < r.ladderMinInterval
	if !limited {
		r.lastAutoLadder = now
		r.haveAutoLadder = true
	}
	r.mu.Unlock()

	if r.ladder == nil {
		return
	}
	if limited {
		// The suppression itself is timeline data: a flap storm shows as
		// edges + rate-limited notes, not as silence.
		r.append(Record{Stamp: r.stamp(), Kind: KindNote, Note: "ladder_rate_limited"})
		return
	}
	res := r.ladder.Run(ctx, "failure-edge")
	r.adoptClass(res.Class)
	r.append(Record{Stamp: r.stamp(), Kind: KindLadder, Ladder: res})
}

func (r *Recorder) closeOutage(at Stamp) {
	r.mu.Lock()
	if !r.outageOpen {
		r.mu.Unlock()
		return
	}
	r.outageOpen = false
	end := at.Wall
	r.recentOutageEnds = append(r.recentOutageEnds, end)
	summary := &OutageSummary{
		Start:    r.outageStart,
		End:      end,
		Class:    r.outageClass,
		Count24h: r.countRecentLocked(r.clock.Now()),
	}
	r.lastOutage = summary
	r.stabilityGen++
	gen := r.stabilityGen
	r.mu.Unlock()

	rec := *summary
	r.append(Record{Stamp: at, Kind: KindOutageEnd, Outage: &rec})
	// Seal the episode's segment so the whole outage lands in a finished
	// file — the natural unit uploadLogs bundles.
	if err := r.ring.Roll(); err != nil {
		r.logger.Warn("netlog: segment roll failed", zap.Error(err))
	}

	if r.selfUpload != nil {
		r.armStabilityUpload(gen)
	}
}

// armStabilityUpload waits the stability window, then fires the self-upload
// if no relapse (stabilityGen unchanged) happened meanwhile. A goroutine per
// close is fine: gen invalidation makes stale ones exit silently, and closes
// are rare by definition.
func (r *Recorder) armStabilityUpload(gen int) {
	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { // translate recorder shutdown into ctx cancel for the sleep
			select {
			case <-r.done:
				cancel()
			case <-ctx.Done():
			}
		}()
		if err := r.clock.SleepContext(ctx, r.stabilityWindow); err != nil {
			return
		}
		r.mu.Lock()
		stale := gen != r.stabilityGen
		online := r.lastInternet != nil && *r.lastInternet
		now := r.clock.Now()
		limited := r.haveSelfUpload && now.Sub(r.lastSelfUpload) < defaultSelfUploadMinInterval
		if !stale && online && !limited {
			r.lastSelfUpload = now
			r.haveSelfUpload = true
		}
		r.mu.Unlock()
		if stale || !online {
			return
		}
		if limited {
			// The suppression is timeline data too: support reading the ring
			// later must see WHY no bundle arrived for this episode.
			r.append(Record{Stamp: r.stamp(), Kind: KindUploadState, Note: "self_upload_rate_limited"})
			return
		}
		r.append(Record{Stamp: r.stamp(), Kind: KindUploadState, Note: "self_upload_triggered"})
		r.selfUpload()
	}()
}

func (r *Recorder) handleRelayer(ev event) {
	obs := ev.relayer

	r.mu.Lock()
	if r.lastRelayer != nil && *r.lastRelayer == obs.connected {
		r.mu.Unlock()
		return
	}
	v := obs.connected
	r.lastRelayer = &v
	r.mu.Unlock()

	r.append(Record{Stamp: ev.stamp, Kind: KindRelayer, Relayer: &RelayerEvent{Connected: obs.connected, CloseCode: obs.closeCode}})
}

// adoptClass upgrades an open outage's class with a more specific verdict.
// unknown-* never overwrites a confident class; any confident class may
// replace an unknown one or a previous confident one (fresher evidence wins).
func (r *Recorder) adoptClass(c Class) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.outageOpen {
		return
	}
	if c == ClassUnknownProbe || c == ClassUnknownLink {
		return
	}
	r.outageClass = c
}

// countRecentLocked prunes and counts outage ends within the 24h window.
func (r *Recorder) countRecentLocked(now time.Time) int {
	cutoff := now.Add(-outageCountWindow)
	kept := r.recentOutageEnds[:0]
	for _, t := range r.recentOutageEnds {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.recentOutageEnds = kept
	return len(kept)
}

// flushDrops surfaces producer-side drops as an in-band note.
func (r *Recorder) flushDrops() {
	r.droppedMu.Lock()
	n := r.dropped
	r.dropped = 0
	r.droppedMu.Unlock()
	if n > 0 {
		r.append(Record{Stamp: r.stamp(), Kind: KindNote, Note: "events_dropped", Dropped: n})
	}
}

// append writes one record; failures degrade to a Warn log (never Error —
// see the package doc's Sentry rule) and never propagate to producers.
func (r *Recorder) append(rec Record) {
	if err := r.ring.Append(rec); err != nil {
		r.logger.Warn("netlog: append failed", zap.String("kind", rec.Kind), zap.Error(err))
	}
}

// levelReconcileLoop is the edge+level discipline's level half: a slow cached
// read that heals a missed connectivity_change edge within a minute. Errors
// skip the tick — an unanswerable query is non-evidence, and fabricating
// "offline" here would open a phantom outage in the timeline.
func (r *Recorder) levelReconcileLoop(ctx context.Context) {
	defer r.wg.Done()
	ticker := r.clock.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-r.done:
			return
		case <-ticker.C():
			v, err := r.internetLevel(ctx)
			if err != nil {
				continue
			}
			r.observeInternet(v, "level")
		}
	}
}
