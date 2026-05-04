package devicectl_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
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
	mockDBus         *mocks.MockDBus
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
	mockDBus := mocks.NewMockDBus(ctrl)
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
	panelDDC := ddc.New(mockExec, logger)

	// Create executor with mocks
	executor := devicectl.New(
		mockCDP,
		mockDBus,
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
		mockDBus:         mockDBus,
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

	// Mock state manager get
	ts.mockStateManager.EXPECT().
		GetState().
		Return(&state.State{
			ConnectedDevice: &state.Device{
				ID:       device.ID,
				Name:     device.Name,
				Platform: device.Platform,
			},
		}).Times(2)

	// Mock state manager save
	ts.mockStateManager.EXPECT().
		Save(gomock.Any()).
		Return(nil)

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

				// Mock state manager get
				ts.mockStateManager.EXPECT().
					GetState().
					Return(&state.State{
						ConnectedDevice: &state.Device{
							ID:       "test-device-id",
							Name:     "Test Device",
							Platform: 1,
						},
					})

				// Mock state manager save to fail
				ts.mockStateManager.EXPECT().
					Save(gomock.Any()).
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

func TestExecutor_ShowPairingQRCode_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type: commands.CMD_SHOW_PAIRING_QR_CODE,
		Arguments: map[string]interface{}{
			"show": true,
		},
	}

	arguments := `{"show":true}`

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)

	// Mock JSON unmarshaling
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			args := v.(*struct {
				Show bool `json:"show"`
			})
			args.Show = true
			return nil
		})

	// Mock DBus call
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_SHOW_PAIRING_QR_CODE,
			Body:      []interface{}{true},
		}).
		Return(nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ShowPairingQRCode_DBusError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type: commands.CMD_SHOW_PAIRING_QR_CODE,
		Arguments: map[string]interface{}{
			"show": true,
		},
	}

	arguments := `{"show":true}`

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(arguments), nil)

	// Mock JSON unmarshaling
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			args := v.(*struct {
				Show bool `json:"show"`
			})
			args.Show = true
			return nil
		})

	// Mock DBus call to fail
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, gomock.Any()).
		Return(errors.New("dbus error"))

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to send show pairing QR code")
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

func TestExecutor_KeyboardEvent_Success(t *testing.T) {
	testCases := []struct {
		name     string
		keyCode  int
		expected map[string]interface{}
		wantText bool
	}{
		{name: "LetterA", keyCode: 65, expected: map[string]interface{}{"key": "A", "code": "KeyA", "text": "A", "unmodifiedText": "A"}, wantText: true},
		{name: "Letterz", keyCode: 122, expected: map[string]interface{}{"key": "z", "code": "KeyZ", "text": "z", "unmodifiedText": "z"}, wantText: true},
		{name: "Number0", keyCode: 48, expected: map[string]interface{}{"key": "0", "code": "Digit0", "text": "0", "unmodifiedText": "0"}, wantText: true},
		{name: "Exclamation", keyCode: 33, expected: map[string]interface{}{"key": "!", "code": "Digit1", "text": "!", "unmodifiedText": "!"}, wantText: true},
		{name: "AtSign", keyCode: 64, expected: map[string]interface{}{"key": "@", "code": "Digit2", "text": "@", "unmodifiedText": "@"}, wantText: true},
		{name: "Percent", keyCode: 37, expected: map[string]interface{}{"key": "%", "code": "Digit5", "text": "%", "unmodifiedText": "%"}, wantText: true},
		{name: "Ampersand", keyCode: 38, expected: map[string]interface{}{"key": "&", "code": "Digit7", "text": "&", "unmodifiedText": "&"}, wantText: true},
		{name: "Apostrophe", keyCode: 39, expected: map[string]interface{}{"key": "'", "code": "Quote", "text": "'", "unmodifiedText": "'"}, wantText: true},
		{name: "LeftParen", keyCode: 40, expected: map[string]interface{}{"key": "(", "code": "Digit9", "text": "(", "unmodifiedText": "("}, wantText: true},
		{name: "Underscore", keyCode: 95, expected: map[string]interface{}{"key": "_", "code": "Minus", "text": "_", "unmodifiedText": "_"}, wantText: true},
		{name: "Tilde", keyCode: 126, expected: map[string]interface{}{"key": "~", "code": "Backquote", "text": "~", "unmodifiedText": "~"}, wantText: true},
		{name: "Space", keyCode: 32, expected: map[string]interface{}{"key": " ", "code": "Space", "text": " ", "unmodifiedText": " "}, wantText: true},
		{name: "Tab", keyCode: 9, expected: map[string]interface{}{"key": "Tab", "code": "Tab"}, wantText: false},
		{name: "Enter", keyCode: 13, expected: map[string]interface{}{"key": "Enter", "code": "Enter"}, wantText: false},
		{name: "Escape", keyCode: 27, expected: map[string]interface{}{"key": "Escape", "code": "Escape"}, wantText: false},
		{name: "Backspace", keyCode: 8, expected: map[string]interface{}{"key": "Backspace", "code": "Backspace"}, wantText: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			cmd := commands.Command{
				Type: commands.CMD_KEYBOARD_EVENT,
				Arguments: map[string]interface{}{
					"code": tc.keyCode,
				},
			}

			arguments := fmt.Sprintf(`{"code":%d}`, tc.keyCode)

			ts.mockJSON.EXPECT().
				Marshal(cmd.Arguments).
				Return([]byte(arguments), nil)

			ts.mockJSON.EXPECT().
				Unmarshal([]byte(arguments), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					args := v.(*struct {
						Code int `json:"code"`
					})
					args.Code = tc.keyCode
					return nil
				})

			first := ts.mockCDP.EXPECT().
				Send("Input.dispatchKeyEvent", gomock.Any()).
				DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
					assert.Equal(t, "keyDown", params["type"])
					assert.Equal(t, tc.keyCode, params["windowsVirtualKeyCode"])
					assert.Equal(t, tc.keyCode, params["nativeVirtualKeyCode"])
					for k, v := range tc.expected {
						assert.Equal(t, v, params[k], "unexpected value for %s", k)
					}
					if tc.wantText {
						assert.Equal(t, tc.expected["text"], params["text"])
						assert.Equal(t, tc.expected["unmodifiedText"], params["unmodifiedText"])
					} else {
						assert.NotContains(t, params, "text")
						assert.NotContains(t, params, "unmodifiedText")
					}
					return nil, nil
				})

			ts.mockCDP.EXPECT().
				Send("Input.dispatchKeyEvent", gomock.Any()).
				DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
					assert.Equal(t, "keyUp", params["type"])
					assert.Equal(t, tc.keyCode, params["windowsVirtualKeyCode"])
					assert.Equal(t, tc.keyCode, params["nativeVirtualKeyCode"])
					for k, v := range tc.expected {
						assert.Equal(t, v, params[k], "unexpected value for %s", k)
					}
					if tc.wantText {
						assert.Equal(t, tc.expected["text"], params["text"])
						assert.Equal(t, tc.expected["unmodifiedText"], params["unmodifiedText"])
					} else {
						assert.NotContains(t, params, "text")
						assert.NotContains(t, params, "unmodifiedText")
					}
					return nil, nil
				}).After(first)

			result, err := ts.executor.Execute(ts.ctx, cmd)
			assert.NoError(t, err)
			assert.Equal(t, devicectl.CmdOK, result)
		})
	}
}

func TestExecutor_KeyboardEvent_UnsupportedCode(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_KEYBOARD_EVENT,
		Arguments: map[string]interface{}{
			"code": 31,
		},
	}

	arguments := `{"code":31}`

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			args := v.(*struct {
				Code int `json:"code"`
			})
			args.Code = 31
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "unsupported keyboard event code")
}

