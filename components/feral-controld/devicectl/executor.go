package devicectl

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/otagate"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

var CmdOK = struct {
	OK bool `json:"ok"`
}{
	OK: true,
}

const (
	// AnalyticsToggleOffFile is the sentinel file that disables proactive metrics collection.
	AnalyticsToggleOffFile = "/home/feralfile/.state/analytics-toggle-off"
	// BetaFeaturesToggleOnFile is the sentinel file that enables beta features (default is off).
	BetaFeaturesToggleOnFile = "/home/feralfile/.state/beta-features-toggle-on"
	// SavedVolumeFile stores the user's volume setting to persist across reboots.
	SavedVolumeFile = "/home/feralfile/.state/saved-volume"
	// maxClickAndDragCursorOffsets bounds a single pressed drag batch so one
	// relayer request cannot hold Chromium in mouse-down state for unbounded work.
	maxClickAndDragCursorOffsets = 16
	// maxZoomGestureSteps bounds a single zoom request so relayer traffic cannot
	// monopolize the executor on arbitrarily large gesture batches.
	maxZoomGestureSteps = 16
)

type Device struct {
	ID       string `json:"device_id"`
	Name     string `json:"device_name"`
	Platform int    `json:"platform"`
}

//go:generate mockgen -source=executor.go -destination=../mocks/executor.go -package=mocks -mock_names=Executor=MockExecutor
type Executor interface {
	SaveLastSysMetrics(metrics []byte)
	Execute(ctx context.Context, cmd commands.Command) (interface{}, error)
	// SetClaimObserver registers a callback invoked when the device's claim
	// state changes (e.g. a successful connect claims the device). It is the
	// seam that lets the mediator re-register mDNS with an updated `claimed` TXT
	// without coupling the executor to the mediator. Set once at wiring time.
	SetClaimObserver(observer func(claimed bool))
	// SetSetupUI injects the process-wide setup-narration surface so the
	// controld-owned claim/factory-reset/OTA-failure narration shares ONE
	// setupui.Service with the provisioning domain. Set once at wiring time; the
	// lazy setupUI() fallback still covers tests that do not inject.
	SetSetupUI(ui *setupui.Service)
	// ResetStaged reports that a factory reset is staged and its reboot is
	// pending, so the command surface must stay closed (see the resetStaged
	// field). It is a REQUIRED interface method rather than an optional
	// type-asserted seam on purpose: an implementation that silently lacked it
	// would fail OPEN, which for this guard means serving commands to a former
	// owner on a device mid-wipe. Enforcement lives in commandrouter.Process —
	// the one dispatch point every transport and every command family shares;
	// devicectl only owns the latch.
	ResetStaged() bool
}

type executor struct {
	sync.Mutex
	cdp          cdp.CDP
	deviceStatus status.DeviceStatus
	logger       *zap.Logger

	// State
	lastSysMetrics []byte

	// claimObserver, when set, is notified on claim-state transitions. Set once
	// at wiring time before commands are served, so it needs no lock.
	claimObserver func(claimed bool)

	// Add reference to StatusPoller to get metrics
	statusPoller status.Poller

	// Mouse position tracking
	cursorPositionX   float64
	cursorPositionY   float64
	screenWidth       float64
	screenHeight      float64
	screenInitialized bool
	movingScaleFactor float64

	// Deps
	json  wrapper.JSON
	os    wrapper.OS
	exec  wrapper.Exec
	math  wrapper.Math
	clock wrapper.Clock

	panelDDC ddc.PanelDDC

	// otaGate owns the OTA update flow (single-flight guard, retry ladder,
	// version check, permanent-failure latch) ported from feral-setupd. Built
	// lazily via otaGateOnce so New()'s wiring is unchanged during the
	// setupd->controld merge.
	otaGate     *otagate.Gate
	otaGateOnce sync.Once

	// otaUpdateInFlight single-flights the detached user-triggered update
	// (updateToLatest). The command is fire-and-forget, so without this a retry
	// storm would stack goroutines that all join the gate's ONE flight and then
	// log the same outcome N times. The gate's own single-flight still governs
	// correctness — this only keeps the command path from spawning redundant
	// waiters. Deliberately scoped to this command: the pre-claim and startup
	// entry points must still be free to open a flight that a later
	// updateToLatest joins (see otagate.Gate.do).
	//
	// Unlike logUploadInFlight this needs no timeout to pair with. The latch is
	// released when RequestUpdate returns, so its lifetime is a strict subset of
	// the gate's own singleflight key: a wedged updater (otagate's tail has no
	// overall deadline and runs until ctx cancel) would already have wedged every
	// later update by parking it on that dead flight, latch or no latch. Adding a
	// bound here would release the latch while the flight it guards was still
	// stuck, which buys nothing and hides the real wedge.
	otaUpdateInFlight atomic.Bool

	// setupNarrator is the on-screen setup-narration surface driven by the
	// controld-owned claim/factory-reset flows (dormant while setupd owns setup).
	// Built lazily via setupUI() from the existing CDP seam so New()'s wiring is
	// unchanged during the merge; tests set it directly to a spy.
	setupNarrator     setupNarrator
	setupNarratorOnce sync.Once

	// autoClaimInFlight single-flights the online-triggered claim flow
	// (MaybeShowClaimQROnOnline): connectivity flaps must not stack concurrent
	// pre-claim gates.
	autoClaimInFlight atomic.Bool

	// autoClaimWake (built lazily via autoClaimWakeChan, buffered 1) lets an
	// online transition or topic assignment that lands while a claim-flow run
	// is in flight preempt that run's stretched ladder-failure backoff. The
	// in-flight guard above would otherwise silently swallow those
	// re-triggers for hours — the invariant the guard protects is "no
	// concurrent gate runs", not "no wake-ups".
	autoClaimWakeOnce sync.Once
	autoClaimWake     chan struct{}

	// pairingConfirmed latches the cloud's showPairingQRCode(false) pairing
	// confirmation. connect() (the claim) sets ConnectedDevice, but the
	// confirmation does NOT — without this latch the auto-claim loop stayed
	// blind to it and could repaint the claim QR over a paired device's
	// Ready/hidden screen. In-memory: a restart re-derives via deviceClaimed
	// (connect precedes the confirmation in the normal flow).
	pairingConfirmed atomic.Bool

	// resetStaged latches while a factory reset is staged and its reboot is
	// pending. It exists because the reset's own unclaim (clearPersistedClaim)
	// ARMS the very things it needs held off during the reset script's ~8s
	// pre-reboot window, on a relayer session that stays open:
	//
	//   - the auto-claim flow, which stops being a no-op the moment
	//     claimSettled() goes false, and repaints "finalizing" then the claim QR
	//     over the factory_reset narration. Its trigger is a topic-less relayer
	//     reconnect: the persisted topic is cleared, so ANY reconnect (the read
	//     loop's own on a socket error — relayer.background — or the mediator's
	//     sysmetrics reconcile) draws a fresh topic, and that empty->set edge
	//     fires the mediator's topic observer. The same edge also fires if the
	//     server re-sends a system message on the ESTABLISHED socket, so
	//     removing any single reconnect path does not close this.
	//   - every inbound command with an effect that OUTLIVES the reset. The
	//     candidate boot can roll back to this subvolume, so a write under it
	//     survives: connect re-persists ConnectedDevice, sshAccess writes
	//     authorized_keys, the analytics/beta toggles write state sentinels,
	//     and updateToLatestVersion arms a competing bootctl one-shot that can
	//     displace the reset's own. commandrouter.Process rejects all of them
	//     (see servedDuringFactoryReset) — deliberately NOT devicectl.Execute,
	//     which only ever sees the device-control subset.
	//
	// Released on the two paths where the reboot provably is not coming: the
	// unit failing to start, and the stuck-reset watchdog. Both matter because
	// a stuck latch is worse than the bug it prevents — it would leave a live,
	// unclaimed device permanently refusing commands behind a "do not power
	// off" panel. In-memory by design: a successful reset reboots into a
	// subvolume where none of this state exists.
	resetStaged atomic.Bool

	// staleOverlaySwept gates the once-per-process boot reconciliation of the
	// player's overlay: narration is in-memory, so after a daemon restart the
	// player keeps rendering the PREVIOUS life's overlay (e.g. a claim QR
	// painted before a crash on a device that has since been claimed).
	staleOverlaySwept atomic.Bool

	// startupOTAGateDone latches the once-per-process boot OTA gate for claimed
	// devices (MaybeRunStartupOTAGateOnOnline): later connectivity flaps must
	// not re-run a check the boot already settled. Deliberately left clear
	// only when the retry loop is aborted by ctx (shutdown, or the backoff
	// sleep interrupted) so a later online transition retries; an exhausted
	// VersionCheckFailed budget DOES latch — see startupOTAGateMaxCheckAttempts.
	startupOTAGateDone atomic.Bool

	// startupOTAGateInFlight single-flights the boot OTA gate the same way
	// autoClaimInFlight guards the claim flow: online flaps must not stack
	// concurrent gate runs (each holds a retry-backoff loop).
	startupOTAGateInFlight atomic.Bool

	// startupOTAGateDeferLogged bounds the post-window deferral log to once
	// per process: deferral deliberately does NOT latch the done flag, and
	// the notifier re-fires the hook on every WAN-confirmed transition, so a
	// flapping WAN would otherwise emit the Info line unboundedly.
	startupOTAGateDeferLogged atomic.Bool

	// startupOTAGate is the gate call MaybeRunStartupOTAGateOnOnline drives.
	// Overridable in tests (same pattern as logUploaderFactory); nil in
	// production, where it resolves to otaGateInstance().EnsureLatestAtStartup.
	startupOTAGate func(ctx context.Context) (otagate.Result, error)

	// bootRecoveryMu serializes EVERY boot player recovery state-machine
	// transition (design doc §5): state, executed-attempt count, the
	// backoff-ladder index, the conservative-mode capability-fuse latch, the
	// preview_update_failed one-shot-tolerance flag, and the pending backoff
	// timer's cancel func. See boot_recovery.go.
	bootRecoveryMu sync.Mutex
	// bootRecoveryState is the machine's resting state (Idle/Armed/Attempting/
	// Deferred/Succeeded/Expired/Exhausted). Idle is the zero value, so a bare
	// executor starts un-armed exactly like the old bootPlayerRecoveryDone==false.
	bootRecoveryState bootRecoveryState
	// bootRecoveryAttempts counts EXECUTED attempts only (a refreshArtwork
	// evaluate, or a NavigateHome call that reached NavExecuted) — no-connection
	// deferrals and NavSuperseded/NavEvicted/NavSkipped* outcomes don't count.
	// Exhausted(bootRecoveryMaxExecuted) fires once this would otherwise defer
	// again.
	bootRecoveryAttempts int
	// bootRecoveryAttemptRecordedThisRound latches once
	// recordBootRecoveryAttemptExecuted has counted an attempt for the
	// CURRENT round, so a round that reaches both an
	// evaluateRefreshArtwork refusal AND an escalated navigateForRecovery
	// call only ever consumes one budget slot. Cleared at the start of every
	// new round (enterBootRecoveryRound).
	bootRecoveryAttemptRecordedThisRound bool
	// bootRecoveryDeferCount indexes bootRecoveryBackoffLadder for the NEXT
	// scheduled backoff; separate from bootRecoveryAttempts because a deferral
	// still backs off even when it doesn't count as an attempt.
	bootRecoveryDeferCount int
	// bootRecoveryConservative latches once the player contract manifest is
	// READ but lacks contracts.playerStatus (§5.3 capability fuse): from
	// then on an unclassifiable refusal is logged and deferred, never escalated
	// to NavigateHome. A read FAILURE (unreadable manifest) never latches this —
	// see checkPlayerStatusCapability.
	bootRecoveryConservative bool
	// bootRecoveryPreviewUpdateFailedSeen tracks the "once" in code=
	// preview_update_failed → Deferred once, then attempt: NavigateHome (§5.2
	// row). Scoped to one Arm→terminal cycle (the machine only Arms once per
	// boot), so it never needs resetting.
	bootRecoveryPreviewUpdateFailedSeen bool
	// bootRecoveryBackoffCancel cancels the currently-scheduled backoff timer,
	// if any. Canceled and cleared whenever a round is entered (Armed/Deferred
	// -> Attempting) so a superseding trigger (CDP connect, generation bump,
	// onAwake) cannot leave a stale timer that later double-fires a round.
	bootRecoveryBackoffCancel context.CancelFunc
	// bootRecoverySession, when set (devicectl.SetBootRecoverySession), is the
	// playersession.Session the state machine escalates dead-page classification
	// to via NavigateHome (§5). nil (tests, builds that predate the session)
	// makes every escalation degrade to a counted, deferred attempt instead of
	// navigating — never panics.
	bootRecoverySession BootRecoverySession
	// bootRecoveryContractPath overrides the player contract manifest path the
	// capability fuse reads (zero means setupui.DefaultContractPath). Test seam
	// only; production never sets it.
	bootRecoveryContractPath string
	// bootRecoveryDaemonCtx, when set (SetBootRecoveryDaemonContext), roots the
	// backoff timer's sleep so process shutdown cancels a pending
	// sleep instead of leaking the goroutine until it naturally elapses (up to
	// 240s). nil (tests, builds that predate the wiring) falls back to
	// context.Background() — the timer is then only ever canceled by a newer
	// trigger superseding it, exactly as before this seam existed.
	bootRecoveryDaemonCtx context.Context

	// bootLifecycleProbe reports whether the kernel is still within the boot
	// window (main wires it to re-read /proc/uptime). It scopes the two
	// boot-hook paths whose late execution would be a mid-exhibition
	// disturbance, and deliberately NOT the one whose late execution is the
	// repair itself:
	//   - the startup OTA gate's ENTRY: a device that booted offline and
	//     gains WAN hours later must not launch a Required-mode update (and
	//     reboot) mid-exhibition — that update belongs to the nightly timer;
	//   - the parked player recovery's DEFERRED completion: a CDP connection
	//     arriving hours after boot means Chromium just started (display
	//     plugged in) and its page loaded with the network already up;
	//   - but NOT the player recovery's INLINE path: however late WAN
	//     arrives, the page it repairs loaded broken at boot and stays
	//     broken until repaired.
	// nil (tests, doubles) means no expiry.
	bootLifecycleProbe func() bool

	// networkHealth, when wired (SetNetworkHealth), composes the additive
	// §4.7 network object attached to getDeviceStatus replies.
	networkHealth func(ctx context.Context) *status.NetworkHealth

	// networkDiagnostics, when wired (SetNetworkDiagnostics), runs the netlog
	// diagnosis ladder once on demand for CMD_RUN_NETWORK_DIAGNOSTICS (stage
	// 2c). nil renders the command unavailable — same posture as
	// wifiSetupStarter: reject, never pretend.
	networkDiagnostics func(ctx context.Context) (any, error)

	// wifiSetupStarter, when wired (SetWifiSetupStarter), runs the
	// provisioning machine's startWifiSetup admission and queues the
	// user-requested raise (provisioning.Machine.StartWifiSetup). nil renders
	// the command unavailable — a wiring that predates the seam must reject,
	// not pretend.
	wifiSetupStarter func(ctx context.Context) error

	// internetProbe, when wired (SetInternetProbe), reports cached internet
	// reachability. The claim flow's topic-wait expiry consults it to tell
	// "reachable LAN but no WAN — the topic can never arrive" (narrate it:
	// the unclaimed wired no-WAN device otherwise ends at a black screen,
	// docs/network-recovery-ux.md §4.6) from "online but the relayer is slow"
	// (keep today's silent hide). Deliberately TRI-STATE (value, error), not a
	// bool: only a real "offline" verdict may narrate — a monitord restart or
	// D-Bus timeout at expiry proves nothing, and degrading it to false would
	// paint "no internet access" over a healthy network. nil, like an error,
	// keeps the silent hide unconditionally.
	internetProbe func(ctx context.Context) (bool, error)

	// otaGateEntryProbe is the startup OTA gate's OWN entry-window predicate
	// (main wires it to re-read /proc/uptime against the wider
	// startupOTAGateEntryWindow). Split from bootLifecycleProbe because the
	// two consumers tolerate very different lateness: the parked player
	// recovery expiry guards a page that loaded with the network already up
	// (minutes matter), while the gate's entry only needs to exclude a WAN
	// arriving mid-exhibition, hours later — and WAN routinely trails boot
	// past the 2-minute window on a site-wide power restore, which must still
	// force the boot OTA. nil falls back to bootLifecycleProbe (older wiring,
	// test doubles), preserving the tighter gating rather than none.
	otaGateEntryProbe func() bool

	// playerReadyPoll{Interval,Timeout} pace awaitPlayerCommandHandlerReady;
	// zero means the package defaults. Overridable in tests so the
	// handler-never-appears path needn't wait out the real timeout.
	playerReadyPollInterval time.Duration
	playerReadyPollTimeout  time.Duration

	// logUploaderFactory builds the in-process log uploader. Overridable in tests
	// to avoid a real network transfer; nil in production, where newLogUploader
	// builds the HTTP-backed uploader.
	logUploaderFactory func() logUploaderIface

	// logUploadInFlight single-flights the detached log upload. The command is
	// fire-and-forget (ACKs before the transfer finishes), so a retry storm of
	// uploadLogs commands would otherwise stack concurrent goroutines each
	// zipping the whole log directory into memory. A duplicate while one upload
	// runs is ACKed and dropped — support re-requesting logs mid-upload wants
	// the one already in flight.
	logUploadInFlight atomic.Bool

	// Serialized, coalescing queue for applyFfpPowerStateAsync (see sleep_schedule.go).
	sleepPowerAlignCh        chan sleepPowerAlignJob
	sleepPowerAlignOnce      sync.Once
	sleepPowerAlignEnqueueMu sync.Mutex

	sleepScheduleWakeCh chan struct{}
	sleepScheduleMu     sync.Mutex
	sleepScheduleRun    bool

	// sleepScheduleFileMu: serialize sleep-schedule.json Load/Save (loop + commands).
	// Do not hold across waits, applySleepTransition, or wakeSleepScheduleLoop.
	sleepScheduleFileMu sync.Mutex

	// rotationMu serializes the whole screen-rotation step — orientation-file
	// read, next-step computation, wlr-randr apply, and the write-back. Each
	// rotate command is a RELATIVE step and the perceived orientation lives in
	// SCREEN_ORIENTATION_FILE, so an unserialized overlap is a lost update:
	// both taps read the same start, compute the same target, and two taps
	// advance one step. The command-storm gate deliberately does not dedupe
	// rotations (each byte-identical tap is a distinct user intent), so
	// overlapping commands are routine, not exceptional.
	rotationMu sync.Mutex

	// sleepApplyMu serializes the whole apply — player CDP send, FFP DDC enqueue,
	// and the tracker writes below — so a manual override and a schedule tick can
	// never interleave and leave the player in one state while the tracker records
	// the other. It also guards every tracker field, so the last completed apply
	// deterministically owns both the player command and the recorded state.
	sleepApplyMu sync.Mutex
	// Player (CDP) leg: the four-value tracker (design doc §7). sleepPlayerState
	// is the resting record; sleepAttempted/sleepLastGood/sleepDesiredAtInvalidate
	// are context fields each meaningful only for one specific state value (see
	// playerSleepState's doc). The zero value (playerAwake, with every context
	// field at sleepschedule.State's zero "") preserves today's pre-tracker nil
	// semantics: a bare executor reports aligned-if-target-is-awake, nothing to
	// redrive.
	sleepPlayerState playerSleepState
	// sleepAttempted is valid only when sleepPlayerState==playerUnknownFailed:
	// the state the failed (or generation-raced) apply attempted.
	sleepAttempted sleepschedule.State
	// sleepLastGood is the last state a player apply actually SUCCEEDED at.
	// Updated only on success; untouched by a failed apply so a subsequent
	// failed-WAKE retry can still see the player was last confirmed sleeping
	// (onAwake's M1 disjunct).
	sleepLastGood sleepschedule.State
	// sleepDesiredAtInvalidate is valid only when
	// sleepPlayerState==playerFreshDocument: the schedule's desired state at
	// invalidation time, read from sleepScheduleDesiredCache — never a
	// fresh schedule read (see invalidatePlayerSleepState).
	sleepDesiredAtInvalidate sleepschedule.State
	// sleepScheduleDesiredCache is the schedule loop's per-tick desired state
	// (status.CurrentState, sleep_schedule.go's runSleepScheduleLoop), cached
	// under sleepApplyMu on every tick so invalidatePlayerSleepState — called
	// from the CDP-connect reconciler, a different goroutine — can read it
	// without a fresh sleepschedule.Load(): that would nil-panic a bare
	// executor, violate the sleepScheduleFileMu discipline, and put file I/O on
	// the CDP connect loop. Zero value "" is neither awake nor sleeping.
	sleepScheduleDesiredCache sleepschedule.State
	// Panel (FFP DDC) leg: sleepPanelState/sleepPanelOK is the last state the async
	// panel-power worker reached. It lets the loop re-enqueue a best-effort DDC
	// alignment when a transient ddcutil failure left the panel behind, without
	// re-driving the player. Written by the align worker under sleepApplyMu.
	sleepPanelState *sleepschedule.State
	sleepPanelOK    bool
	// sleepPanelFailStreak counts consecutive worker failures for the current
	// sleepPanelState. Once it reaches panelRetryMax the loop stops re-enqueuing
	// the panel alignment in steady state, so a panel that cannot do DDC power
	// (e.g. an older display lacking VCP 0xD6) is not hammered with ddcutil every
	// tick. A genuine state change resets the streak and retries fresh.
	sleepPanelFailStreak int
	// sleepPanelGen is the ddc display generation observed at the last panel
	// apply. The retry cap above only holds while the generation is unchanged:
	// plugging/swapping a display bumps ddc.PanelDDC.Generation(), so a capped
	// give-up on the OLD panel never sticks to a NEW one — the next tick
	// re-drives the panel leg instead of waiting for the next schedule boundary.
	sleepPanelGen uint64

	// onAwake is an optional hook fired after a successful transition into the
	// awake state. Used by displayAt scheduling to recompute the active set
	// immediately on wake (a timer that fired while sleeping must not leave a
	// stale playlist on screen). Not part of the exported Executor interface so
	// mocks stay unchanged; wire via SetOnAwake.
	onAwake func(context.Context)

	// withPlayerPush, when set, serializes claim-time default playlist fallback
	// writes with displayAt timer/wake/reconnect pushes. The fallback remains
	// player-owned and must not clear scheduler authority because
	// onlyIfNoPlaylist:true can succeed as a no-op.
	withPlayerPush func(func())

	// sessionGeneration, when set (SetSessionGeneration), is
	// playersession.Session.Generation narrowed to a func() uint64 seam so
	// this package needs no import of playersession and tests need no real
	// session (design doc §4 generation re-check contract). nil reads as
	// generation 0 always, which never appears to move.
	sessionGeneration func() uint64
}

