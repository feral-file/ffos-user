package devicectl

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
)

// Package boot_recovery.go implements the boot player recovery state machine
// (design doc §3.2), replacing the Phase-1 atomics choreography
// (bootPlayerRecoveryDone/bootPlayerRecoveryPending +
// MaybeRecoverPlayerOnBootOnline/CompletePendingBootPlayerRecovery/
// finishPendingBootPlayerRecovery) with a bounded, re-entrant machine:
//
//	Idle -> Armed(bootWindow) -> Attempting(n) -> Succeeded
//	                           \-> Deferred(reason, backoff) -> Attempting
//	                           \-> Expired | Exhausted(maxExecuted=3)
//
// ALL transitions run under bootRecoveryMu (see the executor struct fields in
// executor.go). Every Deferred schedules a backoff timer
// (bootRecoveryBackoffLadder: 15s -> 60s -> 240s cap) as the PRIMARY
// re-entry; a session-generation reconciler and the sleep tracker's onAwake
// hook (both wired in main.go) are accelerators that re-enter early via
// RetryBootRecovery — never the only wake-up. `n` (bootRecoveryAttempts)
// counts EXECUTED attempts only (an evaluate or a navigation); no-connection
// deferrals and NavSuperseded/NavEvicted/NavSkipped* outcomes don't count.
//
// MaybeRecoverPlayerOnBootOnline and CompletePendingBootPlayerRecovery keep
// their Phase-1 names and signatures (the provisioning notifier and main.go's
// CDP on-connect callback call them unchanged) but are now thin adapters into
// this machine.

// bootRecoveryState is the machine's resting state.
type bootRecoveryState int

const (
	bootRecIdle bootRecoveryState = iota
	bootRecArmed
	bootRecAttempting
	bootRecDeferred
	bootRecSucceeded
	bootRecExpired
	bootRecExhausted
)

func (s bootRecoveryState) String() string {
	switch s {
	case bootRecIdle:
		return "idle"
	case bootRecArmed:
		return "armed"
	case bootRecAttempting:
		return "attempting"
	case bootRecDeferred:
		return "deferred"
	case bootRecSucceeded:
		return "succeeded"
	case bootRecExpired:
		return "expired"
	case bootRecExhausted:
		return "exhausted"
	default:
		return fmt.Sprintf("bootRecoveryState(%d)", int(s))
	}
}

// bootRecoveryMaxExecuted is Exhausted's budget: once this many attempts have
// EXECUTED (an evaluate or a navigation actually ran), a further deferral
// stops instead of scheduling another round.
const bootRecoveryMaxExecuted = 3

// bootRecoveryBackoffLadder is the Deferred re-entry backoff (design doc
// §3.2): 15s, 60s, 240s, then holds at 240s for any further deferral.
var bootRecoveryBackoffLadder = []time.Duration{15 * time.Second, 60 * time.Second, 240 * time.Second}

// BootRecoverySession is the narrow slice of playersession.Session the boot
// recovery state machine needs: the recovery navigation primitive and the
// generation counter for transition logging. Consumer-owned, mirroring
// setupui.NavigationSession; *playersession.Session satisfies it.
type BootRecoverySession interface {
	NavigateHome(opts playersession.NavOptions, done func(playersession.NavResult))
	Generation() uint64
}

// SetBootRecoverySession wires the playersession.Session escalations call
// (design doc §3.2: "Escalations call session.NavigateHome"). A nil-wired
// executor (test doubles, or a build wired before Phase 2b) degrades every
// escalation to a counted, deferred attempt rather than navigating — never
// panics.
func SetBootRecoverySession(exec Executor, sess BootRecoverySession, logger *zap.Logger) {
	setter, ok := exec.(interface{ setBootRecoverySession(BootRecoverySession) })
	if !ok {
		logger.Warn("Executor does not support boot recovery session wiring")
		return
	}
	setter.setBootRecoverySession(sess)
}

func (e *executor) setBootRecoverySession(sess BootRecoverySession) {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	e.bootRecoverySession = sess
}

