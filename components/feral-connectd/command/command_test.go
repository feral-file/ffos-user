package command_test

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"

	"github.com/Feral-File/ffos-user/components/feral-connectd/command"
	"github.com/Feral-File/ffos-user/components/feral-connectd/dbus"
	"github.com/Feral-File/ffos-user/components/feral-connectd/mocks"
	"github.com/Feral-File/ffos-user/components/feral-connectd/relayer"
	"github.com/Feral-File/ffos-user/components/feral-connectd/state"
	"github.com/Feral-File/ffos-user/components/feral-connectd/status"
	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type testSetup struct {
	ctrl             *gomock.Controller
	ctx              context.Context
	handler          command.CommandHandler
	mockCDP          *mocks.MockCDP
	mockDBus         *mocks.MockDBus
	mockStatus       *mocks.MockStatusPoller
	mockJSON         *mocks.MockJSON
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
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockExecCmd := mocks.NewMockExecCmd(ctrl)
	mockDeviceStatus := mocks.NewMockDeviceStatus(ctrl)
	mockMath := mocks.NewMockMath(ctrl)
	mockStateManager := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(mockStateManager)

	// Create handler with mocks
	handler := command.New(mockCDP, mockDBus, mockDeviceStatus, mockJSON, mockOS, mockExec, mockMath, logger)
	handler.SetStatusPoller(mockStatus)

	return &testSetup{
		ctrl:             ctrl,
		ctx:              ctx,
		handler:          handler,
		mockCDP:          mockCDP,
		mockDBus:         mockDBus,
		mockStatus:       mockStatus,
		mockJSON:         mockJSON,
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

func TestHandler_Execute_InvalidCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command: "invalid_command",
		Arguments: map[string]interface{}{
			"test": "value",
		},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{"test":"value"}`), nil)

	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid command")
}

func TestHandler_Execute_InvalidArguments(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command: relayer.CMD_CONNECT,
		Arguments: map[string]interface{}{
			"invalid": make(chan int), // This can't be marshaled to JSON
		},
	}

	// Mock JSON marshaling to fail
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return(nil, errors.New("json: unsupported type: chan int"))

	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "invalid arguments")
}

func TestHandler_Connect_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	primaryAddress := "192.168.1.100"
	device := command.Device{
		ID:       "test-device-id",
		Name:     "Test Device",
		Platform: 1,
	}

	cmd := command.Command{
		Command: relayer.CMD_CONNECT,
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
				Device         command.Device `json:"clientDevice"`
				PrimaryAddress string         `json:"primaryAddress"`
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)

	// Verify state was saved
	savedState := state.GetState()
	assert.NotNil(t, savedState.ConnectedDevice)
	assert.Equal(t, device.ID, savedState.ConnectedDevice.ID)
	assert.Equal(t, device.Name, savedState.ConnectedDevice.Name)
	assert.Equal(t, device.Platform, savedState.ConnectedDevice.Platform)
}

func TestHandler_Connect_Errors(t *testing.T) {
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
							Device         command.Device `json:"clientDevice"`
							PrimaryAddress string         `json:"primaryAddress"`
						})
						args.Device = command.Device{
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
			cmd := command.Command{
				Command: relayer.CMD_CONNECT,
				Arguments: map[string]interface{}{
					"clientDevice": command.Device{
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
			result, err := ts.handler.Execute(ts.ctx, cmd)

			// Assert error occurred and contains expected message
			assert.Error(t, err, "expected error, got %v", err)
			assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())
			assert.Nil(t, result, "expected nil result on error")
		})
	}
}

func TestHandler_ShowPairingQRCode_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command: relayer.CMD_SHOW_PAIRING_QR_CODE,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)
}

func TestHandler_ShowPairingQRCode_DBusError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command: relayer.CMD_SHOW_PAIRING_QR_CODE,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to send show pairing QR code")
}

func TestHandler_DeviceStatus_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_DEVICE_STATUS,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
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

func TestHandler_DeviceStatus_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_DEVICE_STATUS,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "device status error")
}

func TestHandler_KeyboardEvent_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	testCases := []struct {
		name           string
		keyCode        int
		expectedKey    string
		shouldGetKeyUp bool
		description    string
	}{
		// Printable ASCII characters (should get keyUp)
		{name: "LetterA", keyCode: 65, expectedKey: "A", shouldGetKeyUp: true, description: "Uppercase A"},
		{name: "LetterZ", keyCode: 90, expectedKey: "Z", shouldGetKeyUp: true, description: "Uppercase Z"},
		{name: "Lettera", keyCode: 97, expectedKey: "a", shouldGetKeyUp: true, description: "Lowercase a"},
		{name: "Letterz", keyCode: 122, expectedKey: "z", shouldGetKeyUp: true, description: "Lowercase z"},
		{name: "Number0", keyCode: 48, expectedKey: "0", shouldGetKeyUp: true, description: "Number 0"},
		{name: "Number9", keyCode: 57, expectedKey: "9", shouldGetKeyUp: true, description: "Number 9"},
		{name: "Tilde", keyCode: 126, expectedKey: "~", shouldGetKeyUp: true, description: "Tilde character (max printable ASCII)"},

		// Special keys that should NOT get keyUp (fixed behavior)
		{name: "Space", keyCode: 32, expectedKey: "space", shouldGetKeyUp: false, description: "Space key (special key, no keyUp)"},
		{name: "Tab", keyCode: 9, expectedKey: "tab", shouldGetKeyUp: false, description: "Tab key (special key, no keyUp)"},
		{name: "Enter", keyCode: 13, expectedKey: "return", shouldGetKeyUp: false, description: "Enter key (special key, no keyUp)"},
		{name: "Escape", keyCode: 27, expectedKey: "escape", shouldGetKeyUp: false, description: "Escape key (special key, no keyUp)"},
		{name: "Backspace", keyCode: 8, expectedKey: "backspace", shouldGetKeyUp: false, description: "Backspace key (special key, no keyUp)"},

		// Arrow keys now correctly mapped (fixed behavior)
		{name: "ArrowLeft", keyCode: 37, expectedKey: "left", shouldGetKeyUp: false, description: "Left arrow (special key, no keyUp)"},
		{name: "ArrowUp", keyCode: 38, expectedKey: "up", shouldGetKeyUp: false, description: "Up arrow (special key, no keyUp)"},
		{name: "ArrowRight", keyCode: 39, expectedKey: "right", shouldGetKeyUp: false, description: "Right arrow (special key, no keyUp)"},
		{name: "ArrowDown", keyCode: 40, expectedKey: "down", shouldGetKeyUp: false, description: "Down arrow (special key, no keyUp)"},

		// Edge cases
		{name: "BelowPrintable", keyCode: 31, expectedKey: "", shouldGetKeyUp: false, description: "Below printable ASCII range"},
		{name: "AbovePrintable", keyCode: 127, expectedKey: "", shouldGetKeyUp: false, description: "Above printable ASCII range"},
		{name: "UnknownKey", keyCode: 999, expectedKey: "", shouldGetKeyUp: false, description: "Unknown key code"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Setup test data
			cmd := command.Command{
				Command: relayer.CMD_KEYBOARD_EVENT,
				Arguments: map[string]interface{}{
					"code": tc.keyCode,
				},
			}

			arguments := fmt.Sprintf(`{"code":%d}`, tc.keyCode)

			// Mock JSON marshaling
			ts.mockJSON.EXPECT().
				Marshal(cmd.Arguments).
				Return([]byte(arguments), nil)

			// Mock JSON unmarshaling
			ts.mockJSON.EXPECT().
				Unmarshal([]byte(arguments), gomock.Any()).
				DoAndReturn(func(data []byte, v interface{}) error {
					args := v.(*struct {
						Code int `json:"code"`
					})
					args.Code = tc.keyCode
					return nil
				})

			// Mock CDP Send for keyDown
			ts.mockCDP.EXPECT().
				Send("Input.dispatchKeyEvent", gomock.Any()).
				DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
					// Verify keyDown parameters
					assert.Equal(t, "keyDown", params["type"])
					assert.Equal(t, tc.keyCode, params["windowsVirtualKeyCode"])
					assert.Equal(t, tc.expectedKey, params["key"])
					assert.Equal(t, tc.expectedKey, params["text"])
					assert.Equal(t, tc.expectedKey, params["unmodifiedText"])
					assert.Equal(t, tc.keyCode, params["nativeVirtualKeyCode"])
					return nil, nil
				})

			// Mock CDP Send for keyUp (only if shouldGetKeyUp is true)
			if tc.shouldGetKeyUp {
				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						// Verify keyUp parameters
						assert.Equal(t, "keyUp", params["type"])
						assert.Equal(t, tc.keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, tc.expectedKey, params["key"])
						assert.Equal(t, tc.expectedKey, params["text"])
						assert.Equal(t, tc.expectedKey, params["unmodifiedText"])
						assert.Equal(t, tc.keyCode, params["nativeVirtualKeyCode"])
						return nil, nil
					})
			}

			// Execute command
			result, err := ts.handler.Execute(ts.ctx, cmd)
			assert.NoError(t, err)
			assert.Equal(t, command.CmdOK, result)
		})
	}
}

func TestHandler_KeyboardEvent_Errors(t *testing.T) {
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

				// Mock JSON operations success
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

				// Mock CDP Send for keyDown to fail
				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						// Verify keyDown parameters
						assert.Equal(t, "keyDown", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, "A", params["key"])
						return nil, errors.New("cdp keyDown failed")
					})
			},
			wantErr: "failed to send keyboard event",
		},
		{
			name: "CDP keyUp failure for printable key (should succeed)",
			setupFunc: func(ts *testSetup) {
				keyCode := 65 // 'A' - printable ASCII

				// Mock JSON operations success
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

				// Mock CDP Send for keyDown success
				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						// Verify keyDown parameters
						assert.Equal(t, "keyDown", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, "A", params["key"])
						return nil, nil
					})

				// Mock CDP Send for keyUp to fail (but command should still succeed)
				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						// Verify keyUp parameters
						assert.Equal(t, "keyUp", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, "A", params["key"])
						return nil, errors.New("cdp keyUp failed")
					})
			},
			wantErr: "", // Should succeed despite keyUp failure
		},
		{
			name: "CDP keyDown failure for special key",
			setupFunc: func(ts *testSetup) {
				keyCode := 32 // Space - special key (no keyUp)

				// Mock JSON operations success
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

				// Mock CDP Send for keyDown to fail
				ts.mockCDP.EXPECT().
					Send("Input.dispatchKeyEvent", gomock.Any()).
					DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
						// Verify keyDown parameters
						assert.Equal(t, "keyDown", params["type"])
						assert.Equal(t, keyCode, params["windowsVirtualKeyCode"])
						assert.Equal(t, "space", params["key"])
						return nil, errors.New("cdp keyDown failed")
					})
				// No keyUp expected for special keys
			},
			wantErr: "failed to send keyboard event",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := command.Command{
				Command: relayer.CMD_KEYBOARD_EVENT,
				Arguments: map[string]interface{}{
					"code": 65, // Default value, overridden in setupFunc if needed
				},
			}

			// Setup error condition
			tt.setupFunc(ts)

			// Execute the method under test
			result, err := ts.handler.Execute(ts.ctx, cmd)

			// Assert error occurred and contains expected message or success
			if tt.wantErr != "" {
				assert.Error(t, err, "expected error, got %v", err)
				assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())
				assert.Nil(t, result, "expected nil result on error")
			} else {
				assert.NoError(t, err, "expected no error, got %v", err)
				assert.Equal(t, command.CmdOK, result, "expected CmdOK result on success")
			}
		})
	}
}

func TestHandler_MouseMoveEvent_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command: relayer.CMD_MOUSE_DRAG_EVENT,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)
	assert.InEpsilon(t, 985.0, gotX, 0.0001, "final X position should match")
	assert.InEpsilon(t, 542.0, gotY, 0.0001, "final Y position should match")
}

func TestHandler_MouseMoveEvent_EmptyOffsets(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := command.Command{
		Command: relayer.CMD_MOUSE_DRAG_EVENT,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)
}

func TestHandler_MouseMoveEvent_CalculationScenarios(t *testing.T) {
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
			cmd := command.Command{
				Command: relayer.CMD_MOUSE_DRAG_EVENT,
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
			result, err := ts.handler.Execute(ts.ctx, cmd)
			assert.NoError(t, err)
			assert.Equal(t, command.CmdOK, result)

			// Assert final position matches our assumed final position
			assert.InEpsilon(t, finalPos.x, gotX, 0.0001, "final X position should match")
			assert.InEpsilon(t, finalPos.y, gotY, 0.0001, "final Y position should match")
		})
	}
}

func TestHandler_MouseMoveEvent_Errors(t *testing.T) {
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
			cmd := command.Command{
				Command: relayer.CMD_MOUSE_DRAG_EVENT,
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
			result, err := ts.handler.Execute(ts.ctx, cmd)

			// Assert error occurred and contains expected message or success
			if tt.wantErr != "" {
				assert.Error(t, err, "expected error, got %v", err)
				assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())
				assert.Nil(t, result, "expected nil result on error")
			} else {
				assert.NoError(t, err, "expected no error, got %v", err)
				assert.Equal(t, command.CmdOK, result, "expected CmdOK result on success")
			}
		})
	}
}

func TestHandler_MouseTapEvent_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up test data
	screenWidth := 1920.0
	screenHeight := 1080.0
	centerX := screenWidth / 2
	centerY := screenHeight / 2

	cmd := command.Command{
		Command:   relayer.CMD_MOUSE_TAP_EVENT,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)
}

func TestHandler_MouseTapEvent_Errors(t *testing.T) {
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
					})

				// Mock CDP Send for mouse release to fail
				ts.mockCDP.EXPECT().
					Send("Input.dispatchMouseEvent", gomock.Any()).
					Return(nil, errors.New("cdp mouse release failed"))
			},
			wantErr: "failed to release mouse button",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Setup test data
			cmd := command.Command{
				Command:   relayer.CMD_MOUSE_TAP_EVENT,
				Arguments: map[string]interface{}{},
			}

			// Setup error condition
			tt.setupFunc(ts)

			// Execute the method under test
			result, err := ts.handler.Execute(ts.ctx, cmd)

			// Assert error occurred and contains expected message or success
			if tt.wantErr != "" {
				assert.Error(t, err, "expected error, got %v", err)
				assert.Contains(t, err.Error(), tt.wantErr, "expected error message to contain %q, got %q", tt.wantErr, err.Error())
				assert.Nil(t, result, "expected nil result on error")
			} else {
				assert.NoError(t, err, "expected no error, got %v", err)
				assert.Equal(t, command.CmdOK, result, "expected CmdOK result on success")
			}
		})
	}
}

func TestHandler_ScreenRotation_Success(t *testing.T) {
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
			cmd := command.Command{
				Command: relayer.CMD_SCREEN_ROTATION,
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
			configPath := "/home/feralfile/.config/screen-orientation"
			if tc.configFileReadError != nil {
				ts.mockOS.EXPECT().
					ReadFile(configPath).
					Return(nil, tc.configFileReadError)
			} else {
				ts.mockOS.EXPECT().
					ReadFile(configPath).
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
				WriteFile(configPath, []byte(tc.expectedNewRotation), os.FileMode(0600)).
				Return(nil)

			// Mock status poller force refresh
			ts.mockStatus.EXPECT().
				ForceRefresh()

			// Execute command
			result, err := ts.handler.Execute(ts.ctx, cmd)
			assert.NoError(t, err)
			assert.NotNil(t, result)

			// Verify result contains expected orientation
			resultMap, ok := result.(map[string]string)
			assert.True(t, ok)
			assert.Equal(t, tc.expectedOrientation, resultMap["orientation"])
		})
	}
}

func TestHandler_ScreenRotation_Errors(t *testing.T) {
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
				configPath := "/home/feralfile/.config/screen-orientation"
				ts.mockOS.EXPECT().
					ReadFile(configPath).
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
				configPath := "/home/feralfile/.config/screen-orientation"
				ts.mockOS.EXPECT().
					ReadFile(configPath).
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
					WriteFile(configPath, []byte("270"), os.FileMode(0600)).
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
				configPath := "/home/feralfile/.config/screen-orientation"
				ts.mockOS.EXPECT().
					ReadFile(configPath).
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
					WriteFile(configPath, []byte("270"), os.FileMode(0600)).
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
			cmd := command.Command{
				Command: relayer.CMD_SCREEN_ROTATION,
				Arguments: map[string]interface{}{
					"clockwise": true, // Default value, overridden in setupFunc if needed
				},
			}

			// Setup error condition
			tt.setupFunc(ts)

			// Execute the method under test
			result, err := ts.handler.Execute(ts.ctx, cmd)

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

func TestHandler_Shutdown_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_SHUTDOWN,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)
}

func TestHandler_Shutdown_CommandError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_SHUTDOWN,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to execute shutdown command")
}

func TestHandler_Reboot_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_REBOOT,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)
}

func TestHandler_Reboot_CommandError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_REBOOT,
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
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to execute reboot command")
	assert.Nil(t, result)
}

func TestHandler_GetSysMetrics_Success(t *testing.T) {
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
				ts.handler.SaveLastSysMetrics(testMetrics)

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
			cmd := command.Command{
				Command:   relayer.RELAYER_CMD_SYS_METRICS,
				Arguments: map[string]interface{}{},
			}

			// Setup test conditions
			tt.setupFunc(ts)

			// Execute command
			result, err := ts.handler.Execute(ts.ctx, cmd)
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

func TestHandler_GetSysMetrics_Failure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.RELAYER_CMD_SYS_METRICS,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling for command arguments
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return([]byte(`{}`), nil)

	// Set invalid JSON metrics
	invalidMetrics := []byte(`{"cpu": invalid_json}`)
	ts.handler.SaveLastSysMetrics(invalidMetrics)

	// Mock JSON unmarshaling to fail
	ts.mockJSON.EXPECT().
		Unmarshal(invalidMetrics, gomock.Any()).
		Return(errors.New("json unmarshal failed"))

	// Execute command
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err, "expected error when JSON unmarshal fails")
	assert.Contains(t, err.Error(), "failed to unmarshal last sys metrics", "error should mention unmarshal failure")
	assert.Nil(t, result, "expected nil result on error")
}

func TestHandler_SysMetrics_ConcurrentAccess(t *testing.T) {
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

		cmd := command.Command{
			Command:   relayer.RELAYER_CMD_SYS_METRICS,
			Arguments: map[string]interface{}{},
		}

		firstResult, firstErr = ts.handler.Execute(ts.ctx, cmd)
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
		ts.handler.SaveLastSysMetrics(testMetrics)

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

		cmd := command.Command{
			Command:   relayer.RELAYER_CMD_SYS_METRICS,
			Arguments: map[string]interface{}{},
		}

		secondResult, secondErr = ts.handler.Execute(ts.ctx, cmd)
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

func TestHandler_UpdateToLatest_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_UPDATE_TO_LATEST,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock exec.CommandContext for systemctl
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "systemctl", "start", "feral-updater@00:00.service").
		Return(ts.mockExecCmd)

	// Mock cmd.Run() to succeed
	ts.mockExecCmd.EXPECT().
		Run().
		Return(nil)

	// Execute command
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.NoError(t, err)
	assert.Equal(t, command.CmdOK, result)
}

func TestHandler_UpdateToLatest_CommandError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Setup test data
	cmd := command.Command{
		Command:   relayer.CMD_UPDATE_TO_LATEST,
		Arguments: map[string]interface{}{},
	}

	// Mock JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(cmd.Arguments).
		Return([]byte(`{}`), nil)

	// Mock exec.CommandContext for systemctl to fail
	ts.mockExec.EXPECT().
		CommandContext(ts.ctx, "systemctl", "start", "feral-updater@00:00.service").
		Return(ts.mockExecCmd)

	// Mock cmd.Run() to fail
	ts.mockExecCmd.EXPECT().
		Run().
		Return(errors.New("update to latest command failed"))

	// Execute command
	result, err := ts.handler.Execute(ts.ctx, cmd)
	assert.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "failed to execute update to latest command")
}

func TestHandler_NewHandler(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDBus := mocks.NewMockDBus(ctrl)
	mockDeviceStatus := mocks.NewMockDeviceStatus(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	mockMath := mocks.NewMockMath(ctrl)

	handler := command.New(mockCDP, mockDBus, mockDeviceStatus, mockJSON, mockOS, mockExec, mockMath, logger)
	assert.NotNil(t, handler)
}