func New(
	cdp cdp.CDP,
	deviceStatus status.DeviceStatus,
	statusPoller status.Poller,
	panelDDC ddc.PanelDDC,
	json wrapper.JSON,
	os wrapper.OS,
	exec wrapper.Exec,
	math wrapper.Math,
	clock wrapper.Clock,
	l *zap.Logger,
) Executor {
	return &executor{
		cdp:          cdp,
		deviceStatus: deviceStatus,
		statusPoller: statusPoller,
		logger:       l,
		json:         json,
		os:           os,
		exec:         exec,
		math:         math,
		clock:        clock,
		panelDDC:     panelDDC,
	}
}

func (e *executor) SetClaimObserver(observer func(claimed bool)) {
	e.claimObserver = observer
}

// SetSetupUI injects the shared setup-narration surface so the controld-owned
// claim/factory-reset/OTA-failure narration and the provisioning domain's
// narration all flow through ONE setupui.Service — the same instance main wires
// to CDP's on-connect Resync(), so every narration state (not just
// provisioning's) is re-pushed when Chromium reconnects mid-setup. It consumes
// setupNarratorOnce so the lazy setupUI() builder never later replaces the
// injected instance; tests that skip injection keep the lazy fallback.
//
// Intent — why one shared instance is safe despite setupui's single last-state,
// newest-intent-wins model: the executor's claim flow narrates only once the
// device is provisioned and online (claim runs AFTER Wi-Fi setup completes),
// which is exactly when the provisioning machine sits at StateOnline/idle and is
// not narrating. The two surfaces never legitimately drive the overlay at the
// same time, so newest-intent-wins never drops a state the other still needs.
//
// KNOWN LIMITATION — mintPairingDisplay is a SEPARATE player overlay (driven
// via qrdisplay, not this Service) with no cross-surface arbitration: a
// post-claim setupDisplay narration (updating progress, factory_reset, or a
// latched-OTA join_failed) can be driven while a mint-pairing session's
// overlay is up, and the player renders both. Contained in practice because
// pairing sessions are short-lived and updating/factory_reset both end in a
// reboot; a real fix needs the controller to arbitrate the two overlays.
func (e *executor) SetSetupUI(ui *setupui.Service) {
	if ui == nil {
		return
	}
	e.setupNarratorOnce.Do(func() {
		e.setupNarrator = ui
	})
}

func (e *executor) SaveLastSysMetrics(metrics []byte) {
	e.Lock()
	defer e.Unlock()
	e.lastSysMetrics = metrics
}

// ResetStaged reports the staged-factory-reset latch. See the Executor
// interface doc for why enforcement lives in commandrouter, not here.
func (e *executor) ResetStaged() bool { return e.resetStaged.Load() }

func (e *executor) Execute(ctx context.Context, cmd commands.Command) (interface{}, error) {
	cmdJSON, _ := cmd.JSON()
	e.logger.Info("Executing command", zap.ByteString("command", helper.TruncateBytes(cmdJSON, logger.MAX_FIELD_LENGTH)))

	var err error
	var bytes []byte

	bytes, err = e.json.Marshal(cmd.Arguments)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	var result interface{}
	switch cmd.Type {
	case commands.CMD_CONNECT:
		result, err = e.connect(bytes)
	case commands.CMD_SHOW_PAIRING_QR_CODE:
		result, err = e.showPairingQRCode(ctx, bytes)
	case commands.CMD_KEYBOARD_EVENT:
		result, err = e.handleKeyboardEvent(bytes)
	case commands.CMD_MOUSE_DRAG_EVENT:
		result, err = e.handleMouseMoveEvent(bytes)
	case commands.CMD_MOUSE_TAP_EVENT:
		result, err = e.handleMouseTapEvent(bytes)
	case commands.CMD_MOUSE_DOUBLE_TAP_EVENT:
		result, err = e.handleMouseDoubleTapEvent(bytes)
	case commands.CMD_MOUSE_LONG_PRESS_EVENT:
		result, err = e.handleMouseLongPressEvent(ctx, bytes)
	case commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT:
		result, err = e.handleMouseClickAndDragEvent(bytes)
	case commands.CMD_ZOOM_GESTURE:
		result, err = e.handleZoomGestureEvent(ctx, bytes)
	case commands.CMD_PROFILE:
		result, err = e.getSysMetrics()
	case commands.CMD_SCREEN_ROTATION:
		result, err = e.handleScreenRotation(ctx, bytes)
	case commands.CMD_SHUTDOWN:
		result, err = e.shutdown(ctx)
	case commands.CMD_REBOOT:
		result, err = e.reboot(ctx)
	case commands.CMD_ANALYTICS_TOGGLE:
		result, err = e.setAnalyticsToggle(ctx, bytes)
	case commands.CMD_BETA_FEATURES_TOGGLE:
		result, err = e.setBetaFeaturesToggle(ctx, bytes)
	case commands.CMD_DEVICE_STATUS:
		result, err = e.getDeviceStatus(ctx)
	case commands.CMD_START_WIFI_SETUP:
		result, err = e.startWifiSetup(ctx)
	case commands.CMD_RUN_NETWORK_DIAGNOSTICS:
		result, err = e.runNetworkDiagnostics(ctx)
	case commands.CMD_UPDATE_TO_LATEST:
		result, err = e.updateToLatest(ctx)
	case commands.CMD_FACTORY_RESET:
		result, err = e.factoryReset(ctx)
	case commands.CMD_UPLOAD_LOGS:
		result, err = e.uploadLogs(ctx, bytes)
	case commands.CMD_SET_VOLUME:
		result, err = e.setVolume(ctx, bytes)
	case commands.CMD_TOGGLE_MUTE:
		result, err = e.toggleMute(ctx)
	case commands.CMD_SSH_ACCESS:
		result, err = e.setSshAccess(ctx, bytes)
	case commands.CMD_DDC_PANEL_CONTROL:
		result, err = e.ddcPanelControl(ctx, bytes)
	case commands.CMD_DDC_PANEL_STATUS:
		result, err = e.ddcPanelStatus(ctx, bytes)
	case commands.CMD_SET_SLEEP_SCHEDULE:
		result, err = e.setSleepSchedule(ctx, bytes)
	case commands.CMD_SLEEP_NOW:
		result, err = e.sleepNow(ctx)
	case commands.CMD_WAKE_NOW:
		result, err = e.wakeNow(ctx)
	default:
		return nil, fmt.Errorf("invalid command: %s", cmd)
	}

	return result, err
}