// SetBootRecoveryContractPath overrides the player contract manifest path
// the capability fuse reads. Test seam only.
func SetBootRecoveryContractPath(exec Executor, path string, logger *zap.Logger) {
	setter, ok := exec.(interface{ setBootRecoveryContractPath(string) })
	if !ok {
		logger.Warn("Executor does not support boot recovery contract path override")
		return
	}
	setter.setBootRecoveryContractPath(path)
}

func (e *executor) setBootRecoveryContractPath(path string) {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	e.bootRecoveryContractPath = path
}

// SetBootRecoveryDaemonContext injects the daemon-lifetime context the
// backoff timer's sleep roots on [minor #4], so process shutdown cancels a
// pending backoff sleep instead of leaking the goroutine until it naturally
// elapses (up to 240s). Wired once at composition time.
func SetBootRecoveryDaemonContext(exec Executor, ctx context.Context, logger *zap.Logger) {
	setter, ok := exec.(interface{ setBootRecoveryDaemonContext(context.Context) })
	if !ok {
		logger.Warn("Executor does not support boot recovery daemon context wiring")
		return
	}
	setter.setBootRecoveryDaemonContext(ctx)
}

func (e *executor) setBootRecoveryDaemonContext(ctx context.Context) {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	e.bootRecoveryDaemonCtx = ctx
}

// RetryBootRecovery re-enters the boot recovery state machine early — the
// generation-bump and wake-edge ACCELERATORS design doc §3.2 describes.
// Backoff remains the PRIMARY re-entry trigger; this only makes re-entry
// happen sooner when a signal suggests the page or player state likely
// changed (a new document generation, or a confirmed wake). A no-op outside
// Armed/Deferred (Idle, Attempting, and every terminal state ignore it), and
// on an executor that doesn't support the state machine (test doubles).
func RetryBootRecovery(exec Executor, logger *zap.Logger) {
	r, ok := exec.(interface{ retryBootRecoveryEarly() })
	if !ok {
		logger.Warn("Executor does not support boot recovery early re-entry")
		return
	}
	r.retryBootRecoveryEarly()
}

func (e *executor) retryBootRecoveryEarly() {
	e.bootRecoveryMu.Lock()
	state := e.bootRecoveryState
	if state != bootRecArmed && state != bootRecDeferred {
		e.bootRecoveryMu.Unlock()
		return
	}
	// [M3] The accelerator (a generation bump or a confirmed wake edge —
	// both wired via RetryBootRecovery, design doc §3.2) is EARLY re-entry,
	// same as every other trigger; it must consult the boot-window probe too.
	// Without this, a device that boots into its sleep window keeps a
	// generation/wake-driven probe loop alive all night and NavigateHomes a
	// healthy page at the wake edge, hours past boot.
	if e.bootWindowExpiredLocked("boot-window-elapsed-at-accelerator") {
		e.bootRecoveryMu.Unlock()
		return
	}
	e.bootRecoveryMu.Unlock()
	go e.attemptBootRecovery(context.Background(), "accelerator")
}

// bootWindowExpiredLocked consults the boot lifecycle probe and, if the
// window has closed, transitions the machine straight to Expired (canceling
// any pending backoff timer) and returns true — matching design doc §3.2's
// "boot-window expiry while parked/deferred = Expired". A nil probe (no
// lifecycle wiring, e.g. some test doubles) never expires. Caller holds
// bootRecoveryMu.
func (e *executor) bootWindowExpiredLocked(reason string) bool {
	probe := e.bootLifecycleProbe
	if probe == nil || probe() {
		return false
	}
	e.cancelBootRecoveryBackoffLocked()
	e.transitionBootRecoveryLocked(bootRecExpired, reason)
	return true
}

