package devicectl

import (
	"context"
	"errors"
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
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/otagate"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
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
	e := &executor{logger: zap.NewNop(), exec: mockExec, setupNarrator: spy}

	res, err := e.factoryResetInProcess(context.Background())
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	// The controld-owned reset pushes the on-screen confirmation.
	assert.Equal(t, []string{"factory_reset"}, spy.calls)
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
	assert.Equal(t, "ladder exhausted", spy.lastURL)
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