func (e *executor) connect(args []byte) (interface{}, error) {
	var cmdArgs struct {
		Device         Device `json:"clientDevice"`
		PrimaryAddress string `json:"primaryAddress"`
	}
	err := e.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Claimed state is derived everywhere else (mDNS init, /api/status) as
	// "ConnectedDevice with a non-empty ID". Reject
	// an empty ID so a malformed connect can't flip the claim observer to true
	// while every derived view still reports unclaimed.
	if strings.TrimSpace(cmdArgs.Device.ID) == "" {
		return nil, fmt.Errorf("invalid arguments: clientDevice.id is required")
	}

	wasClaimed := e.deviceClaimed()

	err = state.SetConnectedDevice(state.Device{
		ID:       cmdArgs.Device.ID,
		Name:     cmdArgs.Device.Name,
		Platform: cmdArgs.Device.Platform,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
	}

	// A successful connect claims the device; notify any observer so LAN
	// discovery (mDNS TXT) reflects the new claim state.
	if e.claimObserver != nil {
		e.claimObserver(true)
	}

	// The claim QR's job ended the moment the claim landed. The pairing
	// confirmation (showPairingQRCode(false)) still records Ready later, but
	// it is a separate cloud command this device cannot guarantee arrives —
	// never leave a claimed device stranded behind its own claim QR. Guarded
	// on the claim TRANSITION: an app re-connecting to an already-claimed
	// device (e.g. mid-OTA with the updating narration up) must not wipe an
	// unrelated overlay.
	if !wasClaimed {
		e.setupUI().Hide()
		// First pair: put artwork on screen immediately instead of leaving the
		// player idle until the cloud gets around to sending content. The claim
		// QR only paints after the relayer topic-wait, so the device is online
		// and the player's playlist fetch will succeed. Best-effort: the claim
		// itself already landed and must not fail on a player hiccup.
		if err := e.sendDisplayDefaultPlaylist(); err != nil {
			e.logger.Warn("Failed to start default playlist after first pair",
				zap.Error(err))
		}
	}

	return CmdOK, nil
}

// sendDisplayDefaultPlaylist forwards the displayDefaultPlaylist command to the
// player over CDP — the same command OOM recovery uses to restore playback.
// Unlike OOM recovery's unconditional reset, the claim-time push sets
// onlyIfNoPlaylist: the player's own boot-fallback loop may have already put
// artwork on screen by the time the user pairs (e.g. connectivity recovered
// before the claim), and a force push would visibly restart it. The player
// treats the flag as "make sure something is playing" and no-ops otherwise.
func (e *executor) sendDisplayDefaultPlaylist() error {
	if e.cdp == nil {
		return fmt.Errorf("cdp client is not configured")
	}

	send := func() (interface{}, error) {
		command := commands.Command{
			Type: commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
			Arguments: map[string]any{
				"onlyIfNoPlaylist": true,
			},
		}
		payload, err := command.JSON()
		if err != nil {
			return nil, fmt.Errorf("marshal displayDefaultPlaylist payload: %w", err)
		}

		result, err := e.cdp.Send(cdp.METHOD_EVALUATE, map[string]any{
			"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(payload)),
		})
		if err != nil {
			return nil, fmt.Errorf("send displayDefaultPlaylist command to player: %w", err)
		}
		return result, nil
	}

	e.sleepApplyMu.Lock()
	withPlayerPush := e.withPlayerPush
	e.sleepApplyMu.Unlock()

	var err error
	if withPlayerPush != nil {
		withPlayerPush(func() {
			_, err = send()
		})
		return err
	}
	_, err = send()
	return err
}

func (e *executor) showPairingQRCode(ctx context.Context, args []byte) (interface{}, error) {
	var cmdArgs struct {
		Show bool `json:"show"`
	}
	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// controld drives the claim/pairing QR in-process: on show=true it runs the
	// mandatory pre-claim OTA gate and, only on a supported build, paints the
	// claim QR; on show=false it records the Ready transition then hides.
	if cmdArgs.Show {
		// The cloud explicitly restarting the QR flow supersedes any earlier
		// pairing confirmation (e.g. a re-pair round).
		e.pairingConfirmed.Store(false)
	}
	return e.showPairingQRCodeInProcess(ctx, cmdArgs.Show)
}

// showPairingQRCodeInProcess is the controld-owned claim flow. On show=true it
// runs the mandatory pre-claim OTA gate and, only when the device is already on a
// supported build, paints the claim QR on screen. On show=false (cloud ended
// pairing) it records the Ready transition before hiding the overlay.
func (e *executor) showPairingQRCodeInProcess(ctx context.Context, show bool) (interface{}, error) {
	ui := e.setupUI()

	if !show {
		// Latch the confirmation so the auto-claim loop treats this device as
		// settled (see pairingConfirmed): ConnectedDevice alone misses the
		// paired-but-confirmation-first orderings.
		e.pairingConfirmed.Store(true)
		// Record the Ready transition BEFORE hiding the overlay. This ports
		// callbacks.rs:476's record-before-transition rule: the pairing
		// confirmation is a durable one-shot signal (controld ACKs it and never
		// re-emits), so Ready must be registered even if the subsequent hide is
		// interrupted. Ordering Hide first would risk losing the Ready record and
		// stranding the device in Pairing while the cloud believes pairing
		// succeeded.
		ui.ShowReady()
		ui.Hide()
		return CmdOK, nil
	}

	// The relayer command obeys the cloud unconditionally (skipIfSettled
	// false): an explicit show=true on a claimed device is a deliberate
	// re-pair request.
	e.runPreClaimGateAndPaint(ctx, false)
	return CmdOK, nil
}

// runPreClaimGateAndPaint runs the mandatory pre-claim OTA gate and paints the
// claim QR only when the gate settles on a supported build: the device must
// reach one before it can be claimed. It reports whether the QR was painted,
// whether the outcome is terminal for this process (ResultUpdateStarted:
// the device is rebooting into the new build; ResultTooOldToUpgrade: nothing
// short of a reflash helps) so the auto-claim retry loop knows when another
// attempt is pointless, and whether this call latched an update-LADDER
// failure so the loop stretches its retry cadence (see
// autoClaimLadderFailureBackoffMin/Max for why that is neither terminal nor
// the normal backoff).
func (e *executor) runPreClaimGateAndPaint(ctx context.Context, skipIfSettled bool) (painted, terminal, ladderFailed bool) {
	// t0 anchors the latch-freshness check in the error branch below. It MUST
	// be read from the same clock the gate stamps FailureState.At with (both
	// are e.clock — see otaGateInstance) and MUST stay raw: no .UTC()/.Round()
	// or serialization, so the comparison keeps Go's monotonic reading and an
	// NTP step between t0 and the latch cannot reorder them.
	t0 := e.clock.Now()
	result, err := e.otaGateInstance().EnsureLatestBeforeClaim(ctx)
	if err != nil {
		// Two retryable failure shapes with very different costs (F-12):
		//  - A version-check failure (often fresh-network DNS/route
		//    convergence) or a ctx cancel (which never latches — see otagate
		//    runLocal) is cheap; the normal 30s..5m backoff is right, and the
		//    ladder clears its own latch on the next explicit run.
		//  - The update LADDER ran and latched Failure() during this call.
		//    The latch means ladder EXHAUSTION — a bad signature latches on
		//    attempt one, three transient download failures latch on attempt
		//    three; the latch does NOT carry the classifier's verdict. Either
		//    way the round burned up to MaxUpdateRetries full multi-GB
		//    downloads, so the normal cadence would re-download
		//    near-continuously forever on an unattended device — and a bad
		//    published signature/image strands every unclaimed device in the
		//    batch at once. Report it so the retry loop stretches the cadence
		//    (escalating, so the flaky-network shape recovers within the
		//    hour) — but never hard-terminal: unlike the startup gate, the
		//    claim flow has NO nightly-timer fallback, so a device that
		//    simply stays online would otherwise remain unclaimable for the
		//    whole process lifetime.
		// Failure() is read AFTER the gate returned, so a concurrent
		// RequestUpdate ladder latching inside that tiny window would be
		// misattributed to this round and stretch a cheap version-check
		// failure. Accepted: the loop only runs on unclaimed devices, the
		// singleflight serializes gate runs, and the error direction is
		// conservative (a slower retry, never a lost one — the wake channel
		// still preempts it).
		if updateLadderFailureLatchedSince(e.otaGateInstance().Failure(), err, t0) {
			e.logger.Warn("Pre-claim OTA gate's update ladder failed; stretching claim retry cadence",
				zap.Error(err), zap.Stringer("gateResult", result))
			return false, false, true
		}
		e.logger.Warn("Pre-claim OTA gate did not pass; withholding claim QR",
			zap.Error(err), zap.Stringer("gateResult", result))
		return false, false, false
	}
	if result != otagate.ResultNoUpdateNeeded {
		e.logger.Info("Pre-claim OTA gate did not settle on no-update; withholding claim QR",
			zap.Stringer("gateResult", result))
		// UpdateStarted: the updating narration owns the screen via OnProgress
		// and the device reboots. TooOldToUpgrade: nothing further happens this
		// boot, so don't leave a stale finalizing overlay implying progress —
		// but clear ONLY that overlay: the gate's live version check is a
		// window in which another narrator can take the screen (a link drop
		// re-raises the setup AP and paints softap_qr), and an unconditional
		// Hide would erase it. See the settled branch below for the same rule.
		if result == otagate.ResultTooOldToUpgrade {
			e.setupUI().HideIfShowing(setupui.StateFinalizing)
		}
		return false, true, false
	}
	// The gate is slow (live version check, possibly an update ladder); the
	// device may have been claimed or pairing-confirmed while it ran. The
	// auto loop must never repaint the QR over a settled device; the relayer
	// command path opts out (an explicit cloud show=true is a re-pair).
	// Settling mid-flow is exactly when ANOTHER narrator may own the screen
	// (an updateToLatestVersion ladder's ShowUpdating, a factory reset), so
	// clear only the finalizing overlay this flow painted — an unconditional
	// Hide would erase live narration and blind the reload safeguard keyed on
	// the same intent.
	if skipIfSettled && e.claimSettled() {
		e.setupUI().HideIfShowing(setupui.StateFinalizing)
		return false, true, false
	}
	e.setupUI().ShowClaimQR(e.buildDeviceConnectURL(ctx), e.deviceID())
	return true, false, false
}

// updateLadderFailureLatchedSince reports whether the OTA gate's failure latch
// was set by the gate call that started at t0 (t0 read from the SAME clock the
// gate stamps FailureState.At with). The latch records that an update LADDER
// ran to exhaustion — it does NOT carry the transient/permanent classifier
// verdict, so callers must not read "latched" as "unrecoverable". Freshness
// (At not before t0) is the primary criterion: the gate clears its latch only
// on NoUpdateNeeded/ladder entry — deliberately NOT on VersionCheckFailed — so
// a stale latch from an earlier round must not reclassify a cheap transient
// failure as a ladder one. Error identity is the auxiliary criterion: a caller
// that joined an already in-flight run (otagate singleflight) can have taken
// t0 AFTER that run stamped its latch, and the shared error object is then the
// only evidence the failure belongs to this round. Times are compared raw so
// the monotonic clock reading survives (NTP-step immune).
func updateLadderFailureLatchedSince(fs otagate.FailureState, gateErr error, t0 time.Time) bool {
	if gateErr == nil || !fs.Failed {
		return false
	}
	return !fs.At.Before(t0) || errors.Is(gateErr, fs.Err)
}

const (
	// deviceConnectURLPrefix is the claim link launcher-ui renders as a QR. Kept
	// byte-identical to components/launcher-ui/index.html so the phone's parser
	// cannot tell the controld-built QR from the setupd-built one.
	deviceConnectURLPrefix = "https://link.feralfile.com/device_connect/"

	// claimSetupPhase is the setup_phase reported in the claim device_info.
	// feral-setupd paints the pairing QR while in SetupPhase::Pairing ("pairing");
	// the controld-owned claim QR is that same surface, so it reports "pairing".
	claimSetupPhase = "pairing"
)

// setupNarrator is the narrow slice of setupui.Service the controld-owned setup
// flows drive. Owning the interface here keeps the dependency small and lets
// tests assert call ordering with a spy. *setupui.Service satisfies it.
type setupNarrator interface {
	ShowFinalizing()
	ShowClaimQR(url string, deviceName string)
	ShowReady()
	ShowFactoryReset()
	ShowJoinFailed(reason string)
	ShowUpdating(progress int)
	Hide()
	SweepStaleOverlay()
	HideIfShowing(states ...string)
	ShowConnectingIfShowing(message string, states ...string)
}

// setupUI lazily builds the setup-narration surface from the executor's CDP
// seam. Built once; tests may pre-set e.setupNarrator to a spy.
func (e *executor) setupUI() setupNarrator {
	e.setupNarratorOnce.Do(func() {
		if e.setupNarrator == nil {
			e.setupNarrator = setupui.New(e.cdp, "", e.logger)
		}
	})
	return e.setupNarrator
}

// buildDeviceConnectURL gathers the live claim inputs and formats the claim URL.
// device_id comes from the hostname, topic_id from persisted relayer state, and
// branch/version from the FF1 build descriptor — the same sources setupd's
// AppState used. Reaching this point means the pre-claim OTA gate's live version
// check just succeeded, so the device is online; that is the same fact setupd
// read from is_online_cached(), so internet is reported true.
const (
	// autoClaimTopicWait / autoClaimTopicPoll bound the wait for the relayer
	// topic before the online-triggered claim QR paints. The topic lands a
	// moment after connectivity (relayer dial + system message); painting a
	// topic-less QR would send the app to an unroutable device.
	autoClaimTopicWait = 60 * time.Second
	autoClaimTopicPoll = 2 * time.Second

	// autoClaimRetryMin / autoClaimRetryMax bound the retry backoff when the
	// pre-claim gate fails transiently. Nothing re-triggers the flow while the
	// device simply STAYS online, so without this loop a version check that
	// raced fresh-network DNS convergence would withhold the claim QR forever.
	autoClaimRetryMin = 30 * time.Second
	autoClaimRetryMax = 5 * time.Minute

	// autoClaimLadderFailureBackoffMin/Max bound the stretched retry cadence
	// after a pre-claim gate round whose update LADDER failed. The gate's
	// failure latch records ladder EXHAUSTION, not a classifier verdict:
	// three transient download failures latch exactly like a bad signature
	// (otagate's runUpdateLadder returns the final error either way). Each
	// latched round cost up to MaxUpdateRetries full multi-GB image
	// downloads, so the transient 30s..5m cadence would re-download nearly
	// continuously (F-12) — but a flat hours-long wait would equally punish a
	// device that merely lost three download races on flaky Wi-Fi. Hence the
	// escalation: the first latched round retries within the hour (the flaky
	// network case), and consecutive latched rounds double toward one ladder
	// per day (the bad published image case). Never hard-terminal: the claim
	// flow has no nightly-timer fallback, so the loop must keep retrying on
	// its own; an online transition or topic assignment arriving while the
	// loop is PARKED shortens the wait to at most autoClaimLadderWakeFloor
	// (pokes landing while the gate itself runs are dropped —
	// drain-before-park), so a moved/fixed device recovers within minutes,
	// not hours, without flapping links running ladders back-to-back.
	autoClaimLadderFailureBackoffMin = 1 * time.Hour
	autoClaimLadderFailureBackoffMax = 24 * time.Hour

	// autoClaimLadderWakeFloor is the minimum spacing a wake-preempted park
	// still enforces before re-entering the gate. Deliberately aliased to
	// autoClaimRetryMax so the wake path can never re-run a download ladder
	// faster than the transient cadence's cap allows — retuning either value
	// means deciding for both.
	autoClaimLadderWakeFloor = autoClaimRetryMax
)

// MaybeShowClaimQROnOnline is the SoftAP-era replacement for launcher-ui's
// boot-time claim QR. The relayer showPairingQRCode command cannot START the
// claim flow on a factory-fresh device — the phone app only connects after
// scanning the claim QR that command would paint — so provisioning triggers
// this whenever the device comes online: an unclaimed device runs the same
// mandatory pre-claim gate and paints the claim QR. Claimed devices,
// concurrent runs, and topic-less waits (e.g. an offline wired link) are all
// no-ops; a later online transition re-triggers.
func (e *executor) MaybeShowClaimQROnOnline(ctx context.Context) {
	// A staged factory reset owns the screen until the reboot lands. Checked
	// before the in-flight swap so a poke cannot queue a repaint either — the
	// reset's own unclaim is what made this flow live again (see resetStaged).
	if e.resetStaged.Load() {
		return
	}
	if !e.autoClaimInFlight.CompareAndSwap(false, true) {
		// A run is already in flight — possibly parked in the stretched
		// ladder-failure backoff. Poke it (non-blocking, buffered 1) so the
		// online transition or topic assignment that landed here still
		// shortens the wait instead of being silently swallowed for hours.
		select {
		case e.autoClaimWakeChan() <- struct{}{}:
		default:
		}
		return
	}
	defer e.autoClaimInFlight.Store(false)

	if e.claimSettled() {
		// Boot reconciliation: narration is in-memory, so after a daemon
		// restart the player may still render the PREVIOUS life's overlay
		// (e.g. claim_qr painted before a crash on a device that has since
		// been claimed). Sweep it once — and only once. The sweep itself is
		// conditional-and-atomic on the narration lane (SweepStaleOverlay
		// hides only when THIS process has no narration intent yet): this
		// goroutine races the startup OTA gate's on the same transition, and
		// an unconditional Hide landing after the gate's ShowUpdating would
		// erase live update narration — and blind the reload safeguard, which
		// keys on that same intent.
		if e.staleOverlaySwept.CompareAndSwap(false, true) {
			e.setupUI().SweepStaleOverlay()
		}
		return
	}
	// Narrate the gap: between the join succeeding and the claim QR there are
	// seconds of topic wait + version check (more if the gate retries) that
	// would otherwise be a silent black screen.
	e.setupUI().ShowFinalizing()

	if !e.waitForRelayerTopic(ctx) {
		e.logger.Warn("Auto claim flow: relayer topic not ready; withholding claim QR until the next online transition")
		// The topic wait is 60s — the longest window in this flow for another
		// narrator to take the screen (a link drop re-raises the setup AP and
		// paints softap_qr), so BOTH branches below are conditional on the
		// finalizing overlay this flow painted still being up; an
		// unconditional paint or hide here would clobber an active setup
		// surface. See runPreClaimGateAndPaint's settled branch for the shared
		// rationale.
		//
		// With CONFIRMED no internet (cached verdict, unclaimed device) the
		// topic can never arrive — the old silent hide here was the unclaimed
		// wired no-WAN black screen (docs/network-recovery-ux.md §4.6):
		// reachable over the LAN, claimable over the LAN, and nothing on the
		// panel saying so. Narrate it instead; the overlay is cleared by this
		// flow's own later narrations or any provisioning transition, the same
		// ownership it has today.
		if e.internetProbe != nil {
			if online, perr := e.internetProbe(ctx); perr == nil && !online {
				msg := "Connected by cable, but there is no internet access. " +
					"Setup will continue when the connection is restored."
				if name := e.deviceID(); name != "" {
					msg += " (" + name + ")"
				}
				e.setupUI().ShowConnectingIfShowing(msg, setupui.StateFinalizing)
				return
			}
		}
		// Online, probe error, or no probe wired: nothing is coming this
		// pass, but asserting "no internet" without a real offline verdict
		// would smear a healthy network — keep the silent hide.
		e.setupUI().HideIfShowing(setupui.StateFinalizing)
		return
	}
	// Re-check: the device may have settled while waiting (LAN connect).
	// Clear only our own finalizing overlay — see runPreClaimGateAndPaint's
	// settled branch for why an unconditional Hide races other narrators.
	if e.claimSettled() {
		e.setupUI().HideIfShowing(setupui.StateFinalizing)
		return
	}

	e.logger.Info("Auto claim flow: device online and unclaimed; running pre-claim gate")
	backoff := autoClaimRetryMin
	ladderBackoff := autoClaimLadderFailureBackoffMin
	for {
		painted, terminal, ladderFailed := e.runPreClaimGateAndPaint(ctx, true)
		if painted || terminal {
			return
		}
		wait := backoff
		if ladderFailed {
			// This round already burned a full download ladder (see
			// runPreClaimGateAndPaint). The stretched cadence escalates across
			// consecutive latched rounds (flaky Wi-Fi retries within the
			// hour; a bad published image converges toward one ladder per
			// day) and keeps recovery automatic: the wake channel below lets
			// an online transition or topic assignment preempt the wait. The
			// transient backoff is deliberately NOT advanced by these rounds:
			// a later transient failure resumes the cheap cadence where it
			// left off.
			wait = ladderBackoff
			ladderBackoff *= 2
			if ladderBackoff > autoClaimLadderFailureBackoffMax {
				ladderBackoff = autoClaimLadderFailureBackoffMax
			}
		}
		e.logger.Info("Auto claim flow: gate not settled; retrying", zap.Duration("backoff", wait))
		if ladderFailed {
			// Preemptible sleep, stretched cadence ONLY: transient rounds
			// keep the plain sleep so connectivity flaps cannot multiply
			// cheap gate rounds into extra load, while a parked hours-long
			// wait stays responsive to the re-triggers the narration
			// promises.
			if err := e.sleepClaimBackoffPreemptible(ctx, wait); err != nil {
				return
			}
		} else if err := e.clock.SleepContext(ctx, wait); err != nil {
			return
		}
		if e.claimSettled() {
			// The device settled mid-backoff: clear only our finalizing
			// overlay (see runPreClaimGateAndPaint's settled branch).
			e.setupUI().HideIfShowing(setupui.StateFinalizing)
			return
		}
		if !ladderFailed {
			backoff *= 2
			if backoff > autoClaimRetryMax {
				backoff = autoClaimRetryMax
			}
			// Symmetric to the transient backoff surviving ladder rounds: a
			// transient round resets the ladder escalation. The escalation
			// accumulates evidence of a persistently bad published image
			// (every round latches on a solid network), and a cheap failure
			// slipping in between means the NETWORK itself just broke — which
			// reattributes the earlier exhaustion toward the flaky-network
			// cause the 1h floor exists for. Only consecutive latched rounds
			// converge toward one ladder per day.
			ladderBackoff = autoClaimLadderFailureBackoffMin
		}
	}
}

// autoClaimWakeChan lazily builds the buffered(1) wake channel that lets
// MaybeShowClaimQROnOnline invocations arriving while a run is in flight
// preempt that run's stretched ladder-failure backoff. Lazy (like setupUI) so
// tests constructing a bare executor get a working channel from either side.
func (e *executor) autoClaimWakeChan() chan struct{} {
	e.autoClaimWakeOnce.Do(func() { e.autoClaimWake = make(chan struct{}, 1) })
	return e.autoClaimWake
}

// sleepClaimBackoffPreemptible sleeps like clock.SleepContext but returns
// early when the auto-claim wake channel is poked — an online transition or
// topic assignment landed while this loop was parked in the stretched
// ladder-failure backoff, and both of those wirings call the same in-flight-
// guarded entry point, so without the poke they would be silently swallowed
// for hours. Returns non-nil only when ctx is done (SleepContext's contract),
// so the caller's abort path is unchanged.
//
// Two guards keep the wake path from re-opening F-12's back-to-back download
// loop (a link flap DURING the ladder is the expected case on exactly the
// device whose downloads fail, and the factory-fresh topic assignment
// routinely lands before the first park):
//   - Drain-before-park: a wake buffered while the gate was RUNNING is
//     discarded, so only a transition that arrives while actually parked can
//     preempt the wait.
//   - Wake floor: even a legitimate mid-park wake re-enters the gate no
//     sooner than autoClaimLadderWakeFloor after the park began — the remainder is
//     slept non-preemptibly, capping download volume at one ladder per floor
//     no matter how hard the link flaps, while still recovering a moved or
//     fixed device within minutes instead of hours.
func (e *executor) sleepClaimBackoffPreemptible(ctx context.Context, d time.Duration) error {
	select {
	case <-e.autoClaimWakeChan():
	default:
	}
	parkedAt := e.clock.Now()
	sleepCtx, cancelSleep := context.WithCancel(ctx)
	defer cancelSleep()
	done := make(chan error, 1)
	go func() { done <- e.clock.SleepContext(sleepCtx, d) }()
	select {
	case err := <-done:
		// sleepCtx can only have been canceled via ctx here, so the error
		// passes through as the caller's own cancellation.
		return err
	case <-e.autoClaimWakeChan():
		// Reap the sleeper before returning so no goroutine outlives the
		// loop iteration holding a stale timer.
		cancelSleep()
		<-done
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if elapsed := e.clock.Now().Sub(parkedAt); elapsed < autoClaimLadderWakeFloor {
			return e.clock.SleepContext(ctx, autoClaimLadderWakeFloor-elapsed)
		}
		return nil
	}
}

// startupOTAGateMaxCheckAttempts bounds the VersionCheckFailed retry loop in
// MaybeRunStartupOTAGateOnOnline: with the auto-claim backoff (30s doubling,
// 5m cap) 8 attempts sleep seven times for ~22m of backoff (~27m wall clock
// once each attempt's internal version-check ladder is counted) — generous
// for boot-time DNS/route convergence, after which the failure is structural
// (corrupt local build config, distributor outage) and the nightly updater
// timer is the correct owner. This bound is the deliberate asymmetry with the auto-claim loop,
// which retries forever because it has no fallback: an unclaimed device
// without the claim QR is unusable, while a claimed device that misses the
// boot check merely updates later.
const startupOTAGateMaxCheckAttempts = 8

// MaybeRunStartupOTAGateOnOnline is the boot-time mandatory update check for a
// settled device, restoring the Ready-phase leg of feral-setupd's
// on_startup_with_internet (v1.0.21 startup.rs), which ran a Required-mode
// check on every boot with internet for all phases.
//
// The guard predicate is deliberately claimSettled() — the exact predicate
// MaybeShowClaimQROnOnline returns early on — so for any device state exactly
// one of the two online-triggered flows owns the gate (that flow runs
// EnsureLatestBeforeClaim, and the shared otagate single-flight key coalesces
// any overlap). Changing either predicate without the other opens a hole
// where no boot-time gate runs at all.
//
// Triggered on every provisioning →Online/Unprovisioned transition, but wired
// (wireBootLifecycleHooks) only when the daemon started within the kernel boot
// window: feral-controld.service is Restart=always, so without that gate a
// mid-exhibition crash-restart would re-run this check and could spring a
// required update — and its reboot — on a healthy playing device; mid-life
// updates belong to the nightly updater timer. Wiring-time scoping alone is
// not enough, though: the wired hook lives for the whole process, so entry is
// ALSO gated on the boot window still being open (bootLifecycleProbe) — a
// device that booted offline and gains WAN hours later gets the same
// nightly-timer deferral, not a mid-exhibition reboot. The gate runs once per
// process lifetime. Outcome handling:
//   - NoUpdateNeeded / TooOldToUpgrade: settled for this boot.
//   - UpdateStarted: on success the device reboots into the new build. On
//     error the ladder latched OnPermanentFailure (already narrated); still
//     settled — the nightly updater timer is the fallback, and re-running a
//     failing update ladder on every connectivity flap would hammer the
//     distributor for a failure that needs intervention anyway.
//   - VersionCheckFailed: usually transient (the boot online transition
//     routinely races fresh-network DNS convergence — the same race the
//     auto-claim loop documents), so retry with the auto-claim backoff; but
//     bounded by startupOTAGateMaxCheckAttempts because the same result also
//     covers deterministic failures (unreadable/unparseable local build
//     config) that would otherwise spin the loop for the daemon's lifetime.
//     Only a ctx-aborted loop leaves the done latch clear (shutdown, or the
//     backoff sleep interrupted) so the next online transition retries.
func (e *executor) MaybeRunStartupOTAGateOnOnline(ctx context.Context) {
	if e.startupOTAGateDone.Load() || !e.claimSettled() {
		return
	}
	// Boot-window entry gate: the hook stays wired for the process lifetime,
	// so a device that BOOTED offline and only gains WAN hours later would
	// otherwise run a Required-mode update — and its reboot — mid-exhibition.
	// That late update belongs to the nightly updater timer. The window is
	// the gate's OWN (startupOTAGateEntryWindow, wider than the player
	// recovery's bootLifecycleWindow): WAN routinely trails boot by several
	// minutes on a site-wide power restore — the boot this gate most needs to
	// cover — and the 2-minute window silently deferred exactly those boots.
	// Checked at ENTRY only: a gate that started inside the window may keep
	// retrying a failing version check past it (boot-time DNS convergence is
	// the common cause). The resulting worst case is bounded at ~22.5 minutes
	// of backoff across startupOTAGateMaxCheckAttempts; per-retry probing
	// would cap the ladder and defeat the DNS-convergence rationale.
	probe := e.otaGateEntryProbe
	if probe == nil {
		probe = e.bootLifecycleProbe
	}
	if probe != nil && !probe() {
		if e.startupOTAGateDeferLogged.CompareAndSwap(false, true) {
			e.logger.Info("Startup OTA gate: boot window elapsed before WAN; deferring to the nightly updater timer")
		}
		return
	}
	if !e.startupOTAGateInFlight.CompareAndSwap(false, true) {
		return
	}
	defer e.startupOTAGateInFlight.Store(false)
	// Re-check BOTH pre-checked predicates under the guard: the cheap
	// pre-check above can go stale while a concurrent run settles the latch
	// and releases the in-flight flag — without this a second runner could
	// spawn an update ladder against a device already rebooting into the new
	// build — and a factory reset can flip the device unclaimed in the same
	// gap (the mid-retry re-check below covers the loop's sleeps; this covers
	// the entry).
	if e.startupOTAGateDone.Load() || !e.claimSettled() {
		return
	}

	gate := e.startupOTAGate
	if gate == nil {
		gate = e.otaGateInstance().EnsureLatestAtStartup
	}
	backoff := autoClaimRetryMin
	for attempt := 1; ; attempt++ {
		result, err := gate(ctx)
		if result != otagate.ResultVersionCheckFailed {
			e.startupOTAGateDone.Store(true)
			if err != nil {
				e.logger.Warn("Startup OTA gate: update failed; nightly updater timer is the fallback",
					zap.Error(err), zap.Stringer("gateResult", result))
			} else {
				e.logger.Info("Startup OTA gate settled", zap.Stringer("gateResult", result))
			}
			return
		}
		if attempt >= startupOTAGateMaxCheckAttempts {
			e.startupOTAGateDone.Store(true)
			e.logger.Warn("Startup OTA gate: version check kept failing; giving up until the nightly updater timer",
				zap.Error(err), zap.Int("attempts", attempt))
			return
		}
		e.logger.Warn("Startup OTA gate: version check failed; retrying",
			zap.Error(err), zap.Duration("backoff", backoff))
		if err := e.clock.SleepContext(ctx, backoff); err != nil {
			return
		}
		// Re-check the guard predicate after every backoff sleep, mirroring
		// the auto-claim loop's mid-backoff re-check: a factory reset can
		// clear the claim state while this loop sleeps (its staged reboot can
		// be delayed or fail), and the next attempt would then start a
		// Required-mode update — and its reboot — over the freshly unclaimed
		// setup flow, on the wrong side of the claimSettled partition. Stop
		// WITHOUT latching done: this run no longer owns the gate; if the
		// device is ever re-claimed this process life, a later online
		// transition may legitimately re-enter.
		if !e.claimSettled() {
			e.logger.Info("Startup OTA gate: device no longer settled mid-retry (factory reset); stopping without latching")
			return
		}
		backoff *= 2
		if backoff > autoClaimRetryMax {
			backoff = autoClaimRetryMax
		}
	}
}

// MaybeRecoverPlayerOnBootOnline, CompletePendingBootPlayerRecovery, and the
// boot player recovery state machine itself (design doc §5) live in
// boot_recovery.go. This split keeps the boot-lifecycle probe setters here,
// beside bootLifecycleProbe/otaGateEntryProbe, since both are shared wiring
// seams (SetStartupOTAGateEntryProbe is not boot-recovery-specific).

// SetBootLifecycleProbe injects the still-within-boot-window check used to
// expire a parked recovery at the deferred completion (see bootLifecycleProbe).
// Wired by wireBootLifecycleHooks alongside the hook itself. The field is a
// plain (non-atomic) func on purpose, which makes call ORDER the safety
// invariant: this must run during initializeApp wiring, before run() calls
// CDP.Start and spawns the connect loop that reads it — never afterwards.
func (e *executor) SetBootLifecycleProbe(probe func() bool) {
	e.bootLifecycleProbe = probe
}

// SetStartupOTAGateEntryProbe injects the startup OTA gate's own entry-window
// check (see otaGateEntryProbe for why it is wider than the boot-lifecycle
// probe). Same wiring-before-run ordering contract as SetBootLifecycleProbe.
func (e *executor) SetStartupOTAGateEntryProbe(probe func() bool) {
	e.otaGateEntryProbe = probe
}

// SetWifiSetupStarter injects the provisioning machine's startWifiSetup
// admission+queue entry point. Same wiring-before-run ordering contract as
// SetBootLifecycleProbe.
func (e *executor) SetWifiSetupStarter(starter func(ctx context.Context) error) {
	e.wifiSetupStarter = starter
}

// SetInternetProbe injects a cached internet-reachability check (sys-monitord's
// cached connectivity, never a live network probe) used by the claim flow's
// topic-wait expiry narration (see internetProbe for the tri-state contract).
// Same wiring-before-run ordering contract as SetBootLifecycleProbe.
func (e *executor) SetInternetProbe(probe func(ctx context.Context) (bool, error)) {
	e.internetProbe = probe
}

// Defaults for awaitPlayerCommandHandlerReady. The timeout is shorter than the
// connectivity re-seed's: on timeout this path still proceeds (and escalates
// to the reload), so it only delays the dead-page repair, never skips it.
const (
	defaultPlayerReadyPollInterval = 500 * time.Millisecond
	defaultPlayerReadyPollTimeout  = 20 * time.Second
)

// awaitPlayerCommandHandlerReady polls the page until the player app has
// installed window.handleCDPRequest, bounding the wait with the executor's
// poll seams (package defaults when zero). Returns whether the handler was
// observed; callers treat a timeout as the dead page it indicates. The probe
// evaluates to a JSON STRING because the cdp client decodes only string and
// object evaluate results (the constraint setupui.awaitPageReady pins), and
// typeof never throws, so polling a still-hydrating page is error-free.
// Blocking is safe: both callers run on dedicated goroutines (the notifier's
// hook spawn and main's on-connect spawn), never on the CDP connect loop.
//
// Uses e.clock — the same seam every other retry loop in this package uses
// (scheduleBootRecoveryBackoffLocked, the sleep power-align worker) —
// instead of the raw time package, and observes ctx (the one
// attemptBootRecovery already threads through runBootRecoveryRound): a ctx
// cancellation (process shutdown) must interrupt this wait instead of
// running out its own up-to-20s timeout regardless.
func (e *executor) awaitPlayerCommandHandlerReady(ctx context.Context) bool {
	interval := e.playerReadyPollInterval
	if interval <= 0 {
		interval = defaultPlayerReadyPollInterval
	}
	timeout := e.playerReadyPollTimeout
	if timeout <= 0 {
		timeout = defaultPlayerReadyPollTimeout
	}
	const probe = `JSON.stringify({ready: typeof window.handleCDPRequest === 'function'})`
	deadline := e.clock.Now().Add(timeout)
	for {
		result, err := e.cdp.NoLogSend(cdp.METHOD_EVALUATE, map[string]interface{}{
			"expression":    probe,
			"returnByValue": true,
		})
		if err == nil {
			if m, ok := result.(map[string]interface{}); ok {
				if ready, _ := m["ready"].(bool); ready {
					return true
				}
			}
		}
		if e.clock.Now().After(deadline) {
			return false
		}
		if err := e.clock.SleepContext(ctx, interval); err != nil {
			// ctx done (shutdown, or a caller-scoped cancellation): treat as
			// not-ready, the same outcome as a plain timeout.
			return false
		}
	}
}

// claimSettled reports whether the claim journey is over for this device —
// either claimed (ConnectedDevice persisted by connect()) or pairing-confirmed
// by the cloud this process lifetime. The auto-claim loop must treat both as
// terminal; painting the claim QR over a settled device is always wrong.
func (e *executor) claimSettled() bool {
	return e.deviceClaimed() || e.pairingConfirmed.Load()
}

// deviceClaimed mirrors the claim derivation every other surface uses (mDNS
// init, hub status): a ConnectedDevice with a non-empty ID. Reads through
// state.ClaimSnapshot() — this is the linearization point: the claim/unclaim
// gate loops calling this repeatedly (e.g. mid-backoff re-checks) must see the
// SAME connect()/clearPersistedClaim() write every other consumer sees, never
// a partially-applied one.
func (e *executor) deviceClaimed() bool {
	return state.ClaimSnapshot().Claimed
}

// waitForRelayerTopic polls persisted state until the relayer topic is
// available, the wait window closes, or ctx ends.
func (e *executor) waitForRelayerTopic(ctx context.Context) bool {
	deadline := e.clock.Now().Add(autoClaimTopicWait)
	for {
		if state.ClaimSnapshot().TopicReady {
			return true
		}
		if e.clock.Now().Add(autoClaimTopicPoll).After(deadline) {
			return false
		}
		if err := e.clock.SleepContext(ctx, autoClaimTopicPoll); err != nil {
			return false
		}
	}
}

func (e *executor) buildDeviceConnectURL(ctx context.Context) string {
	topicID := state.ClaimSnapshot().TopicID
	branch, version := "", ""
	if b, v, _, err := otagate.NewFileConfigProvider(e.os, e.json).LocalBuild(ctx); err == nil {
		branch, version = b, v
	} else {
		e.logger.Warn("Claim QR: could not read local build; branch/version omitted", zap.Error(err))
	}
	return formatDeviceConnectURL(e.deviceID(), topicID, true, branch, version, claimSetupPhase)
}

// formatDeviceConnectURL replicates the claim URL built by
// components/launcher-ui/index.html byte-for-byte:
//
//	https://link.feralfile.com/device_connect/<device_info>
//
// where device_info is
//
//	<device_id>|<topic_id>|<internet>|<branch>|<version>|<setup_phase>
//
// with the branch's '/' percent-encoded to %2F and nothing else encoded. Ported
// from feral-setupd startup.rs::build_device_info; the app's parser must not be
// able to tell this apart from the setupd-built payload.
func formatDeviceConnectURL(deviceID, topicID string, online bool, branch, version, setupPhase string) string {
	internet := "false"
	if online {
		internet = "true"
	}
	branch = strings.ReplaceAll(branch, "/", "%2F")
	deviceInfo := fmt.Sprintf("%s|%s|%s|%s|%s|%s", deviceID, topicID, internet, branch, version, setupPhase)
	return deviceConnectURLPrefix + deviceInfo
}

func (e *executor) getDeviceStatus(ctx context.Context) (interface{}, error) {
	resp, err := e.deviceStatus.GetStatus(ctx)
	if err != nil || resp == nil {
		return resp, err
	}
	// Attach the §4.7 health object (probe-free by the seam's contract);
	// nil seam simply omits the additive field.
	if e.networkHealth != nil {
		resp.Network = e.networkHealth(ctx)
	}
	// (The netlog lastOutage summary is attached inside GetStatus itself —
	// the status collector is the shared point both this pulled reply and
	// the poller's pushed device_status feed flow through.)
	return resp, nil
}

// SetNetworkHealth injects the §4.7 network-health composer (the provisioning
// snapshot plus the cached internet verdict — never a live probe). Same
// wiring-before-run ordering contract as SetBootLifecycleProbe.
func (e *executor) SetNetworkHealth(fn func(ctx context.Context) *status.NetworkHealth) {
	e.networkHealth = fn
}

// SetNetworkDiagnostics injects the on-demand diagnosis runner (see the
// networkDiagnostics field). Same wiring-before-run ordering contract.
func (e *executor) SetNetworkDiagnostics(fn func(ctx context.Context) (any, error)) {
	e.networkDiagnostics = fn
}

// runNetworkDiagnosticsTimeout bounds the on-demand ladder run. A full pass
// worst-cases at ~16 s (serial link/snapshot/lease prefix + concurrent lower
// rungs; see netlog.Ladder.Run); this backstop keeps the reply inside the
// hub's 30 s write deadline even if a rung misbehaves.
const runNetworkDiagnosticsTimeout = 25 * time.Second

// runNetworkDiagnostics handles CMD_RUN_NETWORK_DIAGNOSTICS: one synchronous
// ladder run, reply carries the classification and per-rung evidence. The
// reply is deliberately synchronous (unlike fire-and-forget uploadLogs): the
// caller asked a question, and the answer takes probe time to produce.
func (e *executor) runNetworkDiagnostics(ctx context.Context) (interface{}, error) {
	if e.networkDiagnostics == nil {
		return nil, fmt.Errorf("network diagnostics unavailable")
	}
	dctx, cancel := context.WithTimeout(ctx, runNetworkDiagnosticsTimeout)
	defer cancel()
	res, err := e.networkDiagnostics(dctx)
	if err != nil {
		return nil, fmt.Errorf("network diagnostics failed: %w", err)
	}
	return res, nil
}

// startWifiSetup handles CMD_START_WIFI_SETUP
// (docs/app-triggered-wifi-setup.md): admission runs synchronously in the
// provisioning machine, but ACCEPTANCE only queues the raise, so the reply
// below is PRODUCED before the raise is even queued — raising the AP severs
// the station link that would carry it (constraint 1).
//
// Deliberately not claimed: that the reply is FLUSHED first. Nothing here
// synchronizes with the transport, and once the event is queued the machine's
// loop goroutine may start the raise while the reply is still on the wire.
// Two things make that acceptable rather than a gap to close with ordering
// machinery. The margin is enormous — the raise is seconds of nmcli work
// against microseconds of reply flush — and, decisively, the contract already
// tolerates the loss: the app treats a timed-out send as success precisely
// because a raise can sever the reply in flight on either transport (relayer
// or LAN hub). See docs/controld-inbound-controller-messages.md.
//
// A rejection is a NORMAL reply ({ok:false, code, message}), not a transport
// error: the app branches on the code. The message strings render VERBATIM in
// the mobile app, so they carry the app's product voice ("Art Computer") — not
// the captive portal's deliberately separate "frame" voice.
func (e *executor) startWifiSetup(ctx context.Context) (interface{}, error) {
	if e.wifiSetupStarter == nil {
		return map[string]any{"ok": false, "code": "unavailable",
			"message": "Wi-Fi setup is not available on this device"}, nil
	}
	if err := e.wifiSetupStarter(ctx); err != nil {
		code := "busy"
		msg := "The Art Computer is busy joining a network. Try again in a moment."
		if errors.Is(err, provisioning.ErrWiredLinkActive) {
			code = "wired_link_active"
			msg = "The Art Computer is connected by ethernet cable. Unplug the cable to set up Wi-Fi."
		}
		e.logger.Info("startWifiSetup rejected", zap.String("code", code), zap.Error(err))
		return map[string]any{"ok": false, "code": code, "message": msg}, nil
	}
	// The AP SSID is deterministic (softap: the FF1-prefixed device id), so
	// the reply can carry it without waiting for the raise.
	ssid := e.deviceID()
	if !strings.HasPrefix(ssid, "FF1-") {
		ssid = "FF1-" + ssid
	}
	e.logger.Info("startWifiSetup accepted; setup AP raise queued", zap.String("ssid", ssid))
	return map[string]any{"ok": true, "ssid": ssid}, nil
}

func (e *executor) handleScreenRotation(ctx context.Context, args []byte) (interface{}, error) {
	var cmdArgs struct {
		Clockwise bool `json:"clockwise"`
	}

	err := e.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	clockwise := cmdArgs.Clockwise
	e.logger.Info("Screen rotation request",
		zap.Bool("clockwise", clockwise))

	// Hold rotationMu across the full read→compute→apply→write sequence so
	// each tap advances exactly one orientation step; see the field comment
	// for why overlapping rotate commands are routine.
	e.rotationMu.Lock()
	defer e.rotationMu.Unlock()

	// Execute wlr-randr command
	cmd := e.exec.CommandContext(ctx, "wlr-randr")

	// Get current outputs
	output, err := cmd.Output()
	if err != nil {
		e.logger.Error("Failed to execute wlr-randr", zap.Error(err))
		return nil, fmt.Errorf("failed to get display info: %w", err)
	}

	// Find the active output name
	outputName := ""
	lines := strings.Split(string(output), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Output") {
			parts := strings.Split(line, " ")
			if len(parts) > 1 {
				outputName = parts[1]
				break
			}
		} else if i == 0 && len(line) > 0 {
			// First line might directly contain the output name
			parts := strings.Split(line, " ")
			if len(parts) > 0 {
				outputName = parts[0]
				break
			}
		}
	}

	if outputName == "" {
		e.logger.Error("Screen rotation: Could not find active output")
		return nil, fmt.Errorf("could not find active output")
	}

	e.logger.Info("Screen rotation: Found active output",
		zap.String("output_name", outputName))

	// Determine rotation
	// Assume normal is 0, then 90, 180, 270 degrees
	rotations := []string{"normal", "90", "180", "270"}

	// Read current orientation from config file (this is what user perceives)
	currentIndex := 0 // Default to normal
	configData, err := e.os.ReadFile(constants.SCREEN_ORIENTATION_FILE)
	if err == nil && len(configData) > 0 {
		savedRotation := strings.TrimSpace(string(configData))
		for i, rot := range rotations {
			if rot == savedRotation {
				currentIndex = i
				break
			}
		}
		e.logger.Info("Using perceived rotation from config", zap.String("rotation", savedRotation))
	} else {
		e.logger.Warn("No saved rotation found, assuming normal orientation")
	}

	// Calculate new orientation based on perceived current orientation
	var newIndex int
	if clockwise {
		newIndex = (currentIndex - 1 + 4) % 4
	} else {
		newIndex = (currentIndex + 1) % 4
	}

	newRotation := rotations[newIndex]

	// Apply with wlr-randr (force absolute orientation)
	// This makes wlr-randr and config file stay in sync
	//nolint:gosec
	rotateCmd := e.exec.CommandContext(ctx, "wlr-randr", "--output", outputName, "--transform", newRotation)
	e.logger.Info("Screen rotation: Applying rotation command",
		zap.String("output", outputName),
		zap.String("transform", newRotation))
	err = rotateCmd.Run()
	if err != nil {
		e.logger.Error("Failed to rotate screen", zap.Error(err))
		return nil, fmt.Errorf("failed to rotate screen: %w", err)
	}

	e.logger.Info("Screen rotation: Successfully applied rotation",
		zap.String("output", outputName),
		zap.String("transform", newRotation))

	// Write rotation value to file
	if err := e.os.WriteFile(constants.SCREEN_ORIENTATION_FILE, []byte(newRotation), 0600); err != nil {
		e.logger.Warn("Failed to save screen orientation", zap.Error(err))
	} else {
		e.logger.Info("Screen rotation: Saved rotation to config file",
			zap.String("rotation", newRotation))
	}

	e.logger.Info("Screen rotated and saved",
		zap.String("output", outputName),
		zap.String("rotation", newRotation))

	e.screenInitialized = false

	// Force refresh status poller
	e.statusPoller.ForceRefresh()

	orientationReplyMsg := "landscape"
	switch newRotation {
	case "90":
		orientationReplyMsg = "portrait"
	case "180":
		orientationReplyMsg = "landscapeReverse"
	case "270":
		orientationReplyMsg = "portraitReverse"
	}

	e.logger.Info("Screen rotation: Completed successfully",
		zap.String("output", outputName),
		zap.String("rotation", newRotation),
		zap.String("orientation_reply", orientationReplyMsg))

	return map[string]string{"orientation": orientationReplyMsg}, nil
}