func TestExecutor_KeyboardEvent_UnsupportedPrintablePunctuation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_KEYBOARD_EVENT,
		Arguments: map[string]interface{}{
			"code": 127,
		},
	}

	arguments := `{"code":127}`

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			args := v.(*struct {
				Code int `json:"code"`
			})
			args.Code = 127
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "unsupported keyboard event code")
}

func TestExecutor_KeyboardEvent_Errors(t *testing.T) {
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
					Return([]byte(`{"code":65}`), nil)

				// Mock JSON unmarshaling to fail
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					Return(errors.New("json unmarshal failed"))
			},
			wantErr: "invalid arguments",
		},
		{
			name: "CDP keyDown failure for printable key",
			setupFunc: func(ts *testSetup) {
				keyCode := 65 // 'A' - printable ASCII

				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"code":65}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Code int `json:"code"`
						})
						args.Code = keyCode
						return nil
					})

				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						assert.Equal(t, "keyDown", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, "A", params["key"])
						assert.Equal(t, "KeyA", params["code"])
						return nil, errors.New("cdp keyDown failed")
					})
			},
			wantErr: "failed to send keyboard event",
		},
		{
			name: "CDP keyUp failure for printable key (should succeed)",
			setupFunc: func(ts *testSetup) {
				keyCode := 65 // 'A' - printable ASCII

				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"code":65}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Code int `json:"code"`
						})
						args.Code = keyCode
						return nil
					})

				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						assert.Equal(t, "keyDown", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, "A", params["key"])
						assert.Equal(t, "KeyA", params["code"])
						assert.Equal(t, "A", params["text"])
						assert.Equal(t, "A", params["unmodifiedText"])
						return nil, nil
					})

				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						assert.Equal(t, "keyUp", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, "A", params["key"])
						assert.Equal(t, "KeyA", params["code"])
						return nil, errors.New("cdp keyUp failed")
					})
			},
			wantErr: "",
		},
		{
			name: "CDP keyDown failure for special key",
			setupFunc: func(ts *testSetup) {
				keyCode := 32 // Space - special key (no keyUp)

				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"code":32}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Code int `json:"code"`
						})
						args.Code = keyCode
						return nil
					})

				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						assert.Equal(t, "keyDown", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, " ", params["key"])
						assert.Equal(t, "Space", params["code"])
						assert.Equal(t, " ", params["text"])
						assert.Equal(t, " ", params["unmodifiedText"])
						return nil, errors.New("cdp keyDown failed")
					})
			},
			wantErr: "failed to send keyboard event",
		},
		{
			name: "Special key keyUp failure is ignored",
			setupFunc: func(ts *testSetup) {
				keyCode := 13 // Enter

				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"code":13}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Code int `json:"code"`
						})
						args.Code = keyCode
						return nil
					})

				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						assert.Equal(t, "keyDown", params["type"])
						assert.Equal(t, "Enter", params["key"])
						assert.Equal(t, "Enter", params["code"])
						assert.NotContains(t, params, "text")
						assert.NotContains(t, params, "unmodifiedText")
						return nil, nil
					})

				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						assert.Equal(t, "keyUp", params["type"])
						assert.Equal(t, "Enter", params["key"])
						assert.Equal(t, "Enter", params["code"])
						return nil, errors.New("cdp keyUp failed")
					})
			},
			wantErr: "",
		},
		{
			name: "Unsupported key code fails",
			setupFunc: func(ts *testSetup) {
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"code":31}`), nil)
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							Code int `json:"code"`
						})
						args.Code = 31
						return nil
					})
			},
			wantErr: "unsupported keyboard event code",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			cmd := commands.Command{
				Type: commands.CMD_KEYBOARD_EVENT,
				Arguments: map[string]interface{}{
					"code": 65,
				},
			}

			tt.setupFunc(ts)

			result, err := ts.executor.Execute(ts.ctx, cmd)

			if tt.wantErr != "" {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErr)
				assert.Nil(t, result)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, devicectl.CmdOK, result)
			}
		})
	}
}

func TestExecutor_MouseMoveEvent_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type: commands.CMD_MOUSE_DRAG_EVENT,
		Arguments: map[string]interface{}{
			"messageID": "test-msg-id",
			"cursorOffsets": []map[string]interface{}{
				{"dx": 10.0, "dy": 5.0},
				{"dx": 15.0, "dy": -3.0},
			},
		},
	}

	arguments := `{"messageID":"test-msg-id","cursorOffsets":[{"dx":10.0,"dy":5.0},{"dx":15.0,"dy":-3.0}]}`

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)

	// Mock CDP Send for screen dimensions
	dp := map[string]interface{}{
		"expression":    "({width: window.innerWidth, height: window.innerHeight})",
		"returnByValue": true,
	}
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", dp).
		Return(map[string]interface{}{
			"width":  1920.0,
			"height": 1080.0,
		}, nil)

	// Mock JSON unmarshaling
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			args := v.(*struct {
				MessageID     string `json:"messageID"`
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			args.MessageID = "test-msg-id"
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{
				{DX: 10.0, DY: 5.0},
				{DX: 15.0, DY: -3.0},
			}
			return nil
		})

	// Mock math operations for magnitude calculation
	ts.mockMath.EXPECT().Sqrt(125.0).Return(11.180339887498949) // sqrt(10^2 + 5^2)
	ts.mockMath.EXPECT().Sqrt(234.0).Return(15.297058540778355) // sqrt(15^2 + (-3)^2)

	// Mock math operations for bounds checking with exact values
	// Calculate expected cursor positions for this test case
	// Start: (960, 540), Offsets: [(10, 5), (15, -3)]

	// First offset: dx=10.0, dy=5.0 (no clamping)
	// New pos: (970, 545)
	ts.mockMath.EXPECT().Min(970.0, 1920.0).Return(970.0)
	ts.mockMath.EXPECT().Max(0.0, 970.0).Return(970.0)
	ts.mockMath.EXPECT().Min(545.0, 1080.0).Return(545.0)
	ts.mockMath.EXPECT().Max(0.0, 545.0).Return(545.0)

	// Second offset: dx=15.0, dy=-3.0 (no clamping)
	// New pos: (985, 542)
	ts.mockMath.EXPECT().Min(985.0, 1920.0).Return(985.0)
	ts.mockMath.EXPECT().Max(0.0, 985.0).Return(985.0)
	ts.mockMath.EXPECT().Min(542.0, 1080.0).Return(542.0)
	ts.mockMath.EXPECT().Max(0.0, 542.0).Return(542.0)

	// Mock JSON marshal for positions - expect exact structure
	ts.mockJSON.EXPECT().
		Marshal(map[string]interface{}{
			"messageID": "test-msg-id",
			"message": map[string]interface{}{
				"command": "cursorUpdate",
				"request": map[string]interface{}{
					"positions": []map[string]float64{
						{"x": 970.0, "y": 545.0}, // First position
						{"x": 985.0, "y": 542.0}, // Final position
					},
				},
			},
		}).
		Return([]byte(`{"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[{"x":970,"y":545},{"x":985,"y":542}]}}}`), nil)

	// Mock CDP Send for JavaScript cursor positions - expect exact expression
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", map[string]interface{}{
			"expression": `window.handleCDPRequest({"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[{"x":970,"y":545},{"x":985,"y":542}]}}})`,
		}).
		Return(nil, nil)

	// Mock CDP Send for mouse movement - expect exact moveParams
	var gotX, gotY float64
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":       "mouseMoved",
			"x":          985.0,
			"y":          542.0,
			"button":     "none",
			"buttons":    0,
			"clickCount": 0,
		}).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			gotX = params["x"].(float64)
			gotY = params["y"].(float64)
			return nil, nil
		})

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
	assert.InEpsilon(t, 985.0, gotX, 0.0001, "final X position should match")
	assert.InEpsilon(t, 542.0, gotY, 0.0001, "final Y position should match")
}

