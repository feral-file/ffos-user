// Package playersession owns the cross-cutting authority over the player
// (Chromium/ff-player) page that controld talks to over CDP: page generation
// (which document is currently live), per-generation readiness barriers,
// screen-overlay coordination between narrators, and the one recovery
// navigation primitive (navigate-to-entry, never reload-in-place — the
// static export is flat files only, so a client-route reload 404s).
//
// Design reference: docs/player-session-recovery.md §2-§4. Off-lane
// producers (setupui narration, mediator connectivity, qrdisplay/mintpairing,
// sleep_schedule, commandrouter) keep their own queues/locks and consult this
// package's generation counter, readiness barrier, and overlay/navigation
// primitives rather than driving the page directly — see §4 there.
package playersession

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Stage is a per-generation readiness milestone (§2.2). Stages are ordered by
// declaration only for readability; AwaitStage does not require callers to
// await them in order.
type Stage int

const (
	// StageTarget means a CDP target (page) is connected. It never needs a
	// page-side evaluate: connectivity IS the condition.
	StageTarget Stage = iota
	// StageHandler means window.handleCDPRequest is installed — the player
	// app has hydrated far enough to answer CDP commands.
	StageHandler
	// StageStatus means window.__ffosPlayerStatus is answering. New players
	// only (Phase 0 carrier); on an old player this stage never resolves, and
	// AwaitStage callers must bound their own ctx rather than rely on it
	// completing.
	StageStatus
)

func (s Stage) String() string {
	switch s {
	case StageTarget:
		return "target"
	case StageHandler:
		return "handler"
	case StageStatus:
		return "status"
	default:
		return fmt.Sprintf("stage(%d)", int(s))
	}
}

// NavOutcome classifies how a NavigateHome/NavigateHomeInline attempt
// resolved. See NavResult.
type NavOutcome int

const (
	// NavExecuted means the session attempted the navigation (Page.navigate
	// was sent successfully and the generation bumped). Err carries a
	// transport or verification failure; nil Err means verified success
	// (§2.3 step 6).
	NavExecuted NavOutcome = iota
	// NavSkippedOverlay means a registered overlay owner had the screen (or
	// the error-page gate fired pre-nav — the daemon-chosen error page is
	// treated as an implicit, unregistered overlay owner for
	// outcome-classification purposes; see the error-page gate comment in
	// navigateAndVerify), or the navigation EXECUTED but landed on /error —
	// awaitRouteSettled classifies that identically rather than as a
	// NavExecuted route mismatch, since it is not a broken navigation to
	// retry.
	NavSkippedOverlay
	// NavSkippedAsleep means the desired player state is sleeping (or
	// unknown-with-attempted-sleeping); navigating would route a sleeping
	// wall out of /sleep.
	NavSkippedAsleep
	// NavSuperseded means a newer NavigateHome request replaced this one
	// before it started executing.
	NavSuperseded
	// NavEvicted means the request was dropped, e.g. the session is shutting
	// down (root ctx canceled).
	NavEvicted
)

func (o NavOutcome) String() string {
	switch o {
	case NavExecuted:
		return "executed"
	case NavSkippedOverlay:
		return "skipped_overlay"
	case NavSkippedAsleep:
		return "skipped_asleep"
	case NavSuperseded:
		return "superseded"
	case NavEvicted:
		return "evicted"
	default:
		return fmt.Sprintf("outcome(%d)", int(o))
	}
}

// NavOptions configures a recovery navigation.
type NavOptions struct {
	// PurgeCache issues Network.clearBrowserCache before navigating. This is
	// the authoritative cache bypass for recovery navigation (commandrouter's
	// pre-refresh clearBrowserCache and darkhttpd's Cache-Control: no-cache
	// stay as their own, separate defenses — see the design doc's [NV13]).
	PurgeCache bool
}

// NavResult is the outcome of a NavigateHome/NavigateHomeInline attempt. Err
// is meaningful only when Outcome == NavExecuted.
type NavResult struct {
	Outcome NavOutcome
	Err     error
}

// Sentinel errors NavigateHomeInline maps NavResult onto (see navResultErr).
// Phase 2b callers that need a NavigateHome-style done(NavResult) callback
// instead get the full NavResult and need not use these.
var (
	ErrNavSkippedOverlay = fmt.Errorf("playersession: navigation skipped, an overlay owns the screen")
	ErrNavSkippedAsleep  = fmt.Errorf("playersession: navigation skipped, the player is asleep")
	ErrNavSuperseded     = fmt.Errorf("playersession: navigation superseded by a newer request")
	ErrNavEvicted        = fmt.Errorf("playersession: navigation evicted (session shutting down)")

	// ErrNotConnected is AwaitStage's immediate, no-wait error when the CDP
	// transport is not currently live (§2 AwaitStage doc).
	ErrNotConnected = fmt.Errorf("playersession: CDP is not connected")
	// ErrNoGeneration is AwaitStage's error when no generation has been
	// established yet (no CDP-connect, navigation, or stamp-mismatch bump has
	// ever run). Distinct from ErrNotConnected for diagnosability; in
	// practice OnConnect bumps before anything else observes a connection.
	ErrNoGeneration = fmt.Errorf("playersession: no generation established yet")
)

// CDPSender is the narrow slice of the CDP client this package needs —
// evaluates/commands plus a liveness check. Owning a small, consumer-owned
// interface here mirrors setupui.CDPSender; cdp.CDP satisfies it.
type CDPSender interface {
	NoLogSend(method string, params map[string]interface{}) (interface{}, error)
	Initialized() bool
}

