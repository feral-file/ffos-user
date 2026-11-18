package devicectl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type testSetup struct {
	ctrl             *gomock.Controller
	ctx              context.Context
	executor         devicectl.Executor
	mockCDP          *mocks.MockCDP
	mockStatus       *mocks.MockStatusPoller
	mockJSON         *mocks.MockJSON
	mockClock        *mocks.MockClock
	mockOS           *mocks.MockOS
	mockExec         *mocks.MockExec
	mockExecCmd      *mocks.MockExecCmd
	mockDeviceStatus *mocks.MockDeviceStatus
	mockMath         *mocks.MockMath
	mockStateManager *mocks.MockStateManager
	logger           *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Create mocks
	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatus := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockExecCmd := mocks.NewMockExecCmd(ctrl)
	mockDeviceStatus := mocks.NewMockDeviceStatus(ctrl)
	mockMath := mocks.NewMockMath(ctrl)
	mockStateManager := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(mockStateManager)

	// Use a real panelDDC backed by mockExec so that DDC executor tests can
	// keep mocking ddcutil subprocess calls through mockExec.CommandContext.
	// The availability tracker only consults the clock on demotion/reprobe,
	// which no single-test scenario reaches, so the bare MockClock is safe.
	panelDDC := ddc.New(mockExec, mockClock, logger)

	// Create executor with mocks
	executor := devicectl.New(
		mockCDP,
		mockDeviceStatus,
		mockStatus,
		panelDDC,
		mockJSON,
		mockOS,
		mockExec,
		mockMath,
		mockClock,
		logger,
	)

	return &testSetup{
		ctrl:             ctrl,
		ctx:              ctx,
		executor:         executor,
		mockCDP:          mockCDP,
		mockStatus:       mockStatus,
		mockJSON:         mockJSON,
		mockClock:        mockClock,
		mockOS:           mockOS,
		mockExec:         mockExec,
		mockExecCmd:      mockExecCmd,
		mockDeviceStatus: mockDeviceStatus,
		mockMath:         mockMath,
		mockStateManager: mockStateManager,
		logger:           logger,
	}
}

func (ts *testSetup) teardown() {
	state.ResetForTesting()
	ts.ctrl.Finish()
}

func TestExecutor_Execute_InvalidCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type: "invalid_command",
		Arguments: map[string]interface{}{
			"test": "value",
		},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"test":"value"}`), nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid command")
}

func TestExecutor_Execute_InvalidArguments(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type: commands.CMD_CONNECT,
		Arguments: map[string]interface{}{
			"invalid": make(chan int), // This can't be marshaled to JSON
		},
	}

	// Mock JSON marshaling to fail
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return(nil, errors.New("json: unsupported type: chan int"))

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid arguments")
}

func TestExecutor_Connect_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	primaryAddress := "192.168.1.100"
	device := devicectl.Device{
		ID:       "test-device-id",
		Name:     "Test Device",
		Platform: 1,
	}

	cmd := commands.Command{
		Type: commands.CMD_CONNECT,
		Arguments: map[string]interface{}{
			"clientDevice":   device,
			"primaryAddress": primaryAddress,
		},
	}

	arguments := fmt.Sprintf(`{"clientDevice":{"device_id":"%s","device_name":"%s","platform":%d},"primaryAddress":"%s"}`, device.ID, device.Name, device.Platform, primaryAddress)

	// Mock JSON marshaling for command arguments
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)

	// Mock JSON unmarshaling for command arguments
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			// Set the struct fields
			args := v.(*struct {
				Device         devicectl.Device `json:"clientDevice"`
				PrimaryAddress string           `json:"primaryAddress"`
			})
			args.Device = device
			args.PrimaryAddress = primaryAddress
			return nil
		})

	// Mock the claim snapshot connect() reads for the wasClaimed check before
	// writing (already claimed here, matching the device below, so the claim
	// TRANSITION guard on the setup-narration hide is exercised).
	ts.mockStateManager.EXPECT().
		ClaimSnapshot().
		Return(state.ClaimInfo{DeviceID: device.ID, Claimed: true}).
		AnyTimes()

	// Mock state manager write
	ts.mockStateManager.EXPECT().
		SetConnectedDevice(gomock.Any()).
		Return(nil)

	// GetState is exercised only by this test's own post-assertion read
	// below, not by connect() itself anymore.
	ts.mockStateManager.EXPECT().
		GetState().
		Return(&state.State{
			ConnectedDevice: &state.Device{
				ID:       device.ID,
				Name:     device.Name,
				Platform: device.Platform,
			},
		}).AnyTimes()

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)

	// Verify state was saved
	savedState := state.GetState()
	assert.NotNil(t, savedState.ConnectedDevice)
	assert.Equal(t, device.ID, savedState.ConnectedDevice.ID)
	assert.Equal(t, device.Name, savedState.ConnectedDevice.Name)
	assert.Equal(t, device.Platform, savedState.ConnectedDevice.Platform)
}

func TestExecutor_Connect_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		{
			name: "JSON marshal failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling to fail
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return(nil, errors.New("json marshal failed"))
			},
			wantErr: "invalid arguments",
		},
		{
			name: "JSON unmarshal failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"invalid":"data"}`), nil)

				// Mock JSON unmarshaling to fail
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					Return(errors.New("invalid arguments"))
			},
			wantErr: "invalid arguments",
		},
		{
			name: "State save failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success for arguments
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clientDevice":{"device_id":"test-device-id","device_name":"Test Device","platform":1},"primaryAddress":"192.168.1.100"}`), nil)

				// Mock JSON unmarshaling success
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Device         devicectl.Device `json:"clientDevice"`
							PrimaryAddress string           `json:"primaryAddress"`
						})
						args.Device = devicectl.Device{
							ID:       "test-device-id",
							Name:     "Test Device",
							Platform: 1,
						}
						args.PrimaryAddress = "192.168.1.100"
						return nil
					})

				// Mock the claim snapshot connect() reads for the wasClaimed
				// check before writing.
				ts.mockStateManager.EXPECT().
					ClaimSnapshot().
					Return(state.ClaimInfo{DeviceID: "test-device-id", Claimed: true}).
					AnyTimes()

				// Mock state manager write to fail
				ts.mockStateManager.EXPECT().
					SetConnectedDevice(gomock.Any()).
					Return(errors.New("permission denied"))
			},
			wantErr: "failed to save state",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := commands.Command{
				Type: commands.CMD_CONNECT,
				Arguments: map[string]interface{}{
					"clientDevice": devicectl.Device{
						ID:       "test-device-id",
						Name:     "Test Device",
						Platform: 1,
					},
					"primaryAddress": "192.168.1.100",
				},
			}

			// Setup error condition
			tt.setupFunc(ts)

			// Execute the method under test
			result, err := ts.executor.Execute(ts.ctx, cmd)

			// Assert error occurred and contains expected message
			assert.Error(t, err, "expected error, got %v", err)
			assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())
			assert.Nil(t, result, "expected nil result on error")
		})
	}
}

func TestExecutor_DeviceStatus_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_DEVICE_STATUS,
		Arguments: map[string]interface{}{},
	}

	arguments := `{}`

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)

	// Mock status poller to return a DeviceStatusResponse
	ts.mockDeviceStatus.EXPECT().
		GetStatus(ts.ctx).
		Return(&status.DeviceStatusResponse{
			ScreenRotation:   "landscape",
			ConnectedWifi:    "test-wifi",
			InstalledVersion: "1.0.0",
			LatestVersion:    "1.0.1",
		}, nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.NotNil(t, result)

	// Verify result is a DeviceStatusResponse
	statusResponse, ok := result.(*status.DeviceStatusResponse)
	assert.True(t, ok)
	assert.NotNil(t, statusResponse)
	assert.Equal(t, "landscape", statusResponse.ScreenRotation)
	assert.Equal(t, "test-wifi", statusResponse.ConnectedWifi)
	assert.Equal(t, "1.0.0", statusResponse.InstalledVersion)
	assert.Equal(t, "1.0.1", statusResponse.LatestVersion)
}