func (e *executor) handleKeyboardEvent(args []byte) (interface{}, error) {
	var cmdArgs struct {
		Code int `json:"code"`
	}

	err := e.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// The remote keyboard payload uses a constrained numeric code set:
	// standard phone-keyboard characters plus a small special-key subset.
	keyEvent := e.keyboardEventForCode(cmdArgs.Code)
	if keyEvent == nil {
		return nil, fmt.Errorf("unsupported keyboard event code: %d", cmdArgs.Code)
	}

	e.logger.Info("Keyboard event",
		zap.Int("code", cmdArgs.Code),
		zap.String("key", keyEvent.key),
		zap.String("code_name", keyEvent.code))

	keyDownParams := map[string]interface{}{
		"type":                  "keyDown",
		"windowsVirtualKeyCode": cmdArgs.Code,
		"nativeVirtualKeyCode":  cmdArgs.Code,
		"key":                   keyEvent.key,
		"code":                  keyEvent.code,
	}
	if keyEvent.text != "" {
		keyDownParams["text"] = keyEvent.text
		keyDownParams["unmodifiedText"] = keyEvent.text
	}

	_, err = e.cdp.Send("Input.dispatchKeyEvent", keyDownParams)
	if err != nil {
		e.logger.Error("Failed to send key via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to send keyboard event: %w", err)
	}

	keyUpParams := map[string]interface{}{
		"type":                  "keyUp",
		"windowsVirtualKeyCode": cmdArgs.Code,
		"nativeVirtualKeyCode":  cmdArgs.Code,
		"key":                   keyEvent.key,
		"code":                  keyEvent.code,
	}
	if keyEvent.text != "" {
		keyUpParams["text"] = keyEvent.text
		keyUpParams["unmodifiedText"] = keyEvent.text
	}

	_, err = e.cdp.Send("Input.dispatchKeyEvent", keyUpParams)
	if err != nil {
		e.logger.Error("Failed to send keyUp via CDP", zap.Error(err))
	}

	return CmdOK, nil
}