// StageProbeFunc returns the JS expression that answers whether st is ready
// for the document currently loaded. It MUST evaluate to a JSON.stringify'd
// string (the cdp client decodes only string/object evaluate results — the
// same constraint setupui.awaitPageReady pins). navNonce is non-empty only
// for a navigation-triggered generation (see stampNavNonce): the probe then
// additionally requires the nonce to be GONE, so an evaluate answered by the
// pre-navigate document (still tearing down) does not fool the barrier — the
// same hazard setupui's execPageReload nonce closes. The nonce rides its OWN
// global (window.__ffosNavNonce), never window.__ffosDocStamp [M4]: that
// global is the source-3 stamp carrier the status poller echoes back every
// 5s, and a nonce planted there would be visible to ObserveStatusStamp during
// the pre-bump window (stamp written, Page.navigate sent, but the bump that
// updates the generation's baseline hasn't run yet) — a stray poll in that
// window would read the nonce as a "mismatch" against the OLD generation's
// real baseline and bump a second time for a document already being torn
// down, pushing reconcilers into a dying page. window.__ffosDocStamp stays
// exclusively session-written, only at generation-ready (see
// onGenerationReady).
//
// Constructor-injected as a seam for tests; production always uses
// defaultStageProbe.
type StageProbeFunc func(st Stage, navNonce string) string

func defaultStageProbe(st Stage, navNonce string) string {
	switch st {
	case StageHandler:
		if navNonce != "" {
			return fmt.Sprintf(
				`JSON.stringify({ready: typeof window.handleCDPRequest === 'function' && window.__ffosNavNonce !== %q})`,
				navNonce)
		}
		return `JSON.stringify({ready: typeof window.handleCDPRequest === 'function'})`
	case StageStatus:
		return `JSON.stringify({ready: typeof window.__ffosPlayerStatus === 'function'})`
	default:
		return ""
	}
}

// defaultPollInterval paces AwaitStage's re-poll loop (mirrors setupui's
// defaultReadyPollInterval).
const defaultPollInterval = 250 * time.Millisecond

// defaultReadyPollBackoffAfter / defaultReadyPollBackoffInterval pace back
// onGenerationReady's own indefinite background retry (design doc [NV1]:
// never latches a negative, so it keeps polling at whatever cadence for the
// entire life of a generation whose page never installs its command
// handler). A page that hasn't installed the handler within a minute is
// unlikely to in the next 250ms tick, so backing off to a slower cadence
// after that point avoids a permanent 4Hz eval loop against a wedged page.
// Callers with their own bounded ctx (AwaitStage, the verify waits) are
// unaffected — this pacing applies only to onGenerationReady's own wait.
const (
	defaultReadyPollBackoffAfter    = time.Minute
	defaultReadyPollBackoffInterval = 2 * time.Second
)

// defaultVerifyCap bounds NavigateHomeInline's total synchronous wait
// (gates+navigate+bump+barrier+route verification) — [NV7] caps it at 20s,
// comfortably under hub's 30s HTTP timeouts. NavigateHome (async) reuses the
// same cap for its own verification wait so a wedged page cannot hang the
// session worker forever.
const defaultVerifyCap = 20 * time.Second

// routePlaylist / routeSleep / routeError are the structured status probe's
// route values this package classifies against (§2.3 steps 1-2 and 6).
const (
	routePlaylist = "/playlist"
	routeSleep    = "/sleep"
	routeError    = "/error"
)

// statusProtocolVersion is the __ffosPlayerStatus protocol version this
// package understands (design doc §4.1). A response advertising anything
// else is treated as unavailable rather than decoded against a shape it may
// no longer match.
const statusProtocolVersion = 1

// genState is one generation's mutable readiness/identity record. A new
// genState is created on every bump; the previous one's ctx is canceled,
// which promptly unblocks anyone still polling AwaitStage against it (see
// awaitStageOn) and aborts its generation-ready worker before it stamps or
// runs reconcilers for a document that is no longer current.
type genState struct {
	id       uint64
	ctx      context.Context
	cancel   context.CancelFunc
	navNonce string // set only for navigation-triggered generations

	// readyDone closes once the generation-ready worker (onGenerationReady)
	// resolves StageHandler for this generation (success, or superseded/
	// shutdown). readyErr is valid only after readyDone closes; both are
	// written on the worker goroutine strictly before the close, so a
	// receive from readyDone happens-after that write per Go's memory model
	// — no additional lock needed to read readyErr post-receive. Consulted by
	// awaitGenerationReady so a caller that just triggered a bump (navigate)
	// waits on the SAME worker's poll instead of starting a second,
	// independent awaitStageOn loop against the identical stage — which would
	// double the CDP evaluate traffic for the whole pre-ready window.
	readyDone chan struct{}
	readyErr  error

	mu     sync.Mutex
	stages map[Stage]bool // POSITIVE-only cache — see AwaitStage's doc
	stamp  string         // doc-stamp written at barrier resolution; "" until written
}

type overlayOwner struct {
	name   string
	active func() bool
}

type reconcilerEntry struct {
	name string
	fn   func(ctx context.Context)
}

type navRequest struct {
	opts NavOptions
	done func(NavResult)
}