// MaybeRecoverPlayerOnBootOnline is the boot-online entry point (Phase-1
// name/signature preserved): the provisioning notifier calls this on the
// first WAN-confirmed →Online/→Unprovisioned transition of a boot lifecycle
// (main.go wires it only when the daemon started within the boot window —
// see wireBootLifecycleHooks). It ARMS the machine exactly once per boot
// (Idle -> Armed is the one-shot latch, replacing bootPlayerRecoveryDone) and
// immediately attempts a round. See the package doc and design doc §3.2 for
// the full rationale (chromium-kiosk not gating on the network, the
// settled-devices-only scope, CDP-ordering vs. the provisioning domain).
func (e *executor) MaybeRecoverPlayerOnBootOnline(ctx context.Context) {
	e.bootRecoveryMu.Lock()
	if e.bootRecoveryState != bootRecIdle {
		e.bootRecoveryMu.Unlock()
		return
	}
	if !e.claimSettled() {
		// Unclaimed: the auto-claim flow owns the screen, there is no
		// playlist to repair, and anything cast after a claim loads with the
		// network already up — nothing this boot needs to repair. Terminal
		// (Expired) so the one-shot latch stays consumed, same as Phase 1.
		e.transitionBootRecoveryLocked(bootRecExpired, "unclaimed")
		e.bootRecoveryMu.Unlock()
		e.logger.Info("Boot player recovery: device unclaimed; claim flow owns the screen, nothing to recover")
		return
	}
	e.transitionBootRecoveryLocked(bootRecArmed, "wan-confirmed")
	e.bootRecoveryMu.Unlock()
	// Inline attempt: no boot-window gate here (see CompletePendingBootPlayerRecovery
	// for why the CDP-connect trigger gates and this one deliberately does
	// not) — WAN just arrived, so whatever page is on screen predates it and
	// is the broken load this hook repairs, however late that is.
	e.attemptBootRecovery(ctx, "online")
}

// CompletePendingBootPlayerRecovery is the CDP on-connect entry point
// (Phase-1 name/signature preserved): main.go calls this on every CDP
// (re)connect. A call with the machine Idle or already terminal is a no-op.
// Armed/Deferred and past the boot window (a first — or a much later —
// connection arriving after boot means Chromium just started with the
// network already up, a healthy load the boot scoping promises never to
// disturb) transitions straight to Expired without attempting anything.
// Otherwise it attempts a round.
func (e *executor) CompletePendingBootPlayerRecovery() {
	e.bootRecoveryMu.Lock()
	state := e.bootRecoveryState
	if state != bootRecArmed && state != bootRecDeferred {
		e.bootRecoveryMu.Unlock()
		return
	}
	if e.bootWindowExpiredLocked("boot-window-elapsed-at-cdp-connect") {
		e.bootRecoveryMu.Unlock()
		return
	}
	e.bootRecoveryMu.Unlock()
	e.attemptBootRecovery(context.Background(), "cdp-connect")
}

// attemptBootRecovery enters a round (if the machine is in a state that
// allows one) and runs the classification/repair logic. No-op if a round is
// already in flight (Attempting) or the machine is Idle/terminal.
func (e *executor) attemptBootRecovery(ctx context.Context, reason string) {
	if !e.enterBootRecoveryRound(reason) {
		return
	}
	e.runBootRecoveryRound(ctx)
}

func (e *executor) enterBootRecoveryRound(reason string) bool {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	switch e.bootRecoveryState {
	case bootRecArmed, bootRecDeferred:
		e.cancelBootRecoveryBackoffLocked()
		e.transitionBootRecoveryLocked(bootRecAttempting, reason)
		// [minor #9] Fresh round: clear the per-round record-once latch so
		// THIS round's first recordBootRecoveryAttemptExecuted() call
		// counts. See that function's doc for why a round can otherwise
		// consume two budget slots.
		e.bootRecoveryAttemptRecordedThisRound = false
		return true
	default:
		return false
	}
}

// cancelBootRecoveryBackoffLocked cancels and clears the pending backoff
// timer, if any. Caller holds bootRecoveryMu.
func (e *executor) cancelBootRecoveryBackoffLocked() {
	if e.bootRecoveryBackoffCancel != nil {
		e.bootRecoveryBackoffCancel()
		e.bootRecoveryBackoffCancel = nil
	}
}