func TestExecutor_MouseMoveEvent_EmptyOffsets(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_DRAG_EVENT,
		Arguments: map[string]interface{}{
			"messageID":     "test-msg-id",
			"cursorOffsets": []map[string]interface{}{},
		},
	}

	arguments := `{"messageID":"test-msg-id","cursorOffsets":[]}`

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(arguments), nil)

	// Mock CDP Send for screen dimensions (screen initialization happens even for empty offsets)
	dp := map[string]interface{}{
		"expression":    "({width: window.innerWidth, height: window.innerHeight})",
		"returnByValue": true,
	}
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", dp).
		Return(map[string]interface{}{
			"width":  1920.0,
			"height": 1080.0,
		}, nil)

	// Mock JSON unmarshaling
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(arguments), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			args := v.(*struct {
				MessageID     string `json:"messageID"`
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			args.MessageID = "test-msg-id"
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{}
			return nil
		})

	// Execute command - should return early without any CDP calls
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_MouseMoveEvent_CalculationScenarios(t *testing.T) {
	testCases := []struct {
		name                     string
		cursorOffsets            []map[string]interface{}
		expectedMagnitudes       []float64
		shouldClamp              bool
		expectedClampedValues    []struct{ dx, dy float64 }
		expectedBoundedPositions []struct{ x, y float64 }
		description              string
	}{
		{
			name: "NormalMovement",
			cursorOffsets: []map[string]interface{}{
				{"dx": 5.0, "dy": 3.0},
				{"dx": -2.0, "dy": 1.0},
			},
			expectedMagnitudes: []float64{5.830951894845301, 2.23606797749979}, // sqrt(5^2 + 3^2), sqrt((-2)^2 + 1^2)
			shouldClamp:        false,
			expectedClampedValues: []struct{ dx, dy float64 }{
				{dx: 5.0, dy: 3.0},  // first offset unchanged
				{dx: -2.0, dy: 1.0}, // second offset unchanged
			},
			expectedBoundedPositions: []struct{ x, y float64 }{
				{x: 965.0, y: 543.0}, // first position
				{x: 963.0, y: 544.0}, // final position
			},
			description: "Normal movement with small offsets",
		},
		{
			name: "LargeJumpClamping",
			cursorOffsets: []map[string]interface{}{
				{"dx": 200.0, "dy": 150.0}, // magnitude > 150, should clamp
			},
			expectedMagnitudes: []float64{250.0}, // sqrt(200^2 + 150^2)
			shouldClamp:        true,
			expectedClampedValues: []struct{ dx, dy float64 }{
				{dx: 25.0, dy: 25.0}, // assume clamped to max
			},
			expectedBoundedPositions: []struct{ x, y float64 }{
				{x: 985.0, y: 565.0}, // assume bounded position
			},
			description: "Large jump that should be clamped",
		},
		{
			name: "MixedMovement",
			cursorOffsets: []map[string]interface{}{
				{"dx": 10.0, "dy": 5.0},    // normal
				{"dx": 300.0, "dy": 200.0}, // should clamp
				{"dx": 3.0, "dy": -2.0},    // normal
			},
			expectedMagnitudes: []float64{11.180339887498949, 360.5551275463989, 3.605551275463989},
			shouldClamp:        true,
			expectedClampedValues: []struct{ dx, dy float64 }{
				{dx: 10.0, dy: 5.0},  // normal, unchanged
				{dx: 25.0, dy: 25.0}, // assume clamped
				{dx: 3.0, dy: -2.0},  // normal, unchanged
			},
			expectedBoundedPositions: []struct{ x, y float64 }{
				{x: 970.0, y: 545.0}, // after first
				{x: 995.0, y: 570.0}, // after second
				{x: 998.0, y: 568.0}, // final
			},
			description: "Mixed normal and large movements",
		},
		{
			name: "BoundaryMovement",
			cursorOffsets: []map[string]interface{}{
				{"dx": 1000.0, "dy": 1000.0}, // very large, should clamp and hit boundaries
			},
			expectedMagnitudes: []float64{1414.2135623730951}, // sqrt(1000^2 + 1000^2)
			shouldClamp:        true,
			expectedClampedValues: []struct{ dx, dy float64 }{
				{dx: 25.0, dy: 25.0}, // assume heavily clamped
			},
			expectedBoundedPositions: []struct{ x, y float64 }{
				{x: 985.0, y: 565.0}, // assume hit boundaries
			},
			description: "Movement that hits screen boundaries",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create new test setup for each subtest
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := commands.Command{
				Type: commands.CMD_MOUSE_DRAG_EVENT,
				Arguments: map[string]interface{}{
					"messageID":     "test-msg-id",
					"cursorOffsets": tc.cursorOffsets,
				},
			}

			// Create proper JSON string for cursor offsets
			offsetsJSON := `[`
			for i, offset := range tc.cursorOffsets {
				if i > 0 {
					offsetsJSON += `,`
				}
				offsetsJSON += fmt.Sprintf(`{"dx":%v,"dy":%v}`, offset["dx"], offset["dy"])
			}
			offsetsJSON += `]`
			arguments := fmt.Sprintf(`{"messageID":"test-msg-id","cursorOffsets":%s}`, offsetsJSON)

			// Mock CDP Send for screen dimensions
			dp := map[string]interface{}{
				"expression":    "({width: window.innerWidth, height: window.innerHeight})",
				"returnByValue": true,
			}
			ts.mockCDP.EXPECT().
				Send("Runtime.evaluate", dp).
				Return(map[string]interface{}{
					"width":  1920.0,
					"height": 1080.0,
				}, nil)

			// Mock JSON marshaling
			ts.mockJSON.EXPECT().
				Marshal(cmd.Arguments).
				Return([]byte(arguments), nil)

			// Mock JSON unmarshaling
			ts.mockJSON.EXPECT().
				Unmarshal([]byte(arguments), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					args := v.(*struct {
						MessageID     string `json:"messageID"`
						CursorOffsets []struct {
							DX float64 `json:"dx"`
							DY float64 `json:"dy"`
						} `json:"cursorOffsets"`
					})
					args.MessageID = "test-msg-id"

					// Convert interface{} to struct
					offsets := make([]struct {
						DX float64 `json:"dx"`
						DY float64 `json:"dy"`
					}, len(tc.cursorOffsets))
					for i, offset := range tc.cursorOffsets {
						offsets[i].DX = offset["dx"].(float64)
						offsets[i].DY = offset["dy"].(float64)
					}
					args.CursorOffsets = offsets
					return nil
				})

			// Mock math operations for magnitude calculations
			for i, magnitude := range tc.expectedMagnitudes {
				offset := tc.cursorOffsets[i]
				dx := offset["dx"].(float64)
				dy := offset["dy"].(float64)
				ts.mockMath.EXPECT().Sqrt(dx*dx + dy*dy).Return(magnitude)
			}

			// Mock clamping operations if test case requires it
			if tc.shouldClamp {
				for i, offset := range tc.cursorOffsets {
					if tc.expectedMagnitudes[i] > 150 {
						// Mock the actual sequence: Min(maxOffset, offset) then Max(-maxOffset, result)
						dx := offset["dx"].(float64)
						dy := offset["dy"].(float64)

						// For DX: Min(25.0, original_dx) -> intermediate, then Max(-25.0, intermediate) -> final
						ts.mockMath.EXPECT().Min(25.0, dx).Return(tc.expectedClampedValues[i].dx)
						ts.mockMath.EXPECT().Max(-25.0, tc.expectedClampedValues[i].dx).Return(tc.expectedClampedValues[i].dx)

						// For DY: Min(25.0, original_dy) -> intermediate, then Max(-25.0, intermediate) -> final
						ts.mockMath.EXPECT().Min(25.0, dy).Return(tc.expectedClampedValues[i].dy)
						ts.mockMath.EXPECT().Max(-25.0, tc.expectedClampedValues[i].dy).Return(tc.expectedClampedValues[i].dy)
					}
				}
			}

			// Mock bounds checking with assumed return values
			for _, pos := range tc.expectedBoundedPositions {
				// Mock bounds checking to return our assumed bounded positions
				ts.mockMath.EXPECT().Min(gomock.Any(), 1920.0).Return(pos.x)
				ts.mockMath.EXPECT().Max(0.0, pos.x).Return(pos.x)
				ts.mockMath.EXPECT().Min(gomock.Any(), 1080.0).Return(pos.y)
				ts.mockMath.EXPECT().Max(0.0, pos.y).Return(pos.y)
			}

			// Mock JSON marshal for positions - expect specific structure with test case positions
			expectedPositions := make([]map[string]float64, len(tc.expectedBoundedPositions))
			for i, pos := range tc.expectedBoundedPositions {
				expectedPositions[i] = map[string]float64{"x": pos.x, "y": pos.y}
			}
			expectedMarshalStruct := map[string]interface{}{
				"messageID": "test-msg-id",
				"message": map[string]interface{}{
					"command": "cursorUpdate",
					"request": map[string]interface{}{
						"positions": expectedPositions,
					},
				},
			}

			// Generate expected JSON string for the CDP call
			expectedJSON := `{"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[`
			for i, pos := range tc.expectedBoundedPositions {
				if i > 0 {
					expectedJSON += ","
				}
				expectedJSON += fmt.Sprintf(`{"x":%g,"y":%g}`, pos.x, pos.y)
			}
			expectedJSON += `]}}}`

			ts.mockJSON.EXPECT().
				Marshal(expectedMarshalStruct).
				Return([]byte(expectedJSON), nil)

			// Mock CDP Send for JavaScript cursor positions - expect specific expression
			ts.mockCDP.EXPECT().
				Send("Runtime.evaluate", map[string]interface{}{
					"expression": fmt.Sprintf("window.handleCDPRequest(%s)", expectedJSON),
				}).
				Return(nil, nil)

			// Mock CDP Send for mouse movement - expect specific final position
			finalPos := tc.expectedBoundedPositions[len(tc.expectedBoundedPositions)-1]
			var gotX, gotY float64
			ts.mockCDP.EXPECT().
				Send("Input.dispatchMouseEvent", map[string]interface{}{
					"type":       "mouseMoved",
					"x":          finalPos.x,
					"y":          finalPos.y,
					"button":     "none",
					"buttons":    0,
					"clickCount": 0,
				}).
				DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
					gotX = params["x"].(float64)
					gotY = params["y"].(float64)
					return nil, nil
				})

			// Execute command
			result, err := ts.executor.Execute(ts.ctx, cmd)
			assert.NoError(t, err)
			assert.Equal(t, devicectl.CmdOK, result)

			// Assert final position matches our assumed final position
			assert.InEpsilon(t, finalPos.x, gotX, 0.0001, "final X position should match")
			assert.InEpsilon(t, finalPos.y, gotY, 0.0001, "final Y position should match")
		})
	}
}