func TestExecutor_DisplayGrantDeviceStatus(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ctx := control.WithPrincipal(ts.ctx, control.Principal{Role: control.RoleDisplay})
	cmd := commands.Command{Type: commands.CMD_DEVICE_STATUS, Arguments: map[string]any{}}
	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(`{}`), nil)
	ts.mockOS.EXPECT().ReadFile(constants.SCREEN_ORIENTATION_FILE).Return([]byte("90"), nil)

	result, err := ts.executor.Execute(ctx, cmd)
	assert.NoError(t, err)
	body, err := json.Marshal(result)
	assert.NoError(t, err)
	assert.JSONEq(t, `{"screenRotation":"portrait"}`, string(body))
}

func TestExecutor_DeviceStatus_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_DEVICE_STATUS,
		Arguments: map[string]interface{}{},
	}

	arguments := `{}`

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)

	// Mock status poller to return an error
	ts.mockDeviceStatus.EXPECT().
		GetStatus(gomock.Any()).
		Return(nil, errors.New("device status error"))

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "device status error")
}

func TestExecutor_ScreenRotation_Success(t *testing.T) {
	testCases := []struct {
		name                string
		clockwise           bool
		configFileContent   string
		configFileReadError error
		expectedNewRotation string
		expectedOrientation string
		description         string
	}{
		{
			name:                "ClockwiseFromNormal",
			clockwise:           true,
			configFileContent:   "normal",
			expectedNewRotation: "270",
			expectedOrientation: "portraitReverse",
			description:         "Clockwise rotation from normal should go to 270 degrees",
		},
		{
			name:                "CounterClockwiseFromNormal",
			clockwise:           false,
			configFileContent:   "normal",
			expectedNewRotation: "90",
			expectedOrientation: "portrait",
			description:         "Counter-clockwise rotation from normal should go to 90 degrees",
		},
		{
			name:                "ClockwiseFrom90",
			clockwise:           true,
			configFileContent:   "90",
			expectedNewRotation: "normal",
			expectedOrientation: "landscape",
			description:         "Clockwise rotation from 90 degrees should go to normal",
		},
		{
			name:                "CounterClockwiseFrom180",
			clockwise:           false,
			configFileContent:   "180",
			expectedNewRotation: "270",
			expectedOrientation: "portraitReverse",
			description:         "Counter-clockwise rotation from 180 degrees should go to 270 degrees",
		},
		{
			name:                "ClockwiseFrom270",
			clockwise:           true,
			configFileContent:   "270",
			expectedNewRotation: "180",
			expectedOrientation: "landscapeReverse",
			description:         "Clockwise rotation from 270 degrees should go to 180 degrees",
		},
		{
			name:                "NoConfigFileDefaultsToNormal",
			clockwise:           true,
			configFileReadError: errors.New("file not found"),
			expectedNewRotation: "270",
			expectedOrientation: "portraitReverse",
			description:         "When config file doesn't exist, should default to normal orientation",
		},
		{
			name:                "EmptyConfigFileDefaultsToNormal",
			clockwise:           false,
			configFileContent:   "",
			expectedNewRotation: "90",
			expectedOrientation: "portrait",
			description:         "When config file is empty, should default to normal orientation",
		},
		{
			name:                "UnknownConfigValueDefaultsToNormal",
			clockwise:           true,
			configFileContent:   "invalid_rotation",
			expectedNewRotation: "270",
			expectedOrientation: "portraitReverse",
			description:         "When config file contains unknown value, should default to normal orientation",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := commands.Command{
				Type: commands.CMD_SCREEN_ROTATION,
				Arguments: map[string]interface{}{
					"clockwise": tc.clockwise,
				},
			}

			arguments := fmt.Sprintf(`{"clockwise":%t}`, tc.clockwise)

			// Mock JSON marshaling
			ts.mockJSON.EXPECT().
				Marshal(cmd.Arguments).
				Return([]byte(arguments), nil)

			// Mock JSON unmarshaling for screen rotation arguments
			ts.mockJSON.EXPECT().
				Unmarshal([]byte(arguments), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					args := v.(*struct {
						Clockwise bool `json:"clockwise"`
					})
					args.Clockwise = tc.clockwise
					return nil
				})

			// Mock exec.CommandContext for wlr-randr query
			ts.mockExec.EXPECT().
				CommandContext(ts.ctx, "wlr-randr").
				Return(ts.mockExecCmd)

			// Mock cmd.Output() that's successful return cmd output
			ts.mockExecCmd.EXPECT().
				Output().
				Return([]byte("HDMI-A-1 \"Dell Inc. DELL S2721QS D3SNM43 (HDMI-A-1)\""), nil)

			// Mock OS ReadFile for config
			if tc.configFileReadError != nil {
				ts.mockOS.EXPECT().
					ReadFile(constants.SCREEN_ORIENTATION_FILE).
					Return(nil, tc.configFileReadError)
			} else {
				ts.mockOS.EXPECT().
					ReadFile(constants.SCREEN_ORIENTATION_FILE).
					Return([]byte(tc.configFileContent), nil)
			}

			// Mock exec.CommandContext for wlr-randr rotation command
			ts.mockExec.EXPECT().
				CommandContext(ts.ctx, "wlr-randr", "--output", "HDMI-A-1", "--transform", tc.expectedNewRotation).
				Return(ts.mockExecCmd)

			// Mock cmd.Run() that's successful
			ts.mockExecCmd.EXPECT().
				Run().
				Return(nil)

			// Mock OS WriteFile for saving new rotation
			ts.mockOS.EXPECT().
				WriteFile(constants.SCREEN_ORIENTATION_FILE, []byte(tc.expectedNewRotation), os.FileMode(0600)).
				Return(nil)

			// Mock status poller force refresh
			ts.mockStatus.EXPECT().
				ForceRefresh()

			// Execute command
			result, err := ts.executor.Execute(ts.ctx, cmd)
			assert.NoError(t, err)
			assert.NotNil(t, result)

			// Verify result contains expected orientation
			resultMap, ok := result.(map[string]string)
			assert.True(t, ok)
			assert.Equal(t, tc.expectedOrientation, resultMap["orientation"])
		})
	}
}

