package devicectl

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/otagate"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// narratorSpy records setup-narration calls in order so ordering invariants can
// be asserted.
type narratorSpy struct {
	calls        []string
	lastURL      string
	lastName     string
	lastProgress int
}

func (s *narratorSpy) ShowFinalizing() { s.calls = append(s.calls, "finalizing") }
func (s *narratorSpy) ShowClaimQR(url string, deviceName string) {
	s.calls = append(s.calls, "claim")
	s.lastURL = url
	s.lastName = deviceName
}
func (s *narratorSpy) ShowReady()        { s.calls = append(s.calls, "ready") }
func (s *narratorSpy) ShowFactoryReset() { s.calls = append(s.calls, "factory_reset") }
func (s *narratorSpy) ShowJoinFailed(reason string) {
	s.calls = append(s.calls, "join_failed")
	s.lastURL = reason
}
func (s *narratorSpy) ShowUpdating(progress int) {
	s.calls = append(s.calls, "updating")
	s.lastProgress = progress
}
func (s *narratorSpy) Hide() { s.calls = append(s.calls, "hide") }

// SweepStaleOverlay mirrors setupui.Service: the hide runs only when THIS
// process has no narration intent yet — an overlay this process painted (or
// already hid) is by definition not stale.
func (s *narratorSpy) SweepStaleOverlay() {
	if len(s.calls) == 0 {
		s.calls = append(s.calls, "hide")
	}
}

// HideIfShowing mirrors setupui.Service: hide only when the current intent
// (the last recorded call) is one of states. Caveat for future callers: most
// spy labels match the wire state names ("finalizing", "updating",
// "factory_reset"), but ShowClaimQR records "claim" while the wire state is
// "claim_qr" — a HideIfShowing(claim_qr) test through this spy would no-op.
func (s *narratorSpy) HideIfShowing(states ...string) {
	if len(s.calls) == 0 {
		return
	}
	current := s.calls[len(s.calls)-1]
	for _, st := range states {
		if current == st {
			s.calls = append(s.calls, "hide")
			return
		}
	}
}

// ShowConnectingIfShowing mirrors setupui.Service: the connecting paint lands
// only when the current intent is one of states (the topic-wait expiry's
// race-safe conditional paint).
func (s *narratorSpy) ShowConnectingIfShowing(message string, states ...string) {
	if len(s.calls) == 0 {
		return
	}
	current := s.calls[len(s.calls)-1]
	for _, st := range states {
		if current == st {
			s.calls = append(s.calls, "connecting")
			s.lastURL = message
			return
		}
	}
}

func strPtr(s string) *string { return &s }

// claimSnapshotFromState derives the state.ClaimInfo view of a *state.State
// the same way state.ClaimSnapshot() does in production. Tests that mock
// GetState() with a MUTABLE *state.State (mid-test claim/reset) mock
// ClaimSnapshot() with a DoAndReturn closure over this helper so the two
// mocked reads stay consistent with each other as the backing struct changes,
// mirroring how the real state manager derives both from the same memory.
func claimSnapshotFromState(s *state.State) state.ClaimInfo {
	var info state.ClaimInfo
	if s != nil && s.ConnectedDevice != nil {
		info.DeviceID = s.ConnectedDevice.ID
		info.DeviceName = s.ConnectedDevice.Name
		info.Claimed = strings.TrimSpace(s.ConnectedDevice.ID) != ""
	}
	if s != nil && s.Relayer != nil {
		info.TopicID = s.Relayer.TopicID
		info.TopicReady = s.Relayer.IsReady()
	}
	return info
}

func TestFormatDeviceConnectURL_ByteCompatible(t *testing.T) {
	// The exact bytes launcher-ui/index.html renders as the claim QR for a fixture
	// device_info: <device_id>|<topic_id>|<internet>|<branch>|<version>|<phase>,
	// branch '/' -> %2F, nothing else encoded.
	got := formatDeviceConnectURL("test-device", "topic-abc", true, "main/stable", "1.2.3", "pairing")
	want := "https://link.feralfile.com/device_connect/test-device|topic-abc|true|main%2Fstable|1.2.3|pairing"
	assert.Equal(t, want, got)
}

func TestFormatDeviceConnectURL_OfflineEmptyTopic(t *testing.T) {
	// Mirrors feral-setupd build_device_info: empty topic yields two consecutive
	// pipes, internet reports "false", a slash-free branch is unchanged.
	got := formatDeviceConnectURL("test-device", "", false, "develop", "1.0.0", "pairing")
	want := "https://link.feralfile.com/device_connect/test-device||false|develop|1.0.0|pairing"
	assert.Equal(t, want, got)
}

func TestFormatDeviceConnectURL_EncodesEverySlashInBranch(t *testing.T) {
	got := formatDeviceConnectURL("d", "t", true, "feature/a/b", "2.0.0", "pairing")
	want := "https://link.feralfile.com/device_connect/d|t|true|feature%2Fa%2Fb|2.0.0|pairing"
	assert.Equal(t, want, got)
}

func TestShowPairingQRCodeInProcess_RecordsReadyBeforeHide(t *testing.T) {
	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}

	res, err := e.showPairingQRCodeInProcess(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)

	// The Ready record must be registered before the overlay is hidden
	// (callbacks.rs:476 record-before-transition invariant).
	assert.Equal(t, []string{"ready", "hide"}, spy.calls)
}

func TestFactoryResetInProcess_ShowsConfirmationThenStartsService(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExec := mocks.NewMockExec(ctrl)
	mockCmd := mocks.NewMockExecCmd(ctrl)
	mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "start", "set-factory-boot.service").
		Return(mockCmd)
	mockCmd.EXPECT().CombinedOutput().Return([]byte(""), nil)

	spy := &narratorSpy{}
	// pendingRebootClock: a successful unit start arms the stuck-reset
	// watchdog, whose sleep must not resolve here.
	e := &executor{logger: zap.NewNop(), exec: mockExec, setupNarrator: spy, clock: &pendingRebootClock{}}

	res, err := e.factoryResetInProcess(context.Background())
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	// The controld-owned reset pushes the on-screen confirmation.
	assert.Equal(t, []string{"factory_reset"}, spy.calls)
}

// pendingRebootClock models the normal post-reset world for the stuck-reset
// watchdog: the reboot arrives long before the timeout, so the watchdog's sleep
// never returns. Tests that want the fired policy call releaseStuckResetLatch
// directly — keeping the timer out of the assertions makes them deterministic
// and keeps the watchdog goroutine off the narration spy.
type pendingRebootClock struct{ autoClaimClock }

func (c *pendingRebootClock) SleepContext(ctx context.Context, _ time.Duration) error {
	<-ctx.Done()
	return ctx.Err()
}