func TestExecutor_MouseMoveEvent_Errors(t *testing.T) {
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
					Return([]byte(`{"messageID":"test-msg-id","cursorOffsets":[{"dx":10.0,"dy":5.0}]}`), nil)

				// Mock CDP Send for screen dimensions
				dp := map[string]interface{}{
					"expression":    "({width: window.innerWidth, height: window.innerHeight})",
					"returnByValue": true,
				}
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", dp).
					Return(map[string]interface{}{
						"width":  1920.0,
						"height": 1080.0,
					}, nil)

				// Mock JSON unmarshaling to fail
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					Return(errors.New("json unmarshal failed"))
			},
			wantErr: "invalid arguments",
		},
		{
			name: "CDP screen dimensions failure (should succeed with defaults)",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"messageID":"test-msg-id","cursorOffsets":[{"dx":10.0,"dy":5.0}]}`), nil)

				// Mock CDP Send for screen dimensions to fail (gracefully handled)
				dp := map[string]interface{}{
					"expression":    "({width: window.innerWidth, height: window.innerHeight})",
					"returnByValue": true,
				}
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", dp).
					Return(nil, errors.New("cdp screen failed"))

				// Mock JSON unmarshaling success
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							MessageID     string `json:"messageID"`
							CursorOffsets []struct {
								DX float64 `json:"dx"`
								DY float64 `json:"dy"`
							} `json:"cursorOffsets"`
						})
						args.MessageID = "test-msg-id"
						args.CursorOffsets = []struct {
							DX float64 `json:"dx"`
							DY float64 `json:"dy"`
						}{
							{DX: 10.0, DY: 5.0},
						}
						return nil
					})

				// Mock math operations for default screen size
				ts.mockMath.EXPECT().Sqrt(125.0).Return(11.180339887498949)
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1920.0).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(545.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1080.0).Return(545.0).AnyTimes()

				// Mock JSON marshal for positions
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[{"x":970,"y":545}]}}}`), nil)

				// Mock CDP Send for JavaScript cursor positions
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", gomock.Any()).
					Return(nil, nil)

				// Mock CDP Send for mouse movement
				ts.mockCDP.EXPECT().
					Send("Input.dispatchMouseEvent", gomock.Any()).
					Return(nil, nil)
			},
			wantErr: "", // Should succeed despite screen error
		},
		{
			name: "JSON positions marshal failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"messageID":"test-msg-id","cursorOffsets":[{"dx":10.0,"dy":5.0}]}`), nil)

				// Mock CDP Send for screen dimensions
				dp := map[string]interface{}{
					"expression":    "({width: window.innerWidth, height: window.innerHeight})",
					"returnByValue": true,
				}
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", dp).
					Return(map[string]interface{}{
						"width":  1920.0,
						"height": 1080.0,
					}, nil)

				// Mock JSON unmarshaling success
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							MessageID     string `json:"messageID"`
							CursorOffsets []struct {
								DX float64 `json:"dx"`
								DY float64 `json:"dy"`
							} `json:"cursorOffsets"`
						})
						args.MessageID = "test-msg-id"
						args.CursorOffsets = []struct {
							DX float64 `json:"dx"`
							DY float64 `json:"dy"`
						}{
							{DX: 10.0, DY: 5.0},
						}
						return nil
					})

				// Mock math operations
				ts.mockMath.EXPECT().Sqrt(125.0).Return(11.180339887498949)
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1920.0).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(545.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1080.0).Return(545.0).AnyTimes()

				// Mock JSON marshal for positions to fail
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return(nil, errors.New("positions marshal failed"))
			},
			wantErr: "failed to marshal positions",
		},
		{
			name: "CDP JavaScript execution failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"messageID":"test-msg-id","cursorOffsets":[{"dx":10.0,"dy":5.0}]}`), nil)

				// Mock CDP Send for screen dimensions
				dp := map[string]interface{}{
					"expression":    "({width: window.innerWidth, height: window.innerHeight})",
					"returnByValue": true,
				}
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", dp).
					Return(map[string]interface{}{
						"width":  1920.0,
						"height": 1080.0,
					}, nil)

				// Mock JSON unmarshaling success
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							MessageID     string `json:"messageID"`
							CursorOffsets []struct {
								DX float64 `json:"dx"`
								DY float64 `json:"dy"`
							} `json:"cursorOffsets"`
						})
						args.MessageID = "test-msg-id"
						args.CursorOffsets = []struct {
							DX float64 `json:"dx"`
							DY float64 `json:"dy"`
						}{
							{DX: 10.0, DY: 5.0},
						}
						return nil
					})

				// Mock math operations
				ts.mockMath.EXPECT().Sqrt(125.0).Return(11.180339887498949)
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1920.0).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(545.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1080.0).Return(545.0).AnyTimes()

				// Mock JSON marshal for positions success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[{"x":970,"y":545}]}}}`), nil)

				// Mock CDP Send for JavaScript cursor positions to fail
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", gomock.Any()).
					Return(nil, errors.New("cdp javascript failed"))
			},
			wantErr: "failed to process cursor positions",
		},
		{
			name: "CDP mouse movement failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"messageID":"test-msg-id","cursorOffsets":[{"dx":10.0,"dy":5.0}]}`), nil)

				// Mock CDP Send for screen dimensions
				dp := map[string]interface{}{
					"expression":    "({width: window.innerWidth, height: window.innerHeight})",
					"returnByValue": true,
				}
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", dp).
					Return(map[string]interface{}{
						"width":  1920.0,
						"height": 1080.0,
					}, nil)

				// Mock JSON unmarshaling success
				ts.mockJSON.EXPECT().
					Unmarshal(gomock.Any(), gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						args := v.(*struct {
							MessageID     string `json:"messageID"`
							CursorOffsets []struct {
								DX float64 `json:"dx"`
								DY float64 `json:"dy"`
							} `json:"cursorOffsets"`
						})
						args.MessageID = "test-msg-id"
						args.CursorOffsets = []struct {
							DX float64 `json:"dx"`
							DY float64 `json:"dy"`
						}{
							{DX: 10.0, DY: 5.0},
						}
						return nil
					})

				// Mock math operations
				ts.mockMath.EXPECT().Sqrt(125.0).Return(11.180339887498949)
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1920.0).Return(970.0).AnyTimes()
				ts.mockMath.EXPECT().Max(0.0, gomock.Any()).Return(545.0).AnyTimes()
				ts.mockMath.EXPECT().Min(gomock.Any(), 1080.0).Return(545.0).AnyTimes()

				// Mock JSON marshal for positions success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[{"x":970,"y":545}]}}}`), nil)

				// Mock CDP Send for JavaScript cursor positions success
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", gomock.Any()).
					Return(nil, nil)

				// Mock CDP Send for mouse movement to fail
				ts.mockCDP.EXPECT().
					Send("Input.dispatchMouseEvent", gomock.Any()).
					Return(nil, errors.New("cdp mouse failed"))
			},
			wantErr: "failed to move mouse",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := commands.Command{
				Type: commands.CMD_MOUSE_DRAG_EVENT,
				Arguments: map[string]interface{}{
					"messageID": "test-msg-id",
					"cursorOffsets": []map[string]interface{}{
						{"dx": 10.0, "dy": 5.0},
					},
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
				assert.Equal(t, devicectl.CmdOK, result, "expected CmdOK result on success")
			}
		})
	}
}

