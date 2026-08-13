package devicectl

// Pins the Sentry posture on the automatic netlog self-upload path
// (docs/wan-outage-observability.md): every logger.Error becomes a Sentry
// event, and the self-upload fires on freshly-healed — often still
// restricted — networks where a failed transfer is routine. The
// controller-initiated command keeps Error (a support engineer explicitly
// asked and the fire-and-forget reply makes Sentry the only failure signal).

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// failingUploader always fails the transfer, exercising the detached worker's
// completion-failure log path.
type failingUploader struct{}

func (failingUploader) Upload(context.Context, string, string, logUploadBuildInfo, string) error {
	return errors.New("presign refused: 403")
}

// uploadLevelExecutor builds a real executor over an observed logger and a
// failing uploader (same harness shape as the LAN-recovery upload test).
func uploadLevelExecutor(t *testing.T) (*executor, *observer.ObservedLogs) {
	t.Helper()
	ctrl := gomock.NewController(t)
	core, observed := observer.New(zap.WarnLevel)
	logger := zap.New(core)

	mockOS := mocks.NewMockOS(ctrl)
	mockOS.EXPECT().ReadFile(gomock.Any()).Return(nil, os.ErrNotExist).AnyTimes()
	mockOS.EXPECT().IsNotExist(gomock.Any()).Return(true).AnyTimes()
	mockExec := mocks.NewMockExec(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	ex := New(
		mocks.NewMockCDP(ctrl), mocks.NewMockDeviceStatus(ctrl), mocks.NewMockStatusPoller(ctrl),
		ddc.New(mockExec, mockClock, logger),
		wrapper.NewJSON(), mockOS, mockExec, mocks.NewMockMath(ctrl), mockClock, logger,
	).(*executor)
	ex.logUploaderFactory = func() logUploaderIface { return failingUploader{} }
	return ex, observed
}

// waitUploadSettled waits for the detached worker to release the single-flight
// latch (the failure log is written before the deferred release).
func waitUploadSettled(t *testing.T, ex *executor) {
	t.Helper()
	require.Eventually(t, func() bool { return !ex.logUploadInFlight.Load() },
		3*time.Second, 5*time.Millisecond, "detached upload worker never settled")
}

func TestSelfUploadLogs_FailureLogsWarnNotError(t *testing.T) {
	ex, observed := uploadLevelExecutor(t)

	ex.SelfUploadLogs("provisioned-key")
	waitUploadSettled(t, ex)

	assert.Zero(t, observed.FilterLevelExact(zap.ErrorLevel).Len(),
		"an automatic self-upload failure must not produce a Sentry-bound Error")
	assert.NotZero(t, observed.FilterMessage("Netlog self-upload failed").Len(),
		"the failure must still be visible at Warn")
}

func TestUploadLogsCommand_FailureKeepsErrorLevel(t *testing.T) {
	ex, observed := uploadLevelExecutor(t)

	_, err := ex.uploadLogsInProcess(context.Background(), "controller-key", "", false)
	require.NoError(t, err) // fire-and-forget ACK; the failure is async
	waitUploadSettled(t, ex)

	assert.NotZero(t, observed.FilterMessage("In-process log upload failed").Len(),
		"controller-initiated failures keep their Error-level Sentry signal")
}