// stagedResetExecutor builds an executor mid-factory-reset: the state manager
// is injected, the reset unit's start is mocked with unitOK, and factoryReset
// has run. Shared by the resetStaged tests below.
func stagedResetExecutor(t *testing.T, ctrl *gomock.Controller, unitOK bool) (*executor, *narratorSpy, *mocks.MockStateManager) {
	t.Helper()

	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	t.Cleanup(state.ResetForTesting) // don't leave a finished mock as the global manager
	sm.EXPECT().ClearClaim().Return(true, nil)
	// Unclaimed for the rest of the test: the reset just cleared the claim,
	// which is exactly what arms the flows resetStaged guards.
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()

	mockExec := mocks.NewMockExec(ctrl)
	mockCmd := mocks.NewMockExecCmd(ctrl)
	mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "start", "set-factory-boot.service").
		Return(mockCmd)
	if unitOK {
		mockCmd.EXPECT().CombinedOutput().Return([]byte(""), nil)
	} else {
		mockCmd.EXPECT().CombinedOutput().Return([]byte("unit failed"), errors.New("exit status 1"))
	}

	// The reset clears the owner's device name alongside the claim, so the
	// executor needs a real OS seam here even though this test is about the
	// claim-QR latch.
	mockOS := mocks.NewMockOS(ctrl)
	// Clear removes the staged temp first (resold-frame leak guard).
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE + ".tmp").Return(nil)
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE).Return(nil)

	spy := &narratorSpy{}
	e := &executor{
		logger:        zap.NewNop(),
		exec:          mockExec,
		os:            mockOS,
		setupNarrator: spy,
		json:          wrapper.NewJSON(),
		clock:         &pendingRebootClock{},
	}

	_, err := e.factoryReset(context.Background())
	if unitOK {
		require.NoError(t, err)
	} else {
		require.Error(t, err)
	}
	return e, spy, sm
}

// TestFactoryReset_StagedResetSuppressesClaimQRRepaint pins the user-visible
// regression the resetStaged latch exists for. The reset's own unclaim makes
// the auto-claim flow live again, and because the persisted topic is cleared
// too, ANY relayer reconnect inside factory_reset.sh's ~8s pre-reboot window
// draws a fresh topic whose assignment edge fires the mediator's topic
// observer — wired straight to MaybeShowClaimQROnOnline (main.go). This test
// drives that observer's exact call, so the invariant stays pinned no matter
// which internal path (read-loop reconnect, heartbeat reconcile, a re-sent
// system message) trips it.
func TestFactoryReset_StagedResetSuppressesClaimQRRepaint(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, spy, _ := stagedResetExecutor(t, ctrl, true)

	e.MaybeShowClaimQROnOnline(context.Background())

	assert.Equal(t, []string{"factory_reset"}, spy.calls,
		"nothing may repaint over the factory reset narration while the reset is staged")
}

// TestFactoryReset_LatchesResetStagedForTheCommandGuard: devicectl owns the
// latch, commandrouter.Process enforces it (that is the one dispatch point
// every transport and every command family shares — see the Executor
// interface). This pins the half that lives here; the rejection behavior is
// pinned in commandrouter's TestCommandHandler_Process_StagedFactoryReset_*.
func TestFactoryReset_LatchesResetStagedForTheCommandGuard(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, spy, _ := stagedResetExecutor(t, ctrl, true)

	assert.True(t, e.ResetStaged(), "a staged reset must close the command surface")
	assert.Equal(t, []string{"factory_reset"}, spy.calls)
}

// TestFactoryReset_UnitFailureReleasesResetLatch: a reset whose unit never
// started stages nothing and reboots nothing. The device keeps running
// unclaimed, so it must stay claimable — a latch that outlived the failure
// would strand it with no way back — and the panel promising an imminent
// reboot must come down with it.
func TestFactoryReset_UnitFailureReleasesResetLatch(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	e, spy, _ := stagedResetExecutor(t, ctrl, false)

	assert.False(t, e.ResetStaged())
	assert.Equal(t, []string{"factory_reset", "hide"}, spy.calls)
}

// TestFactoryReset_StuckResetWatchdogArms pins that a successful reset ARMS
// the watchdog — the half a directly-called release policy cannot cover, and
// the half that matters most: without the arming line the latch never lifts.
// The clock returns from every sleep immediately, so the detached watchdog
// goroutine runs its full path (including the real setupui.Service, which
// no-ops before touching the nil CDP because no player manifest exists here).
func TestFactoryReset_StuckResetWatchdogArms(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	t.Cleanup(state.ResetForTesting)
	sm.EXPECT().ClearClaim().Return(true, nil)
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()

	mockExec := mocks.NewMockExec(ctrl)
	mockCmd := mocks.NewMockExecCmd(ctrl)
	mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "start", "set-factory-boot.service").
		Return(mockCmd)
	mockCmd.EXPECT().CombinedOutput().Return([]byte(""), nil)

	mockOS := mocks.NewMockOS(ctrl)
	// Clear removes the staged temp first (resold-frame leak guard).
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE + ".tmp").Return(nil)
	mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE).Return(nil)

	// No narratorSpy here: the watchdog fires on its own goroutine, and the spy
	// is not synchronized. The lazily-built real Service is.
	e := &executor{logger: zap.NewNop(), exec: mockExec, os: mockOS, json: wrapper.NewJSON(), clock: &autoClaimClock{}}

	_, err := e.factoryReset(context.Background())
	require.NoError(t, err)

	require.Eventually(t, func() bool { return !e.ResetStaged() }, time.Second, 10*time.Millisecond,
		"the stuck-reset watchdog must release the latch when no reboot follows")
}

// TestNarrateUpdateProgress_ForwardsPercentSkipsUnparsed exercises the exact
// Deps.OnProgress callback otaGateInstance installs: a parsed percent paints the
// updating overlay with that value; a percent-less progress line (pct -1) is
// skipped so the panel never shows a misleading 0%.
func TestNarrateUpdateProgress_ForwardsPercentSkipsUnparsed(t *testing.T) {
	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}

	e.narrateUpdateProgress(42)
	assert.Equal(t, []string{"updating"}, spy.calls)
	assert.Equal(t, 42, spy.lastProgress)

	// A percent-less line must not add another narration nor overwrite the value.
	e.narrateUpdateProgress(-1)
	assert.Equal(t, []string{"updating"}, spy.calls)
	assert.Equal(t, 42, spy.lastProgress)
}

// --- OTA gate narrator policy (design doc §3.3) -----------------------------