func TestExecutor_ScreenRotation_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		{
			name: "JSON marshal failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling to fail
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return(nil, errors.New("json marshal failed"))
			},
			wantErr: "invalid arguments",
		},
		{
			name: "JSON unmarshal failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)

				// Mock JSON unmarshaling to fail
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					Return(errors.New("json unmarshal failed"))
			},
			wantErr: "invalid arguments",
		},
		{
			name: "wlr-randr output command failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to fail
				ts.mockExecCmd.EXPECT().
					Output().
					Return(nil, errors.New("wlr-randr command failed"))
			},
			wantErr: "failed to get display info",
		},
		{
			name: "wlr-randr empty output",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return empty output
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte(""), nil)
			},
			wantErr: "could not find active output",
		},
		{
			name: "wlr-randr output with only newlines",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return only newlines
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("\n\n"), nil)
			},
			wantErr: "could not find active output",
		},
		{
			name: "output line with insufficient parts",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return "Output" without name
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("Output"), nil)
			},
			wantErr: "could not find active output",
		},
		{
			name: "output line with only spaces after keyword",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return "Output" with only spaces
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("Output    "), nil)
			},
			wantErr: "could not find active output",
		},
		{
			name: "first line with only whitespace",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return only whitespace
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("   "), nil)
			},
			wantErr: "could not find active output",
		},
		{
			name: "output with extra spaces due to strings.Split limitation",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return output with extra spaces (strings.Split creates empty strings)
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("Output    HDMI-A-1    \"Dell Inc. DELL S2721QS D3SNM43 (HDMI-A-1)\""), nil)
			},
			wantErr: "could not find active output",
		},
		{
			name: "wlr-randr rotate command failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return valid output
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("HDMI-A-1 \"Dell Inc. DELL S2721QS D3SNM43 (HDMI-A-1)\""), nil)

				// Mock OS ReadFile for config (normal rotation)
				ts.mockOS.EXPECT().
					ReadFile(constants.SCREEN_ORIENTATION_FILE).
					Return([]byte("normal"), nil)

				// Mock exec.CommandContext for wlr-randr rotation command (clockwise from normal = 270)
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr", "--output", "HDMI-A-1", "--transform", "270").
					Return(ts.mockExecCmd)

				// Mock cmd.Run() to fail
				ts.mockExecCmd.EXPECT().
					Run().
					Return(errors.New("wlr-randr rotate command failed"))
			},
			wantErr: "failed to rotate screen",
		},
		{
			name: "config file write failure (should succeed)",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return valid output
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("HDMI-A-1 \"Dell Inc. DELL S2721QS D3SNM43 (HDMI-A-1)\""), nil)

				// Mock OS ReadFile for config
				ts.mockOS.EXPECT().
					ReadFile(constants.SCREEN_ORIENTATION_FILE).
					Return([]byte("normal"), nil)

				// Mock exec.CommandContext for wlr-randr rotation command
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr", "--output", "HDMI-A-1", "--transform", "270").
					Return(ts.mockExecCmd)

				// Mock cmd.Run() success
				ts.mockExecCmd.EXPECT().
					Run().
					Return(nil)

				// Mock OS WriteFile to fail (but command should still succeed)
				ts.mockOS.EXPECT().
					WriteFile(constants.SCREEN_ORIENTATION_FILE, []byte("270"), os.FileMode(0600)).
					Return(errors.New("permission denied"))

				// Mock status poller force refresh (reached despite write failure)
				ts.mockStatus.EXPECT().
					ForceRefresh()
			},
			wantErr: "", // Should succeed despite write failure
		},
		{
			name: "invalid config file content (should succeed with defaults)",
			setupFunc: func(ts *testSetup) {
				// Mock JSON operations success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"clockwise":true}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Clockwise bool `json:"clockwise"`
						})
						args.Clockwise = true
						return nil
					})

				// Mock exec.CommandContext for wlr-randr query
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr").
					Return(ts.mockExecCmd)

				// Mock cmd.Output() to return valid output
				ts.mockExecCmd.EXPECT().
					Output().
					Return([]byte("HDMI-A-1 \"Dell Inc. DELL S2721QS D3SNM43 (HDMI-A-1)\""), nil)

				// Mock OS ReadFile for config with invalid content
				ts.mockOS.EXPECT().
					ReadFile(constants.SCREEN_ORIENTATION_FILE).
					Return([]byte("invalid_rotation_value"), nil)

				// Mock exec.CommandContext for wlr-randr rotation command (defaults to normal, clockwise = 270)
				ts.mockExec.EXPECT().
					CommandContext(ts.ctx, "wlr-randr", "--output", "HDMI-A-1", "--transform", "270").
					Return(ts.mockExecCmd)

				// Mock cmd.Run() success
				ts.mockExecCmd.EXPECT().
					Run().
					Return(nil)

				// Mock OS WriteFile success
				ts.mockOS.EXPECT().
					WriteFile(constants.SCREEN_ORIENTATION_FILE, []byte("270"), os.FileMode(0600)).
					Return(nil)

				// Mock status poller force refresh
				ts.mockStatus.EXPECT().
					ForceRefresh()
			},
			wantErr: "", // Should succeed
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := commands.Command{
				Type: commands.CMD_SCREEN_ROTATION,
				Arguments: map[string]interface{}{
					"clockwise": true, // Default value, overridden in setupFunc if needed
				},
			}

			// Setup error condition
			tt.setupFunc(ts)

			// Execute the method under test
			result, err := ts.executor.Execute(ts.ctx, cmd)

			// Assert error occurred and contains expected message or success
			if tt.wantErr != "" {
				assert.Error(t, err, "expected error, got %v", err)
				assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())
				assert.Nil(t, result, "expected nil result on error")
			} else {
				assert.NoError(t, err, "expected no error, got %v", err)
				assert.NotNil(t, result, "expected non-nil result on success")
			}
		})
	}
}

func TestExecutor_AnalyticsToggle_Disable_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_ANALYTICS_TOGGLE,
		Arguments: map[string]interface{}{"enabled": false},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"enabled":false}`), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"enabled":false}`), gomock.Any()).
		Return(nil)

	configDir := filepath.Dir(devicectl.AnalyticsToggleOffFile)
	ts.mockOS.EXPECT().
		MkdirAll(configDir, os.FileMode(0755)).
		Return(nil)

	ts.mockOS.EXPECT().
		WriteFile(devicectl.AnalyticsToggleOffFile, gomock.Any(), os.FileMode(0644)).
		DoAndReturn(func(path string, data []byte, perm os.FileMode) error {
			assert.Contains(t, string(data), "disabled")
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_AnalyticsToggle_Enable_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_ANALYTICS_TOGGLE,
		Arguments: map[string]interface{}{"enabled": true},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"enabled":true}`), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"enabled":true}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			reflect.ValueOf(v).Elem().FieldByName("Enabled").SetBool(true)
			return nil
		})

	configDir := filepath.Dir(devicectl.AnalyticsToggleOffFile)
	ts.mockOS.EXPECT().
		MkdirAll(configDir, os.FileMode(0755)).
		Return(nil)

	ts.mockOS.EXPECT().
		IsNotExist(gomock.Any()).
		Return(true)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_BetaFeaturesToggle_Enable_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_BETA_FEATURES_TOGGLE,
		Arguments: map[string]interface{}{"enabled": true},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"enabled":true}`), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"enabled":true}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			reflect.ValueOf(v).Elem().FieldByName("Enabled").SetBool(true)
			return nil
		})

	configDir := filepath.Dir(devicectl.BetaFeaturesToggleOnFile)
	ts.mockOS.EXPECT().
		MkdirAll(configDir, os.FileMode(0755)).
		Return(nil)

	ts.mockOS.EXPECT().
		WriteFile(devicectl.BetaFeaturesToggleOnFile, gomock.Any(), os.FileMode(0644)).
		DoAndReturn(func(path string, data []byte, perm os.FileMode) error {
			assert.Contains(t, string(data), "enabled")
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_BetaFeaturesToggle_Disable_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_BETA_FEATURES_TOGGLE,
		Arguments: map[string]interface{}{"enabled": false},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"enabled":false}`), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"enabled":false}`), gomock.Any()).
		Return(nil)

	configDir := filepath.Dir(devicectl.BetaFeaturesToggleOnFile)
	ts.mockOS.EXPECT().
		MkdirAll(configDir, os.FileMode(0755)).
		Return(nil)

	ts.mockOS.EXPECT().
		IsNotExist(gomock.Any()).
		Return(true)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_SshAccess_Enable_RequiresPublicKey(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_SSH_ACCESS,
		Arguments: map[string]interface{}{
			"enabled":   true,
			"publicKey": " ",
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"enabled":true,"publicKey":" "}`), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"enabled":true,"publicKey":" "}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Enabled    bool   `json:"enabled"`
				PublicKey  string `json:"publicKey"`
				TTLSeconds *int   `json:"ttlSeconds"`
			})
			args.Enabled = true
			args.PublicKey = " "
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "publicKey is required")
}

func TestExecutor_SshAccess_Enable_CapsTtlAndSchedulesDisable(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_SSH_ACCESS,
		Arguments: map[string]interface{}{
			"enabled":    true,
			"publicKey":  "ssh-ed25519 AAAA-test",
			"ttlSeconds": 90000,
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"enabled":true,"publicKey":"ssh-ed25519 AAAA-test","ttlSeconds":90000}`), nil)
	ts.mockJSON.EXPECT().
		Unmarshal(
			[]byte(`{"enabled":true,"publicKey":"ssh-ed25519 AAAA-test","ttlSeconds":90000}`),
			gomock.Any(),
		).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Enabled    bool   `json:"enabled"`
				PublicKey  string `json:"publicKey"`
				TTLSeconds *int   `json:"ttlSeconds"`
			})
			ttl := 90000
			args.Enabled = true
			args.PublicKey = "ssh-ed25519 AAAA-test"
			args.TTLSeconds = &ttl
			return nil
		})

	sshDir := filepath.Dir(constants.SSH_AUTHORIZED_KEYS_FILE)
	ts.mockOS.EXPECT().
		MkdirAll(sshDir, os.FileMode(0700)).
		Return(nil)
	ts.mockOS.EXPECT().
		WriteFile(constants.SSH_AUTHORIZED_KEYS_FILE, gomock.Any(), os.FileMode(0600)).
		DoAndReturn(func(_ string, data []byte, _ os.FileMode) error {
			assert.Contains(t, string(data), "ssh-ed25519 AAAA-test")
			return nil
		})

	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".timer").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "reset-failed", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "start", "sshd.service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(
				gomock.Any(),
				"sudo",
				"systemd-run",
				"--unit",
				constants.SSH_DISABLE_UNIT,
				"--on-active",
				"86400s",
				"/bin/bash",
				"-c",
				"pkill -u feralfile sshd || true; systemctl stop sshd.service",
			).
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)

	response, ok := result.(map[string]interface{})
	if assert.True(t, ok) {
		assert.Equal(t, true, response["enabled"])
		assert.Equal(t, 86400, response["ttlSeconds"])
	}
}