type keyboardEvent struct {
	key  string
	code string
	text string
}

// keyboardEventForCode translates the remote command's numeric code into the
// browser-facing values Chromium expects. This is intentionally a CDP-level
// approximation of keyboard input, not an OS/kernel keyboard injection path.
func (e *executor) keyboardEventForCode(keyCode int) *keyboardEvent {
	switch keyCode {
	case 32:
		return &keyboardEvent{key: " ", code: "Space", text: " "}
	case 9:
		return &keyboardEvent{key: "Tab", code: "Tab"}
	case 13:
		return &keyboardEvent{key: "Enter", code: "Enter"}
	case 27:
		return &keyboardEvent{key: "Escape", code: "Escape"}
	case 8:
		return &keyboardEvent{key: "Backspace", code: "Backspace"}
	}

	if keyCode < 32 || keyCode > 126 {
		e.logger.Warn("Unhandled keyboard event code", zap.Int("code", keyCode))
		return nil
	}

	key, code, text := printableASCIIKeyEvent(keyCode)
	if code == "" {
		e.logger.Warn("Unhandled printable keyboard event code", zap.Int("code", keyCode))
		return nil
	}
	return &keyboardEvent{key: key, code: code, text: text}
}

func printableASCIIKeyEvent(keyCode int) (key string, code string, text string) {
	switch keyCode {
	case 32:
		return " ", "Space", " "
	case 33:
		return "!", "Digit1", "!"
	case 34:
		return "\"", "Quote", "\""
	case 35:
		return "#", "Digit3", "#"
	case 36:
		return "$", "Digit4", "$"
	case 37:
		return "%", "Digit5", "%"
	case 38:
		return "&", "Digit7", "&"
	case 39:
		return "'", "Quote", "'"
	case 40:
		return "(", "Digit9", "("
	case 41:
		return ")", "Digit0", ")"
	case 42:
		return "*", "Digit8", "*"
	case 43:
		return "+", "Equal", "+"
	case 44:
		return ",", "Comma", ","
	case 45:
		return "-", "Minus", "-"
	case 46:
		return ".", "Period", "."
	case 47:
		return "/", "Slash", "/"
	case 48, 49, 50, 51, 52, 53, 54, 55, 56, 57:
		return string(rune(keyCode)), "Digit" + string(rune(keyCode)), string(rune(keyCode))
	case 58:
		return ":", "Semicolon", ":"
	case 59:
		return ";", "Semicolon", ";"
	case 60:
		return "<", "Comma", "<"
	case 61:
		return "=", "Equal", "="
	case 62:
		return ">", "Period", ">"
	case 63:
		return "?", "Slash", "?"
	case 64:
		return "@", "Digit2", "@"
	case 65, 66, 67, 68, 69, 70, 71, 72, 73, 74,
		75, 76, 77, 78, 79, 80, 81, 82, 83, 84,
		85, 86, 87, 88, 89, 90:
		return string(rune(keyCode)), "Key" + string(rune(keyCode)), string(rune(keyCode))
	case 91:
		return "[", "BracketLeft", "["
	case 92:
		return "\\", "Backslash", "\\"
	case 93:
		return "]", "BracketRight", "]"
	case 94:
		return "^", "Digit6", "^"
	case 95:
		return "_", "Minus", "_"
	case 96:
		return "`", "Backquote", "`"
	case 97, 98, 99, 100, 101, 102, 103, 104, 105, 106,
		107, 108, 109, 110, 111, 112, 113, 114, 115, 116,
		117, 118, 119, 120, 121, 122:
		upper := strings.ToUpper(string(rune(keyCode)))
		return string(rune(keyCode)), "Key" + upper, string(rune(keyCode))
	case 123:
		return "{", "BracketLeft", "{"
	case 124:
		return "|", "Backslash", "|"
	case 125:
		return "}", "BracketRight", "}"
	case 126:
		return "~", "Backquote", "~"
	default:
		return "", "", ""
	}
}