// TestOTAPermanentFailureNarration_NotSettledShowsJoinFailed pins today's
// behavior for a pre-claim device: a latched permanent OTA failure narrates
// through the pairing flow's join_failed state.
func TestOTAPermanentFailureNarration_NotSettledShowsJoinFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes() // unclaimed

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}

	e.otaPermanentFailureNarration(otagate.FailureState{Failed: true, Err: errors.New("ladder exhausted")})

	assert.Equal(t, []string{"join_failed"}, spy.calls)
	// The body must be the fixed legible sentence, never the raw updater
	// error: the join_failed TITLE is player-owned and already misleading for
	// an OTA failure (open half of F-12), so the body must not add leaked
	// internals on top.
	assert.Equal(t, "System update failed. The device will keep retrying automatically.", spy.lastURL)
	assert.NotContains(t, spy.lastURL, "ladder exhausted")
}

// TestOTAPermanentFailureNarration_SettledClearsUpdatingNeverJoinFailed pins
// the settled-device policy: a claimed device has nothing to "join" — the
// permanent failure clears the stuck updating overlay (only if it owns it)
// and is logged, never repainted as join_failed.
func TestOTAPermanentFailureNarration_SettledClearsUpdatingNeverJoinFailed(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{DeviceID: "phone-1", Claimed: true}).AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}

	spy.ShowUpdating(40) // the ladder's own progress narration, still up
	e.otaPermanentFailureNarration(otagate.FailureState{Failed: true, Err: errors.New("ladder exhausted")})

	assert.Equal(t, []string{"updating", "hide"}, spy.calls)
	assert.NotContains(t, spy.calls, "join_failed", "a settled device must never see join_failed narration")
}

// TestOTAPermanentFailureNarration_ClaimPrecedenceOverJoinedFlight is the
// claim-precedence test design doc §3.3 asks for: the policy is decided at
// EMIT time by reading live claim state, not captured when a flight (or a
// caller) started — so a device that settles WHILE an update ladder it
// joined pre-claim is still running gets the settled policy when that ladder
// ultimately fails, even though the flight began pre-claim.
func TestOTAPermanentFailureNarration_ClaimPrecedenceOverJoinedFlight(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	claimed := false
	sm.EXPECT().ClaimSnapshot().DoAndReturn(func() state.ClaimInfo {
		if claimed {
			return state.ClaimInfo{DeviceID: "phone-1", Claimed: true}
		}
		return state.ClaimInfo{}
	}).AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}

	// The flight started pre-claim (no narration yet from this callback's
	// perspective — the ladder's own ShowUpdating already ran). The device
	// claims WHILE the ladder is still in flight (updateToLatest joined it
	// after settling).
	claimed = true

	e.otaPermanentFailureNarration(otagate.FailureState{Failed: true, Err: errors.New("ladder exhausted")})

	assert.NotContains(t, spy.calls, "join_failed",
		"live claim state at emit time must win over the flight's pre-claim origin")
}

// TestClearStuckUpdatingOverlay_HidesRegardlessOfClaimState pins [W10]: the
// post-ladder watchdog's callback is unconditional — whichever entry point's
// updating narration is still up, a successful ladder with no reboot means
// it no longer reflects real progress.
func TestClearStuckUpdatingOverlay_HidesRegardlessOfClaimState(t *testing.T) {
	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy}

	spy.ShowUpdating(80)
	e.clearStuckUpdatingOverlay()

	assert.Equal(t, []string{"updating", "hide"}, spy.calls)
}

// --- online-triggered auto claim (launcher-ui replacement) -------------------

// autoClaimClock is a fake clock whose SleepContext advances fake time, so the
// topic-wait window closes deterministically. onSleep, when set, runs after
// each advance — tests use it to cancel the ctx and break retry loops.
type autoClaimClock struct {
	mu      sync.Mutex
	now     time.Time
	onSleep func()
}

func (c *autoClaimClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *autoClaimClock) Sleep(time.Duration) {}
func (c *autoClaimClock) SleepContext(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.now = c.now.Add(d)
	hook := c.onSleep
	c.mu.Unlock()
	if hook != nil {
		hook()
	}
	return ctx.Err()
}
func (c *autoClaimClock) NewTicker(time.Duration) wrapper.Ticker { panic("unused") }

// TestMaybeShowClaimQROnOnline_ClaimedSweepsStaleOverlayOnce: a claimed device
// coming online must not re-run the claim flow; the FIRST such call performs
// the boot reconciliation (Hide a possibly-stale overlay from the previous
// daemon life), and later flaps must not wipe live narration.
func TestMaybeShowClaimQROnOnline_ClaimedSweepsStaleOverlayOnce(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().
		Return(&state.State{ConnectedDevice: &state.Device{ID: "phone-1"}}).
		AnyTimes()
	sm.EXPECT().ClaimSnapshot().
		Return(state.ClaimInfo{DeviceID: "phone-1", Claimed: true}).
		AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}}

	e.MaybeShowClaimQROnOnline(context.Background())
	assert.Equal(t, []string{"hide"}, spy.calls, "boot reconciliation sweeps the stale overlay once")

	e.MaybeShowClaimQROnOnline(context.Background())
	assert.Equal(t, []string{"hide"}, spy.calls, "later online flaps must not wipe live narration")
}

// TestPairingConfirmationSettlesAutoClaim: the cloud's showPairingQRCode(false)
// does not set ConnectedDevice, but it must still stop the auto-claim loop
// from ever repainting the claim QR (the paired-device repaint bug).
func TestPairingConfirmationSettlesAutoClaim(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes() // never claimed
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}}

	// Cloud confirms pairing.
	res, err := e.showPairingQRCodeInProcess(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	require.Equal(t, []string{"ready", "hide"}, spy.calls)

	// A later online transition must treat the device as settled: the boot
	// sweep no-ops (this process already narrated ready→hide, so the on-screen
	// state is not stale), no finalizing, no gate, no claim QR.
	e.MaybeShowClaimQROnOnline(context.Background())
	assert.Equal(t, []string{"ready", "hide"}, spy.calls)
	assert.NotContains(t, spy.calls, "claim")
	assert.NotContains(t, spy.calls, "finalizing")
}

// TestMaybeShowClaimQROnOnline_SweepYieldsToLiveOTANarration is the
// cross-flow regression: on a settled device's online transition the claim
// flow's stale-overlay sweep and the startup OTA gate run as UNORDERED
// goroutines, and a sweep landing after the gate's ShowUpdating must not
// erase the live update narration (an unconditional Hide also blinded the
// reload safeguard, which keys on the same narration intent). Modeled here
// with the sweep maximally delayed: the gate has already narrated when the
// sweep runs.
func TestMaybeShowClaimQROnOnline_SweepYieldsToLiveOTANarration(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().
		Return(&state.State{ConnectedDevice: &state.Device{ID: "phone-1"}}).
		AnyTimes()
	sm.EXPECT().ClaimSnapshot().
		Return(state.ClaimInfo{DeviceID: "phone-1", Claimed: true}).
		AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}}

	spy.ShowUpdating(40) // the concurrent OTA gate narrated first
	e.MaybeShowClaimQROnOnline(context.Background())
	assert.Equal(t, []string{"updating"}, spy.calls,
		"the delayed sweep must not erase live OTA narration")
}