// Session is the single cross-cutting authority described in package doc and
// design §2. Safe for concurrent use.
type Session struct {
	rootCtx context.Context
	cdp     CDPSender
	clock   wrapper.Clock
	logger  *zap.Logger

	// asleep reports the schedule's desired sleeping state. Production wires
	// devicectl.PlayerSleepGate, backed by the four-value sleep tracker
	// (§3.4); a nil asleep (test doubles, a build wired without the tracker)
	// is treated as always-false — never blocking navigation.
	asleep func() bool

	// stageProbe / pollInterval / inlineVerifyCap are constructor-injected
	// seams for tests; zero value in tests means "use the default".
	stageProbe      StageProbeFunc
	pollInterval    time.Duration
	inlineVerifyCap time.Duration

	// readyPollBackoffAfter / readyPollBackoffInterval pace back
	// onGenerationReady's own indefinite background retry (see
	// onGenerationReadyPollInterval); zero means "use the package default".
	// Test-only seams, same contract as pollInterval/inlineVerifyCap above.
	readyPollBackoffAfter    time.Duration
	readyPollBackoffInterval time.Duration

	mu        sync.Mutex
	current   *genState
	nextGenID uint64

	overlayOwners []overlayOwner
	reconcilers   []reconcilerEntry

	// navPending is a REFCOUNT, not a bool [M1]: NavigateHomeInline can run
	// concurrently with an async NavigateHome (Inline's own concurrency
	// guard — see claimNavSlot/finishNavSlot — closes most of that window,
	// but this stays a counter as the structural fix regardless of how many
	// navigateAndVerify calls end up in flight at once). A shared bool would
	// let two overlapping navigateAndVerify calls' deferred Store(false)s
	// race: whichever finishes first clears NavigationPending while the
	// other is still executing, letting a parked narration/display send
	// through mid-navigation. NavigationPending() reports count>0.
	navPending atomic.Int32

	// navMu serializes the gate-check + optional purge + stamp + navigate +
	// generation-bump critical section across NavigateHome and
	// NavigateHomeInline so two recovery navigations never race the page
	// underneath one another. It is session-private and never held across a
	// barrier wait, an overlay-owner probe's OWN locking, or anything that
	// could re-enter the session — Inline's caller-holds-no-external-lock
	// contract [NV7] only has teeth because of that.
	navMu sync.Mutex

	navSlotMu sync.Mutex
	navBusy   bool
	navSlot   *navRequest

	// navTargetGen is the generation ID an in-flight navigation bumped TO,
	// or 0 when no navigation has bumped one yet (pre-bump) or none is in
	// flight at all. [Park predicate fix] Off-lane parkers (setupui,
	// mintpairing) cannot reliably use "Generation() != <snapshot taken at
	// park entry>" to detect the bump: a park entered AFTER the bump already
	// happened snapshots the NEW generation as its baseline, so that
	// comparison never becomes true and the park stalls for its full
	// timeout even though the target generation may already be
	// handler-ready. NavigationTargetGeneration gives parkers a value that
	// identifies the SPECIFIC generation the in-flight navigation produced,
	// independent of when they happened to enter their own park.
	navTargetGen atomic.Uint64
}

// New builds a Session. ctx is the daemon-lifetime context: generation
// contexts derive from it, so canceling it (process shutdown) promptly
// unblocks every in-flight AwaitStage wait and lets a queued NavigateHome
// request resolve NavEvicted the next time it is picked up. clk may be nil
// (defaults to the real wrapper.Clock). asleep may be nil (treated as
// never-asleep). logger may be nil (defaults to a no-op logger).
func New(ctx context.Context, sender CDPSender, clk wrapper.Clock, asleep func() bool, logger *zap.Logger) *Session {
	if clk == nil {
		clk = wrapper.NewClock()
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	return &Session{
		rootCtx:      ctx,
		cdp:          sender,
		clock:        clk,
		asleep:       asleep,
		logger:       logger,
		stageProbe:   defaultStageProbe,
		pollInterval: defaultPollInterval,
	}
}

// Generation returns the current generation id, or 0 if none has been
// established yet (no CDP-connect, navigation, or stamp-mismatch bump has
// ever run).
func (s *Session) Generation() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.current == nil {
		return 0
	}
	return s.current.id
}

// OnConnect is generation source 1 (§2.1): wire it as cdp.CDP.Start's
// onConnect callback. It bumps the generation; the standard generation-ready
// worker stamps the new document and runs every registered reconciler once
// StageHandler resolves (see RegisterReconciler).
func (s *Session) OnConnect() {
	s.bump("", "cdp-connect")
}

// ObserveStatusStamp is generation source 3 (§2.1): wire it as the status
// poller's per-round stamp-observation callback (the checkStatus round-trip's
// echoed window.__ffosDocStamp). present carries the nil/""-vs-absent
// distinction through from the poller: false means the connected player
// omitted the stamp field entirely (an old player without the carrier;
// "source unavailable") and NEVER bumps, regardless of the string value.
// present==true with stamp=="" is a REAL observation — a new player whose
// current document has no stamp of its own — and is classified exactly like
// any other value: a mismatch against a non-empty baseline. A mismatch
// against the current generation's own stamp means something replaced the
// document without going through NavigateHome (feral-watchdog's recovery
// navigate, or a player-initiated reload), so the session bumps and
// reconciles. A generation that has not reached barrier resolution yet (no
// baseline to compare against — including the session's own bump's
// stamp-write window) is "nothing to compare", never a bump.
func (s *Session) ObserveStatusStamp(stamp string, present bool) {
	if !present {
		return
	}
	s.mu.Lock()
	gen := s.current
	s.mu.Unlock()
	if gen == nil {
		return
	}
	gen.mu.Lock()
	baseline := gen.stamp
	gen.mu.Unlock()
	if baseline == "" || baseline == stamp {
		return
	}
	s.logger.Info("playersession: document stamp mismatch reported by status poller; bumping generation",
		zap.Uint64("previous_generation", gen.id),
		zap.String("observed_stamp", stamp),
		zap.String("expected_stamp", baseline),
	)
	s.bump("", "stamp-mismatch")
}