func TestExecutor_MouseTapEvent_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up test data
	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type:      commands.CMD_MOUSE_TAP_EVENT,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock CDP Send for screen dimensions
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{
			"width":  screenWidth,
			"height": screenHeight,
		}, nil)

	// parseMouseButton default left
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Button string `json:"button"`
			})
			args.Button = ""
			return nil
		})

	// Mock CDP Send for mouse press
	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", downParams).
		Return(nil, nil)

	// Mock CDP Send for mouse release
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 1,
	}
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", upParams).
		Return(nil, nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_MouseTapEvent_RightButton(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up test data
	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_TAP_EVENT,
		Arguments: map[string]interface{}{
			"button": "right",
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"button":"right"}`), nil)

	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{
			"width":  screenWidth,
			"height": screenHeight,
		}, nil)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"button":"right"}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Button string `json:"button"`
			})
			args.Button = "right"
			return nil
		})

	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "right",
		"buttons":    2,
		"clickCount": 1,
	}
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", downParams).
		Return(nil, nil)

	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "right",
		"buttons":    0,
		"clickCount": 1,
	}
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", upParams).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_MouseTapEvent_InvalidButton(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_TAP_EVENT,
		Arguments: map[string]interface{}{
			"button": "nope",
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"button":"nope"}`), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": 1920.0, "height": 1080.0}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"button":"nope"}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			p := v.(*struct {
				Button string `json:"button"`
			})
			p.Button = "nope"
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unknown mouse button")
	assert.Nil(t, result)
}

func TestExecutor_MouseDoubleTapEvent_DefaultLeft(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type:      commands.CMD_MOUSE_DOUBLE_TAP_EVENT,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{
			"width":  screenWidth,
			"height": screenHeight,
		}, nil)

	// parseMouseButton default left
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Button string `json:"button"`
			})
			args.Button = ""
			return nil
		})

	firstDown := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	firstUp := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 1,
	}
	secondDown := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 2,
	}
	secondUp := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 2,
	}
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", firstDown).Return(nil, nil)
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", firstUp).Return(nil, nil)
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", secondDown).Return(nil, nil)
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", secondUp).Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_MouseDoubleTapEvent_ReleaseFailureAttemptsCleanup(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type:      commands.CMD_MOUSE_DOUBLE_TAP_EVENT,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{
			"width":  screenWidth,
			"height": screenHeight,
		}, nil)

	// parseMouseButton default left
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Button string `json:"button"`
			})
			args.Button = ""
			return nil
		})

	firstDown := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	firstUp := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 1,
	}

	releaseErr := errors.New("release failed")
	gomock.InOrder(
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", firstDown).Return(nil, nil),
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", firstUp).Return(nil, releaseErr),
		// cleanup attempt
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", firstUp).Return(nil, nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to release mouse button")
	assert.Nil(t, result)
}

func TestExecutor_MouseLongPressEvent_SleepsAndReleases(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type:      commands.CMD_MOUSE_LONG_PRESS_EVENT,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{
			"width":  screenWidth,
			"height": screenHeight,
		}, nil)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Button string `json:"button"`
			})
			args.Button = ""
			return nil
		})

	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 1,
	}

	gomock.InOrder(
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", downParams).Return(nil, nil),
		ts.mockClock.EXPECT().Sleep(1*time.Second),
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", upParams).Return(nil, nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_MouseLongPressEvent_ReleaseFailureAttemptsCleanup(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type:      commands.CMD_MOUSE_LONG_PRESS_EVENT,
		Arguments: map[string]interface{}{},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{
			"width":  screenWidth,
			"height": screenHeight,
		}, nil)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Button string `json:"button"`
			})
			args.Button = ""
			return nil
		})

	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 1,
	}

	releaseErr := errors.New("release failed")
	gomock.InOrder(
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", downParams).Return(nil, nil),
		ts.mockClock.EXPECT().Sleep(1*time.Second),
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", upParams).Return(nil, releaseErr),
		// cleanup attempt
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", upParams).Return(nil, nil),
	)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to release mouse button")
	assert.Nil(t, result)
}