// transitionBootRecoveryLocked records a state transition and emits the
// structured log design doc §3.2 requires: {from, to, reason, attempt,
// generation}. Caller holds bootRecoveryMu.
func (e *executor) transitionBootRecoveryLocked(to bootRecoveryState, reason string) {
	from := e.bootRecoveryState
	e.bootRecoveryState = to
	var gen uint64
	if e.bootRecoverySession != nil {
		gen = e.bootRecoverySession.Generation()
	}
	e.logger.Info("Boot player recovery: state transition",
		zap.Stringer("from", from),
		zap.Stringer("to", to),
		zap.String("reason", reason),
		zap.Int("attempt", e.bootRecoveryAttempts),
		zap.Uint64("generation", gen))
}

// deferBootRecovery moves the machine to Deferred and schedules the next
// backoff-ladder timer, UNLESS the executed-attempt budget is already spent,
// in which case it moves to Exhausted instead (no further timer).
// bootRecoveryTerminalLocked reports whether the machine already sits in a
// terminal resting state (Succeeded/Expired/Exhausted). Caller holds
// bootRecoveryMu.
func (e *executor) bootRecoveryTerminalLocked() bool {
	switch e.bootRecoveryState {
	case bootRecSucceeded, bootRecExpired, bootRecExhausted:
		return true
	default:
		return false
	}
}

func (e *executor) deferBootRecovery(reason string) {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	// [minor #12] Guard against a late NavigateHome NavResult callback (or
	// any other stray caller) moving an already-terminal state: the machine
	// settled via some other path in the meantime, and re-deferring (or
	// worse, exhausting) it now would corrupt the terminal invariant and
	// pollute the transition log with a bogus entry.
	if e.bootRecoveryTerminalLocked() {
		e.logger.Debug("Boot player recovery: ignoring late defer against an already-terminal state",
			zap.String("reason", reason), zap.Stringer("state", e.bootRecoveryState))
		return
	}
	if e.bootRecoveryAttempts >= bootRecoveryMaxExecuted {
		e.transitionBootRecoveryLocked(bootRecExhausted, reason)
		return
	}
	e.transitionBootRecoveryLocked(bootRecDeferred, reason)
	e.scheduleBootRecoveryBackoffLocked()
}

func (e *executor) scheduleBootRecoveryBackoffLocked() {
	idx := e.bootRecoveryDeferCount
	if idx >= len(bootRecoveryBackoffLadder) {
		idx = len(bootRecoveryBackoffLadder) - 1
	}
	d := bootRecoveryBackoffLadder[idx]
	e.bootRecoveryDeferCount++
	// [minor #4] Root the sleep on the daemon-lifetime ctx (falling back to
	// Background for callers that predate the wiring) so process shutdown
	// cancels it too, not just a newer trigger superseding this timer.
	root := e.bootRecoveryDaemonCtx
	if root == nil {
		root = context.Background()
	}
	ctx, cancel := context.WithCancel(root)
	e.bootRecoveryBackoffCancel = cancel
	clk := e.clock
	go func() {
		if err := clk.SleepContext(ctx, d); err != nil {
			return // canceled: a newer trigger (or shutdown) superseded this timer
		}
		// [M3] The backoff timer is EARLY-ELIGIBLE re-entry same as every
		// other trigger — it must consult the boot-window probe too, or a
		// device that boots into its sleep window keeps a 240s probe loop
		// alive all night and NavigateHomes a healthy page at the wake edge,
		// hours past boot.
		e.bootRecoveryMu.Lock()
		state := e.bootRecoveryState
		if state != bootRecArmed && state != bootRecDeferred {
			e.bootRecoveryMu.Unlock()
			return
		}
		if e.bootWindowExpiredLocked("boot-window-elapsed-at-backoff-timer") {
			e.bootRecoveryMu.Unlock()
			return
		}
		e.bootRecoveryMu.Unlock()
		e.attemptBootRecovery(context.Background(), "backoff-timer")
	}()
}

// recordBootRecoveryAttemptExecuted counts AT MOST ONE executed attempt per
// ROUND [minor #9], even though a single round's classification can reach
// TWO distinct CDP-level actions: an evaluateRefreshArtwork call that gets
// refused, followed by an escalated navigateForRecovery call whose async
// NavResult callback ALSO calls this. Without the latch that pair would
// silently consume two of the three-attempt budget for what is conceptually
// one round's repair action. enterBootRecoveryRound clears the latch at the
// START of every new round.
func (e *executor) recordBootRecoveryAttemptExecuted() {
	e.bootRecoveryMu.Lock()
	if !e.bootRecoveryAttemptRecordedThisRound {
		e.bootRecoveryAttempts++
		e.bootRecoveryAttemptRecordedThisRound = true
	}
	e.bootRecoveryMu.Unlock()
}