// bump creates a new generation, cancels the previous one's ctx (unblocking
// any AwaitStage wait pinned to it and aborting its generation-ready worker
// before it stamps or reconciles a stale document), and spawns the new
// generation's ready worker. Caller must NOT hold s.mu.
func (s *Session) bump(navNonce string, reason string) *genState {
	s.mu.Lock()
	if s.current != nil {
		s.current.cancel()
	}
	s.nextGenID++
	id := s.nextGenID
	ctx, cancel := context.WithCancel(s.rootCtx)
	gen := &genState{
		id:        id,
		ctx:       ctx,
		cancel:    cancel,
		navNonce:  navNonce,
		readyDone: make(chan struct{}),
		stages:    make(map[Stage]bool, 3),
	}
	s.current = gen
	s.mu.Unlock()

	s.logger.Info("playersession: generation bumped", zap.Uint64("generation", id), zap.String("reason", reason))
	go s.onGenerationReady(gen)
	return gen
}

// onGenerationReady waits for the new document's command handler, stamps it
// (the source-3 carrier for THIS generation, so connect-created generations
// are stamped too — not just navigation-created ones), and then runs every
// registered reconciler in registration order. Aborts without stamping or
// reconciling if gen is superseded (a newer bump canceled gen.ctx) or the
// session is shutting down before the handler ever appears.
func (s *Session) onGenerationReady(gen *genState) {
	err := s.awaitStageOnPaced(gen.ctx, gen, StageHandler, s.onGenerationReadyPollInterval)
	if err != nil {
		gen.readyErr = err
		close(gen.readyDone)
		s.logger.Debug("playersession: generation superseded or unreachable before it became ready",
			zap.Uint64("generation", gen.id), zap.Error(err))
		return
	}

	stamp := fmt.Sprintf("%d-%s", gen.id, strconv.FormatInt(s.clock.Now().UnixNano(), 36))
	if _, stampErr := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    fmt.Sprintf(`JSON.stringify({stamped: Boolean(window.__ffosDocStamp = %q)})`, stamp),
		"returnByValue": true,
	}); stampErr != nil {
		// Best-effort: a failed stamp only degrades source-3 mismatch
		// detection for this generation, it never blocks reconcilers.
		s.logger.Debug("playersession: document stamp write failed; source-3 mismatch detection degraded for this generation",
			zap.Uint64("generation", gen.id), zap.Error(stampErr))
	} else {
		gen.mu.Lock()
		gen.stamp = stamp
		gen.mu.Unlock()
	}

	gen.readyErr = nil
	close(gen.readyDone)

	s.mu.Lock()
	reconcilers := make([]reconcilerEntry, len(s.reconcilers))
	copy(reconcilers, s.reconcilers)
	s.mu.Unlock()

	for _, r := range reconcilers {
		if gen.ctx.Err() != nil {
			// Superseded mid-run: a newer generation's worker owns reconciling
			// from here; running a stale reconciler now would race it.
			return
		}
		s.runReconciler(gen, r)
	}
}

func (s *Session) runReconciler(gen *genState, r reconcilerEntry) {
	defer func() {
		if rec := recover(); rec != nil {
			s.logger.Error("playersession: reconciler panicked",
				zap.String("reconciler", r.name), zap.Uint64("generation", gen.id), zap.Any("panic", rec))
		}
	}()
	s.logger.Debug("playersession: running reconciler", zap.String("reconciler", r.name), zap.Uint64("generation", gen.id))
	r.fn(gen.ctx)
}

// RegisterOverlayOwner registers a probe an off-lane narrator uses to claim
// screen ownership (setupui's Narrating(), qrdisplay/mintpairing's live
// display flag, ...). NavigateHome/Inline's overlay gate (§2.3 step 3) treats
// ANY registered owner reporting true as "the screen is owned" and skips the
// navigation (NavSkippedOverlay). Not safe to call concurrently with itself
// from multiple goroutines at true parallel-init time; production wiring
// registers all owners once, before Start.
func (s *Session) RegisterOverlayOwner(name string, active func() bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.overlayOwners = append(s.overlayOwners, overlayOwner{name: name, active: active})
}

// RegisterReconciler registers a function run on the session's
// generation-ready worker every time a generation reaches StageHandler
// readiness (CDP connect, a session-executed navigation, or a stamp-mismatch
// bump) — in registration order. fn's ctx is the generation's own ctx: it is
// canceled if a newer generation supersedes this one mid-run, so a
// long-running reconciler should observe it.
func (s *Session) RegisterReconciler(name string, fn func(ctx context.Context)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.reconcilers = append(s.reconcilers, reconcilerEntry{name: name, fn: fn})
}

// StageReady is a non-blocking, cache-only check of whether the CURRENT
// generation's stage already resolved positive. It issues no CDP traffic —
// callers that want to actively poll should use AwaitStage. Its intended use
// is a cheap park-loop condition (setupui's NavigationPending park: "the new
// generation's barrier reaches StageHandler" is exactly this flag flipping
// true, set by the SAME generation-ready worker AwaitStage(StageHandler)
// itself waits on).
func (s *Session) StageReady(st Stage) bool {
	s.mu.Lock()
	gen := s.current
	s.mu.Unlock()
	if gen == nil {
		return false
	}
	gen.mu.Lock()
	defer gen.mu.Unlock()
	return gen.stages[st]
}