func TestExecutor_MouseLongPressEvent_MiddleButton(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_LONG_PRESS_EVENT,
		Arguments: map[string]interface{}{
			"button": "middle",
		},
	}

	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"button":"middle"}`), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"button":"middle"}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				Button string `json:"button"`
			})
			args.Button = "middle"
			return nil
		})

	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "middle",
		"buttons":    4,
		"clickCount": 1,
	}
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          centerX,
		"y":          centerY,
		"button":     "middle",
		"buttons":    0,
		"clickCount": 1,
	}
	gomock.InOrder(
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", downParams).Return(nil, nil),
		ts.mockClock.EXPECT().Sleep(1*time.Second),
		ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", upParams).Return(nil, nil),
	)
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_MouseClickAndDragEvent_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT,
		Arguments: map[string]interface{}{
			"messageID": "test-msg-id",
			"cursorOffsets": []map[string]interface{}{
				{"dx": 10.0, "dy": 5.0},
			},
		},
	}

	argsJSON := `{"messageID":"test-msg-id","cursorOffsets":[{"dx":10.0,"dy":5.0}]}`
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(argsJSON), nil)

	// Screen init
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{
			"width":  screenWidth,
			"height": screenHeight,
		}, nil)

	// First unmarshal: clickAndDrag checks offsets are non-empty
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{
				{DX: 10.0, DY: 5.0},
			}
			return nil
		})

	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", downParams).
		Return(nil, nil)

	// Second unmarshal: handleMouseMoveEventWithButtons does full decode
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args := v.(*struct {
				MessageID     string `json:"messageID"`
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			args.MessageID = "test-msg-id"
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{
				{DX: 10.0, DY: 5.0},
			}
			return nil
		})

	// magnitude sqrt(10^2 + 5^2)
	ts.mockMath.EXPECT().Sqrt(125.0).Return(11.180339887498949)

	// bounds after one step: (960,540) -> (970,545)
	ts.mockMath.EXPECT().Min(970.0, 1920.0).Return(970.0)
	ts.mockMath.EXPECT().Max(0.0, 970.0).Return(970.0)
	ts.mockMath.EXPECT().Min(545.0, 1080.0).Return(545.0)
	ts.mockMath.EXPECT().Max(0.0, 545.0).Return(545.0)

	ts.mockJSON.EXPECT().
		Marshal(map[string]interface{}{
			"messageID": "test-msg-id",
			"message": map[string]interface{}{
				"command": "cursorUpdate",
				"request": map[string]interface{}{
					"positions": []map[string]float64{
						{"x": 970.0, "y": 545.0},
					},
				},
			},
		}).
		Return([]byte(`{"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[{"x":970,"y":545}]}}}`), nil)

	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", map[string]interface{}{
			"expression": `window.handleCDPRequest({"messageID":"test-msg-id","message":{"command":"cursorUpdate","request":{"positions":[{"x":970,"y":545}]}}})`,
		}).
		Return(nil, nil)

	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":       "mouseMoved",
			"x":          970.0,
			"y":          545.0,
			"button":     "none",
			"buttons":    1,
			"clickCount": 0,
		}).
		Return(nil, nil)

	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":       "mouseReleased",
			"x":          970.0,
			"y":          545.0,
			"button":     "left",
			"buttons":    0,
			"clickCount": 1,
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_MouseClickAndDragEvent_ReleaseFailureReturnsError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT,
		Arguments: map[string]interface{}{
			"messageID": "release-failure",
			"cursorOffsets": []map[string]interface{}{
				{"dx": 2.0, "dy": 0.0},
			},
		},
	}
	argsJSON := `{"messageID":"release-failure","cursorOffsets":[{"dx":2.0,"dy":0.0}]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args, ok := v.(*struct {
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			if !ok {
				return errors.New("unexpected unmarshal type (click-and-drag probe)")
			}
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{{DX: 2, DY: 0}}
			return nil
		}).
		Times(1)
	down := map[string]interface{}{
		"type":       "mousePressed",
		"x":          centerX,
		"y":          centerY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", down).Return(nil, nil)
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args, ok := v.(*struct {
				MessageID     string `json:"messageID"`
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			if !ok {
				return errors.New("unexpected unmarshal type (click-and-drag move)")
			}
			args.MessageID = "release-failure"
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{{DX: 2, DY: 0}}
			return nil
		})
	ts.mockMath.EXPECT().Sqrt(4.0).Return(2.0)
	ts.mockMath.EXPECT().Min(962.0, 1920.0).Return(962.0)
	ts.mockMath.EXPECT().Max(0.0, 962.0).Return(962.0)
	ts.mockMath.EXPECT().Min(540.0, 1080.0).Return(540.0)
	ts.mockMath.EXPECT().Max(0.0, 540.0).Return(540.0)
	ts.mockJSON.EXPECT().
		Marshal(map[string]interface{}{
			"messageID": "release-failure",
			"message": map[string]interface{}{
				"command": "cursorUpdate",
				"request": map[string]interface{}{
					"positions": []map[string]float64{{"x": 962.0, "y": 540.0}},
				},
			},
		}).
		Return([]byte(`{"messageID":"release-failure","message":{"command":"cursorUpdate","request":{"positions":[{"x":962,"y":540}]}}}`), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", map[string]interface{}{
			"expression": `window.handleCDPRequest({"messageID":"release-failure","message":{"command":"cursorUpdate","request":{"positions":[{"x":962,"y":540}]}}})`,
		}).
		Return(nil, nil)
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":       "mouseMoved",
			"x":          962.0,
			"y":          540.0,
			"button":     "none",
			"buttons":    1,
			"clickCount": 0,
		}).
		Return(nil, nil)
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":       "mouseReleased",
			"x":          962.0,
			"y":          540.0,
			"button":     "left",
			"buttons":    0,
			"clickCount": 1,
		}).
		Return(nil, errors.New("release failed"))

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to release mouse button")
	assert.Nil(t, result)
}

func TestExecutor_MouseClickAndDragEvent_ReleasesOnMoveError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT,
		Arguments: map[string]interface{}{
			"messageID": "m1",
			"cursorOffsets": []map[string]interface{}{
				{"dx": 1.0, "dy": 0.0},
			},
		},
	}
	argsJSON := `{"messageID":"m1","cursorOffsets":[{"dx":1.0,"dy":0.0}]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args, ok := v.(*struct {
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			if !ok {
				return errors.New("unexpected unmarshal type (click-and-drag probe)")
			}
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{{DX: 1, DY: 0}}
			return nil
		}).
		Times(1)
	down := map[string]interface{}{
		"type": "mousePressed", "x": centerX, "y": centerY,
		"button": "left", "buttons": 1, "clickCount": 1,
	}
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", down).Return(nil, nil)
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		Return(errors.New("unmarshal move failed")).
		Times(1)
	// best-effort release at current cursor (still at center) when move step fails
	release := map[string]interface{}{
		"type": "mouseReleased", "x": centerX, "y": centerY,
		"button": "left", "buttons": 0, "clickCount": 1,
	}
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", release).Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "unmarshal move failed")
	assert.Nil(t, result)
}

func TestExecutor_MouseClickAndDragEvent_MoveErrorAndCleanupReleaseErrorJoined(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT,
		Arguments: map[string]interface{}{
			"messageID": "m2",
			"cursorOffsets": []map[string]interface{}{
				{"dx": 1.0, "dy": 0.0},
			},
		},
	}
	argsJSON := `{"messageID":"m2","cursorOffsets":[{"dx":1.0,"dy":0.0}]}`

	moveRoot := errors.New("unmarshal move failed")
	cleanupRoot := errors.New("cdp cleanup release failed")

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			args, ok := v.(*struct {
				CursorOffsets []struct {
					DX float64 `json:"dx"`
					DY float64 `json:"dy"`
				} `json:"cursorOffsets"`
			})
			if !ok {
				return errors.New("unexpected unmarshal type (click-and-drag probe)")
			}
			args.CursorOffsets = []struct {
				DX float64 `json:"dx"`
				DY float64 `json:"dy"`
			}{{DX: 1, DY: 0}}
			return nil
		}).
		Times(1)
	down := map[string]interface{}{
		"type": "mousePressed", "x": centerX, "y": centerY,
		"button": "left", "buttons": 1, "clickCount": 1,
	}
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", down).Return(nil, nil)
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		Return(moveRoot).
		Times(1)
	release := map[string]interface{}{
		"type": "mouseReleased", "x": centerX, "y": centerY,
		"button": "left", "buttons": 0, "clickCount": 1,
	}
	ts.mockCDP.EXPECT().Send("Input.dispatchMouseEvent", release).Return(nil, cleanupRoot)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.True(t, errors.Is(err, moveRoot), "expected move error in joined result")
	assert.True(t, errors.Is(err, cleanupRoot), "expected cleanup release error in joined result")
}