func (e *executor) succeedBootRecovery(reason string) {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	// [minor #12] See deferBootRecovery's identical guard doc.
	if e.bootRecoveryTerminalLocked() {
		e.logger.Debug("Boot player recovery: ignoring late succeed against an already-terminal state",
			zap.String("reason", reason), zap.Stringer("state", e.bootRecoveryState))
		return
	}
	e.transitionBootRecoveryLocked(bootRecSucceeded, reason)
}

func (e *executor) expireBootRecovery(reason string) {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	// [minor #12] See deferBootRecovery's identical guard doc.
	if e.bootRecoveryTerminalLocked() {
		e.logger.Debug("Boot player recovery: ignoring late expire against an already-terminal state",
			zap.String("reason", reason), zap.Stringer("state", e.bootRecoveryState))
		return
	}
	e.transitionBootRecoveryLocked(bootRecExpired, reason)
}

// capabilityState is checkPlayerStatusCapability's verdict (design doc
// §3.2/§4.3 capability fuse).
type capabilityState int

const (
	capabilityPresent capabilityState = iota
	capabilityUnreadable
	capabilityAbsent
)

// checkPlayerStatusCapability resolves whether the connected player
// advertises contracts.playerStatus. UNREADABLE (a transient read failure —
// boot ordering, an OTA mid-replace of the player bundle) is re-checked on
// every call and NEVER latches. ABSENT from a manifest that WAS successfully
// read latches conservative mode for the rest of this boot's recovery
// lifecycle (bootRecoveryConservative).
//
// [minor #19] The precise scope of what conservative mode gates, since
// "never navigates" overstates it: it gates ONLY runBootRecoveryRound's own
// unclassified-refusal fallback row (the bare non-ACK / unknown code default
// case that calls this function) — the caller there defers instead of
// escalating when this returns capabilityAbsent. It does NOT fuse the
// handler-never-ready row or the preview_update_failed-repeat row: both
// navigate UNCONDITIONALLY per the design doc §3.2 total table, regardless
// of bootRecoveryConservative's value, because those two rows already have
// their own independent evidence a dead/wedged page is the problem (a timed-
// out StageHandler probe, or a second identical live-page refusal) that does
// not depend on classifying the player's structured-status capability at
// all.
func (e *executor) checkPlayerStatusCapability() capabilityState {
	e.bootRecoveryMu.Lock()
	if e.bootRecoveryConservative {
		e.bootRecoveryMu.Unlock()
		return capabilityAbsent
	}
	path := e.bootRecoveryContractPath
	e.bootRecoveryMu.Unlock()
	if path == "" {
		path = setupui.DefaultContractPath
	}

	err := setupui.ValidatePlayerStatusContract(path)
	if err == nil {
		return capabilityPresent
	}
	if errors.Is(err, setupui.ErrPlayerContractUnreadable) {
		return capabilityUnreadable
	}
	e.bootRecoveryMu.Lock()
	e.bootRecoveryConservative = true
	e.bootRecoveryMu.Unlock()
	e.logger.Warn("Boot player recovery: player contract lacks contracts.playerStatus; unclassified refusals will defer conservatively instead of escalating (handler-never-ready and repeat preview-update-failed rows are unaffected and still navigate)",
		zap.Error(err))
	return capabilityAbsent
}

// playerStatus is the decoded window.__ffosPlayerStatus payload (design doc
// §4.1) this classifier needs.
type playerStatus struct {
	Route             string
	HandlerRegistered bool
	HasArtwork        bool
	BootHydration     string
}

const (
	bootHydrationPending          = "pending"
	bootHydrationOK               = "ok"
	bootHydrationHaltedCleared    = "halted_cleared"
	bootHydrationHaltedPreserving = "halted_preserving"
	bootHydrationFailed           = "failed"
)