// AwaitStage blocks until st resolves for the CURRENT generation (snapshotted
// once at call entry), ctx is done, or the generation is superseded.
// POSITIVE results are cached once per generation and never re-probed. A
// NEGATIVE resolution (ctx deadline reached while still not ready on a live
// connection) is NOT cached [NV1]: the cache is a write-once-true flag with no
// "false" state, so the very next AwaitStage call for the same
// generation/stage simply re-polls from scratch — this is what lets a slow
// cold boot (a cache purge, #234) delay readiness instead of latching a
// consumer dead for the process. Fails fast (immediate error, no polling) when
// cdp.Initialized() is false, both at entry and on every poll tick (a
// connection that drops mid-wait aborts the same way).
func (s *Session) AwaitStage(ctx context.Context, st Stage) error {
	if !s.cdp.Initialized() {
		return ErrNotConnected
	}
	s.mu.Lock()
	gen := s.current
	s.mu.Unlock()
	if gen == nil {
		return ErrNoGeneration
	}
	return s.awaitStageOn(ctx, gen, st)
}

// awaitGenerationReady waits for gen's OWN generation-ready worker (spawned
// by bump, see onGenerationReady) to resolve StageHandler, bounded by ctx.
// Used by navigateAndVerify right after a bump it just triggered: bump
// already started exactly one awaitStageOn poll loop for this generation
// (onGenerationReady's), so calling awaitStageOn a second time here would
// double the CDP evaluate traffic for the entire pre-ready window. ctx bounds
// only THIS wait — the generation's own worker is unaffected by ctx expiring
// and keeps retrying past it (see onGenerationReady / [NV1]).
func (s *Session) awaitGenerationReady(ctx context.Context, gen *genState) error {
	select {
	case <-gen.readyDone:
		return gen.readyErr
	case <-ctx.Done():
		return ctx.Err()
	}
}

// awaitStageOn is AwaitStage pinned to a specific generation (used internally
// by onGenerationReady so a superseded generation's worker cannot resolve
// against whatever generation happens to be current by the time it wakes).
// Paces at the flat per-call pollInterval; see awaitStageOnPaced for a
// variant that can back off over time.
func (s *Session) awaitStageOn(ctx context.Context, gen *genState, st Stage) error {
	return s.awaitStageOnPaced(ctx, gen, st, s.flatPollInterval)
}

// flatPollInterval is the pacing function every caller except
// onGenerationReady's own background retry uses: a constant cadence for the
// whole wait, bounded by the caller's own ctx.
func (s *Session) flatPollInterval(elapsed time.Duration) time.Duration {
	if s.pollInterval > 0 {
		return s.pollInterval
	}
	return defaultPollInterval
}

// onGenerationReadyPollInterval is onGenerationReady's pacing function: the
// flat cadence for the first readyPollBackoffAfter, then
// readyPollBackoffInterval — see the defaults' doc for why onGenerationReady
// specifically (an indefinite, uncapped-by-ctx background retry) needs this
// and bounded callers (AwaitStage, the verify waits) do not.
func (s *Session) onGenerationReadyPollInterval(elapsed time.Duration) time.Duration {
	after := s.readyPollBackoffAfter
	if after <= 0 {
		after = defaultReadyPollBackoffAfter
	}
	if elapsed < after {
		return s.flatPollInterval(elapsed)
	}
	backoff := s.readyPollBackoffInterval
	if backoff <= 0 {
		backoff = defaultReadyPollBackoffInterval
	}
	return backoff
}

// awaitStageOnPaced is awaitStageOn generalized over a pacing function, so
// onGenerationReady's indefinite background retry can back off over time
// while every other caller keeps the flat per-call pollInterval cadence (see
// onGenerationReadyPollInterval's doc for why the two must differ).
func (s *Session) awaitStageOnPaced(ctx context.Context, gen *genState, st Stage, pace func(elapsed time.Duration) time.Duration) error {
	gen.mu.Lock()
	if gen.stages[st] {
		gen.mu.Unlock()
		return nil
	}
	gen.mu.Unlock()

	if st == StageTarget {
		gen.mu.Lock()
		gen.stages[st] = true
		gen.mu.Unlock()
		return nil
	}

	expr := s.stageProbe(st, gen.navNonce)
	if expr == "" {
		return fmt.Errorf("playersession: no probe for stage %s", st)
	}

	start := s.clock.Now()
	for {
		if !s.cdp.Initialized() {
			return ErrNotConnected
		}
		result, err := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
			"expression":    expr,
			"returnByValue": true,
		})
		if err == nil && probeReady(result) {
			gen.mu.Lock()
			gen.stages[st] = true
			gen.mu.Unlock()
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-gen.ctx.Done():
			return gen.ctx.Err()
		case <-time.After(pace(s.clock.Now().Sub(start))):
		}
	}
}

func probeReady(result any) bool {
	m, ok := result.(map[string]any)
	if !ok {
		return false
	}
	v, _ := m["ready"].(bool)
	return v
}

// NavigationPending reports an armed-or-executing navigation. See the package
// doc and design §2 for the full parking contract; off-lane senders (setupui,
// qrdisplay/mintpairing) consult this inside their OWN send critical sections
// before each send and park while it is true.
func (s *Session) NavigationPending() bool {
	return s.navPending.Load() > 0
}

// NavigationTargetGeneration reports the generation ID an in-flight
// navigation bumped to, or 0 when no navigation is in flight past its own
// bump (pre-bump, or none in flight at all). Off-lane parkers combine this
// with Generation() and StageReady: target != 0 && target == Generation()
// && StageReady(StageHandler) means the SPECIFIC generation this
// navigation produced is handler-ready — safe to exit a park regardless of
// whether the parker entered its wait before or after the bump happened
// (see navTargetGen's doc for why a Generation()-snapshot comparison alone
// cannot tell those two cases apart from a post-bump entry point).
func (s *Session) NavigationTargetGeneration() uint64 {
	return s.navTargetGen.Load()
}