func TestExecutor_ZoomGestureEvent_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.05, 0.98},
		},
	}
	argsJSON := `{"scaleSteps":[1.05,0.98]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.05, 0.98}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.05,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, nil)
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       0.98,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_WithMessageID(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"messageID":  "z1",
			"scaleSteps": []float64{1.1},
		},
	}
	argsJSON := `{"messageID":"z1","scaleSteps":[1.1]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.MessageID = "z1"
			in.ScaleSteps = []float64{1.1}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.1,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_EmptyScaleSteps(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type:      commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{"scaleSteps": []float64{}},
	}
	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(`{"scaleSteps":[]}`), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": 1920.0, "height": 1080.0}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(`{"scaleSteps":[]}`), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = nil
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_CDPError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.01},
		},
	}
	argsJSON := `{"scaleSteps":[1.01]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.01}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.01,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, errors.New("cdp pinch failed"))

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to process zoom gesture")
	assert.Nil(t, result)
}

func TestExecutor_ZoomGestureEvent_UnsupportedExperimentalMethod(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.02},
		},
	}
	argsJSON := `{"scaleSteps":[1.02]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.02}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.02,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, &cdp.RemoteError{Method: "Input.synthesizePinchGesture", Description: "unknown method", Unsupported: true})
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":      "mouseWheel",
			"x":         centerX,
			"y":         centerY,
			"deltaX":    0,
			"deltaY":    -120.0,
			"button":    "none",
			"buttons":   0,
			"modifiers": 2,
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_PinchUnsupportedUsesFallback(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{0.98},
		},
	}
	argsJSON := `{"scaleSteps":[0.98]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{0.98}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       0.98,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, &cdp.RemoteError{Method: "Input.synthesizePinchGesture", Description: "Method not found", Unsupported: true})
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":      "mouseWheel",
			"x":         centerX,
			"y":         centerY,
			"deltaX":    0,
			"deltaY":    120.0,
			"button":    "none",
			"buttons":   0,
			"modifiers": 2,
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_NeutralScaleDoesNotFallback(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.0},
		},
	}
	argsJSON := `{"scaleSteps":[1]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.0}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.0,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, &cdp.RemoteError{Method: "Input.synthesizePinchGesture", Description: "Method not found", Unsupported: true})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_FallbackMagnitudeGrowsWithScale(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.5},
		},
	}
	argsJSON := `{"scaleSteps":[1.5]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.5}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.5,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, &cdp.RemoteError{Method: "Input.synthesizePinchGesture", Description: "unknown method", Unsupported: true})
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":      "mouseWheel",
			"x":         centerX,
			"y":         centerY,
			"deltaX":    0,
			"deltaY":    -600.0,
			"button":    "none",
			"buttons":   0,
			"modifiers": 2,
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_PinchNotSupportedFails(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.02},
		},
	}
	argsJSON := `{"scaleSteps":[1.02]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.02}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.02,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, errors.New("feature not supported"))

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to process zoom gesture")
	assert.Nil(t, result)
}

func TestExecutor_ZoomGestureEvent_InvalidScaleStepFailsFast(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{0},
		},
	}
	argsJSON := `{"scaleSteps":[0]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": 1920.0, "height": 1080.0}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{0}
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scaleSteps must be positive")
	assert.Nil(t, result)
}

func TestExecutor_ZoomGestureEvent_InvalidScaleStepFailsBeforeDispatch(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.05, 0},
		},
	}
	argsJSON := `{"scaleSteps":[1.05,0]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": 1920.0, "height": 1080.0}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.05, 0}
			return nil
		})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "scaleSteps must be positive")
	assert.Nil(t, result)
}

func TestExecutor_ZoomGestureEvent_UnsupportedWordingFallbacks(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{0.98},
		},
	}
	argsJSON := `{"scaleSteps":[0.98]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{0.98}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       0.98,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, &cdp.RemoteError{Method: "Input.synthesizePinchGesture", Description: "wasn't found", Unsupported: true})
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":      "mouseWheel",
			"x":         centerX,
			"y":         centerY,
			"deltaX":    0,
			"deltaY":    120.0,
			"button":    "none",
			"buttons":   0,
			"modifiers": 2,
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_UnsupportedMethodWithNameFallbacks(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.02},
		},
	}
	argsJSON := `{"scaleSteps":[1.02]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.02}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.02,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, &cdp.RemoteError{Method: "Input.synthesizePinchGesture", Description: "method not found", Unsupported: true})
	ts.mockCDP.EXPECT().
		Send("Input.dispatchMouseEvent", map[string]interface{}{
			"type":      "mouseWheel",
			"x":         centerX,
			"y":         centerY,
			"deltaX":    0,
			"deltaY":    -120.0,
			"button":    "none",
			"buttons":   0,
			"modifiers": 2,
		}).
		Return(nil, nil)

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_ZoomGestureEvent_TargetNotFoundDoesNotFallback(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := commands.Command{
		Type: commands.CMD_ZOOM_GESTURE,
		Arguments: map[string]interface{}{
			"scaleSteps": []float64{1.02},
		},
	}
	argsJSON := `{"scaleSteps":[1.02]}`

	ts.mockJSON.EXPECT().Marshal(cmd.Arguments).Return([]byte(argsJSON), nil)
	ts.mockCDP.EXPECT().
		Send("Runtime.evaluate", gomock.Any()).
		Return(map[string]interface{}{"width": screenWidth, "height": screenHeight}, nil)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(argsJSON), gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			in, ok := v.(*struct {
				MessageID  string    `json:"messageID"`
				ScaleSteps []float64 `json:"scaleSteps"`
			})
			if !ok {
				return errors.New("unexpected type in zoomGesture unmarshal")
			}
			in.ScaleSteps = []float64{1.02}
			return nil
		})
	ts.mockCDP.EXPECT().
		Send("Input.synthesizePinchGesture", map[string]interface{}{
			"x":                 centerX,
			"y":                 centerY,
			"scaleFactor":       1.02,
			"relativeSpeed":     800,
			"gestureSourceType": "default",
		}).
		Return(nil, &cdp.RemoteError{Method: "Input.synthesizePinchGesture", Description: "Target not found", Unsupported: false})

	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to process zoom gesture")
	assert.Nil(t, result)
}

