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

	// pairingConfirmed latches the cloud's showPairingQRCode(false) pairing
	// confirmation. connect() (the claim) sets ConnectedDevice, but the
	// confirmation does NOT — without this latch the auto-claim loop stayed
	// blind to it and could repaint the claim QR over a paired device's
	// Ready/hidden screen. In-memory: a restart re-derives via deviceClaimed
	// (connect precedes the confirmation in the normal flow).
	pairingConfirmed atomic.Bool

	// staleOverlaySwept gates the once-per-process boot reconciliation of the
	// player's overlay: narration is in-memory, so after a daemon restart the
	// player keeps rendering the PREVIOUS life's overlay (e.g. a claim QR
	// painted before a crash on a device that has since been claimed).
	staleOverlaySwept atomic.Bool

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
	// Player (CDP) leg: sleepAppliedState/sleepApplyOK is the last state the
	// synchronous player leg reached. ok=false (or a state mismatch) forces the
	// next applySleepTransitionIfChanged to re-drive the player.
	sleepAppliedState *sleepschedule.State
	sleepApplyOK      bool
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

	// Claimed state is derived everywhere else (mDNS init, /api/status, the LAN
	// topic-withholding guard) as "ConnectedDevice with a non-empty ID". Reject
	// an empty ID so a malformed connect can't flip the claim observer to true
	// while every derived view still reports unclaimed.
	if strings.TrimSpace(cmdArgs.Device.ID) == "" {
		return nil, fmt.Errorf("invalid arguments: clientDevice.id is required")
	}

	wasClaimed := e.deviceClaimed()

	s := state.GetState()
	s.ConnectedDevice = &state.Device{
		ID:       cmdArgs.Device.ID,
		Name:     cmdArgs.Device.Name,
		Platform: cmdArgs.Device.Platform,
	}
	err = s.Save()
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
func (e *executor) sendDisplayDefaultPlaylist() error {
	if e.cdp == nil {
		return fmt.Errorf("cdp client is not configured")
	}

	command := commands.Command{
		Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
		Arguments: map[string]any{},
	}
	payload, err := command.JSON()
	if err != nil {
		return fmt.Errorf("marshal displayDefaultPlaylist payload: %w", err)
	}

	_, err = e.cdp.Send(cdp.METHOD_EVALUATE, map[string]any{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(payload)),
	})
	if err != nil {
		return fmt.Errorf("send displayDefaultPlaylist command to player: %w", err)
	}
	return nil
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
// reach one before it can be claimed. It reports whether the QR was painted
// and whether the outcome is terminal for this process (ResultUpdateStarted:
// the device is rebooting into the new build; ResultTooOldToUpgrade: nothing
// short of a reflash helps) so the auto-claim retry loop knows when another
// attempt is pointless.
func (e *executor) runPreClaimGateAndPaint(ctx context.Context, skipIfSettled bool) (painted, terminal bool) {
	result, err := e.otaGateInstance().EnsureLatestBeforeClaim(ctx)
	if err != nil {
		// Retryable: version-check failures (often fresh-network DNS/route
		// convergence) and failed update ladders (the ladder clears its own
		// latch on the next explicit run).
		e.logger.Warn("Pre-claim OTA gate did not pass; withholding claim QR",
			zap.Error(err), zap.Int("gateResult", int(result)))
		return false, false
	}
	if result != otagate.ResultNoUpdateNeeded {
		e.logger.Info("Pre-claim OTA gate did not settle on no-update; withholding claim QR",
			zap.Int("gateResult", int(result)))
		// UpdateStarted: the updating narration owns the screen via OnProgress
		// and the device reboots. TooOldToUpgrade: nothing further happens this
		// boot, so don't leave a stale finalizing overlay implying progress.
		if result == otagate.ResultTooOldToUpgrade {
			e.setupUI().Hide()
		}
		return false, true
	}
	// The gate is slow (live version check, possibly an update ladder); the
	// device may have been claimed or pairing-confirmed while it ran. The
	// auto loop must never repaint the QR over a settled device; the relayer
	// command path opts out (an explicit cloud show=true is a re-pair).
	if skipIfSettled && e.claimSettled() {
		e.setupUI().Hide()
		return false, true
	}
	e.setupUI().ShowClaimQR(e.buildDeviceConnectURL(ctx), e.deviceID())
	return true, false
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
	if !e.autoClaimInFlight.CompareAndSwap(false, true) {
		return
	}
	defer e.autoClaimInFlight.Store(false)

	if e.claimSettled() {
		// Boot reconciliation: narration is in-memory, so after a daemon
		// restart the player may still render the PREVIOUS life's overlay
		// (e.g. claim_qr painted before a crash on a device that has since
		// been claimed). Sweep it once — and only once: later online flaps on
		// a settled device must not wipe a live updating/factory-reset
		// narration.
		if e.staleOverlaySwept.CompareAndSwap(false, true) {
			e.setupUI().Hide()
		}
		return
	}
	// Narrate the gap: between the join succeeding and the claim QR there are
	// seconds of topic wait + version check (more if the gate retries) that
	// would otherwise be a silent black screen.
	e.setupUI().ShowFinalizing()

	if !e.waitForRelayerTopic(ctx) {
		e.logger.Warn("Auto claim flow: relayer topic not ready; withholding claim QR until the next online transition")
		e.setupUI().Hide() // clear our finalizing narration; nothing is coming
		return
	}
	// Re-check: the device may have settled while waiting (LAN connect).
	if e.claimSettled() {
		e.setupUI().Hide()
		return
	}

	e.logger.Info("Auto claim flow: device online and unclaimed; running pre-claim gate")
	backoff := autoClaimRetryMin
	for {
		painted, terminal := e.runPreClaimGateAndPaint(ctx, true)
		if painted || terminal {
			return
		}
		e.logger.Info("Auto claim flow: gate not settled; retrying", zap.Duration("backoff", backoff))
		if err := e.clock.SleepContext(ctx, backoff); err != nil {
			return
		}
		if e.claimSettled() {
			e.setupUI().Hide() // clear finalizing; the device settled mid-backoff
			return
		}
		backoff *= 2
		if backoff > autoClaimRetryMax {
			backoff = autoClaimRetryMax
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
// init, hub status): a ConnectedDevice with a non-empty ID.
func (e *executor) deviceClaimed() bool {
	s := state.GetState()
	return s != nil && s.ConnectedDevice != nil && strings.TrimSpace(s.ConnectedDevice.ID) != ""
}

// waitForRelayerTopic polls persisted state until the relayer topic is
// available, the wait window closes, or ctx ends.
func (e *executor) waitForRelayerTopic(ctx context.Context) bool {
	deadline := e.clock.Now().Add(autoClaimTopicWait)
	for {
		if s := state.GetState(); s != nil && s.Relayer != nil && strings.TrimSpace(s.Relayer.TopicID) != "" {
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
	topicID := ""
	if s := state.GetState(); s != nil && s.Relayer != nil {
		topicID = s.Relayer.TopicID
	}
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
	return e.deviceStatus.GetStatus(ctx)
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

func (e *executor) updateToLatest(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing system update command")

	// Route the user-triggered update through the OTA gate so it is single-flighted
	// with the mandatory pre-claim gate. The gate drives the updater locally.
	if _, err := e.otaGateInstance().RequestUpdate(ctx); err != nil {
		return nil, fmt.Errorf("failed to request system update: %w", err)
	}

	return CmdOK, nil
}

// otaGateInstance lazily builds the shared OTA gate from the executor's existing
// seams. Built once so its single-flight guard and permanent-failure latch persist
// across relayer commands.
func (e *executor) otaGateInstance() *otagate.Gate {
	e.otaGateOnce.Do(func() {
		e.otaGate = otagate.New(otagate.Deps{
			HTTP:   wrapper.NewHTTPClient(),
			Clock:  e.clock,
			Runner: otagate.NewSystemdRunner(e.exec, e.clock, e.logger),
			Config: otagate.NewFileConfigProvider(e.os, e.json),
			Logger: e.logger,
			// Narrate update progress on-screen: the gate hands each parsed percent
			// to setupui's updating overlay via this callback.
			OnProgress: e.narrateUpdateProgress,
		})
		// Surface a latched permanent OTA failure on-screen. There is no dedicated
		// update-failed CDP state in the shipping player contract, so we reuse the
		// existing join_failed narration (the least-surprising "setup could not
		// complete" surface) rather than inventing a new state. Best-effort: a dead
		// or absent player must never gate the OTA latch.
		e.otaGate.OnPermanentFailure(func(fs otagate.FailureState) {
			reason := "update-failed"
			if fs.Err != nil {
				reason = fs.Err.Error()
			}
			e.setupUI().ShowJoinFailed(reason)
		})
	})
	return e.otaGate
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

	// Topic rotation (security invariant): set-factory-boot.service stages a
	// one-shot boot into a pristine factory btrfs snapshot and reboots; it does
	// NOT wipe the currently running subvolume, so the persisted relayer topicID
	// (and the live relayer session) survive on disk until that reboot actually
	// completes. A resold device — or one whose reset is interrupted before the
	// reboot — would otherwise keep running on the old subvolume and stay
	// commandable via the saved topic. Clear the persisted topic here so that
	// window is closed.
	e.clearPersistedRelayerTopic()

	// controld runs the reset in-process: start the system reset unit directly.
	return e.factoryResetInProcess(ctx)
}

// clearPersistedRelayerTopic zeroes the persisted relayer topicID. See
// factoryReset for the security rationale. Best-effort: a save failure is logged
// but does not abort the reset (the reboot into the factory snapshot still
// discards the topic). A no-op when no topic is persisted.
func (e *executor) clearPersistedRelayerTopic() {
	s := state.GetState()
	if s == nil || s.Relayer == nil || s.Relayer.TopicID == "" {
		return
	}
	s.Relayer.TopicID = ""
	if err := s.Save(); err != nil {
		e.logger.Error("Factory reset: failed to clear persisted relayer topic", zap.Error(err))
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
		return nil, fmt.Errorf("failed to start factory reset service: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return CmdOK, nil
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