type mouseButtonWire string

const (
	mouseButtonLeft   mouseButtonWire = "left"
	mouseButtonRight  mouseButtonWire = "right"
	mouseButtonMiddle mouseButtonWire = "middle"
)

func (e *executor) parseMouseButton(args []byte) (button string, buttons int, err error) {
	var cmdArgs struct {
		Button string `json:"button"`
	}
	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return "", 0, fmt.Errorf("invalid arguments: %w", err)
	}

	wire := mouseButtonWire(cmdArgs.Button)
	if wire == "" {
		wire = mouseButtonLeft
	}
	switch wire {
	case mouseButtonLeft:
		return "left", 1, nil
	case mouseButtonRight:
		return "right", 2, nil
	case mouseButtonMiddle:
		return "middle", 4, nil
	default:
		return "", 0, fmt.Errorf("invalid arguments: unknown mouse button: %s", cmdArgs.Button)
	}
}

func (e *executor) initializeScreenDimensions() {
	if e.screenInitialized {
		return
	}

	// Get screen dimensions using CDP's Runtime.evaluate
	evalParams := map[string]interface{}{
		"expression":    "({width: window.innerWidth, height: window.innerHeight})",
		"returnByValue": true,
	}

	result, err := e.cdp.Send("Runtime.evaluate", evalParams)
	if err != nil {
		e.logger.Error("Failed to get screen dimensions", zap.Error(err))
		// Use default values
		e.screenWidth = 1920
		e.screenHeight = 1080
	} else if result != nil {
		if dimensions, ok := result.(map[string]interface{}); ok {
			if width, ok := dimensions["width"].(float64); ok {
				e.screenWidth = width
			} else {
				e.screenWidth = 1920
			}
			if height, ok := dimensions["height"].(float64); ok {
				e.screenHeight = height
			} else {
				e.screenHeight = 1080
			}
		}
	}

	// Initialize cursor at the center of the screen
	e.cursorPositionX = e.screenWidth / 2
	e.cursorPositionY = e.screenHeight / 2
	e.screenInitialized = true
	e.movingScaleFactor = e.screenWidth / 1920

	e.logger.Info("Screen dimensions initialized",
		zap.Float64("width", e.screenWidth),
		zap.Float64("height", e.screenHeight),
		zap.Float64("cursorX", e.cursorPositionX),
		zap.Float64("cursorY", e.cursorPositionY))
}

func (e *executor) handleMouseMoveEventWithButtons(
	args []byte,
	pressedButtons int,
) (interface{}, error) {
	// Initialize screen dimensions if not done already
	e.initializeScreenDimensions()

	// Parse cursor offsets
	var cursorArgs struct {
		MessageID     string `json:"messageID"`
		CursorOffsets []struct {
			DX float64 `json:"dx"`
			DY float64 `json:"dy"`
		} `json:"cursorOffsets"`
	}

	err := e.json.Unmarshal(args, &cursorArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Convert relative positions to absolute positions
	absolutePositions := make([]map[string]float64, 0, len(cursorArgs.CursorOffsets))

	for i, offset := range cursorArgs.CursorOffsets {
		// Calculate the magnitude of this offset
		magnitude := e.math.Sqrt(offset.DX*offset.DX + offset.DY*offset.DY)

		var clampedDX, clampedDY float64

		// Only clamp obvious outliers (very large jumps)
		if magnitude > 150 {
			// This is likely a catch-up jump, clamp aggressively
			maxOffset := 25.0
			clampedDX = e.math.Max(-maxOffset, e.math.Min(maxOffset, offset.DX))
			clampedDY = e.math.Max(-maxOffset, e.math.Min(maxOffset, offset.DY))

			e.logger.Debug("Clamping outlier offset",
				zap.Int("index", i),
				zap.Float64("magnitude", magnitude),
				zap.Float64("originalDX", offset.DX),
				zap.Float64("originalDY", offset.DY),
				zap.Float64("clampedDX", clampedDX),
				zap.Float64("clampedDY", clampedDY))
		} else {
			// Normal movement, use original values
			clampedDX = offset.DX
			clampedDY = offset.DY
		}

		// Update cursor position with the offset
		e.cursorPositionX += (clampedDX * e.movingScaleFactor)
		e.cursorPositionY += (clampedDY * e.movingScaleFactor)

		// Ensure position stays within screen bounds
		e.cursorPositionX = e.math.Max(0, e.math.Min(e.cursorPositionX, e.screenWidth))
		e.cursorPositionY = e.math.Max(0, e.math.Min(e.cursorPositionY, e.screenHeight))

		// Add to absolute positions array
		absolutePositions = append(absolutePositions, map[string]float64{
			"x": e.cursorPositionX,
			"y": e.cursorPositionY,
		})
	}

	// Skip if there are no positions
	if len(absolutePositions) == 0 {
		return CmdOK, nil
	}

	// 1. Pass the entire array of absolute positions to JavaScript via CDP
	positionsJSON, err := e.json.Marshal(map[string]interface{}{
		"messageID": cursorArgs.MessageID,
		"message": map[string]interface{}{
			"command": "cursorUpdate",
			"request": map[string]interface{}{
				"positions": absolutePositions,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal positions: %w", err)
	}

	// Call JavaScript function to process all positions
	_, err = e.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(positionsJSON)),
	})
	if err != nil {
		e.logger.Error("Failed to execute JavaScript cursor positions", zap.Error(err))
		return nil, fmt.Errorf("failed to process cursor positions: %w", err)
	}

	// 2. Send the final mouse event to actually move the cursor
	moveParams := map[string]interface{}{
		"type":       "mouseMoved",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     "none",
		"buttons":    pressedButtons,
		"clickCount": 0,
	}

	_, err = e.cdp.Send("Input.dispatchMouseEvent", moveParams)
	if err != nil {
		e.logger.Error("Failed to move mouse via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to move mouse: %w", err)
	}

	e.logger.Info("Mouse moved to final position",
		zap.Float64("x", e.cursorPositionX),
		zap.Float64("y", e.cursorPositionY))

	return CmdOK, nil
}

func (e *executor) handleMouseMoveEvent(args []byte) (interface{}, error) {
	return e.handleMouseMoveEventWithButtons(args, 0)
}

func (e *executor) handleMouseTapEvent(args []byte) (interface{}, error) {
	// Initialize screen dimensions if not done already
	e.initializeScreenDimensions()

	button, pressedButtons, err := e.parseMouseButton(args)
	if err != nil {
		return nil, err
	}

	e.logger.Info("Mouse tap event at current position",
		zap.Float64("x", e.cursorPositionX),
		zap.Float64("y", e.cursorPositionY))

	if err := e.dispatchMouseClick(button, pressedButtons, 1); err != nil {
		return nil, err
	}

	return CmdOK, nil
}

func (e *executor) dispatchMouseClick(button string, pressedButtons int, clickCount int) (err error) {
	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     button,
		"buttons":    pressedButtons,
		"clickCount": clickCount,
	}
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     button,
		"buttons":    0,
		"clickCount": clickCount,
	}

	pressed := false
	defer func() {
		if !pressed {
			return
		}
		_, releaseErr := e.cdp.Send("Input.dispatchMouseEvent", upParams)
		if releaseErr != nil {
			e.logger.Error("Failed to release mouse button via CDP during cleanup", zap.Error(releaseErr))
		}
	}()

	_, err = e.cdp.Send("Input.dispatchMouseEvent", downParams)
	if err != nil {
		e.logger.Error("Failed to press mouse button via CDP", zap.Error(err))
		return fmt.Errorf("failed to press mouse button: %w", err)
	}
	pressed = true

	_, err = e.cdp.Send("Input.dispatchMouseEvent", upParams)
	if err != nil {
		e.logger.Error("Failed to release mouse button via CDP", zap.Error(err))
		return fmt.Errorf("failed to release mouse button: %w", err)
	}
	pressed = false

	return nil
}

func (e *executor) handleMouseDoubleTapEvent(args []byte) (interface{}, error) {
	e.initializeScreenDimensions()

	button, pressedButtons, err := e.parseMouseButton(args)
	if err != nil {
		return nil, err
	}

	e.logger.Info("Mouse double tap event at current position",
		zap.Float64("x", e.cursorPositionX),
		zap.Float64("y", e.cursorPositionY))

	// Chromium's dblclick handling is sequence-sensitive: a single press/release
	// pair with clickCount=2 can still collapse to a plain click on targets that
	// inspect the full click sequence. Emit two clicks so the second one carries
	// the double-click count expected by dblclick/double-tap handlers.
	for clickCount := 1; clickCount <= 2; clickCount++ {
		if err := e.dispatchMouseClick(button, pressedButtons, clickCount); err != nil {
			return nil, err
		}
	}

	return CmdOK, nil
}

func (e *executor) handleMouseLongPressEvent(ctx context.Context, args []byte) (result interface{}, err error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	e.initializeScreenDimensions()

	button, pressedButtons, err := e.parseMouseButton(args)
	if err != nil {
		return nil, err
	}

	e.logger.Info("Mouse long press event at current position",
		zap.Float64("x", e.cursorPositionX),
		zap.Float64("y", e.cursorPositionY))

	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     button,
		"buttons":    pressedButtons,
		"clickCount": 1,
	}
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     button,
		"buttons":    0,
		"clickCount": 1,
	}

	pressed := false
	defer func() {
		if !pressed {
			return
		}
		_, releaseErr := e.cdp.Send("Input.dispatchMouseEvent", upParams)
		pressed = false
		if releaseErr != nil {
			e.logger.Error("Failed to release mouse button via CDP during cleanup", zap.Error(releaseErr))
			// Join with any return err (e.g. ctx cancel during hold vs CDP stuck-button cleanup failure).
			cleanupErr := fmt.Errorf("failed to release mouse button during cleanup: %w", releaseErr)
			if err != nil {
				err = errors.Join(err, cleanupErr)
				return
			}
			err = cleanupErr
		}
	}()

	_, err = e.cdp.Send("Input.dispatchMouseEvent", downParams)
	if err != nil {
		e.logger.Error("Failed to press mouse button via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to press mouse button: %w", err)
	}
	pressed = true

	// Hold duration must respect ctx so teardown can unwind without waiting the full second
	// while the button is still logically down in Chromium.
	if err = e.clock.SleepContext(ctx, 1*time.Second); err != nil {
		return nil, err
	}

	_, err = e.cdp.Send("Input.dispatchMouseEvent", upParams)
	if err != nil {
		e.logger.Error("Failed to release mouse button via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to release mouse button: %w", err)
	}
	pressed = false

	return CmdOK, nil
}

