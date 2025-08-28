package mediator_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-connectd/cdp"
	"github.com/feral-file/ffos-user/components/feral-connectd/command"
	"github.com/feral-file/ffos-user/components/feral-connectd/dbus"
	"github.com/feral-file/ffos-user/components/feral-connectd/mediator"
	"github.com/feral-file/ffos-user/components/feral-connectd/mocks"
	"github.com/feral-file/ffos-user/components/feral-connectd/relayer"
	"github.com/feral-file/ffos-user/components/feral-connectd/state"
)

type testSetup struct {
	ctrl             *gomock.Controller
	ctx              context.Context
	mockRelayer      *mocks.MockRelayer
	mockDbus         *mocks.MockDBus
	mockCDP          *mocks.MockCDP
	mockCmd          *mocks.MockCommandHandler
	mockStatusPoller *mocks.MockStatusPoller
	mockClock        *mocks.MockClock
	mediator         mediator.Mediator
	logger           *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockDbus := mocks.NewMockDBus(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	med := mediator.New(
		mockRelayer,
		mockDbus,
		mockCDP,
		mockCmd,
		mockClock,
		logger,
	)

	return &testSetup{
		ctrl:             ctrl,
		ctx:              ctx,
		mockRelayer:      mockRelayer,
		mockDbus:         mockDbus,
		mockCDP:          mockCDP,
		mockCmd:          mockCmd,
		mockStatusPoller: mockStatusPoller,
		mockClock:        mockClock,
		mediator:         med,
		logger:           logger,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

func TestMediator_StartStop(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect Start to register handlers
	ts.mockDbus.EXPECT().OnBusSignal(gomock.Any()).Times(1)
	ts.mockRelayer.EXPECT().OnRelayerMessage(gomock.Any()).Times(1)

	// Expect Stop to remove handlers
	ts.mockRelayer.EXPECT().RemoveRelayerMessage(gomock.Any()).Times(1)
	ts.mockDbus.EXPECT().RemoveBusSignal(gomock.Any()).Times(1)

	// Test Start
	ts.mediator.Start()

	// Test Stop
	ts.mediator.Stop()
}

func TestMediator_HandleDBusSignal_SysMetrics(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (godbus.DBusPayload, error)
	}{
		{
			name: "valid sysmetrics payload",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				metricsData := []byte(`{"cpu": 50, "memory": 75}`)

				ts.mockCmd.EXPECT().
					SaveLastSysMetrics(metricsData).
					Times(1)

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_SYSMETRICS,
					Body:   []interface{}{metricsData},
				}

				return payload, nil
			},
		},
		{
			name: "invalid number of arguments",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_SYSMETRICS,
					Body:   []interface{}{[]byte("data"), "extra"},
				}

				return payload, errors.New("invalid number of arguments")
			},
		},
		{
			name: "invalid body type",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_SYSMETRICS,
					Body:   []interface{}{"not-bytes"},
				}

				return payload, errors.New("invalid body type")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload, expectedError := tt.setupFunc(ts)

			// Register handler
			var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
					capturedHandler = handler
				}).Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).
				Times(1)

			ts.mediator.Start()

			// Call handler directly
			result, err := capturedHandler(ts.ctx, payload)

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
				assert.Nil(t, result)
			}
		})
	}
}