// NavigateHome runs a recovery navigation asynchronously. done, when non-nil,
// is invoked exactly once with the outcome. Concurrent calls coalesce to the
// latest: a call that arrives while another is executing REPLACES any
// still-queued (not yet started) request, resolving the replaced one
// NavSuperseded.
func (s *Session) NavigateHome(opts NavOptions, done func(NavResult)) {
	s.navSlotMu.Lock()
	if s.navBusy {
		old := s.navSlot
		s.navSlot = &navRequest{opts: opts, done: done}
		s.navSlotMu.Unlock()
		if old != nil && old.done != nil {
			old.done(s.logNavOutcome(NavResult{Outcome: NavSuperseded}, "superseded-by-newer-request"))
		}
		return
	}
	s.navBusy = true
	s.navSlotMu.Unlock()
	go s.runNavWorker(opts, done)
}

func (s *Session) runNavWorker(opts NavOptions, done func(NavResult)) {
	for {
		ctx, cancel := context.WithTimeout(s.rootCtx, s.verifyCap())
		res := s.navigateAndVerify(ctx, opts)
		cancel()
		if done != nil {
			done(res)
		}

		s.navSlotMu.Lock()
		next := s.navSlot
		s.navSlot = nil
		if next == nil {
			s.navBusy = false
			s.navSlotMu.Unlock()
			return
		}
		s.navSlotMu.Unlock()
		opts, done = next.opts, next.done
	}
}

// NavigateHomeInline runs gates + optional purge + Page.navigate + generation
// bump SYNCHRONOUSLY on the caller's own goroutine, then waits (bounded by the
// 20s cap) for the same generation-ready worker every bump spawns to reach
// StageHandler, plus a best-effort route check (§2.3 step 6). It is for
// callers that owe a synchronous reply (commandrouter's relayer contract) and
// hold no external lock while calling it [NV7] — every lock this path takes
// (navMu, per-generation state) is session-private, so a pushMu-holding caller
// cannot self-deadlock through it.
//
// [M1] Inline claims the SAME navBusy slot NavigateHome's async worker uses
// (claimNavSlot), so the two paths can never run navigateAndVerify
// concurrently against the page. Unlike NavigateHome, a loss here is never
// queued: Inline's caller owes a bounded synchronous reply, so a
// already-in-flight-or-queued navigation is reported NavSuperseded
// immediately rather than waited out (which could blow well past the 20s
// cap, and hub's 30s HTTP timeout with it).
func (s *Session) NavigateHomeInline(opts NavOptions) error {
	if !s.claimNavSlot() {
		return navResultErr(s.logNavOutcome(NavResult{Outcome: NavSuperseded}, "inline-superseded-by-concurrent-navigation"))
	}
	ctx, cancel := context.WithTimeout(s.rootCtx, s.verifyCap())
	res := s.navigateAndVerify(ctx, opts)
	cancel()
	s.finishNavSlot()
	return navResultErr(res)
}

// claimNavSlot attempts to become the sole active navigateAndVerify executor
// (the navBusy claim NavigateHome's async path also uses), for
// NavigateHomeInline's use. Returns false when a navigation is already
// running or queued — Inline never queues on a loss (see its doc); it
// reports NavSuperseded immediately instead.
func (s *Session) claimNavSlot() bool {
	s.navSlotMu.Lock()
	defer s.navSlotMu.Unlock()
	if s.navBusy {
		return false
	}
	s.navBusy = true
	return true
}

// finishNavSlot releases the claim NavigateHomeInline took via claimNavSlot.
// If a NavigateHome call queued behind Inline's claim while it ran, this
// hands off to runNavWorker to drain it — mirroring runNavWorker's own tail,
// since Inline itself does not loop.
func (s *Session) finishNavSlot() {
	s.navSlotMu.Lock()
	next := s.navSlot
	s.navSlot = nil
	if next == nil {
		s.navBusy = false
		s.navSlotMu.Unlock()
		return
	}
	s.navSlotMu.Unlock()
	go s.runNavWorker(next.opts, next.done)
}

func navResultErr(res NavResult) error {
	switch res.Outcome {
	case NavExecuted:
		return res.Err
	case NavSkippedOverlay:
		return ErrNavSkippedOverlay
	case NavSkippedAsleep:
		return ErrNavSkippedAsleep
	case NavSuperseded:
		return ErrNavSuperseded
	case NavEvicted:
		return ErrNavEvicted
	default:
		return fmt.Errorf("playersession: unknown nav outcome %d", res.Outcome)
	}
}

func (s *Session) verifyCap() time.Duration {
	if s.inlineVerifyCap > 0 {
		return s.inlineVerifyCap
	}
	return defaultVerifyCap
}