func TestExecutor_MouseTapEvent_Errors(t *testing.T) {
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
			name: "CDP screen dimensions failure (should succeed with defaults)",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{}`), nil)

				// Mock CDP Send for screen dimensions to fail (gracefully handled)
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", gomock.Any()).
					Return(nil, errors.New("cdp screen failed"))

				// parseMouseButton default left
				ts.mockJSON.EXPECT().
					Unmarshal([]byte(`{}`), gomock.Any()).
					DoAndReturn(func(_ []byte, v interface{}) error {
						args := v.(*struct {
							Button string `json:"button"`
						})
						args.Button = ""
						return nil
					})

				// Mock CDP Send for mouse press (should still work with default dimensions)
				ts.mockCDP.EXPECT().
					Send("Input.dispatchMouseEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						// Verify mousePressed event with default center position
						assert.Equal(t, "mousePressed", params["type"])
						assert.Equal(t, 960.0, params["x"]) // Default screen width 1920 / 2
						assert.Equal(t, 540.0, params["y"]) // Default screen height 1080 / 2
						assert.Equal(t, "left", params["button"])
						assert.Equal(t, 1, params["buttons"])
						assert.Equal(t, 1, params["clickCount"])
						return nil, nil
					})

				// Mock CDP Send for mouse release
				ts.mockCDP.EXPECT().
					Send("Input.dispatchMouseEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						// Verify mouseReleased event
						assert.Equal(t, "mouseReleased", params["type"])
						assert.Equal(t, 960.0, params["x"])
						assert.Equal(t, 540.0, params["y"])
						assert.Equal(t, "left", params["button"])
						assert.Equal(t, 0, params["buttons"])
						assert.Equal(t, 1, params["clickCount"])
						return nil, nil
					})
			},
			wantErr: "", // Should succeed despite screen error
		},
		{
			name: "CDP mouse press failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{}`), nil)

				// Mock CDP Send for screen dimensions success
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", gomock.Any()).
					Return(map[string]interface{}{
						"width":  1920.0,
						"height": 1080.0,
					}, nil)

				// parseMouseButton default left
				ts.mockJSON.EXPECT().
					Unmarshal([]byte(`{}`), gomock.Any()).
					DoAndReturn(func(_ []byte, v interface{}) error {
						args := v.(*struct {
							Button string `json:"button"`
						})
						args.Button = ""
						return nil
					})

				// Mock CDP Send for mouse press to fail
				ts.mockCDP.EXPECT().
					Send("Input.dispatchMouseEvent", gomock.Any()).
					Return(nil, errors.New("cdp mouse press failed"))
			},
			wantErr: "failed to press mouse button",
		},
		{
			name: "CDP mouse release failure",
			setupFunc: func(ts *testSetup) {
				// Mock JSON marshaling success
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte(`{}`), nil)

				// Mock CDP Send for screen dimensions success
				ts.mockCDP.EXPECT().
					Send("Runtime.evaluate", gomock.Any()).
					Return(map[string]interface{}{
						"width":  1920.0,
						"height": 1080.0,
					}, nil)

				// parseMouseButton default left
				ts.mockJSON.EXPECT().
					Unmarshal([]byte(`{}`), gomock.Any()).
					DoAndReturn(func(_ []byte, v interface{}) error {
						args := v.(*struct {
							Button string `json:"button"`
						})
						args.Button = ""
						return nil
					})

				releaseErr := errors.New("cdp mouse release failed")
				gomock.InOrder(
					// Mock CDP Send for mouse press success
					ts.mockCDP.EXPECT().
						Send("Input.dispatchMouseEvent", gomock.Any()).
						DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
							// Verify mousePressed event
							assert.Equal(t, "mousePressed", params["type"])
							assert.Equal(t, 960.0, params["x"]) // Screen center
							assert.Equal(t, 540.0, params["y"])
							assert.Equal(t, "left", params["button"])
							assert.Equal(t, 1, params["buttons"])
							assert.Equal(t, 1, params["clickCount"])
							return nil, nil
						}),
					// Mock CDP Send for mouse release to fail
					ts.mockCDP.EXPECT().
						Send("Input.dispatchMouseEvent", gomock.Any()).
						DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
							assert.Equal(t, "mouseReleased", params["type"])
							return nil, releaseErr
						}),
					// Cleanup: best-effort release retry
					ts.mockCDP.EXPECT().
						Send("Input.dispatchMouseEvent", gomock.Any()).
						DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
							assert.Equal(t, "mouseReleased", params["type"])
							return nil, nil
						}),
				)
			},
			wantErr: "failed to release mouse button",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := commands.Command{
				Type:      commands.CMD_MOUSE_TAP_EVENT,
				Arguments: map[string]interface{}{},
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
				assert.Equal(t, devicectl.CmdOK, result, "expected CmdOK result on success")
			}
		})
	}
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
			CommandContext(ts.ctx, "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".timer").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "sudo", "systemctl", "reset-failed", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "sudo", "systemctl", "start", "sshd.service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(
				ts.ctx,
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
			CommandContext(ts.ctx, "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".timer").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "sudo", "systemctl", "stop", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "sudo", "systemctl", "reset-failed", constants.SSH_DISABLE_UNIT+".service").
			Return(ts.mockExecCmd),
		ts.mockExecCmd.EXPECT().
			CombinedOutput().
			Return([]byte(""), nil),
		ts.mockExec.EXPECT().
			CommandContext(ts.ctx, "sudo", "systemctl", "stop", "sshd.service").
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

		saveComplete <- true
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

func TestExecutor_SystemUpdate_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_UPDATE_TO_LATEST,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock DBus call for factory reset
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_SYSTEM_UPDATE,
			Body:      []interface{}{},
		}).
		Return(nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_SystemUpdate_DBusError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_UPDATE_TO_LATEST,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock DBus call to fail
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, gomock.Any()).
		Return(errors.New("dbus error"))

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to send system update signal")
}

func TestExecutor_FactoryReset_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_FACTORY_RESET,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock DBus call for factory reset
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_FACTORY_RESET,
			Body:      []interface{}{},
		}).
		Return(nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
}

func TestExecutor_FactoryReset_DBusError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := commands.Command{
		Type:      commands.CMD_FACTORY_RESET,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock DBus call to fail
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, gomock.Any()).
		Return(errors.New("dbus error"))

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to send factory reset signal")
}

func TestExecutor_UploadLogs_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_UPLOAD_LOGS,
		Arguments: map[string]interface{}{
			"userId": "test-user-id",
			"apiKey": "test-api-key",
			"title":  "test-title",
		},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"userId":"test-user-id","apiKey":"test-api-key","title":"test-title"}`), nil)

	// Mock JSON unmarshaling
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			if args, ok := v.(*struct {
				UserID string `json:"userId"`
				APIKey string `json:"apiKey"`
				Title  string `json:"title"`
			}); ok {
				args.UserID = "test-user-id"
				args.APIKey = "test-api-key"
				args.Title = "test-title"
			}
			return nil
		})

	// Mock DBus call for upload logs
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_UPLOAD_LOGS,
			Body:      []interface{}{"test-user-id", "test-api-key", "test-title"},
		}).
		Return(nil)

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, devicectl.CmdOK, result)
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

func TestExecutor_UploadLogs_DBusError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Command{
		Type: commands.CMD_UPLOAD_LOGS,
		Arguments: map[string]interface{}{
			"userId": "test-user-id",
			"apiKey": "test-api-key",
			"title":  "test-title",
		},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"userId":"test-user-id","apiKey":"test-api-key","title":"test-title"}`), nil)

	// Mock JSON unmarshaling
	ts.mockJSON.EXPECT().
		Unmarshal(gomock.Any(), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			if args, ok := v.(*struct {
				UserID string `json:"userId"`
				APIKey string `json:"apiKey"`
				Title  string `json:"title"`
			}); ok {
				args.UserID = "test-user-id"
				args.APIKey = "test-api-key"
				args.Title = "test-title"
			}
			return nil
		})

	// Mock DBus call to fail
	ts.mockDBus.EXPECT().
		RetryableSend(ts.ctx, gomock.Any()).
		Return(errors.New("dbus error"))

	// Execute command
	result, err := ts.executor.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to send upload logs signal")
}

func TestExecutor_NewHandler(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDBus := mocks.NewMockDBus(ctrl)
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
		mockDBus,
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