func TestMediator_HandleDBusSignal_ConnectivityChange(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (godbus.DBusPayload, error)
	}{
		{
			name: "connectivity gained - relayer not connected",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(true)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				// Expect IsConnected to be called
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
					Times(1)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					Return(nil).
					Times(1)

				// Expect payload to be sent
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{true},
				}

				return payload, nil
			},
		},
		{
			name: "connectivity gained - relayer already connected",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(true)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				// Expect IsConnected to be called
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(true).
					Times(1)

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{true},
				}

				return payload, nil
			},
		},
		{
			name: "connectivity lost",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(false)",
					}).
					Return(map[string]interface{}{"result": "ok"}, nil).
					Times(1)

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{false},
				}

				return payload, nil
			},
		},
		{
			name: "CDP send error",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				cdpError := errors.New("CDP send failed")

				// Expect CDP send to be called and return error
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleConnectivityChange(true)",
					}).
					Return(nil, cdpError).
					Times(1)

				// Expect IsConnected to be called
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
					Times(1)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					Return(nil).
					Times(1)

				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{true},
				}

				return payload, nil // Should not return error despite CDP failure
			},
		},
		{
			name: "invalid body type",
			setupFunc: func(ts *testSetup) (godbus.DBusPayload, error) {
				payload := godbus.DBusPayload{
					Member: dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE,
					Body:   []interface{}{"not-bool"},
				}

				return payload, errors.New("invalid body type")
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload, expectedError := tt.setupFunc(ts)

			// Register handler
			var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
					capturedHandler = handler
				}).Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).
				Times(1)

			ts.mediator.Start()

			// Call handler directly
			result, err := capturedHandler(ts.ctx, payload)

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
				assert.Nil(t, result)
			}
		})
	}
}