// navigateAndVerify is the shared implementation behind NavigateHome and
// NavigateHomeInline (§2.3). NavigationPending is set BEFORE any gate probe —
// in particular before the overlay probe [NV4] — and cleared on every
// terminal outcome via the deferred Store(false), closing the check-then-act
// window against an overlay owner's own send worker: a push racing past the
// owner's probe parks at its own send site (see setupui's worker) and is
// delivered post-navigation instead of racing this function's overlay check.
func (s *Session) navigateAndVerify(ctx context.Context, opts NavOptions) NavResult {
	s.navPending.Add(1)
	defer s.navPending.Add(-1)

	if s.closed() {
		return s.logNavOutcome(NavResult{Outcome: NavEvicted}, "session-shutting-down")
	}
	if s.isAsleep() {
		return s.logNavOutcome(NavResult{Outcome: NavSkippedAsleep}, "sleep-gate")
	}
	// Error-page gate [NV2]: the daemon-chosen error page deliberately takes
	// the wall off playback; navigating to "/" would resume playback over it.
	// The NavOutcome enum has no dedicated "error page" member, so this is
	// classified as NavSkippedOverlay — the error page is, for outcome
	// purposes, an unregistered overlay owner. "treat unavailable status as
	// not-error": a probe failure or an old player without the status carrier
	// does not gate.
	if s.isErrorPage(ctx) {
		return s.logNavOutcome(NavResult{Outcome: NavSkippedOverlay}, "error-page-gate")
	}
	if active, owner := s.overlayOwnersActive(); active {
		return s.logNavOutcome(NavResult{Outcome: NavSkippedOverlay}, "overlay:"+owner)
	}

	s.navMu.Lock()
	if opts.PurgeCache {
		if _, err := s.cdp.NoLogSend("Network.clearBrowserCache", map[string]interface{}{}); err != nil {
			s.logger.Debug("playersession: cache purge failed (best-effort)", zap.Error(err))
		}
	}
	navNonce := s.stampNavNonce()
	_, navErr := s.cdp.NoLogSend("Page.navigate", map[string]interface{}{"url": constants.WEBAPP_URL})
	if navErr != nil {
		s.navMu.Unlock()
		return s.logNavOutcome(NavResult{Outcome: NavExecuted, Err: fmt.Errorf("navigate: %w", navErr)}, "navigate-send-failed")
	}
	// cdp.send decodes only the nested Runtime.evaluate result shape (see
	// cdp.CDP.send's switch) — Page.navigate has no nested "result" field, so
	// NoLogSend's decoded return for it is always (nil, nil) on a successful
	// send, never a value carrying an in-band errorText. There is no
	// reachable way to observe a failed-but-transport-successful navigation
	// from this call: the awaitGenerationReady barrier timeout and
	// awaitRouteSettled below are the only detectors for an in-band
	// navigation failure (a blocked/invalid URL, etc.) — both bounded by the
	// same verifyCap the caller already applies.
	gen := s.bump(navNonce, "navigate")
	s.navMu.Unlock()

	// [Park predicate fix] Publish the specific generation THIS navigation
	// produced so off-lane parkers can identify it independent of when they
	// entered their own park (see navTargetGen's doc). Reset to 0 once this
	// navigation finishes, regardless of outcome — no navigation is
	// asserting a target after that.
	s.navTargetGen.Store(gen.id)
	defer s.navTargetGen.Store(0)

	if err := s.awaitGenerationReady(ctx, gen); err != nil {
		return s.logNavOutcome(NavResult{Outcome: NavExecuted, Err: fmt.Errorf("verification: %w", err)}, "verify-handler-timeout")
	}
	if err := s.awaitRouteSettled(ctx); err != nil {
		// [Finding 4] A navigation that lands on /error is not a broken
		// navigation to retry: the [NV2] error-page gate would have refused
		// it as a pre-nav check too had the daemon already been showing it,
		// so a post-nav landing there is classified identically —
		// NavSkippedOverlay, reason "error-page-gate" — rather than
		// NavExecuted with an "unexpected route" error. Boot recovery maps
		// NavSkippedOverlay to Expired and, critically, does NOT count it as
		// an executed attempt (only the NavExecuted case does).
		if errors.Is(err, errRouteLandedOnErrorPage) {
			return s.logNavOutcome(NavResult{Outcome: NavSkippedOverlay}, "error-page-gate")
		}
		return s.logNavOutcome(NavResult{Outcome: NavExecuted, Err: fmt.Errorf("verification: %w", err)}, "verify-route-mismatch")
	}
	return s.logNavOutcome(NavResult{Outcome: NavExecuted}, "verified")
}

// errRouteLandedOnErrorPage marks awaitRouteSettled's landed-on-/error
// outcome distinctly from a genuine unexpected-route mismatch — see the
// navigateAndVerify call site for how it is classified.
var errRouteLandedOnErrorPage = fmt.Errorf("playersession: navigation landed on the error page")

// awaitRouteSettled is §2.3 step 6's post-navigation verification, corrected:
// Page.navigate always lands the document on "/" first, and the auto-route to
// /playlist (AppWrapper.tsx) only fires once hydration finishes (isInitialized) —
// well after StageHandler resolves. "/" is ALSO a legitimate steady state
// (nothing currently cast), not just a transient. So this polls the route
// (bounded by ctx, the same cap NavigateHome/Inline already apply) until
// either it reaches /playlist or /sleep (immediate success), it reaches
// /error (errRouteLandedOnErrorPage — see the navigateAndVerify call site for
// why that is classified separately, not as a generic mismatch), it reaches
// anything ELSE (a genuine mismatch — the navigation landed somewhere truly
// unexpected), or ctx is done while it is still "/" — which is treated as a
// verified landing, not a failure: a navigation that repaired the wall into
// an idle steady state must not burn a boot-recovery attempt or report
// failure to the relayer. An unavailable probe (old player, CDP hiccup) is
// "not classifiable", same as the gate checks elsewhere in this package —
// never a mismatch.
func (s *Session) awaitRouteSettled(ctx context.Context) error {
	interval := s.pollInterval
	if interval <= 0 {
		interval = defaultPollInterval
	}
	for {
		route, ok := s.currentRoute(ctx)
		if !ok {
			return nil
		}
		switch route {
		case routePlaylist, routeSleep:
			return nil
		case routeError:
			// [Finding 4] Landed on /error: not a mismatch to retry — see
			// the navigateAndVerify call site for how this is classified.
			return errRouteLandedOnErrorPage
		case "/":
			// Still deciding (pre-hydration) or a legitimate idle steady
			// state — keep polling until it moves or ctx runs out.
		default:
			return fmt.Errorf("unexpected route %q", route)
		}
		select {
		case <-ctx.Done():
			// "/" persisted through the whole verification window: accept it
			// as settled rather than timing this out as a failure.
			return nil
		case <-time.After(interval):
		}
	}
}