// TestMaybeShowClaimQROnOnline_MidBackoffSettleYieldsToLiveNarration pins the
// HideIfShowing call site at the mid-backoff settled re-check — the branch
// reachable minutes after the flow started, which is exactly when another
// narrator can own the screen. The device is claimed DURING the backoff sleep
// while a concurrent update ladder narrates; the flow's clear must yield (its
// finalizing overlay is already superseded), never blanket-Hide. Reverting
// the call site to Hide() fails this test with a trailing "hide".
func TestMaybeShowClaimQROnOnline_MidBackoffSettleYieldsToLiveNarration(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	// Mutable shared state: unclaimed with a topic, so the flow passes the
	// topic wait and enters the pre-claim gate loop.
	st := &state.State{Relayer: &state.RelayerState{TopicID: "topic-abc"}}
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(st).AnyTimes()
	// DoAndReturn (not a static Return): st.ConnectedDevice mutates mid-test
	// (the backoff sleep hook below), and the gate's mid-backoff re-check must
	// observe that mutation exactly as ClaimSnapshot() would in production.
	sm.EXPECT().ClaimSnapshot().
		DoAndReturn(func() state.ClaimInfo { return claimSnapshotFromState(st) }).
		AnyTimes()
	// No local build config: the gate's version check fails, which is the
	// retryable shape that reaches the backoff sleep.
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).
		Return(nil, errors.New("no local config")).
		AnyTimes()

	spy := &narratorSpy{}
	clk := &autoClaimClock{}
	clk.onSleep = func() {
		// During the backoff sleep: the phone claims the device, and a
		// concurrent updateToLatestVersion ladder takes the screen.
		st.ConnectedDevice = &state.Device{ID: "phone-1"}
		spy.ShowUpdating(30)
	}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: clk, os: mockOS}

	e.MaybeShowClaimQROnOnline(context.Background())

	assert.Equal(t, []string{"finalizing", "updating"}, spy.calls,
		"the settled-mid-backoff clear must not erase live update narration")
}

// TestConnectClaimTransitionHidesOverlay: the claim landing is the moment the
// claim QR's job ends — connect() must clear the overlay itself rather than
// depending on the separate cloud confirmation command, and start default
// playback so the first-pair screen never sits blank; a RE-connect to an
// already-claimed device must do NEITHER (it could wipe an unrelated live
// narration such as updating, or stomp already-playing content).
func TestConnectClaimTransitionHidesOverlay(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	st := &state.State{}
	sm.EXPECT().GetState().Return(st).AnyTimes()
	// ClaimSnapshot must reflect the SAME st the SetConnectedDevice mock below
	// mutates, so the second connect()'s wasClaimed check sees the first
	// connect's write (mirrors production: both accessors read/write the same
	// underlying state).
	sm.EXPECT().ClaimSnapshot().
		DoAndReturn(func() state.ClaimInfo { return claimSnapshotFromState(st) }).
		AnyTimes()
	sm.EXPECT().SetConnectedDevice(gomock.Any()).
		DoAndReturn(func(d state.Device) error {
			st.ConnectedDevice = &d
			return nil
		}).
		Times(2)

	// Exactly ONE displayDefaultPlaylist reaches the player: sent on the claim
	// transition, not on the re-connect.
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(method string, params map[string]any) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.Contains(t, expr, string(commands.CMD_DISPLAY_DEFAULT_PLAYLIST))
			// The claim push must be conditional — the player's own fallback
			// pull may already have artwork up, and only OOM recovery is
			// allowed to force-replace playing content.
			assert.Contains(t, expr, `"onlyIfNoPlaylist":true`)
			return nil, nil
		}).
		Times(1)

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, json: wrapper.NewJSON(), cdp: mockCDP}

	args := []byte(`{"clientDevice":{"device_id":"phone-1","device_name":"Phone","platform":1},"primaryAddress":"192.168.1.50"}`)

	res, err := e.connect(args)
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	assert.Equal(t, []string{"hide"}, spy.calls, "the claim transition must clear the claim QR")

	// Re-connect on the now-claimed device (connect mutated st in place).
	res, err = e.connect(args)
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	assert.Equal(t, []string{"hide"}, spy.calls, "a re-connect must not hide again")
}

// TestConnectClaimTransitionPlayerDownDoesNotFailClaim: the default-playlist
// kick is best-effort — a player hiccup at claim time must not fail the connect
// (the claim already landed and the app is waiting on the OK).
func TestConnectClaimTransitionPlayerDownDoesNotFailClaim(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()
	sm.EXPECT().SetConnectedDevice(gomock.Any()).Return(nil)

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("player not connected"))

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, json: wrapper.NewJSON(), cdp: mockCDP}

	res, err := e.connect([]byte(`{"clientDevice":{"device_id":"phone-1","device_name":"Phone","platform":1},"primaryAddress":"192.168.1.50"}`))
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	assert.Equal(t, []string{"hide"}, spy.calls)
}

func TestConnectClaimTransitionDefaultPlaybackDoesNotClearDisplayAtCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()
	sm.EXPECT().SetConnectedDevice(gomock.Any()).Return(nil)

	mockClock := mocks.NewMockClock(ctrl)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	enteredOldPush := make(chan struct{})
	releaseOldPush := make(chan struct{})
	var enteredOnce sync.Once
	var mu sync.Mutex
	var pushed []string

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			switch {
			case strings.Contains(expr, `"id":"old"`):
				mu.Lock()
				pushed = append(pushed, "old")
				mu.Unlock()
				enteredOnce.Do(func() { close(enteredOldPush) })
				<-releaseOldPush
			case strings.Contains(expr, string(commands.CMD_DISPLAY_DEFAULT_PLAYLIST)):
				assert.Contains(t, expr, `"onlyIfNoPlaylist":true`)
				mu.Lock()
				pushed = append(pushed, "default")
				mu.Unlock()
			default:
				mu.Lock()
				pushed = append(pushed, "other")
				mu.Unlock()
			}
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		},
	).AnyTimes()

	scheduler := playlistschedule.New(context.Background(), mockCDP, mockClock, func() *time.Location {
		return time.UTC
	}, zap.NewNop())
	_ = scheduler.Prepare(&dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Daily",
			Items: []dp1playlist.PlaylistItem{
				{
					ID:        "old",
					Title:     "old",
					Source:    "https://example.com/old.html",
					DisplayAt: strPtr("2026-07-22T00:00:00Z"),
				},
			},
		},
	})
	require.True(t, scheduler.HasCache())

	go scheduler.RecomputeNow(context.Background())
	<-enteredOldPush

	spy := &narratorSpy{}
	e := &executor{
		logger:        zap.NewNop(),
		setupNarrator: spy,
		json:          wrapper.NewJSON(),
		cdp:           mockCDP,
	}
	SetWithPlayerPush(e, scheduler.WithPlayerPush, zap.NewNop())

	doneConnect := make(chan struct{})
	go func() {
		res, err := e.connect([]byte(`{"clientDevice":{"device_id":"phone-1","device_name":"Phone","platform":1},"primaryAddress":"192.168.1.50"}`))
		require.NoError(t, err)
		assert.Equal(t, CmdOK, res)
		close(doneConnect)
	}()

	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	assert.NotContains(t, pushed, "default", "claim-time fallback must wait for the in-flight scheduler push lock")
	mu.Unlock()
	close(releaseOldPush)

	select {
	case <-doneConnect:
	case <-time.After(2 * time.Second):
		t.Fatal("connect did not finish")
	}

	assert.True(t, scheduler.HasCache())
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, pushed)
	assert.Contains(t, pushed, "default", "claim-time default is still sent as conditional player fallback")
	assert.Contains(t, pushed, "old", "existing displayAt schedule remains authoritative in this branch")
}