func TestExecutor_SshAccess_Disable_StopsService(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_SSH_ACCESS,
		Arguments: map[string]interface{}{
			"enabled": false,
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"enabled":false}`), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"enabled":false}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Enabled    bool   `json:"enabled"`
				PublicKey  string `json:"publicKey"`
				TTLSeconds *int   `json:"ttlSeconds"`
			})
			args.Enabled = false
			return nil
		})

	ts.mockOS.EXPECT().
		IsNotExist(gomock.Any()).
		Return(true).
		AnyTimes()

	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".timer").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "reset-failed", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(gomock.Any(), "sudo", "systemctl", "stop", "sshd.service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)

	response, ok := result.(map[string]interface{})
	if assert.True(t, ok) {
		assert.Equal(t, false, response["enabled"])
	}
}

func TestExecutor_Shutdown_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_SHUTDOWN,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock exec.CommandContext for shutdown
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "sudo", "shutdown", "-h", "now").
		Return(ts.mockExecCmd)

	// Mock cmd.Run() to succeed
	ts.mockExecCmd.EXPECT().
		Run().
		Return(nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_Shutdown_CommandError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_SHUTDOWN,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock exec.CommandContext for shutdown to fail
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "sudo", "shutdown", "-h", "now").
		Return(ts.mockExecCmd)

	// Mock cmd.Run() to fail
	ts.mockExecCmd.EXPECT().
		Run().
		Return(errors.New("shutdown command failed"))

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to execute shutdown command")
}

func TestExecutor_Reboot_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_REBOOT,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock exec.CommandContext for reboot
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "sudo", "reboot", "-h", "now").
		Return(ts.mockExecCmd)

	// Mock cmd.Run() to succeed
	ts.mockExecCmd.EXPECT().
		Run().
		Return(nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_Reboot_CommandError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_REBOOT,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock exec.CommandContext for reboot to fail
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "sudo", "reboot", "-h", "now").
		Return(ts.mockExecCmd)

	// Mock Run() to return an error
	ts.mockExecCmd.EXPECT().
		Run().
		Return(fmt.Errorf("command failed"))

	// Execute command and expect error
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to execute reboot command")
	assert.Nil(t, result)
}

func TestExecutor_GetSysMetrics_Success(t *testing.T) {
	tests := []struct {
		name        string
		setupFunc   func(*testSetup)
		wantResult  map[string]interface{}
		description string
	}{
		{
			name: "With saved metrics",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling for command arguments
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{}`), nil)

				// Set last sys metrics
				testMetrics := []byte(`{"cpu": 50.0, "memory": 75.0}`)
				ts.executor.SaveLastSysMetrics(testMetrics)

				// Mock JSON unmarshaling for saved metrics
				ts.mockJSON.EXPECT().
					Unmarshal(testMetrics, gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						result := v.(*map[string]interface{})
						*result = map[string]interface{}{
							"cpu":    50.0,
							"memory": 75.0,
						}
						return nil
					})
			},
			wantResult: map[string]interface{}{
				"cpu":    50.0,
				"memory": 75.0,
			},
			description: "Should return saved metrics when available",
		},
		{
			name: "With no saved metrics (empty state)",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling for command arguments
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{}`), nil)

				// Don't save any metrics (handler starts with nil lastSysMetrics)
				// No JSON unmarshaling expected since lastSysMetrics is nil
			},
			wantResult:  nil, // Should return nil when no metrics are saved
			description: "Should return nil when no metrics have been saved",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := commands.Command{
				Type:      commands.CMD_PROFILE,
				Arguments: map[string]interface{}{},
			}

			// Setup test conditions
			tt.setupFunc(ts)

			// Execute command
			result, err := ts.executor.Execute(ts.ctx, cmd)
			assert.NoError(t, err, "expected no error for %s", tt.description)

			// Verify result
			if tt.wantResult == nil {
				assert.Nil(t, result, "expected nil result for %s", tt.description)
			} else {
				assert.NotNil(t, result, "expected non-nil result for %s", tt.description)
				resultMap, ok := result.(map[string]interface{})
				assert.True(t, ok, "expected result to be map[string]interface{} for %s", tt.description)
				assert.Equal(t, tt.wantResult, resultMap, "result mismatch for %s", tt.description)
			}
		})
	}
}

func TestExecutor_GetSysMetrics_Failure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_PROFILE,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling for command arguments
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(`{}`), nil)

	// Set invalid JSON metrics
	invalidMetrics := []byte(`{"cpu": invalid_json}`)
	ts.executor.SaveLastSysMetrics(invalidMetrics)

	// Mock JSON unmarshaling to fail
	ts.mockJSON.EXPECT().
		Unmarshal(invalidMetrics, gomock.Any()).
		Return(errors.New("json unmarshal failed"))

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err, "expected error when JSON unmarshal fails")
	assert.Contains(t, err.Error(), "failed to unmarshal last sys metrics", "error should mention unmarshal failure")
	assert.Nil(t, result, "expected nil result on error")
}

func TestExecutor_SysMetrics_ConcurrentAccess(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Use channels to enforce ordering: read -> save -> read
	readFirst := make(chan bool, 1)
	saveComplete := make(chan bool, 1)
	readSecond := make(chan bool, 1)

	var firstResult, secondResult interface{}
	var firstErr, secondErr error

	// First goroutine: Read (should get nil/empty)
	go func() {
		// Mock JSON marshaling for first read
		ts.mockJSON.EXPECT().
			Marshal(gomock.Any()).
			Return([]byte(`{}`), nil)

		cmd := commands.Command{
			Type:      commands.CMD_PROFILE,
			Arguments: map[string]interface{}{},
		}

		firstResult, firstErr = ts.executor.Execute(ts.ctx, cmd)
		readFirst <- true

		// Wait for save to complete before continuing
		<-saveComplete
	}()

	// Second goroutine: Save
	go func() {
		// Wait for first read to complete
		<-readFirst

		// Save metrics
		testMetrics := []byte(`{"cpu": 85.5, "memory": 60.2, "disk": 45.0}`)
		ts.executor.SaveLastSysMetrics(testMetrics)

		// Closed, not sent: saveComplete has TWO receivers — the first
		// goroutine's tail wait and the third goroutine's head wait — so
		// a single token is claimed by whichever gets there first and the
		// other blocks forever. When the first goroutine won that race
		// the third never ran, nothing ever sent readSecond, and the test
		// hung until the 10-minute panic (seen on CI, ~10% of runs in a
		// standalone reproduction of this channel topology). Closing
		// releases both, which is what "save is done" was always meant
		// to mean.
		close(saveComplete)
	}()

	// Third goroutine: Read (should get saved data)
	go func() {
		// Wait for save to complete
		<-saveComplete

		// Mock JSON marshaling for second read
		ts.mockJSON.EXPECT().
			Marshal(gomock.Any()).
			Return([]byte(`{}`), nil)

		// Mock JSON unmarshaling for the saved metrics
		testMetrics := []byte(`{"cpu": 85.5, "memory": 60.2, "disk": 45.0}`)
		ts.mockJSON.EXPECT().
			Unmarshal(testMetrics, gomock.Any()).
			DoAndReturn(func(data []byte, v interface{}) error {
				result := v.(*map[string]interface{})
				*result = map[string]interface{}{
					"cpu":    85.5,
					"memory": 60.2,
					"disk":   45.0,
				}
				return nil
			})

		cmd := commands.Command{
			Type:      commands.CMD_PROFILE,
			Arguments: map[string]interface{}{},
		}

		secondResult, secondErr = ts.executor.Execute(ts.ctx, cmd)
		readSecond <- true
	}()

	// Wait for all goroutines to complete
	<-readSecond

	// Verify first read (before save) - should be nil/empty
	assert.NoError(t, firstErr, "first read should not error")
	assert.Nil(t, firstResult, "first read should return nil when no metrics saved")

	// Verify second read (after save) - should have saved data
	assert.NoError(t, secondErr, "second read should not error")
	assert.NotNil(t, secondResult, "second read should return saved metrics")

	resultMap, ok := secondResult.(map[string]interface{})
	assert.True(t, ok, "second read result should be map[string]interface{}")
	assert.Equal(t, 85.5, resultMap["cpu"], "cpu metric should match saved value")
	assert.Equal(t, 60.2, resultMap["memory"], "memory metric should match saved value")
	assert.Equal(t, 45.0, resultMap["disk"], "disk metric should match saved value")
}

func TestExecutor_FactoryReset_StartsServiceAndRotatesTopic(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_FACTORY_RESET,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Topic is cleared before the reset proceeds (security property). The
	// clearing semantics themselves (TopicID AND ConnectedDevice both zeroed)
	// are pinned by state_test.go's TestStateManager_ClearClaim_* — this test
	// only needs to verify factoryReset calls through to ClearClaim.
	ts.mockStateManager.EXPECT().
		InvalidateRelayerTopic().
		Return("topic-1", true, nil)
	ts.mockStateManager.EXPECT().
		ClearClaim().
		Return(true, nil)

	// The reset clears the owner's device name with the claim.
	// Clear removes the staged temp first (resold-frame leak guard).
	ts.mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE + ".tmp").Return(nil)
	ts.mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE).Return(nil)

	// The in-process reset starts the system reset unit directly.
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "systemctl", "start", "set-factory-boot.service").
		Return(ts.mockExecCmd)
	ts.mockExecCmd.EXPECT().
		CombinedOutput().
		Return([]byte(""), nil)
	// A successful start arms the stuck-reset watchdog on its own goroutine.
	// A non-nil sleep error is the "process is going away" shape, so it returns
	// without firing — this test is about the reset boundary, not the watchdog
	// (TestFactoryReset_StuckResetWatchdogReleasesLatch covers that).
	ts.mockClock.EXPECT().
		SleepContext(gomock.Any(), gomock.Any()).
		Return(context.Canceled).
		AnyTimes()

	var observed []bool
	ts.executor.SetClaimObserver(func(claimed bool) { observed = append(observed, claimed) })

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	// The executor RETURNS CmdOK with the relayer session left intact (no
	// relayer participates here — delivery is the mediator's Send). The former
	// shape closed the socket first, so this ack could never reach the
	// controller; see factoryReset.
	assert.Equal(t, devicectl.CmdOK, result)
	assert.Equal(t, []bool{false}, observed,
		"the claim observer must see the unclaim so mDNS re-advertises claimed=false")
}

// TestExecutor_FactoryReset_UnitFailureStillClearsClaim: set-factory-boot only
// ARMS a one-shot candidate boot (the btrfs default is left untouched), and its
// start can fail outright — so the persisted-claim clear must hold BEFORE that
// outcome, leaving a rolled-back or never-started reset unclaimed with no
// usable topic on disk (the fail-safe direction).
func TestExecutor_FactoryReset_UnitFailureStillClearsClaim(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_FACTORY_RESET,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	ts.mockStateManager.EXPECT().
		InvalidateRelayerTopic().
		Return("topic-1", true, nil)
	ts.mockStateManager.EXPECT().
		ClearClaim().
		Return(true, nil)

	// The reset clears the owner's device name with the claim.
	// Clear removes the staged temp first (resold-frame leak guard).
	ts.mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE + ".tmp").Return(nil)
	ts.mockOS.EXPECT().Remove(constants.DEVICE_NAME_FILE).Return(nil)

	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "systemctl", "start", "set-factory-boot.service").
		Return(ts.mockExecCmd)
	ts.mockExecCmd.EXPECT().
		CombinedOutput().
		Return([]byte("Failed to start set-factory-boot.service"), errors.New("exit status 1"))

	var observed []bool
	ts.executor.SetClaimObserver(func(claimed bool) { observed = append(observed, claimed) })

	_, err := ts.executor.Execute(ts.ctx, cmd)
	require.Error(t, err)
	assert.Equal(t, []bool{false}, observed,
		"a failed reset unit must still leave the device locally unclaimed")
}

func TestExecutor_UploadLogs_MissingArguments(t *testing.T) {
	tests := []struct {
		name      string
		arguments map[string]interface{}
	}{
		{
			name: "missing userId",
			arguments: map[string]interface{}{
				"apiKey": "test-api-key",
				"title":  "test-title",
			},
		},
		{
			name: "missing apiKey",
			arguments: map[string]interface{}{
				"userId": "test-user-id",
				"title":  "test-title",
			},
		},
		{
			name: "missing title",
			arguments: map[string]interface{}{
				"userId": "test-user-id",
				"apiKey": "test-api-key",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			cmd := commands.Command{
				Type:      commands.CMD_UPLOAD_LOGS,
				Arguments: tt.arguments,
			}

			// Mock JSON marshaling
			ts.mockJSON.EXPECT().
				Marshal(cmd.Arguments).
				Return([]byte(`{}`), nil)

			// Mock JSON unmarshaling to return an empty struct
			ts.mockJSON.EXPECT().
				Unmarshal(gomock.Any(), gomock.Any()).
				Return(nil)

			// Execute command
			result, err := ts.executor.Execute(ts.ctx, cmd)
			assert.Error(t, err)
			assert.Nil(t, result)
			assert.Contains(t, err.Error(), "missing required arguments")
		})
	}
}

func TestExecutor_NewHandler(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDeviceStatus := mocks.NewMockDeviceStatus(ctrl)
	mockStatus := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockMath := mocks.NewMockMath(ctrl)
	panelDDC := mocks.NewMockPanelDDC(ctrl)

	handler := devicectl.New(
		mockCDP,
		mockDeviceStatus,
		mockStatus,
		panelDDC,
		mockJSON,
		mockOS,
		mockExec,
		mockMath,
		mockClock,
		logger,
	)
	assert.NotNil(t, handler)
}

func TestExecutor_DdcPanelControl_Success(t *testing.T) {
	tests := []struct {
		name        string
		payload     string
		wantArgs    []string
		description string
	}{
		{
			name:        "brightness",
			payload:     `{"action":"brightness","value":42}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "10", "42"},
			description: "VCP 0x10 brightness",
		},
		{
			name:        "contrast",
			payload:     `{"action":"contrast","value":77}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "12", "77"},
			description: "VCP 0x12 contrast",
		},
		{
			name:        "volume",
			payload:     `{"action":"volume","value":0}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "62", "0"},
			description: "VCP 0x62 speaker volume",
		},
		{
			name:        "muteOn",
			payload:     `{"action":"mute","value":"on"}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "8D", "1"},
			description: "VCP 0x8D mute on",
		},
		{
			name:        "muteOff",
			payload:     `{"action":"mute","value":"off"}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "8D", "2"},
			description: "VCP 0x8D mute off",
		},
		{
			name:        "powerStandby",
			payload:     `{"action":"power","value":"standby"}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "D6", "04"},
			description: "DDC power standby",
		},
		{
			name:        "powerOff",
			payload:     `{"action":"power","value":"off"}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "D6", "05"},
			description: "DDC power off soft",
		},
		{
			name:        "powerOn",
			payload:     `{"action":"power","value":"on"}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "D6", "01"},
			description: "DDC power on",
		},
		{
			name:        "actionCaseInsensitive",
			payload:     `{"action":"BRIGHTNESS","value":1}`,
			wantArgs:    []string{"ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "10", "1"},
			description: "action normalized to lower case",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			cmd := commands.Command{
				Type:      commands.CMD_DDC_PANEL_CONTROL,
				Arguments: map[string]interface{}{},
			}
			_ = json.Unmarshal([]byte(tt.payload), &cmd.Arguments)

			ts.mockJSON.EXPECT().
				Marshal(cmd.Arguments).
				Return([]byte(tt.payload), nil)
			ts.mockJSON.EXPECT().
				Unmarshal([]byte(tt.payload), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					return json.Unmarshal(data, v)
				})

			wantArgv := tt.wantArgs
			ts.mockExec.EXPECT().
				CommandContext(ts.ctx,
					gomock.Any(), // ddcutil
					gomock.Any(), // --noverify
					gomock.Any(), // setvcp
					gomock.Any(), // --sleep-multiplier
					gomock.Any(), // 1.5
					gomock.Any(), // vcpCode
					gomock.Any(), // value
				).
				DoAndReturn(func(_ context.Context, name string, arg ...string) wrapper.ExecCmd {
					got := append([]string{name}, arg...)
					assert.Equal(t, wantArgv, got, tt.description)
					return ts.mockExecCmd
				})
			ts.mockExecCmd.EXPECT().
				CombinedOutput().
				Return([]byte(""), nil)

			result, err := ts.executor.Execute(ts.ctx, cmd)
			assert.NoError(t, err, tt.description)
			assert.Equal(t, devicectl.CmdOK, result)
		})
	}
}

func TestExecutor_DdcPanelControl_DisplayNotFoundRunsDetectAndRetries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	payload := `{"action":"brightness","value":5}`
	cmd := commands.Command{
		Type:      commands.CMD_DDC_PANEL_CONTROL,
		Arguments: map[string]interface{}{},
	}
	_ = json.Unmarshal([]byte(payload), &cmd.Arguments)

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(payload), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(payload), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			return json.Unmarshal(data, v)
		})

	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "10", "5").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("Display not found\n"), errors.New("exit 1")),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "60", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("Invalid display\n"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "10", "5").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_DdcPanelControl_Errors(t *testing.T) {
	tests := []struct {
		name    string
		payload string
		setup   func(*testSetup)
		wantErr string
	}{
		{
			name:    "unknownAction",
			payload: `{"action":"gamma","value":1}`,
			wantErr: "invalid ddcPanelControl action",
		},
		{
			name:    "muteBadValue",
			payload: `{"action":"mute","value":"maybe"}`,
			wantErr: "invalid mute value",
		},
		{
			name:    "powerBadValue",
			payload: `{"action":"power","value":"fast"}`,
			wantErr: "invalid power value",
		},
		{
			name:    "percentOutOfRange",
			payload: `{"action":"brightness","value":101}`,
			wantErr: "between 0 and 100",
		},
		{
			name:    "missingValue",
			payload: `{"action":"brightness"}`,
			wantErr: "value is required",
		},
		{
			name:    "ddcutilFails",
			payload: `{"action":"brightness","value":5}`,
			setup: func(ts *testSetup) {
				gomock.InOrder(
					ts.mockExec.EXPECT().
						CommandContext(ts.ctx, "ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "10", "5").
						Return(ts.mockExecCmd),
					ts.mockExecCmd.EXPECT().
						CombinedOutput().
						Return([]byte("no display"), errors.New("exit 1")),
					ts.mockExec.EXPECT().
						CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "60", "--brief").
						Return(ts.mockExecCmd),
					ts.mockExecCmd.EXPECT().
						CombinedOutput().
						Return([]byte("getvcp 60 ok"), nil),
					ts.mockExec.EXPECT().
						CommandContext(ts.ctx, "ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", "10", "5").
						Return(ts.mockExecCmd),
					ts.mockExecCmd.EXPECT().
						CombinedOutput().
						Return([]byte("no display"), errors.New("exit 1")),
				)
			},
			wantErr: "ddcutil setvcp",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			cmd := commands.Command{
				Type:      commands.CMD_DDC_PANEL_CONTROL,
				Arguments: map[string]interface{}{},
			}
			_ = json.Unmarshal([]byte(tt.payload), &cmd.Arguments)

			ts.mockJSON.EXPECT().
				Marshal(cmd.Arguments).
				Return([]byte(tt.payload), nil)
			ts.mockJSON.EXPECT().
				Unmarshal([]byte(tt.payload), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					return json.Unmarshal(data, v)
				})

			if tt.setup != nil {
				tt.setup(ts)
			}

			result, err := ts.executor.Execute(ts.ctx, cmd)
			assert.Error(t, err)
			assert.Nil(t, result)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestExecutor_DdcPanelStatus_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_DDC_PANEL_STATUS,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "detect", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("Monitor: ASUS : ROG-Strix\n"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("VCP 10 C 50 100\nVCP 12 C 30 100\nVCP 62 C 15 100\nVCP 8D SNC x01\nVCP D6 SNC x01\n"), nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.NoError(t, err)
	st, ok := result.(*ddc.DdcPanelStatus)
	require.True(t, ok)
	require.NotNil(t, st.Brightness)
	assert.Equal(t, 50, *st.Brightness)
	require.NotNil(t, st.Contrast)
	assert.Equal(t, 30, *st.Contrast)
	require.NotNil(t, st.Volume)
	assert.Equal(t, 15, *st.Volume)
	require.NotNil(t, st.Mute)
	assert.Equal(t, "on", *st.Mute)
	require.NotNil(t, st.Power)
	assert.Equal(t, "on", *st.Power)
	require.NotNil(t, st.Monitor)
	assert.Equal(t, "ASUS:ROG-Strix", *st.Monitor)
	assert.Nil(t, st.Errors)
}

func TestExecutor_DdcPanelStatus_PartialErrors(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_DDC_PANEL_STATUS,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "detect", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("Monitor: ASUS : ROG-Strix\n"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("VCP 10 C 50 100\nVCP 12 ERR\nVCP 62 C 1 100\nVCP 8D SNC x02\nVCP D6 SNC x99\n"), nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.NoError(t, err)
	st, ok := result.(*ddc.DdcPanelStatus)
	require.True(t, ok)
	require.NotNil(t, st.Brightness)
	assert.Nil(t, st.Contrast)
	require.NotNil(t, st.Volume)
	require.NotNil(t, st.Mute)
	assert.Equal(t, "off", *st.Mute)
	assert.Nil(t, st.Power)
	require.NotNil(t, st.Monitor)
	assert.Equal(t, "ASUS:ROG-Strix", *st.Monitor)
	require.NotNil(t, st.Errors)
	assert.Contains(t, st.Errors, "contrast")
	assert.Contains(t, st.Errors, "power")
}

func TestExecutor_DdcPanelStatus_OnlyMuteErrWithExitCode(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_DDC_PANEL_STATUS,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Simulate ddcutil emitting usable VCP lines plus ERR for mute, and
	// returning a non-zero exit status.
	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "detect", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("Monitor: DEL : DELL S2721QS\n"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("VCP 10 C 75 100\nVCP 12 C 75 100\nVCP 62 C 0 100\nVCP 8D ERR\nVCP D6 SNC x01\n"), errors.New("exit status 1")),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "60", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("getvcp 60 ok"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("VCP 10 C 75 100\nVCP 12 C 75 100\nVCP 62 C 0 100\nVCP 8D ERR\nVCP D6 SNC x01\n"), errors.New("exit status 1")),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.NoError(t, err)
	st, ok := result.(*ddc.DdcPanelStatus)
	require.True(t, ok)

	require.NotNil(t, st.Brightness)
	assert.Equal(t, 75, *st.Brightness)
	require.NotNil(t, st.Contrast)
	assert.Equal(t, 75, *st.Contrast)
	require.NotNil(t, st.Volume)
	assert.Equal(t, 0, *st.Volume)
	require.NotNil(t, st.Power)
	assert.Equal(t, "on", *st.Power)
	require.NotNil(t, st.Monitor)
	assert.Equal(t, "DEL:DELL S2721QS", *st.Monitor)

	require.NotNil(t, st.Errors)
	// Only mute should report an error; others should be clean.
	assert.Contains(t, st.Errors, "mute")
	assert.NotContains(t, st.Errors, "brightness")
	assert.NotContains(t, st.Errors, "contrast")
	assert.NotContains(t, st.Errors, "volume")
	assert.NotContains(t, st.Errors, "power")
}

func TestExecutor_DdcPanelStatus_RetryDetectOnAnyError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_DDC_PANEL_STATUS,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "detect", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("Monitor: ASUS : ROG-Strix\n"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("boom output"), errors.New("boom")),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "60", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("getvcp 60 ok"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("VCP 10 C 50 100\nVCP 12 C 30 100\nVCP 62 C 15 100\nVCP 8D SNC x01\nVCP D6 SNC x01\n"), nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.NoError(t, err)
	st, ok := result.(*ddc.DdcPanelStatus)
	require.True(t, ok)
	require.NotNil(t, st.Brightness)
	assert.Equal(t, 50, *st.Brightness)
	require.NotNil(t, st.Contrast)
	assert.Equal(t, 30, *st.Contrast)
	require.NotNil(t, st.Volume)
	assert.Equal(t, 15, *st.Volume)
	require.NotNil(t, st.Mute)
	assert.Equal(t, "on", *st.Mute)
	require.NotNil(t, st.Power)
	assert.Equal(t, "on", *st.Power)
	require.NotNil(t, st.Monitor)
	assert.Equal(t, "ASUS:ROG-Strix", *st.Monitor)
	assert.Nil(t, st.Errors)
}

func TestExecutor_DdcPanelStatus_RetryWhenNoVcpLines(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_DDC_PANEL_STATUS,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	gomock.InOrder(
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "detect", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("Monitor: ASUS : ROG-Strix\n"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			// Only leading noise, no `VCP ...` line.
			Return([]byte("Discarding cached sleep adjustment data for bus /dev/i2c-4. EDID has changed.\n"), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "60", "--brief").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "ddcutil", "--noverify", "getvcp", "--brief", "10", "12", "62", "8D", "D6").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte("VCP 10 C 50 100\nVCP 12 C 30 100\nVCP 62 C 15 100\nVCP 8D SNC x01\nVCP D6 SNC x01\n"), nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	require.NoError(t, err)
	st, ok := result.(*ddc.DdcPanelStatus)
	require.True(t, ok)
	require.NotNil(t, st.Brightness)
	assert.Equal(t, 50, *st.Brightness)
	require.NotNil(t, st.Contrast)
	assert.Equal(t, 30, *st.Contrast)
	require.NotNil(t, st.Volume)
	assert.Equal(t, 15, *st.Volume)
	require.NotNil(t, st.Mute)
	assert.Equal(t, "on", *st.Mute)
	require.NotNil(t, st.Power)
	assert.Equal(t, "on", *st.Power)
	require.NotNil(t, st.Monitor)
	assert.Equal(t, "ASUS:ROG-Strix", *st.Monitor)
	assert.Nil(t, st.Errors)
}

// TestExecutor_Connect_EmptyDeviceID: an empty clientDevice ID must be rejected
// before any state is saved or the claim observer fires — every derived claim
// view (mDNS init, /api/status, topic withholding) keys on a non-empty ID, so
// accepting it would desync mDNS TXT claimed=true from /api/status claimed=false.
func TestExecutor_Connect_EmptyDeviceID(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	device := devicectl.Device{ID: "  ", Name: "Test Device", Platform: 1}
	cmd := commands.Command{
		Type: commands.CMD_CONNECT,
		Arguments: map[string]interface{}{
			"clientDevice":   device,
			"primaryAddress": "192.168.1.100",
		},
	}

	arguments := `{"clientDevice":{"device_id":"  ","device_name":"Test Device","platform":1},"primaryAddress":"192.168.1.100"}`
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			args := v.(*struct {
				Device         devicectl.Device `json:"clientDevice"`
				PrimaryAddress string           `json:"primaryAddress"`
			})
			args.Device = device
			args.PrimaryAddress = "192.168.1.100"
			return nil
		})
	// No state-manager or claim-observer expectations: rejection must happen first.

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "clientDevice.id is required")
}

// TestExecutor_ScreenRotation_ConcurrentTapsAdvanceTwoSteps pins the
// serialization contract the command-storm gate now relies on: rotation is
// not deduped (each byte-identical tap is a distinct relative step), so two
// overlapping taps must advance two orientation steps. Without rotationMu the
// orientation-file read-modify-write is a lost update — both taps read the
// same start and apply the same target, collapsing two taps into one step.
func TestExecutor_ScreenRotation_ConcurrentTapsAdvanceTwoSteps(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_SCREEN_ROTATION,
		Arguments: map[string]interface{}{"clockwise": true},
	}
	arguments := `{"clockwise":true}`

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil).
		Times(2)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			v.(*struct {
				Clockwise bool `json:"clockwise"`
			}).Clockwise = true
			return nil
		}).
		Times(2)

	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "wlr-randr").
		Return(ts.mockExecCmd).
		Times(2)
	ts.mockExecCmd.EXPECT().
		Output().
		Return([]byte("HDMI-A-1 \"Dell Inc. DELL S2721QS D3SNM43 (HDMI-A-1)\""), nil).
		Times(2)

	// stored is the in-memory stand-in for SCREEN_ORIENTATION_FILE. The first
	// ReadFile parks until the second ReadFile arrives (unserialized overlap)
	// or a grace period elapses — under rotationMu the second command is
	// parked on the mutex and can never reach ReadFile concurrently, so the
	// timeout path is the serialized outcome.
	var stateMu sync.Mutex
	stored := "normal"
	readCount := 0
	secondReadEntered := make(chan struct{})
	ts.mockOS.EXPECT().
		ReadFile(constants.SCREEN_ORIENTATION_FILE).
		DoAndReturn(func(string) ([]byte, error) {
			stateMu.Lock()
			readCount++
			idx := readCount
			val := stored
			stateMu.Unlock()
			if idx == 2 {
				close(secondReadEntered)
			} else {
				select {
				case <-secondReadEntered:
				case <-time.After(100 * time.Millisecond):
				}
			}
			return []byte(val), nil
		}).
		Times(2)

	var transforms []string
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "wlr-randr", "--output", "HDMI-A-1", "--transform", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, arg ...string) wrapper.ExecCmd {
			stateMu.Lock()
			transforms = append(transforms, arg[len(arg)-1])
			stateMu.Unlock()
			return ts.mockExecCmd
		}).
		Times(2)
	ts.mockExecCmd.EXPECT().
		Run().
		Return(nil).
		Times(2)

	ts.mockOS.EXPECT().
		WriteFile(constants.SCREEN_ORIENTATION_FILE, gomock.Any(), os.FileMode(0600)).
		DoAndReturn(func(_ string, data []byte, _ os.FileMode) error {
			stateMu.Lock()
			stored = string(data)
			stateMu.Unlock()
			return nil
		}).
		Times(2)
	ts.mockStatus.EXPECT().
		ForceRefresh().
		Times(2)

	var wg sync.WaitGroup
	wg.Add(2)
	for i := 0; i < 2; i++ {
		go func() {
			defer wg.Done()
			_, err := ts.executor.Execute(ts.ctx, cmd)
			assert.NoError(t, err)
		}()
	}
	wg.Wait()

	// Two clockwise taps from normal must land on 180 (normal → 270 → 180).
	// A lost update leaves 270 with both applies targeting 270.
	assert.Equal(t, "180", stored)
	assert.Equal(t, []string{"270", "180"}, transforms)
}

// Review round 4 L6: SSH granted while the device had no owner is revoked
// when the first owner enrolls (same steps as sshAccess{enabled:false}).
func TestExecutor_OwnerEnrolledRevokesLegacySsh(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	stopped, done := make(chan struct{}), make(chan struct{})
	ts.mockJSON.EXPECT().Unmarshal([]byte(`{"enabled":false}`), gomock.Any()).Return(nil)
	// The authorized_keys removal is the last step.
	ts.mockOS.EXPECT().IsNotExist(gomock.Any()).DoAndReturn(func(error) bool {
		close(done)
		return true
	})
	ts.mockExecCmd.EXPECT().CombinedOutput().Return([]byte(""), nil).AnyTimes()
	ts.mockExec.EXPECT().CommandContext(gomock.Any(), "sudo", "systemctl", gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, args ...string) wrapper.ExecCmd {
			if args[1] == "stop" && args[2] == "sshd.service" {
				close(stopped)
			}
			return ts.mockExecCmd
		}).AnyTimes()

	ts.executor.(interface{ ControlOwnerEnrolled() }).ControlOwnerEnrolled()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("sshd was not stopped after owner enrollment")
	}
	<-done
}

// Hardware round 3: an sshAccess the new owner sends right after the first
// enrollment ran concurrently with the revocation, which then deleted the
// key it had just installed. The revocation is queued first and the owner's
// command waits for it.
func TestExecutor_OwnerSshAccessRunsAfterEnrollmentRevocation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	var mu sync.Mutex
	var events []string
	record := func(e string) { mu.Lock(); events = append(events, e); mu.Unlock() }
	release := make(chan struct{})

	ts.mockJSON.EXPECT().Unmarshal([]byte(`{"enabled":false}`), gomock.Any()).Return(nil)
	ts.mockOS.EXPECT().IsNotExist(gomock.Any()).DoAndReturn(func(error) bool {
		record("revoke:remove-key")
		return true
	})
	ts.mockExecCmd.EXPECT().CombinedOutput().Return([]byte(""), nil).AnyTimes()
	ts.mockExec.EXPECT().CommandContext(gomock.Any(), "sudo", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, args ...string) wrapper.ExecCmd {
			if len(args) == 3 && args[1] == "stop" && args[2] == "sshd.service" {
				record("revoke:stop-sshd")
				<-release // hold the revocation mid-way
			}
			if len(args) == 3 && args[1] == "start" && args[2] == "sshd.service" {
				record("enable:start-sshd")
			}
			return ts.mockExecCmd
		}).AnyTimes()

	enable := commands.Command{Type: commands.CMD_SSH_ACCESS, Arguments: map[string]interface{}{
		"enabled": true, "publicKey": "ssh-ed25519 AAAA-owner", "ttlSeconds": 0,
	}}
	raw := []byte(`{"enabled":true,"publicKey":"ssh-ed25519 AAAA-owner","ttlSeconds":0}`)
	ts.mockJSON.EXPECT().Marshal(enable.Arguments).Return(raw, nil)
	ts.mockJSON.EXPECT().Unmarshal(raw, gomock.Any()).DoAndReturn(func(_ []byte, v interface{}) error {
		args := v.(*struct {
			Enabled    bool   `json:"enabled"`
			PublicKey  string `json:"publicKey"`
			TTLSeconds *int   `json:"ttlSeconds"`
		})
		zero := 0
		args.Enabled, args.PublicKey, args.TTLSeconds = true, "ssh-ed25519 AAAA-owner", &zero
		return nil
	})
	ts.mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil)
	ts.mockOS.EXPECT().WriteFile(constants.SSH_AUTHORIZED_KEYS_FILE, gomock.Any(), gomock.Any()).
		DoAndReturn(func(string, []byte, os.FileMode) error { record("enable:write-key"); return nil })

	ts.executor.(interface{ ControlOwnerEnrolled() }).ControlOwnerEnrolled()
	result := make(chan error, 1)
	go func() { _, err := ts.executor.Execute(ts.ctx, enable); result <- err }()

	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	early := append([]string(nil), events...)
	mu.Unlock()
	assert.NotContains(t, early, "enable:write-key", "the owner's sshAccess ran while the revocation was still in progress")
	close(release)

	select {
	case err := <-result:
		assert.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("sshAccess did not finish")
	}
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"revoke:stop-sshd", "revoke:remove-key", "enable:write-key", "enable:start-sshd"}, events)
}