// stampNavNonce writes a per-navigation nonce into the CURRENT (pre-navigate)
// document before Page.navigate, exactly like setupui.execPageReload. The old
// and new documents run the SAME player app, so handler presence alone cannot
// tell them apart during the transition window; a StageHandler probe answered
// by the dying document would resolve the barrier early (the exact loss the
// nonce prevents). The nonce is written to window.__ffosNavNonce, a SEPARATE
// global from window.__ffosDocStamp [M4] — see StageProbeFunc's doc for why
// reusing the stamp carrier here would cause spurious generation bumps. This
// also means a FAILED Page.navigate (the nonce left behind in the still-live
// document) can no longer cause one either, since ObserveStatusStamp never
// reads this global. Best-effort: a failed stamp write only degrades the
// probe back to plain handler-presence, it never blocks the navigation.
func (s *Session) stampNavNonce() string {
	nonce := strconv.FormatInt(s.clock.Now().UnixNano(), 36)
	if _, err := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    fmt.Sprintf(`JSON.stringify({nonce: window.__ffosNavNonce = %q})`, nonce),
		"returnByValue": true,
	}); err != nil {
		s.logger.Debug("playersession: pre-navigate stamp failed; readiness probe degrades to handler presence", zap.Error(err))
		return ""
	}
	return nonce
}

func (s *Session) closed() bool {
	return s.rootCtx != nil && s.rootCtx.Err() != nil
}

func (s *Session) isAsleep() bool {
	if s.asleep == nil {
		return false
	}
	return s.asleep()
}

// rawPlayerStatus issues a one-shot best-effort read of the structured
// status probe, returning the decoded payload UNGATED by protocol version.
// ok is false only when the probe truly did not answer (CDP down, ctx
// already done, old player without __ffosPlayerStatus, or a malformed/absent
// response) — never because of the protocol field's value. Shared by
// currentRoute (which layers the protocol gate back on for its own callers)
// and isErrorPage (which deliberately does not — see its doc).
func (s *Session) rawPlayerStatus(ctx context.Context) (map[string]any, bool) {
	if err := ctx.Err(); err != nil {
		return nil, false
	}
	if !s.cdp.Initialized() {
		return nil, false
	}
	result, err := s.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression":    `window.__ffosPlayerStatus ? window.__ffosPlayerStatus() : null`,
		"returnByValue": true,
	})
	if err != nil {
		return nil, false
	}
	m, ok := result.(map[string]any)
	if !ok {
		return nil, false
	}
	return m, true
}

// currentRoute issues a one-shot best-effort read of the structured status
// probe's route field, PROTOCOL-GATED. It does NOT participate in the
// generation stage cache (route must be read fresh on every gate check /
// verification, not cached across an entire generation's lifetime). ok is
// false when the probe is unavailable (CDP down, old player without
// __ffosPlayerStatus, a malformed/absent response, OR a protocol newer than
// this package understands — never misread against a payload shape it may no
// longer match) — callers must treat that as "not classifiable", never as a
// specific route. Used by post-navigation verification (awaitRouteSettled)
// and boot recovery's classifier. NOT used by the error-page safety gate
// (isErrorPage) — see its doc for why that one stays protocol-independent.
func (s *Session) currentRoute(ctx context.Context) (string, bool) {
	m, ok := s.rawPlayerStatus(ctx)
	if !ok {
		return "", false
	}
	if protocol, ok := m["protocol"].(float64); !ok || protocol != statusProtocolVersion {
		return "", false
	}
	route, ok := m["route"].(string)
	if !ok {
		return "", false
	}
	return route, true
}

// isErrorPage is the [NV2] error-page safety gate: it reads route directly
// from the raw status payload, INDEPENDENT of protocol version. Gating this
// specific safety check behind "protocol == statusProtocolVersion" (as
// currentRoute does for the rest of the classifier) would let a future
// protocol-bumped player silently disable "never navigate over /error" the
// moment it starts advertising a newer protocol number this package hasn't
// been taught yet — route is a plain string field, stable across protocol
// versions, so there is no decoding hazard in reading it unconditionally.
// The full classifier (currentRoute, and boot recovery's own readPlayerStatus)
// stays protocol-gated; only this narrow safety gate bypasses it.
func (s *Session) isErrorPage(ctx context.Context) bool {
	m, ok := s.rawPlayerStatus(ctx)
	if !ok {
		return false
	}
	route, _ := m["route"].(string)
	return route == routeError
}

func (s *Session) overlayOwnersActive() (bool, string) {
	s.mu.Lock()
	owners := make([]overlayOwner, len(s.overlayOwners))
	copy(owners, s.overlayOwners)
	s.mu.Unlock()
	for _, o := range owners {
		if o.active != nil && o.active() {
			return true, o.name
		}
	}
	return false, ""
}

func (s *Session) logNavOutcome(res NavResult, reason string) NavResult {
	s.logger.Info("playersession: navigation outcome",
		zap.String("reason", reason),
		zap.Uint64("generation", s.Generation()),
		zap.String("outcome", res.Outcome.String()),
		zap.Error(res.Err),
	)
	return res
}