// TestMaybeShowClaimQROnOnline_NoTopicWithholds: without a relayer topic the
// claim QR would send the app to an unroutable device; the flow must exhaust
// its bounded wait and withhold, leaving the next online transition to retry.
func TestMaybeShowClaimQROnOnline_NoTopicWithholds(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}}

	e.MaybeShowClaimQROnOnline(context.Background())

	// The gap narration paints, then clears when the flow gives up — never a
	// stale "preparing" overlay with nothing coming, and never a claim QR.
	assert.Equal(t, []string{"finalizing", "hide"}, spy.calls)
}

// TestMaybeShowClaimQROnOnline_UnclaimedRunsPreClaimGate: an unclaimed device
// with a topic must enter the same mandatory pre-claim gate as the relayer
// showPairingQRCode command (here the gate fails its version check — no local
// build config — so the QR is correctly withheld, but the gate MUST have run).
func TestMaybeShowClaimQROnOnline_UnclaimedRunsPreClaimGate(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().
		Return(&state.State{Relayer: &state.RelayerState{TopicID: "topic-abc"}}).
		AnyTimes()
	sm.EXPECT().ClaimSnapshot().
		Return(state.ClaimInfo{TopicID: "topic-abc", TopicReady: true}).
		AnyTimes()
	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).
		Return(nil, errors.New("no local config")).
		AnyTimes()

	core, observed := observer.New(zap.InfoLevel)
	spy := &narratorSpy{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// A transiently failing gate is retried with backoff; the first backoff
	// sleep cancels the ctx so the test observes exactly one retry cycle.
	clk := &autoClaimClock{onSleep: cancel}
	e := &executor{
		logger:        zap.New(core),
		setupNarrator: spy,
		clock:         clk,
		os:            mockOS,
	}

	e.MaybeShowClaimQROnOnline(ctx)

	gateRan, retried := false, false
	for _, entry := range observed.All() {
		if entry.Message == "Auto claim flow: device online and unclaimed; running pre-claim gate" {
			gateRan = true
		}
		if entry.Message == "Auto claim flow: gate not settled; retrying" {
			retried = true
		}
	}
	assert.True(t, gateRan, "unclaimed device must enter the pre-claim gate")
	assert.True(t, retried, "a transient gate failure must schedule a retry")
	assert.Contains(t, spy.calls, "finalizing", "the gap narration must paint before the gate")
	assert.NotContains(t, spy.calls, "claim", "failed gate must withhold the claim QR")
	assert.NotContains(t, spy.calls, "hide", "finalizing must stay up while the gate retries")
}

// --- ladder-failure backoff stretching (F-12) --------------------------------

// fakeGateConfig satisfies otagate.ConfigProvider with a build below the
// distributor minimum, so a ModeRequired gate always decides to update.
// failFromCall (when > 0) makes every call numbered >= failFromCall error,
// letting a test turn a later round into a version-check failure; failCalls
// errors only the listed call numbers, so a test can make ONE mid-sequence
// round transient and then let ladder rounds resume. Note the gate reads
// LocalBuild TWICE per round (runLocal + fetchRemoteVersion).
type fakeGateConfig struct {
	calls        int
	failFromCall int
	failCalls    map[int]bool
}

func (c *fakeGateConfig) LocalBuild(context.Context) (string, string, string, error) {
	c.calls++
	if (c.failFromCall > 0 && c.calls >= c.failFromCall) || c.failCalls[c.calls] {
		return "", "", "", errors.New("simulated local-config read failure")
	}
	return "stable", "1.0.0", "http://distributor.test", nil
}

// fakeGateRunner satisfies otagate.UpdateRunner and fails every spawn with the
// configured error, standing in for the real updater unit. onRun (when set)
// fires before returning — tests use it to cancel the flow ctx mid-ladder.
type fakeGateRunner struct {
	err   error
	onRun func()
}

func (r fakeGateRunner) Run(context.Context, func(int, string)) error {
	if r.onRun != nil {
		r.onRun()
	}
	return r.err
}

// parkClock is a clock whose SleepContext BLOCKS on stretched waits (anything
// longer than autoClaimRetryMax) until its ctx is done, so a test can hold the
// claim loop parked inside a stretched backoff and exercise the wake channel
// deterministically (the advancing autoClaimClock returns immediately, which
// would race the wake select). Short waits — the transient cadence and the
// wake-floor remainder — pass through instantly while advancing fake time,
// and every requested duration is recorded so tests can assert the floor was
// actually slept.
type parkClock struct {
	mu     sync.Mutex
	now    time.Time
	sleeps []time.Duration
}

func (c *parkClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *parkClock) Sleep(time.Duration)                    {}
func (c *parkClock) NewTicker(time.Duration) wrapper.Ticker { panic("unused") }
func (c *parkClock) SleepContext(ctx context.Context, d time.Duration) error {
	c.mu.Lock()
	c.sleeps = append(c.sleeps, d)
	if d <= autoClaimRetryMax {
		c.now = c.now.Add(d)
		c.mu.Unlock()
		return ctx.Err()
	}
	c.mu.Unlock()
	<-ctx.Done()
	return ctx.Err()
}
func (c *parkClock) recorded() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.sleeps...)
}