const (
	statusRoutePlaylist = "/playlist"
	statusRouteSleep    = "/sleep"
	statusRouteError    = "/error"
)

// statusProtocolVersion is the __ffosPlayerStatus protocol version this
// classifier understands (design doc §4.1); mirrors
// playersession.statusProtocolVersion.
const statusProtocolVersion = 1

// readPlayerStatusRaw issues a ONE-SHOT evaluate of window.__ffosPlayerStatus
// and returns the raw decoded payload, UNGATED by protocol version. ok is
// false only when the probe truly did not answer (CDP down, old player
// without the structured status carrier, or a malformed/absent response) —
// never because of the protocol field's value. Shared by runBootRecoveryRound's
// [NV2] route-error safety pre-check (deliberately protocol-independent —
// route is a plain string, stable across protocol versions, and the "never
// navigate over /error" invariant must hold even against a future protocol
// bump decodePlayerStatus cannot otherwise decode) and decodePlayerStatus
// (which layers the protocol gate on top for the rest of the classifier), so
// both share the SAME evaluate result instead of costing two CDP round-trips
// per round. Mirrors playersession.Session.rawPlayerStatus's identical split.
func (e *executor) readPlayerStatusRaw() (map[string]any, bool) {
	if e.cdp == nil || !e.cdp.Initialized() {
		return nil, false
	}
	result, err := e.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
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

// decodePlayerStatus applies the protocol gate and the full precondition
// field decode to an already-fetched raw status payload (see
// readPlayerStatusRaw) — split out so the NV2 pre-check and this
// classification can share one evaluate result. ok is false when the raw
// fetch itself failed, OR the protocol advertised is one this classifier
// doesn't understand (never misread against a payload shape it may no
// longer match), OR the decoded fields are all zero-value (malformed/absent
// response) — callers must treat that as "not classifiable by structured
// status", never as a specific value.
func decodePlayerStatus(m map[string]any, rawOK bool) (playerStatus, bool) {
	if !rawOK {
		return playerStatus{}, false
	}
	if protocol, ok := m["protocol"].(float64); !ok || protocol != statusProtocolVersion {
		return playerStatus{}, false
	}
	var st playerStatus
	st.Route, _ = m["route"].(string)
	st.HandlerRegistered, _ = m["handlerRegistered"].(bool)
	st.HasArtwork, _ = m["hasArtwork"].(bool)
	st.BootHydration, _ = m["bootHydration"].(string)
	if st.Route == "" && !st.HandlerRegistered && !st.HasArtwork && st.BootHydration == "" {
		return playerStatus{}, false
	}
	return st, true
}

// runBootRecoveryRound performs ONE classification-and-action round (design
// doc §3.2's total table). Called with the machine already transitioned to
// Attempting; every exit path below calls exactly one of
// succeedBootRecovery/deferBootRecovery/expireBootRecovery/navigateForRecovery,
// leaving the machine in a well-defined resting state.
func (e *executor) runBootRecoveryRound(ctx context.Context) {
	// Row: cdp.Initialized()==false (fast-fail) -> Deferred, NO attempt counted.
	if e.cdp == nil || !e.cdp.Initialized() {
		e.deferBootRecovery("no-cdp-connection")
		return
	}

	// ONE evaluate of window.__ffosPlayerStatus per round, shared by the NV2
	// pre-check below and the protocol-gated classification that follows —
	// see readPlayerStatusRaw/decodePlayerStatus's doc for why this must not
	// cost two CDP round-trips.
	rawStatus, rawOK := e.readPlayerStatusRaw()

	// [NV2 fail-open fix] Read route independently of protocol version
	// BEFORE anything else: this must hold even against a future protocol
	// bump the protocol-gated classification below cannot otherwise decode.
	// The daemon-chosen error page deliberately owns the wall (§2.3's
	// error-page gate would refuse a navigate here too); nothing this
	// machine can repair.
	if rawOK {
		if route, _ := rawStatus["route"].(string); route == statusRouteError {
			e.expireBootRecovery("route-error")
			return
		}
	}

	// statusProbedThisRound is LIVE PROOF the connected player supports the
	// structured status probe [minor #10]: when true, the unclassified-
	// refusal fallback below skips the on-disk manifest fuse entirely — a
	// stale/misread manifest file has nothing to add once this round already
	// has direct evidence, and consulting it anyway risks the fuse's own
	// ABSENT-from-a-successfully-read-manifest side effect (latching
	// conservative mode) against a manifest that no longer matches reality.
	statusProbedThisRound := false
	if status, ok := decodePlayerStatus(rawStatus, rawOK); ok {
		statusProbedThisRound = true
		// [minor #7] Route gates are checked FIRST, before any hydration or
		// artwork classification — they are safety, not just another
		// signal. This is a deliberate divergence from the §3.2 table's row
		// ORDER (which lists bootHydration=failed and hasArtwork=false
		// above the route rows): a hydration-failed page AND an
		// error-routed wall can co-occur (e.g. the watchdog's own restart
		// path, or a stale error carrying into a fresh hydration attempt),
		// and [NV2] — NEVER navigate on /error — must hold regardless of
		// what bootHydration says. The route==/error row itself is handled
		// by the protocol-independent pre-check above (unreachable here as
		// a result — route-sleep is the only route row left in this
		// protocol-gated switch); checking route first, unconditionally, is
		// what makes NV2 true structurally instead of relying on it being
		// re-derived correctly at every future callsite (the downstream
		// §2.3 error-page gate is defense-in-depth, not the only place this
		// is enforced).
		switch status.Route {
		case statusRouteSleep:
			e.deferBootRecovery("route-sleep")
			return
		}

		switch status.BootHydration {
		case bootHydrationPending:
			e.deferBootRecovery("boot-hydration-pending")
			return
		case bootHydrationHaltedCleared:
			e.succeedBootRecovery("boot-hydration-halted-cleared")
			return
		case bootHydrationFailed:
			e.navigateForRecovery(ctx, "boot-hydration-failed")
			return
		case bootHydrationHaltedPreserving:
			// [NV9]: already classified by route above (sleep/error) when
			// applicable — neither fired here, so there is nothing further
			// tied to halted_preserving itself; fall through to the
			// hasArtwork/handler checks below like any other route.
		}

		if !status.HasArtwork && status.BootHydration == bootHydrationOK {
			e.succeedBootRecovery("no-artwork-hydration-ok")
			return
		}
		if status.HasArtwork && !status.HandlerRegistered && status.Route == statusRoutePlaylist {
			e.deferBootRecovery("handler-not-yet-registered")
			return
		}
		// Structured status says nothing more (e.g. handler registered, has
		// artwork, on /playlist): fall through to the live refresh attempt
		// below, which is the actual "does the command work" test.
	}

	// Row: StageHandler timeout on a LIVE connection -> attempt: NavigateHome.
	if !e.awaitPlayerCommandHandlerReady() {
		if e.cdp.Initialized() {
			e.navigateForRecovery(ctx, "handler-never-ready")
			return
		}
		e.deferBootRecovery("cdp-dropped-during-handler-wait")
		return
	}

	code, refreshErr := e.evaluateRefreshArtwork()
	e.recordBootRecoveryAttemptExecuted()
	if refreshErr == nil {
		e.succeedBootRecovery("refresh-acked")
		return
	}

	switch code {
	case playerresponse.CodeHandlerPending:
		e.deferBootRecovery("refusal-handler-pending")
	case playerresponse.CodeNoArtwork:
		e.deferBootRecovery("refusal-no-artwork")
	case playerresponse.CodePreviewUpdateFailed:
		if e.consumeBootRecoveryPreviewUpdateFailedTolerance() {
			e.deferBootRecovery("refusal-preview-update-failed-first")
			return
		}
		e.navigateForRecovery(ctx, "refusal-preview-update-failed-repeat")
	default:
		// Bare non-ACK / unknown code (design doc §3.2 rows 15/16): the
		// capability fuse decides whether an unclassifiable refusal is safe
		// to escalate — UNLESS this round already read the structured status
		// successfully [minor #10], which is live proof of the same
		// capability and makes the on-disk manifest fuse redundant.
		if statusProbedThisRound || e.checkPlayerStatusCapability() == capabilityPresent {
			e.navigateForRecovery(ctx, "unclassified-refusal")
			return
		}
		e.logger.Warn("Boot player recovery: unclassified refusal on a player without confirmed status capability; deferring conservatively",
			zap.Error(refreshErr))
		e.deferBootRecovery("unclassified-refusal-conservative")
	}
}

// consumeBootRecoveryPreviewUpdateFailedTolerance returns true (and marks the
// one-shot tolerance consumed) the FIRST time code=preview_update_failed is
// seen this boot; false on every subsequent occurrence.
func (e *executor) consumeBootRecoveryPreviewUpdateFailedTolerance() bool {
	e.bootRecoveryMu.Lock()
	defer e.bootRecoveryMu.Unlock()
	if e.bootRecoveryPreviewUpdateFailedSeen {
		return false
	}
	e.bootRecoveryPreviewUpdateFailedSeen = true
	return true
}

// navigateForRecovery escalates to the session's recovery navigation
// primitive. The NavResult rows (design doc §3.2) drive the machine from the
// done callback, which runs asynchronously — this function returns
// immediately.
func (e *executor) navigateForRecovery(ctx context.Context, reason string) {
	_ = ctx // reserved: NavigateHome's own bounded ctx is used internally
	e.bootRecoveryMu.Lock()
	sess := e.bootRecoverySession
	e.bootRecoveryMu.Unlock()
	if sess == nil {
		// No session wired: treat as a failed executed attempt so the
		// machine still backs off instead of spinning silently.
		e.logger.Warn("Boot player recovery: navigation escalation requested but no session is wired; deferring",
			zap.String("reason", reason))
		e.recordBootRecoveryAttemptExecuted()
		e.deferBootRecovery("no-session-wired:" + reason)
		return
	}
	sess.NavigateHome(playersession.NavOptions{}, func(res playersession.NavResult) {
		switch res.Outcome {
		case playersession.NavExecuted:
			e.recordBootRecoveryAttemptExecuted()
			if res.Err == nil {
				e.succeedBootRecovery("navigate-verified:" + reason)
			} else {
				e.deferBootRecovery("navigate-failed:" + reason)
			}
		case playersession.NavSkippedOverlay:
			e.expireBootRecovery("navigate-skipped-overlay:" + reason)
		case playersession.NavSkippedAsleep:
			e.deferBootRecovery("navigate-skipped-asleep:" + reason)
		case playersession.NavSuperseded, playersession.NavEvicted:
			e.deferBootRecovery("navigate-superseded-or-evicted:" + reason)
		}
	})
}

// evaluateRefreshArtwork asks the live player app to re-mount the current
// artwork via the same window.handleCDPRequest envelope every player command
// uses. code is the classified refusal code (playerresponse.Refusal; "" for
// a transport failure or an ACKed refresh) so the caller can drive the §3.2
// classification table without re-parsing the reply. err is nil only on an
// ACK.
func (e *executor) evaluateRefreshArtwork() (code string, err error) {
	if e.cdp == nil {
		return "", fmt.Errorf("cdp client is not configured")
	}
	command := commands.Command{Type: commands.CMD_REFRESH_ARTWORK}
	payload, jerr := command.JSON()
	if jerr != nil {
		return "", fmt.Errorf("marshal refreshArtwork payload: %w", jerr)
	}
	result, serr := e.cdp.Send(cdp.METHOD_EVALUATE, map[string]any{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(payload)),
	})
	if serr != nil {
		return "", fmt.Errorf("send refreshArtwork to player: %w", serr)
	}
	if playerresponse.OK(result) {
		return "", nil
	}
	reason, refusalCode, ok := playerresponse.Refusal(result)
	if !ok {
		return "", fmt.Errorf("player did not acknowledge refreshArtwork")
	}
	if reason == "" {
		return refusalCode, fmt.Errorf("player refused refreshArtwork with no reason")
	}
	return refusalCode, fmt.Errorf("player refused refreshArtwork: %s", reason)
}
