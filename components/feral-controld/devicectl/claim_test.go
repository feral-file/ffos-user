package devicectl

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
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

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, clock: &autoClaimClock{}}

	// Cloud confirms pairing.
	res, err := e.showPairingQRCodeInProcess(context.Background(), false)
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	require.Equal(t, []string{"ready", "hide"}, spy.calls)

	// A later online transition must treat the device as settled: only the
	// one-time boot sweep runs, no finalizing, no gate, no claim QR.
	e.MaybeShowClaimQROnOnline(context.Background())
	assert.Equal(t, []string{"ready", "hide", "hide"}, spy.calls)
	assert.NotContains(t, spy.calls, "claim")
	assert.NotContains(t, spy.calls, "finalizing")
}

// TestConnectClaimTransitionHidesOverlay: the claim landing is the moment the
// claim QR's job ends — connect() must clear the overlay itself rather than
// depending on the separate cloud confirmation command; and a RE-connect to an
// already-claimed device must NOT hide (it could wipe an unrelated live
// narration such as updating).
func TestConnectClaimTransitionHidesOverlay(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	st := &state.State{}
	sm.EXPECT().GetState().Return(st).AnyTimes()
	sm.EXPECT().Save(gomock.Any()).Return(nil).Times(2)

	spy := &narratorSpy{}
	e := &executor{logger: zap.NewNop(), setupNarrator: spy, json: wrapper.NewJSON()}

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

// TestMaybeShowClaimQROnOnline_NoTopicWithholds: without a relayer topic the
// claim QR would send the app to an unroutable device; the flow must exhaust
// its bounded wait and withhold, leaving the next online transition to retry.
func TestMaybeShowClaimQROnOnline_NoTopicWithholds(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	sm.EXPECT().GetState().Return(&state.State{}).AnyTimes()

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