// newLadderFailureExecutor wires an executor whose pre-set OTA gate (real
// otagate.Gate, fake deps, SAME clock as the executor — the t0/FailureState.At
// comparison contract) runs a Required-mode update ladder against runner and
// config. State mocks present an unclaimed device with a ready topic.
func newLadderFailureExecutor(t *testing.T, ctrl *gomock.Controller, clk wrapper.Clock, runner otagate.UpdateRunner, cfg otagate.ConfigProvider) (*executor, *observer.ObservedLogs, *narratorSpy) {
	t.Helper()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().
		Return(&state.State{Relayer: &state.RelayerState{TopicID: "topic-abc"}}).
		AnyTimes()
	sm.EXPECT().ClaimSnapshot().
		Return(state.ClaimInfo{TopicID: "topic-abc", TopicReady: true}).
		AnyTimes()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockHTTP.EXPECT().NewRequest(gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			return http.NewRequest(method, url, body)
		}).AnyTimes()
	mockHTTP.EXPECT().Do(gomock.Any()).
		DoAndReturn(func(*http.Request) (*http.Response, error) {
			// min_runtime_version above the fake local build forces the
			// Required-mode gate into the update ladder.
			return &http.Response{
				StatusCode: 200,
				Body: io.NopCloser(strings.NewReader(
					`{"min_runtime_version":"2.0.0","latest_version":"2.0.0"}`)),
			}, nil
		}).AnyTimes()

	core, observed := observer.New(zap.InfoLevel)
	spy := &narratorSpy{}
	e := &executor{
		logger:        zap.New(core),
		setupNarrator: spy,
		clock:         clk,
		otaGate: otagate.New(otagate.Deps{
			HTTP:   mockHTTP,
			Clock:  clk,
			Runner: runner,
			Config: cfg,
		}),
	}
	return e, observed, spy
}

// scheduledBackoffs extracts every backoff the claim loop logged, in order.
func scheduledBackoffs(observed *observer.ObservedLogs) []time.Duration {
	var out []time.Duration
	for _, entry := range observed.All() {
		if entry.Message == "Auto claim flow: gate not settled; retrying" {
			if d, ok := entry.ContextMap()["backoff"].(time.Duration); ok {
				out = append(out, d)
			}
		}
	}
	return out
}

func countLogs(observed *observer.ObservedLogs, msg string) int {
	n := 0
	for _, entry := range observed.All() {
		if entry.Message == msg {
			n++
		}
	}
	return n
}

const ladderStretchLog = "Pre-claim OTA gate's update ladder failed; stretching claim retry cadence"

// TestUpdateLadderFailureLatchedSince pins the latch-freshness criterion: only
// a latch stamped at/after this call's t0 — or carrying this call's exact
// error object (a joined singleflight run) — counts as this round's ladder
// failure. A stale latch left by an earlier round (VersionCheckFailed
// deliberately does not clear it) must not reclassify a cheap transient
// failure.
func TestUpdateLadderFailureLatchedSince(t *testing.T) {
	base := time.Now()
	errLadder := errors.New("Error: Signature verification failed for image")
	errOther := errors.New("network: version check failed")
	cases := []struct {
		name string
		fs   otagate.FailureState
		err  error
		want bool
	}{
		{"nil gate error is never a ladder failure", otagate.FailureState{Failed: true, Err: errLadder, At: base}, nil, false},
		{"no latch (version-check failure)", otagate.FailureState{}, errOther, false},
		{"latch stamped exactly at t0 (frozen clock)", otagate.FailureState{Failed: true, Err: errLadder, At: base}, errLadder, true},
		{"latch stamped after t0", otagate.FailureState{Failed: true, Err: errLadder, At: base.Add(time.Second)}, errLadder, true},
		// Freshness alone fires even when the error differs: a concurrent
		// RequestUpdate ladder latching inside this round's window is
		// misattributed to it — accepted deliberately (conservative: a
		// slower, wake-preemptible retry), documented at the Failure() read.
		{"fresh latch with a different current error", otagate.FailureState{Failed: true, Err: errLadder, At: base.Add(time.Second)}, errOther, true},
		{"stale latch with a different current error", otagate.FailureState{Failed: true, Err: errLadder, At: base.Add(-time.Second)}, errOther, false},
		{"pre-t0 latch but same error object (joined in-flight run)", otagate.FailureState{Failed: true, Err: errLadder, At: base.Add(-time.Second)}, errLadder, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, updateLadderFailureLatchedSince(tc.fs, tc.err, base))
		})
	}
}

// TestMaybeShowClaimQROnOnline_LadderFailureStretchesBackoff: a gate round
// whose update ladder fails (here: bad signature, latched on attempt one) must
// be retried on the stretched escalating cadence starting at
// autoClaimLadderFailureBackoffMin, not the transient 30s..5m cadence — and
// must NOT be terminal (the claim flow has no nightly-timer fallback).
func TestMaybeShowClaimQROnOnline_LadderFailureStretchesBackoff(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Seeded non-zero clock: FailureState.At and t0 must compare correctly on
	// real timestamps, not both degenerate to the zero time. The first
	// SleepContext call is the claim loop's backoff (the version check
	// succeeds on attempt one and the signature failure latches on attempt
	// one, so the gate itself never sleeps); cancel there so the test
	// observes exactly one scheduled retry.
	clk := &autoClaimClock{now: time.Unix(1_000_000, 0), onSleep: cancel}
	e, observed, spy := newLadderFailureExecutor(t, ctrl, clk,
		fakeGateRunner{err: errors.New("Error: Signature verification failed for image.sig")},
		&fakeGateConfig{})

	e.MaybeShowClaimQROnOnline(ctx)

	assert.Equal(t, 1, countLogs(observed, ladderStretchLog),
		"the ladder failure must be classified as this round's")
	require.Equal(t, []time.Duration{autoClaimLadderFailureBackoffMin}, scheduledBackoffs(observed),
		"a ladder-failure round must schedule the stretched cadence, not the transient backoff")
	assert.NotContains(t, spy.calls, "claim", "failed gate must withhold the claim QR")
}