func TestMediator_HandleDBusSignal_ACKAndUnknown(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test unknown signal - we'll use a known signal type but test the warning case
	payload := godbus.DBusPayload{
		Member: dbus.SETUPD_EVENT_SHOW_PAIRING_QR_CODE, // This is an unknown signal for the mediator
		Body:   []interface{}{},
	}

	// Register handler
	var capturedHandler func(context.Context, godbus.DBusPayload) ([]interface{}, error)
	ts.mockDbus.EXPECT().
		OnBusSignal(gomock.Any()).
		DoAndReturn(func(handler func(context.Context, godbus.DBusPayload) ([]interface{}, error)) {
			capturedHandler = handler
		}).Times(1)

	// Expect OnRelayerMessage to be called
	ts.mockRelayer.EXPECT().
		OnRelayerMessage(gomock.Any()).
		Times(1)

	ts.mediator.Start()

	// Call handler directly
	result, err := capturedHandler(ts.ctx, payload)

	// Verify - unknown signals should return nil without error
	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestMediator_HandleRelayerMessage_System(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (relayer.Payload, error)
	}{
		{
			name: "valid system message",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				topicID := "test-topic-id"
				s := &state.State{
					Relayer: &state.RelayerState{
						TopicID: topicID,
					},
				}

				// Mock state manager to return empty state
				mockStateManager := mocks.NewMockStateManager(ts.ctrl)
				mockStateManager.EXPECT().
					GetState().
					Return(s).
					Times(1)

				// Expect Save to be called
				mockStateManager.EXPECT().
					Save(s).
					Return(nil).
					Times(1)

				// Inject mock state manager
				state.InjectStateManagerForTesting(mockStateManager)

				payload := relayer.Payload{
					MessageID: relayer.MESSAGE_ID_SYSTEM,
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						TopicID: &topicID,
					},
				}

				return payload, nil
			},
		},
		{
			name: "system message missing topic ID",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				payload := relayer.Payload{
					MessageID: relayer.MESSAGE_ID_SYSTEM,
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						TopicID: nil,
					},
				}

				return payload, errors.New("payload doesn't contain topicID")
			},
		},
		{
			name: "system message state save error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				topicID := "test-topic-id"
				saveError := errors.New("save failed")

				// Mock state manager
				mockStateManager := mocks.NewMockStateManager(ts.ctrl)

				// Expect GetState to be called
				mockStateManager.EXPECT().
					GetState().
					Return(&state.State{
						Relayer: &state.RelayerState{},
					}).
					Times(1)

				// Expect Save to be called and return error
				mockStateManager.EXPECT().
					Save(gomock.Any()).
					Return(saveError).
					Times(1)

				// Inject mock state manager
				state.InjectStateManagerForTesting(mockStateManager)

				payload := relayer.Payload{
					MessageID: relayer.MESSAGE_ID_SYSTEM,
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						TopicID: &topicID,
					},
				}

				return payload, saveError
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload, expectedError := tt.setupFunc(ts)

			// Register handler
			var capturedHandler relayer.Handler
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).DoAndReturn(func(handler relayer.Handler) {
				capturedHandler = handler
			}).Times(1)

			ts.mediator.Start()

			// Call handler directly
			err := capturedHandler(ts.ctx, payload)

			// Reset state after test
			state.ResetForTesting()

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestMediator_HandleRelayerMessage_Command(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (relayer.Payload, error)
	}{
		{
			name: "connectd command execution success",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_CONNECT // Use a real connectd command
				args := map[string]interface{}{"arg1": "value1"}
				result := map[string]interface{}{"result": "success"}

				// Expect the command to be executed
				ts.mockCmd.EXPECT().
					Execute(gomock.Any(),
						command.Command{
							Command:   cmd,
							Arguments: args,
						}).
					Return(result, nil).
					Times(1)

				// Expect the result to be sent
				ts.mockRelayer.EXPECT().
					Send(gomock.Any(), map[string]interface{}{
						"type":      "RPC",
						"messageID": "test-message",
						"message":   result,
					}).
					Return(nil).
					Times(1)

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args:    args,
					},
				}

				return payload, nil
			},
		},
		{
			name: "connectd command execution error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_CONNECT // Use a real connectd command
				args := map[string]interface{}{"arg1": "value1"}
				execError := errors.New("execution failed")

				// Expect the command to be executed and return error
				ts.mockCmd.EXPECT().
					Execute(gomock.Any(), command.Command{
						Command:   cmd,
						Arguments: args,
					}).
					Return(nil, execError).
					Times(1)

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args:    args,
					},
				}

				return payload, execError
			},
		},
		{
			name: "non-connectd command forwarded to CDP",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.RelayerCmd("non-connectd-cmd") // Use a non-connectd command
				args := map[string]interface{}{"arg1": "value1"}
				cdpResult := map[string]interface{}{"result": "cdp-success"}

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args:    args,
					},
				}

				// The payload.JSON() method is called by the real implementation
				payloadJSON, _ := payload.JSON()

				// Expect the command to be sent to CDP
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, map[string]interface{}{
						"expression": "window.handleCDPRequest(" + string(payloadJSON) + ")",
					}).
					Return(cdpResult, nil).
					Times(1)

				// Expect ForceRefresh to be called
				ts.mockStatusPoller.EXPECT().
					ForceRefresh().
					Times(1)

				// Expect the result to be sent
				ts.mockRelayer.EXPECT().
					Send(gomock.Any(), cdpResult).
					Return(nil).
					Times(1)

				// Expect Sleep to be called
				ts.mockClock.EXPECT().
					Sleep(500 * time.Millisecond).
					Times(1)

				return payload, nil
			},
		},
		{
			name: "message with no command",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: nil,
					},
				}

				return payload, nil // Should not return error
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload, expectedError := tt.setupFunc(ts)

			// Set status poller to test ForceRefresh call
			ts.mediator.SetStatusPoller(ts.mockStatusPoller)

			// Register handler
			var capturedHandler relayer.Handler
			ts.mockDbus.EXPECT().
				OnBusSignal(gomock.Any()).
				Times(1)

			// Expect OnRelayerMessage to be called
			ts.mockRelayer.EXPECT().
				OnRelayerMessage(gomock.Any()).
				DoAndReturn(func(handler relayer.Handler) {
					capturedHandler = handler
				}).Times(1)

			ts.mediator.Start()

			// Call handler directly
			err := capturedHandler(ts.ctx, payload)

			// Verify
			if expectedError != nil {
				assert.Error(t, err)
				assert.Contains(t, err.Error(), expectedError.Error())
			} else {
				assert.NoError(t, err)
			}
		})
	}
}
