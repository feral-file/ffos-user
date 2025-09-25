package status_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// stubDeviceStatus implements status.DeviceStatus for tests
type stubDeviceStatus struct{}

func (stubDeviceStatus) GetStatus(ctx context.Context) (*status.DeviceStatusResponse, error) {
	return &status.DeviceStatusResponse{}, nil
}

type testSetup struct {
	ctrl        *gomock.Controller
	ctx         context.Context
	mockCDP     *mocks.MockCDP
	mockRelayer *mocks.MockRelayer
	poller      status.Poller
	logger      *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockRelayer := mocks.NewMockRelayer(ctrl)
	// Use a simple stub for DeviceStatus to avoid package type mismatch issues
	poller := status.NewPoller(mockCDP, mockRelayer, stubDeviceStatus{}, logger)

	return &testSetup{
		ctrl:        ctrl,
		ctx:         ctx,
		mockCDP:     mockCDP,
		mockRelayer: mockRelayer,
		poller:      poller,
		logger:      logger,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

func TestPoller_StartStop(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock IsConnected calls during polling
	ts.mockRelayer.EXPECT().
		IsConnected().
		Return(false).
		AnyTimes()

	// Device status is provided by stubDeviceStatus

	// Test Start - should not panic
	go func() {
		ts.poller.Start(ts.ctx)
	}()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Test Stop - should not panic
	ts.poller.Stop()

	// Test passes if no panics occur
}

func TestPoller_ForceRefresh(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test ForceRefresh - should not panic
	ts.poller.ForceRefresh()

	// Test passes if no panics occur
}

func TestFetchPlayerStatus_Success(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockCDP := mocks.NewMockCDP(ctrl)

	// Mock CDP response
	expectedResult := map[string]interface{}{
		"message": map[string]interface{}{
			"castCommand": "displayPlaylist",
			"playlistURL": "https://example.com/playlist.json",
		},
	}

	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(expectedResult, nil).
		Times(1)

	// Test
	result, err := status.FetchPlayerStatus(ctx, mockCDP, logger)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "displayPlaylist", result["castCommand"])
	assert.Equal(t, "https://example.com/playlist.json", result["playlistURL"])
}

func TestFetchPlayerStatus_CDPError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockCDP := mocks.NewMockCDP(ctrl)

	// Mock CDP error
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("CDP connection failed")).
		Times(1)

	// Test
	result, err := status.FetchPlayerStatus(ctx, mockCDP, logger)

	// Verify
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "CDP connection failed")
}

func TestFetchPlayerStatus_UnexpectedResultType(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockCDP := mocks.NewMockCDP(ctrl)

	// Mock CDP to return a non-map type
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("ok", nil).
		Times(1)

	result, err := status.FetchPlayerStatus(ctx, mockCDP, logger)

	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestFetchPlayerStatus_MissingMessage(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockCDP := mocks.NewMockCDP(ctrl)

	// Return map without 'message'
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{}, nil).
		Times(1)

	result, err := status.FetchPlayerStatus(ctx, mockCDP, logger)

	assert.Error(t, err)
	assert.Nil(t, result)
}

func TestFetchPlayerStatus_InvalidMessageType(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockCDP := mocks.NewMockCDP(ctrl)

	// 'message' exists but wrong type
	mockCDP.EXPECT().
		NoLogSend(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"message": "oops"}, nil).
		Times(1)

	result, err := status.FetchPlayerStatus(ctx, mockCDP, logger)

	assert.Error(t, err)
	assert.Nil(t, result)
}