// TestMaybeShowClaimQROnOnline_LadderThenTransientResumesCheapCadence: after a
// latched ladder round, a round that fails the cheap way (version check) must
// fall back to the UNADVANCED transient cadence — the stale latch from the
// ladder round must not be misread (freshness criterion on a moving clock),
// and ladder rounds must not have consumed transient doublings.
func TestMaybeShowClaimQROnOnline_LadderThenTransientResumesCheapCadence(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Round 1 latches a ladder failure (LocalBuild calls 1+2 succeed). Round 2
	// fails its version check outright (call 3 errors before any HTTP), so
	// the round carries the ROUND-1 latch — now stale because the 1h backoff
	// sleep advanced the shared clock past its At. Sleep 1 is the stretched
	// backoff, sleep 2 the transient one; cancel there.
	sleeps := 0
	clk := &autoClaimClock{now: time.Unix(1_000_000, 0)}
	clk.onSleep = func() {
		sleeps++
		if sleeps == 2 {
			cancel()
		}
	}
	e, observed, _ := newLadderFailureExecutor(t, ctrl, clk,
		fakeGateRunner{err: errors.New("Error: Signature verification failed for image.sig")},
		&fakeGateConfig{failFromCall: 3})

	e.MaybeShowClaimQROnOnline(ctx)

	assert.Equal(t, 1, countLogs(observed, ladderStretchLog),
		"only round 1 is a ladder failure; the stale latch must not stretch round 2")
	assert.Equal(t, 1, countLogs(observed, "Pre-claim OTA gate did not pass; withholding claim QR"),
		"round 2 must take the plain retryable branch")
	require.Equal(t,
		[]time.Duration{autoClaimLadderFailureBackoffMin, autoClaimRetryMin},
		scheduledBackoffs(observed),
		"the transient cadence must resume at its minimum: ladder rounds do not advance it")
}

// TestMaybeShowClaimQROnOnline_TransientRoundResetsLadderEscalation pins the
// consecutive-keying of the stretched cadence: ladder → transient → ladder
// must schedule 1h, 30s, 1h — NOT 1h, 30s, 2h. The escalation accumulates
// bad-published-image evidence; a transient round in between means the
// network itself broke, which reattributes the earlier exhaustion toward the
// flaky-network cause, so the next latched round returns to the 1h floor.
func TestMaybeShowClaimQROnOnline_TransientRoundResetsLadderEscalation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Round 1 latches a ladder failure (LocalBuild calls 1+2). Round 2 fails
	// its version check outright (call 3 errors — transient). Round 3 (calls
	// 4+5) runs the ladder again and latches fresh (the backoff sleeps
	// advanced the shared clock past round 1's At, so freshness is decided by
	// timestamp, not the joined-error identity). Cancel at sleep 3 so exactly
	// three scheduled retries are observed.
	sleeps := 0
	clk := &autoClaimClock{now: time.Unix(1_000_000, 0)}
	clk.onSleep = func() {
		sleeps++
		if sleeps == 3 {
			cancel()
		}
	}
	e, observed, _ := newLadderFailureExecutor(t, ctrl, clk,
		fakeGateRunner{err: errors.New("Error: Signature verification failed for image.sig")},
		&fakeGateConfig{failCalls: map[int]bool{3: true}})

	e.MaybeShowClaimQROnOnline(ctx)

	assert.Equal(t, 2, countLogs(observed, ladderStretchLog),
		"rounds 1 and 3 are ladder failures; round 2 is transient")
	require.Equal(t,
		[]time.Duration{autoClaimLadderFailureBackoffMin, autoClaimRetryMin, autoClaimLadderFailureBackoffMin},
		scheduledBackoffs(observed),
		"a transient round must reset the ladder escalation: only CONSECUTIVE latched rounds double")
}

// TestMaybeShowClaimQROnOnline_CtxCancelDuringLadderNotStretched pins the trap
// the must-fix doc names first: a ctx cancel during the ladder (daemon
// shutdown) never latches (otagate runLocal returns before latch), so it must
// take the plain retryable branch, not the stretched cadence.
func TestMaybeShowClaimQROnOnline_CtxCancelDuringLadderNotStretched(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := &autoClaimClock{now: time.Unix(1_000_000, 0)}
	e, observed, _ := newLadderFailureExecutor(t, ctrl, clk,
		fakeGateRunner{
			// Cancel the flow ctx mid-"update", then fail the spawn: the
			// shape of a daemon shutdown killing the updater run.
			onRun: cancel,
			err:   errors.New("updater service exited without reporting completion"),
		},
		&fakeGateConfig{})

	e.MaybeShowClaimQROnOnline(ctx)

	assert.Zero(t, countLogs(observed, ladderStretchLog),
		"a ctx-canceled ladder must never be classified as a ladder failure")
	assert.Equal(t, 1, countLogs(observed, "Pre-claim OTA gate did not pass; withholding claim QR"))
}

// TestMaybeShowClaimQROnOnline_WakePreemptsStretchedBackoff: an online
// transition or topic assignment landing while the loop is parked in the
// stretched backoff calls the same in-flight-guarded entry point; the guard
// must convert that call into a wake that preempts the sleep (otherwise the
// re-trigger the narration promises is silently swallowed for hours), and
// consecutive latched rounds must escalate the cadence.
func TestMaybeShowClaimQROnOnline_WakePreemptsStretchedBackoff(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := &parkClock{now: time.Unix(1_000_000, 0)}
	e, observed, _ := newLadderFailureExecutor(t, ctrl, clk,
		fakeGateRunner{err: errors.New("Error: Signature verification failed for image.sig")},
		&fakeGateConfig{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.MaybeShowClaimQROnOnline(ctx)
	}()

	// Round 1 latched; the loop is parked in the 1h stretched backoff.
	require.Eventually(t, func() bool { return len(scheduledBackoffs(observed)) == 1 },
		5*time.Second, 5*time.Millisecond, "loop never reached its first stretched backoff")

	// A second invocation — the online-transition/topic-observer wiring —
	// must find the run in flight and wake it instead of no-oping.
	e.MaybeShowClaimQROnOnline(ctx)

	// The woken loop serves the wake floor, runs round 2, and parks again
	// with the escalated cadence.
	require.Eventually(t, func() bool { return len(scheduledBackoffs(observed)) == 2 },
		5*time.Second, 5*time.Millisecond, "wake did not preempt the stretched backoff")
	assert.Equal(t,
		[]time.Duration{autoClaimLadderFailureBackoffMin, 2 * autoClaimLadderFailureBackoffMin},
		scheduledBackoffs(observed),
		"consecutive latched rounds must escalate the stretched cadence")
	assert.Equal(t, 2, countLogs(observed, ladderStretchLog))
	// The wake must not have re-entered the gate for free: the full floor
	// (park held fake time still, so the whole autoClaimRetryMax remains) is
	// slept between the preempted park and round 2.
	require.Eventually(t, func() bool { return len(clk.recorded()) >= 3 },
		5*time.Second, 5*time.Millisecond, "expected park, floor, park sleeps")
	assert.Equal(t,
		[]time.Duration{autoClaimLadderFailureBackoffMin, autoClaimLadderWakeFloor, 2 * autoClaimLadderFailureBackoffMin},
		clk.recorded()[:3],
		"a mid-park wake must sleep the floor remainder before re-entering the gate")

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("claim flow did not exit after ctx cancel")
	}
}

