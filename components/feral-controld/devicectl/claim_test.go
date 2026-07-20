package devicectl

import (
	"context"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

// narratorSpy records setup-narration calls in order so ordering invariants can
// be asserted.
type narratorSpy struct {
	calls        []string
	lastURL      string
	lastProgress int
}

func (s *narratorSpy) ShowClaimQR(url string) { s.calls = append(s.calls, "claim"); s.lastURL = url }
func (s *narratorSpy) ShowReady()             { s.calls = append(s.calls, "ready") }
func (s *narratorSpy) ShowFactoryReset()      { s.calls = append(s.calls, "factory_reset") }
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