func (e *executor) handleMouseClickAndDragEvent(args []byte) (result interface{}, err error) {
	e.initializeScreenDimensions()

	// Parse cursor offsets to decide whether we should press/move/release.
	var cursorArgs struct {
		CursorOffsets []struct {
			DX float64 `json:"dx"`
			DY float64 `json:"dy"`
		} `json:"cursorOffsets"`
	}
	if err := e.json.Unmarshal(args, &cursorArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(cursorArgs.CursorOffsets) == 0 {
		return CmdOK, nil
	}
	if len(cursorArgs.CursorOffsets) > maxClickAndDragCursorOffsets {
		return nil, fmt.Errorf("invalid arguments: cursorOffsets exceeds maximum of %d", maxClickAndDragCursorOffsets)
	}

	e.logger.Info("Mouse click-and-drag event at current position",
		zap.Float64("x", e.cursorPositionX),
		zap.Float64("y", e.cursorPositionY))

	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	_, err = e.cdp.Send("Input.dispatchMouseEvent", downParams)
	if err != nil {
		e.logger.Error("Failed to press mouse button via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to press mouse button: %w", err)
	}
	// Each click-and-drag batch is press + move + release. If the move step fails, we still
	// must release the button; otherwise the page can keep a stuck mouse-down in Chromium
	// until a later event clears it. Failed cleanup releases are always surfaced: alone when
	// the move succeeded, or joined with the move error so a stuck-button cleanup failure is
	// not dropped when the move path already failed.
	defer func() {
		up := map[string]interface{}{
			"type":       "mouseReleased",
			"x":          e.cursorPositionX,
			"y":          e.cursorPositionY,
			"button":     "left",
			"buttons":    0,
			"clickCount": 1,
		}
		if _, relErr := e.cdp.Send("Input.dispatchMouseEvent", up); relErr != nil {
			e.logger.Error("click-and-drag: best-effort release after batch failed", zap.Error(relErr))
			releaseErr := fmt.Errorf("failed to release mouse button during cleanup: %w", relErr)
			if err == nil {
				err = releaseErr
			} else {
				err = errors.Join(err, releaseErr)
			}
			result = nil
		}
	}()

	_, err = e.handleMouseMoveEventWithButtons(args, 1)
	if err != nil {
		return nil, err
	}

	return CmdOK, nil
}

// handleZoomGestureEvent dispatches non-Ctrl wheel input at the CDP boundary.
// This avoids Chromium page zoom while still giving artwork a zoom-like input.
func (e *executor) handleZoomGestureEvent(ctx context.Context, args []byte) (interface{}, error) {
	e.initializeScreenDimensions()

	var in struct {
		MessageID  string    `json:"messageID"`
		ScaleSteps []float64 `json:"scaleSteps"`
	}
	if err := e.json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	if len(in.ScaleSteps) == 0 {
		return CmdOK, nil
	}
	if len(in.ScaleSteps) > maxZoomGestureSteps {
		return nil, fmt.Errorf("invalid arguments: scaleSteps exceeds maximum of %d", maxZoomGestureSteps)
	}

	for _, step := range in.ScaleSteps {
		if step <= 0 {
			return nil, fmt.Errorf("invalid arguments: scaleSteps must be positive: %v", step)
		}
	}

	for _, step := range in.ScaleSteps {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		x, y := e.zoomGesturePoint(step)
		if err := e.sendZoomWheelGesture(step, x, y); err != nil {
			e.logger.Error("Failed to dispatch zoom wheel gesture", zap.Error(err))
			return nil, fmt.Errorf("failed to process zoom gesture: %w", err)
		}
	}

	return CmdOK, nil
}

func (e *executor) zoomGesturePoint(scaleFactor float64) (float64, float64) {
	viewportX, viewportY, viewportWidth, viewportHeight := e.currentVisualViewport()
	return e.innerToVisualViewport(e.cursorPositionX, e.cursorPositionY, viewportX, viewportY, viewportWidth, viewportHeight)
}

func (e *executor) innerToVisualViewport(x, y, viewportX, viewportY, viewportWidth, viewportHeight float64) (float64, float64) {
	visualX := x - viewportX
	visualY := y - viewportY
	return e.clampToViewport(visualX, visualY, 0, 0, viewportWidth, viewportHeight)
}

func (e *executor) currentVisualViewport() (float64, float64, float64, float64) {
	viewportX, viewportY := 0.0, 0.0
	viewportWidth, viewportHeight := e.screenWidth, e.screenHeight

	evalParams := map[string]interface{}{
		"expression": `
			(() => {
				const vv = window.visualViewport;
				return vv ? {
					offsetLeft: vv.offsetLeft,
					offsetTop: vv.offsetTop,
					width: vv.width,
					height: vv.height
				} : null;
			})()`,
		"returnByValue": true,
	}

	result, err := e.cdp.Send("Runtime.evaluate", evalParams)
	if err != nil {
		e.logger.Warn("Failed to get visual viewport; using screen bounds", zap.Error(err))
		return viewportX, viewportY, viewportWidth, viewportHeight
	}

	if result == nil {
		return viewportX, viewportY, viewportWidth, viewportHeight
	}

	viewport, ok := result.(map[string]interface{})
	if !ok {
		return viewportX, viewportY, viewportWidth, viewportHeight
	}

	if offsetLeft, ok := viewport["offsetLeft"].(float64); ok {
		viewportX = offsetLeft
	}
	if offsetTop, ok := viewport["offsetTop"].(float64); ok {
		viewportY = offsetTop
	}
	if width, ok := viewport["width"].(float64); ok && width > 0 {
		viewportWidth = width
	}
	if height, ok := viewport["height"].(float64); ok && height > 0 {
		viewportHeight = height
	}

	return viewportX, viewportY, viewportWidth, viewportHeight
}

func (e *executor) clampToViewport(x, y, viewportX, viewportY, viewportWidth, viewportHeight float64) (float64, float64) {
	minX := viewportX
	maxX := viewportX + viewportWidth
	minY := viewportY
	maxY := viewportY + viewportHeight

	if x < minX {
		x = minX
	} else if x > maxX {
		x = maxX
	}

	if y < minY {
		y = minY
	} else if y > maxY {
		y = maxY
	}

	return x, y
}

func (e *executor) sendZoomWheelGesture(scaleFactor, x, y float64) error {
	if scaleFactor == 1 {
		return nil
	}

	deltaY := zoomWheelDeltaY(scaleFactor)
	if scaleFactor > 1 {
		deltaY = -deltaY
	}

	params := map[string]interface{}{
		"type":      "mouseWheel",
		"x":         x,
		"y":         y,
		"deltaX":    0,
		"deltaY":    deltaY,
		"button":    "none",
		"buttons":   0,
		"modifiers": 0,
	}

	_, err := e.cdp.Send("Input.dispatchMouseEvent", params)
	if err != nil {
		return fmt.Errorf("dispatch wheel gesture: %w", err)
	}

	return nil
}

func zoomWheelDeltaY(scaleFactor float64) float64 {
	magnitude := math.Abs(math.Log2(scaleFactor))
	steps := math.Round(magnitude * 8)
	if steps < 1 {
		steps = 1
	}
	return 120.0 * steps
}

func (e *executor) shutdown(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing shutdown command")

	cmd := e.exec.CommandContext(ctx, "sudo", "shutdown", "-h", "now")

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute shutdown command: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) reboot(ctx context.Context) (interface{}, error) {

	cmd := e.exec.CommandContext(ctx, "sudo", "reboot", "-h", "now")

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute reboot command: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) setAnalyticsToggle(_ context.Context, args []byte) (interface{}, error) {
	var toggleArgs struct {
		Enabled bool `json:"enabled"`
	}
	if err := e.json.Unmarshal(args, &toggleArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	configDir := filepath.Dir(AnalyticsToggleOffFile)

	if err := e.os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}

	if toggleArgs.Enabled {
		if err := e.removeFileIfExists(AnalyticsToggleOffFile); err != nil {
			return nil, fmt.Errorf("failed to enable analytics collection: %w", err)
		}
		e.logger.Info("Analytics collection enabled (toggle file removed)", zap.String("path", AnalyticsToggleOffFile))
		return CmdOK, nil
	}

	content := []byte("analytics collection disabled via controld\n")
	if err := e.os.WriteFile(AnalyticsToggleOffFile, content, 0o644); err != nil {
		return nil, fmt.Errorf("failed to write analytics toggle file: %w", err)
	}

	e.logger.Info("Analytics collection disabled (toggle file created)", zap.String("path", AnalyticsToggleOffFile))

	return CmdOK, nil
}

func (e *executor) setBetaFeaturesToggle(_ context.Context, args []byte) (interface{}, error) {
	var toggleArgs struct {
		Enabled bool `json:"enabled"`
	}
	if err := e.json.Unmarshal(args, &toggleArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	configDir := filepath.Dir(BetaFeaturesToggleOnFile)

	if err := e.os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}

	if toggleArgs.Enabled {
		content := []byte("beta features enabled via controld\n")
		if err := e.os.WriteFile(BetaFeaturesToggleOnFile, content, 0o644); err != nil {
			return nil, fmt.Errorf("failed to write beta features toggle file: %w", err)
		}
		e.logger.Info("Beta features enabled (toggle file created)", zap.String("path", BetaFeaturesToggleOnFile))
		return CmdOK, nil
	}

	if err := e.removeFileIfExists(BetaFeaturesToggleOnFile); err != nil {
		return nil, fmt.Errorf("failed to disable beta features: %w", err)
	}

	e.logger.Info("Beta features disabled (toggle file removed)", zap.String("path", BetaFeaturesToggleOnFile))

	return CmdOK, nil
}

func (e *executor) setSshAccess(ctx context.Context, args []byte) (interface{}, error) {
	var cmdArgs struct {
		Enabled    bool   `json:"enabled"`
		PublicKey  string `json:"publicKey"`
		TTLSeconds *int   `json:"ttlSeconds"`
	}
	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if cmdArgs.Enabled {
		if strings.TrimSpace(cmdArgs.PublicKey) == "" {
			return nil, fmt.Errorf("publicKey is required to enable SSH")
		}
		if err := e.writeAuthorizedKey(cmdArgs.PublicKey); err != nil {
			return nil, fmt.Errorf("failed to write authorized key: %w", err)
		}
		if err := e.clearSshDisableTimer(ctx); err != nil {
			return nil, fmt.Errorf("failed to clear SSH disable timer: %w", err)
		}
		if err := e.runSudoCommand(ctx, "systemctl", "start", "sshd.service"); err != nil {
			e.logger.Error("Failed to start SSH service, rolling back SSH access", zap.Error(err))
			if removeErr := e.removeFileIfExists(constants.SSH_AUTHORIZED_KEYS_FILE); removeErr != nil {
				e.logger.Error("Rollback failed: could not remove authorized_keys", zap.Error(removeErr))
			}
			return nil, fmt.Errorf("failed to start SSH service: %w", err)
		}

		ttlSeconds := normalizeSshTtlSeconds(cmdArgs.TTLSeconds)
		var expiresAt *time.Time
		if ttlSeconds > 0 {
			expiresAtValue := time.Now().Add(time.Duration(ttlSeconds) * time.Second)
			expiresAt = &expiresAtValue
			if err := e.scheduleSshDisable(ctx, ttlSeconds); err != nil {
				e.logger.Error("Failed to schedule SSH disable timer, rolling back SSH access",
					zap.Error(err),
					zap.Int("ttlSeconds", ttlSeconds))

				if stopErr := e.runSudoCommand(ctx, "systemctl", "stop", "sshd.service"); stopErr != nil {
					e.logger.Error("Rollback failed: could not stop sshd service", zap.Error(stopErr))
				}

				if removeErr := e.removeFileIfExists(constants.SSH_AUTHORIZED_KEYS_FILE); removeErr != nil {
					e.logger.Error("Rollback failed: could not remove authorized_keys", zap.Error(removeErr))
				}

				return nil, fmt.Errorf("failed to schedule SSH disable: %w", err)
			}
		}

		return map[string]interface{}{
			"enabled":    true,
			"ttlSeconds": ttlSeconds,
			"expiresAt":  expiresAt,
		}, nil
	}

	if err := e.clearSshDisableTimer(ctx); err != nil {
		return nil, fmt.Errorf("failed to clear SSH disable timer: %w", err)
	}
	if err := e.runSudoCommand(ctx, "systemctl", "stop", "sshd.service"); err != nil {
		return nil, fmt.Errorf("failed to stop SSH service: %w", err)
	}
	if err := e.removeFileIfExists(constants.SSH_AUTHORIZED_KEYS_FILE); err != nil {
		return nil, fmt.Errorf("failed to remove authorized keys: %w", err)
	}

	return map[string]interface{}{
		"enabled": false,
	}, nil
}

func normalizeSshTtlSeconds(ttlSeconds *int) int {
	if ttlSeconds == nil {
		return 0
	}
	if *ttlSeconds <= 0 {
		return 0
	}
	if *ttlSeconds > 86400 {
		return 86400
	}
	return *ttlSeconds
}

func (e *executor) writeAuthorizedKey(publicKey string) error {
	sshDir := filepath.Dir(constants.SSH_AUTHORIZED_KEYS_FILE)
	if err := e.os.MkdirAll(sshDir, 0700); err != nil {
		return err
	}
	key := strings.TrimSpace(publicKey)
	if !strings.HasSuffix(key, "\n") {
		key += "\n"
	}
	return e.os.WriteFile(constants.SSH_AUTHORIZED_KEYS_FILE, []byte(key), 0600)
}

func (e *executor) scheduleSshDisable(ctx context.Context, ttlSeconds int) error {
	// Kill active SSH sessions first, then stop the listener.
	// pkill may exit non-zero if no matching processes exist, so we ignore its exit code with "|| true".
	disableCmd := "pkill -u feralfile sshd || true; systemctl stop sshd.service"
	return e.runSudoCommand(
		ctx,
		"systemd-run",
		"--unit",
		constants.SSH_DISABLE_UNIT,
		"--on-active",
		fmt.Sprintf("%ds", ttlSeconds),
		"/bin/bash",
		"-c",
		disableCmd,
	)
}

func (e *executor) clearSshDisableTimer(ctx context.Context) error {
	_ = e.runSudoCommand(ctx, "systemctl", "stop", constants.SSH_DISABLE_UNIT+".timer")
	_ = e.runSudoCommand(ctx, "systemctl", "stop", constants.SSH_DISABLE_UNIT+".service")
	_ = e.runSudoCommand(ctx, "systemctl", "reset-failed", constants.SSH_DISABLE_UNIT+".service")
	return nil
}

func (e *executor) runSudoCommand(ctx context.Context, command string, args ...string) error {
	cmd := e.exec.CommandContext(ctx, "sudo", append([]string{command}, args...)...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w", strings.TrimSpace(string(output)), err)
	}
	return nil
}

func (e *executor) removeFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !e.os.IsNotExist(err) {
		return err
	}
	return nil
}

func (e *executor) getSysMetrics() (interface{}, error) {
	e.Lock()
	defer e.Unlock()

	var sysMetrics map[string]interface{}
	if e.lastSysMetrics != nil {
		err := e.json.Unmarshal(e.lastSysMetrics, &sysMetrics)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal last sys metrics: %w", err)
		}
	}

	return sysMetrics, nil
}

// updateToLatest schedules the user-triggered OTA gate run and ACKs
// immediately. The ACK means "accepted for evaluation" and nothing more — the
// gate may dedupe the request, fail its version check, or decide no update is
// warranted, all of which answer the same CmdOK as an update that actually runs.
//
// Fire-and-forget on purpose, same shape as uploadLogs. A synchronous call here
// can never deliver its reply: an update that IS warranted runs for minutes and
// ENDS IN A REBOOT, so the updater tears the process down while the caller is
// still waiting on the response. Hardware repro 2026-08-07 (FF1-8EVTK3RE): the app's
// POST /api/cast {"command":"updateToLatestVersion"} produced "Executing system
// update command" with no matching "Hub request served" line ever — the update
// itself ran to completion and rebooted the device 59s later, while the app saw
// only a dropped connection and reported it as a transport error. Every reply
// this handler could produce is therefore either undeliverable (success, reboot
// wins the race) or better narrated on screen (failure, see below).
//
// ctx is the DAEMON-lifetime context, not a per-request one — the hub passes
// h.ctx and the relayer read loop passes the ctx main() started it with — so it
// is both safe and REQUIRED to carry into the detached goroutine: otagate's
// runLocal keys its "a cancel must not latch a permanent failure" guard off
// ctx.Err(), and a shutdown mid-ladder must still cancel the run. This is why
// it does NOT swap in context.Background() the way uploadLogsInProcess does; if
// a future caller ever hands this a request-scoped ctx, that guard has to be
// revisited before this detaches from it.
//
// Failures are logged here and narrated on screen by the gate's OnProgress /
// OnPermanentFailure callbacks rather than returned — by the time one is known
// there is no caller left to return it to.
func (e *executor) updateToLatest(ctx context.Context) (interface{}, error) {
	if !e.otaUpdateInFlight.CompareAndSwap(false, true) {
		e.logger.Info("System update already in flight; duplicate command ignored")
		return CmdOK, nil
	}

	e.logger.Info("Executing system update command")

	// Resolved here rather than inside the goroutine so a test-injected gate is
	// observed deterministically, and so the lazy build's HTTP client, runner and
	// narration callbacks are constructed on the command path instead of the
	// detached one. (otaGateOnce is a sync.Once, so either placement is race-free.)
	gate := e.otaGateInstance()

	go func() {
		defer e.otaUpdateInFlight.Store(false)
		// Route the user-triggered update through the OTA gate so it is
		// single-flighted with the mandatory pre-claim and startup gates. The
		// gate drives the updater locally.
		if _, err := gate.RequestUpdate(ctx); err != nil {
			e.logger.Error("System update request failed", zap.Error(err))
		}
	}()

	return CmdOK, nil
}

// otaGateInstance lazily builds the shared OTA gate from the executor's existing
// seams. Built once so its single-flight guard and permanent-failure latch persist
// across relayer commands.
func (e *executor) otaGateInstance() *otagate.Gate {
	e.otaGateOnce.Do(func() {
		// Test seam (same pattern as setupNarrator): a pre-set gate is kept
		// as-is so tests can drive the failure latch through a Gate built on
		// fake deps. Production wiring never pre-sets it.
		if e.otaGate != nil {
			return
		}
		e.otaGate = otagate.New(otagate.Deps{
			HTTP:   wrapper.NewHTTPClient(),
			Clock:  e.clock,
			Runner: otagate.NewSystemdRunner(e.exec, e.clock, e.logger),
			Config: otagate.NewFileConfigProvider(e.os, e.json),
			Logger: e.logger,
			// Narrate update progress on-screen: the gate hands each parsed percent
			// to setupui's updating overlay via this callback.
			OnProgress: e.narrateUpdateProgress,
			// Post-ladder watchdog (design doc §6): a successful ladder
			// that somehow never reboots must not leave "updating" on screen
			// forever.
			OnUpdateSucceededNoReboot: e.clearStuckUpdatingOverlay,
		})
		// Narrator policy on a latched permanent OTA failure — decided at emit
		// time (otaPermanentFailureNarration), claim-PRIMARY (design doc §6):
		// all three entry points
		// (RequestUpdate/EnsureLatestBeforeClaim/EnsureLatestAtStartup) share
		// this ONE gate and this ONE callback, so a settled-device
		// updateToLatest that happens to join a flight another (pre-claim)
		// caller started still gets the settled policy — the claim snapshot is
		// read live, not captured at flight start.
		e.otaGate.OnPermanentFailure(e.otaPermanentFailureNarration)
	})
	return e.otaGate
}

// otaPermanentFailureNarration is the OTA gate's OnPermanentFailure callback
// (design doc §6): claim NOT settled gets today's behavior (the pairing
// flow's own ShowJoinFailed); claim settled clears the stuck "updating"
// overlay and logs instead — a settled/claimed device has no "join failed"
// to report, that narration belongs to the pre-claim pairing flow only.
// Extracted as a named method (rather than an inline closure) so the policy
// is directly unit-testable without driving the gate's real HTTP/runner
// plumbing.
func (e *executor) otaPermanentFailureNarration(fs otagate.FailureState) {
	if e.claimSettled() {
		e.setupUI().HideIfShowing(setupui.StateUpdating)
		e.logger.Warn("OTA update permanently failed on a settled device", zap.Error(fs.Err))
		return
	}
	// The only pre-claim narration surface is the player's join_failed screen,
	// whose TITLE is player-owned ("Wi-Fi join failed") — only the body text
	// is controllable from here, so a truthful title needs a new ff-player
	// state and remains the open half of F-12 (ux-must-fix M-2 驗收:
	// "畫面不再顯示 Wi-Fi 失敗"). Until then keep the body honest and legible:
	// a fixed sentence the user can act on, never raw updater internals. The
	// raw error goes to the log, where it belongs.
	e.logger.Warn("OTA update ladder failed before claim; narrating via join_failed", zap.Error(fs.Err))
	e.setupUI().ShowJoinFailed("System update failed. The device will keep retrying automatically.")
}

// clearStuckUpdatingOverlay is the post-ladder watchdog's fired callback
// (Deps.OnUpdateSucceededNoReboot, design doc §6): a successful
// update ladder normally ends in a reboot within seconds, so still being
// alive PostLadderWatchdogTimeout later means that reboot did not happen.
// Unconditional (not claim-gated): whichever entry point's "updating"
// narration is still up, it no longer reflects real progress either way.
func (e *executor) clearStuckUpdatingOverlay() {
	e.logger.Warn("OTA update succeeded but no reboot followed within the watchdog timeout; clearing the updating overlay")
	e.setupUI().HideIfShowing(setupui.StateUpdating)
}