// TestMaybeShowClaimQROnOnline_WakeDuringLadderDoesNotSkipPark pins the W1
// regression: a wake poked while the update LADDER is running (an online flap
// mid-download — the expected case on exactly the device whose downloads
// fail), or left over from before the first park (the factory-fresh topic
// assignment), must be drained before parking. Without the drain, buffered
// wakes preempt every stretched park instantly and latched multi-GB download
// rounds run back-to-back — the exact F-12 loop this branch exists to close.
func TestMaybeShowClaimQROnOnline_WakeDuringLadderDoesNotSkipPark(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	clk := &parkClock{now: time.Unix(1_000_000, 0)}

	// The runner pokes the in-flight-guarded entry point mid-run, exactly as
	// a WAN-confirmed online transition landing during the download would.
	var e *executor
	var observed *observer.ObservedLogs
	runner := fakeGateRunner{
		err:   errors.New("Error: Signature verification failed for image.sig"),
		onRun: func() { e.MaybeShowClaimQROnOnline(ctx) },
	}
	e, observed, _ = newLadderFailureExecutor(t, ctrl, clk, runner, &fakeGateConfig{})

	done := make(chan struct{})
	go func() {
		defer close(done)
		e.MaybeShowClaimQROnOnline(ctx)
	}()

	// Round 1 latched with a wake buffered during its ladder; the drain must
	// discard it, so the loop parks and STAYS parked.
	require.Eventually(t, func() bool { return len(scheduledBackoffs(observed)) == 1 },
		5*time.Second, 5*time.Millisecond, "loop never reached its first stretched backoff")
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 1, countLogs(observed, ladderStretchLog),
		"a wake buffered during the ladder must not preempt the park: rounds ran back-to-back")
	assert.Equal(t, []time.Duration{autoClaimLadderFailureBackoffMin}, scheduledBackoffs(observed))

	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("claim flow did not exit after ctx cancel")
	}
}

// TestMaybeShowClaimQROnOnline_NoTopicNoInternetNarrates pins the §4.6 fix for
// the unclaimed wired no-WAN black screen: with the internet probe wired and
// reporting offline, the topic-wait expiry paints the connecting explanation
// (conditionally, over its own finalizing overlay) instead of the silent hide
// — the device is reachable and claimable over the LAN, and the panel must
// say so rather than going dark.
func TestMaybeShowClaimQROnOnline_NoTopicNoInternetNarrates(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).Return([]byte("FF1-TEST\n"), nil).AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}, os: mockOS}
	e.SetInternetProbe(func(context.Context) (bool, error) { return false, nil })

	e.MaybeShowClaimQROnOnline(context.Background())

	assert.Equal(t, []string{"finalizing", "connecting"}, spy.calls,
		"a confirmed-offline topic-wait expiry must narrate, not hide")
	assert.Contains(t, spy.lastURL, "no internet access",
		"the narration must carry the no-WAN explanation")
	assert.Contains(t, spy.lastURL, "FF1-TEST",
		"the narration must carry the device identity")
}

// TestMaybeShowClaimQROnOnline_NoTopicOnlineKeepsSilentHide: with the probe
// reporting ONLINE, asserting "no internet access" would smear a healthy
// network (the topic is merely late) — today's silent hide stays.
func TestMaybeShowClaimQROnOnline_NoTopicOnlineKeepsSilentHide(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}}
	e.SetInternetProbe(func(context.Context) (bool, error) { return true, nil })

	e.MaybeShowClaimQROnOnline(context.Background())

	assert.Equal(t, []string{"finalizing", "hide"}, spy.calls)
}

// TestMaybeShowClaimQROnOnline_NoTopicProbeErrorKeepsSilentHide pins the
// probe's tri-state contract: a monitord restart or D-Bus timeout at
// topic-wait expiry proves nothing, and painting "no internet access" off it
// would smear a healthy network — only a REAL offline verdict narrates.
func TestMaybeShowClaimQROnOnline_NoTopicProbeErrorKeepsSilentHide(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()
	sm.EXPECT().ClaimSnapshot().Return(state.ClaimInfo{}).AnyTimes()

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}}
	e.SetInternetProbe(func(context.Context) (bool, error) {
		return false, errors.New("monitord unavailable")
	})

	e.MaybeShowClaimQROnOnline(context.Background())

	assert.Equal(t, []string{"finalizing", "hide"}, spy.calls,
		"an unknown verdict must keep the silent hide, never the no-WAN narration")
}

// TestStartWifiSetupCommand pins the command handler's reply mapping
// (docs/app-triggered-wifi-setup.md §4.1): acceptance replies {ok, ssid} with
// the FF1-prefixed device id BEFORE any radio work (the starter only queues);
// rejections are NORMAL replies carrying the wire code the app branches on;
// an unwired starter rejects as unavailable.
func TestStartWifiSetupCommand(t *testing.T) {
	newExec := func(t *testing.T, starter func(context.Context) error) *executor {
		t.Helper()
		ctrl := gomock.NewController(t)
		t.Cleanup(ctrl.Finish)
		mockOS := mocks.NewMockOS(ctrl)
		mockOS.EXPECT().ReadFile(gomock.Any()).Return([]byte("FF1-TEST\n"), nil).AnyTimes()
		e := &executor{logger: zap.NewNop(), os: mockOS}
		if starter != nil {
			e.SetWifiSetupStarter(starter)
		}
		return e
	}

	t.Run("acceptance replies ok with the AP SSID", func(t *testing.T) {
		e := newExec(t, func(context.Context) error { return nil })
		res, err := e.startWifiSetup(context.Background())
		require.NoError(t, err)
		assert.Equal(t, map[string]any{"ok": true, "ssid": "FF1-TEST"}, res)
	})

	t.Run("wired rejection carries wired_link_active", func(t *testing.T) {
		e := newExec(t, func(context.Context) error { return provisioning.ErrWiredLinkActive })
		res, err := e.startWifiSetup(context.Background())
		require.NoError(t, err, "a rejection is a reply, not a transport error")
		m := res.(map[string]any)
		assert.Equal(t, false, m["ok"])
		assert.Equal(t, "wired_link_active", m["code"])
	})

	t.Run("busy rejection carries busy", func(t *testing.T) {
		e := newExec(t, func(context.Context) error { return provisioning.ErrSetupBusy })
		res, err := e.startWifiSetup(context.Background())
		require.NoError(t, err)
		m := res.(map[string]any)
		assert.Equal(t, false, m["ok"])
		assert.Equal(t, "busy", m["code"])
	})

	t.Run("unwired starter rejects as unavailable", func(t *testing.T) {
		e := newExec(t, nil)
		res, err := e.startWifiSetup(context.Background())
		require.NoError(t, err)
		m := res.(map[string]any)
		assert.Equal(t, false, m["ok"])
		assert.Equal(t, "unavailable", m["code"])
	})
}