// narrateUpdateProgress paints the OTA update percent on-screen via the shared
// setup narrator. It is the gate's Deps.OnProgress callback. pct == -1 marks a
// progress line the updater emitted without a percent field (e.g. "Preparing");
// that is skipped rather than painting a misleading 0%, since ff-player's
// updating panel renders progress 0 as an actual zero. Best-effort like every
// setupui push: a dead or absent player never gates the update.
func (e *executor) narrateUpdateProgress(pct int) {
	if pct < 0 {
		return
	}
	e.setupUI().ShowUpdating(pct)
}

func (e *executor) factoryReset(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing factory reset command")

	// Latch FIRST, before the unclaim below arms the claim flow and connect()
	// (see resetStaged): the arming and the guard must not be separable by a
	// concurrent relayer message.
	e.resetStaged.Store(true)

	// Topic rotation (security invariant): set-factory-boot.service does NOT
	// swap the running root. It snapshots the factory image to
	// @snapshots/@factory_reset_new and arms a ONE-SHOT boot entry
	// (bootctl set-oneshot) while leaving the btrfs default at @snapshots/@ —
	// so a candidate that fails to boot silently lands the device back on THIS
	// subvolume, claim state and all, with the reset never having happened (see
	// ffos docs/SNAPSHOT_SYSTEM_V2_FLOW.md). Clear the persisted claim (topic
	// AND ConnectedDevice) here so that rollback — and a failed unit start
	// below — cannot leave a resold device commandable on the old topic.
	// Leaving ConnectedDevice would additionally keep the process locally
	// "claimed" (hub /api/status, mDNS TXT, claimSettled), blocking any
	// re-claim.
	//
	// On the SUCCESS path this write is redundant: the state file lives at
	// /home/feralfile/.state/ inside the root subvolume (no separate @home —
	// the install creates only @log, @pkg and @snapshots), so booting the
	// candidate discards it wholesale. It is kept for the rollback path alone.
	e.clearPersistedClaim()

	// The process-lifetime pairing latch must fall with the persisted claim,
	// or claimSettled() would still read true and withhold the claim QR after
	// a failed reset.
	e.pairingConfirmed.Store(false)

	// Reflect the unclaim on every surface that mirrors claim state (mDNS TXT
	// via the mediator's observer), exactly as a claim flips it the other way.
	if e.claimObserver != nil {
		e.claimObserver(false)
	}

	// The LIVE relayer session is deliberately left OPEN. Closing it (the
	// former shape) revoked the old owner's control channel, but it was also
	// the surest way to trip the topic-observer chain described on resetStaged:
	// the close made the mediator reconcile the connection back up within ~2s,
	// topic-less, and the fresh topic's assignment edge repainted the claim QR
	// over this reset's own narration. It cost the command's CmdOK ack too —
	// that ack could never be delivered over the socket it had just closed.
	//
	// What the close bought is now covered from the other side, and covered
	// better: the persisted topic is already gone above, and resetStaged closes
	// the command surface itself (commandrouter.Process's guard — every
	// transport and every command family) rather than one transport to it. What remains reachable on that socket is read-only reporting. Do not
	// reintroduce the close to shrink it further: the reconnect it forces is a
	// repaint trigger, not a revocation.

	// controld runs the reset in-process: start the system reset unit directly.
	return e.factoryResetInProcess(ctx)
}

// clearPersistedClaim zeroes the persisted relayer topicID AND the
// ConnectedDevice claim record. See factoryReset for the security rationale.
// Best-effort: a save failure is logged but does not abort the reset (the
// reboot into the factory snapshot still discards both). A no-op when nothing
// is persisted.
func (e *executor) clearPersistedClaim() {
	changed, err := state.ClearClaim()
	if !changed {
		return
	}
	if err != nil {
		e.logger.Error("Factory reset: failed to clear persisted claim state", zap.Error(err))
	}
}

// factoryResetInProcess runs the controld-owned reset: start the system reset
// unit directly. Ported from feral-setupd system.rs::factory_reset
// (systemctl start set-factory-boot.service; no sudo, matching the otagate
// updater-unit start). set-factory-boot.service stages a one-shot boot into the
// pristine factory snapshot and reboots.
func (e *executor) factoryResetInProcess(ctx context.Context) (interface{}, error) {
	// On-screen confirmation before the device reboots into the factory image.
	// Best-effort like all setup narration — a dead or absent screen must never
	// block the reset. It is sent as an extension state ("factory_reset"): a
	// current player accepts it with {ok:true} and renders nothing, so the panel
	// only paints the confirmation once ff-player adds the state to its renderer.
	e.setupUI().ShowFactoryReset()

	out, err := e.exec.CommandContext(ctx, "systemctl", "start", "set-factory-boot.service").CombinedOutput()
	if err != nil {
		// Nothing is staged and no reboot is coming: release the latch so the
		// device — now unclaimed — can be re-claimed and can paint its claim QR
		// again. Keeping it set would strand a live device with no way back.
		e.releaseStuckResetLatch("factory reset unit failed to start")
		return nil, fmt.Errorf("failed to start factory reset service: %w: %s", err, strings.TrimSpace(string(out)))
	}
	// A clean start is NOT evidence the reset was staged: the unit is
	// Type=simple, so systemctl returns the moment the script is spawned, and
	// factory_reset.sh runs under `set -euo pipefail` with explicit exit paths
	// (subvolume delete, snapshot creation) BEFORE it ever reaches
	// `bootctl set-oneshot`. Arm the watchdog for that gap.
	e.scheduleStuckResetWatchdog()
	return CmdOK, nil
}

// stuckResetWatchdogTimeout bounds how long the staged-reset latch may hold the
// command surface and the screen. factory_reset.sh ends in `sleep 8; systemctl
// reboot`, so a real reset kills this process an order of magnitude sooner;
// still being alive when this elapses IS the "no reboot completed" signal — the
// same detection-free design as otagate's post-ladder watchdog. Strictly it
// cannot distinguish "never staged" from "shutdown itself is hanging past two
// minutes", and the latter would release mid-shutdown; the process is being
// killed either way, so the release is moot there.
const stuckResetWatchdogTimeout = 2 * time.Minute

// scheduleStuckResetWatchdog arms the release for a reset that never rebooted.
// Detached from any caller ctx for the reason otagate's twin documents: in
// production this runs under the daemon-lifetime ctx, and a canceled ctx would
// make SleepContext return early and SKIP the release rather than perform it —
// the wrong direction for a guard whose failure mode is a device stuck
// refusing commands.
func (e *executor) scheduleStuckResetWatchdog() {
	go func() {
		if err := e.clock.SleepContext(context.Background(), stuckResetWatchdogTimeout); err != nil {
			return
		}
		if !e.resetStaged.Load() {
			return
		}
		e.releaseStuckResetLatch("factory reset staged but no reboot followed within the watchdog timeout")
	}()
}

// releaseStuckResetLatch reopens the command surface and takes down the reset
// panel. Extracted as a named method — like otagate's clearStuckUpdatingOverlay
// — so the policy is unit-testable without driving a real timer. The hide is
// conditional (HideIfShowing) so a narrator that legitimately took the screen
// while the reset was staged is not erased.
func (e *executor) releaseStuckResetLatch(why string) {
	e.logger.Warn("Releasing the staged factory-reset latch", zap.String("reason", why))
	e.resetStaged.Store(false)
	e.setupUI().HideIfShowing(setupui.StateFactoryReset)
}

func (e *executor) uploadLogs(ctx context.Context, args []byte) (interface{}, error) {
	e.logger.Info("Executing upload logs command")

	var cmdArgs struct {
		UserID               string `json:"userId"`
		APIKey               string `json:"apiKey"`
		Title                string `json:"title"`
		SupportBundleID      string `json:"supportBundleID"`
		SupportBundleIDSnake string `json:"support_bundle_id"`
	}

	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if cmdArgs.UserID == "" || cmdArgs.APIKey == "" || cmdArgs.Title == "" {
		return nil, fmt.Errorf("missing required arguments: userId, apiKey, and title are required")
	}

	supportBundleID := strings.TrimSpace(cmdArgs.SupportBundleID)
	if supportBundleID == "" {
		supportBundleID = strings.TrimSpace(cmdArgs.SupportBundleIDSnake)
	}

	// controld runs the log upload in-process (ported from feral-setupd
	// log_uploader.rs). userId/title are validated for parity but unused by the
	// v2 API, exactly as the Rust callback ignores them.
	return e.uploadLogsInProcess(ctx, cmdArgs.APIKey, supportBundleID)
}

// logUploadTimeout bounds one detached log upload end-to-end (zip + pre-sign +
// S3 PUT). Generous on purpose: big archives on slow uplinks are the case
// support most needs, and the per-request 30s wrapper timeout that used to
// apply killed exactly those. The bound exists so a hung transfer cannot pin
// the single-flight guard forever.
const logUploadTimeout = 10 * time.Minute

// uploadLogsInProcess zips and uploads the device logs directly (controld-owned
// path). Like feral-setupd's D-Bus callback it is fire-and-forget: the relayer
// command ACKs immediately while the upload runs on a detached context, so a
// slow zip or network transfer never holds the command executor (and its
// command-storm budget) open. Errors are logged, not surfaced, matching setupd.
// Single-flighted via logUploadInFlight (see the field note) and bounded by
// logUploadTimeout so the guard always releases.
func (e *executor) uploadLogsInProcess(ctx context.Context, apiKey, supportBundleID string) (interface{}, error) {
	if !e.logUploadInFlight.CompareAndSwap(false, true) {
		e.logger.Info("Log upload already in flight; duplicate command ignored")
		return CmdOK, nil
	}

	info := e.logUploadBuildInfo(ctx)
	uploader := e.newLogUploader()

	//nolint:gosec // G118 flags the detached context, which is the entire point
	// here (see this function's doc): the command ACKs immediately, so the
	// request ctx would be canceled out from under the transfer. The upload is
	// bounded by logUploadTimeout instead, not left unbounded.
	go func() {
		// Detached from the command ctx: the command returns as soon as the upload
		// is scheduled, so ctx would be canceled out from under the transfer.
		defer e.logUploadInFlight.Store(false)
		uploadCtx, cancel := context.WithTimeout(context.Background(), logUploadTimeout)
		defer cancel()
		if err := uploader.Upload(uploadCtx, apiKey, logUploadSource, info, supportBundleID); err != nil {
			e.logger.Error("In-process log upload failed", zap.Error(err))
		}
	}()

	return CmdOK, nil
}

// SelfUploadLogs is the netlog recorder's stage-2a egress (wired in main.go,
// type-asserted like the other seams — deliberately NOT on the Executor
// interface, so mocks stay untouched): after a reconnect-stability window it
// pushes the log bundle (which includes the netlog ring — uploadLogs walks
// ~/.logs recursively) without a controller in the loop. It reuses
// uploadLogsInProcess wholesale, so the single-flight CAS, the detached
// 10-minute bound, and the wire shape are identical to the command path — a
// concurrent controller-initiated upload simply wins the CAS and the
// self-upload is dropped, which is correct (the bundle is the same).
// apiKey is the operator-provisioned support-logs key (config
// netlog.selfUploadApiKey); callers must not invoke this with an empty key.
func (e *executor) SelfUploadLogs(apiKey string) {
	// Background ctx: there is no request to inherit from; the upload bounds
	// itself with logUploadTimeout exactly like the command path.
	if _, err := e.uploadLogsInProcess(context.Background(), apiKey, ""); err != nil {
		e.logger.Warn("netlog self-upload failed to start", zap.Error(err))
	}
}

// newLogUploader builds the uploader used by the in-process path, honoring a
// test override. The timeout-free HTTP client is deliberate: the default
// client's 30s whole-request timeout covers the entire S3 PUT body, which
// deterministically fails large archives on slow uplinks; the upload is bounded
// by uploadLogsInProcess's 10-minute context instead.
func (e *executor) newLogUploader() logUploaderIface {
	if e.logUploaderFactory != nil {
		return e.logUploaderFactory()
	}
	return newLogUploader(wrapper.NewHTTPClientWithoutTimeout(), e.os, e.json, e.logger)
}

// logUploadBuildInfo gathers the device identity/build metadata the log
// submission reports, from the same on-device sources feral-setupd's AppState
// used: the hostname for device_id and the FF1 build descriptor for
// branch/version. A missing build descriptor is non-fatal — the upload proceeds
// with empty branch/version rather than being blocked on metadata.
func (e *executor) logUploadBuildInfo(ctx context.Context) logUploadBuildInfo {
	info := logUploadBuildInfo{DeviceID: e.deviceID()}
	branch, version, _, err := otagate.NewFileConfigProvider(e.os, e.json).LocalBuild(ctx)
	if err != nil {
		e.logger.Warn("Log upload: could not read local build; branch/version omitted", zap.Error(err))
		return info
	}
	info.Branch = branch
	info.Version = version
	return info
}

// deviceID reads the device identity from the hostname, falling back to "FF1".
// Ported from feral-setupd system.rs::get_device_id; it is the identity the
// claim/pairing device_info and the log submission both report.
func (e *executor) deviceID() string {
	data, err := e.os.ReadFile(constants.HOSTNAME_FILE)
	if err != nil {
		return "FF1"
	}
	id := strings.TrimSpace(string(data))
	if id == "" {
		return "FF1"
	}
	return id
}

func (e *executor) setVolume(ctx context.Context, args []byte) (interface{}, error) {
	e.logger.Info("Executing set-volume command")

	var cmdArgs struct {
		Percent int `json:"percent"`
	}

	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Validate input range
	if cmdArgs.Percent < 0 || cmdArgs.Percent > 100 {
		return nil, fmt.Errorf("percent must be between 0 and 100, got: %d", cmdArgs.Percent)
	}

	// User input 0% maps to 25%, user input 100% maps to 100%
	// Formula: pactl_percent = 25 + (user_percent * 0.75)
	pactlPercent := 0
	if cmdArgs.Percent > 0 {
		pactlPercent = 25 + (cmdArgs.Percent * 75 / 100)
	}

	e.logger.Info("Setting volume", zap.Int("user_percent", cmdArgs.Percent), zap.Int("pactl_percent", pactlPercent))

	// Execute pamixer command
	cmd := e.exec.CommandContext(ctx, "pamixer", "--set-volume", fmt.Sprintf("%d", pactlPercent))
	output, err := cmd.CombinedOutput()
	if err != nil {
		e.logger.Error("Failed to set volume",
			zap.Error(err),
			zap.String("output", string(output)))
		return nil, fmt.Errorf("failed to set volume: %w", err)
	}

	e.logger.Info("Volume set successfully", zap.Int("percent", pactlPercent))

	// Save the user percentage to persist across OTA
	// #nosec G306 -- intentionally world-readable for volume information
	if err := os.WriteFile(SavedVolumeFile, []byte(fmt.Sprintf("%d", pactlPercent)), 0644); err != nil {
		e.logger.Warn("Failed to save volume to file",
			zap.Error(err),
			zap.String("file", SavedVolumeFile))
	}

	return CmdOK, nil
}

func (e *executor) toggleMute(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing toggle-mute command")

	// Execute pamixer command to toggle mute
	cmd := e.exec.CommandContext(ctx, "pamixer", "--toggle-mute")
	output, err := cmd.CombinedOutput()
	if err != nil {
		e.logger.Error("Failed to toggle mute",
			zap.Error(err),
			zap.String("output", string(output)))
		return nil, fmt.Errorf("failed to toggle mute: %w", err)
	}

	e.logger.Info("Mute toggled successfully")

	return CmdOK, nil
}

func (e *executor) ddcPanelControl(ctx context.Context, args []byte) (interface{}, error) {
	var req ddc.DdcPanelControlRequest
	if err := e.json.Unmarshal(args, &req); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	action, err := ddc.ParseDdcPanelAction(req.Action)
	if err != nil {
		return nil, err
	}
	if len(req.Value) == 0 {
		return nil, fmt.Errorf("value is required for ddcPanelControl action %q", action)
	}
	if err := e.panelDDC.ApplyControl(ctx, action, req.Value); err != nil {
		return nil, err
	}
	return CmdOK, nil
}

// ddcPanelStatus reads the standard panel VCPs. Request body is unused (send {}).
func (e *executor) ddcPanelStatus(ctx context.Context, _ []byte) (interface{}, error) {
	return e.panelDDC.CollectStatus(ctx)
}
