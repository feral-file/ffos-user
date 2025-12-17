package mediator_test

import (
	"context"
	"errors"
	"testing"

	"github.com/feral-file/godbus"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

type testSetup struct {
	ctrl               *gomock.Controller
	ctx                context.Context
	mockRelayer        *mocks.MockRelayer
	mockDbus           *mocks.MockDBus
	mockCDP            *mocks.MockCDP
	mockExecutor       *mocks.MockExecutor
	mockCommandHandler *mocks.MockCommandHandler
	mockStatusPoller   *mocks.MockStatusPoller
	mockRefresher      *mocks.MockRefresher
	mockJSON           *mocks.MockJSON
	mediator           mediator.Mediator
	logger             *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockDbus := mocks.NewMockDBus(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockCommandHandler := mocks.NewMockCommandHandler(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockRefresher := mocks.NewMockRefresher(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)

	med := mediator.New(
		mockRelayer,
		mockDbus,
		mockCDP,
		mockCommandHandler,
		mockExecutor,
		mockRefresher,
		mockStatusPoller,
		mockJSON,
		logger,
	)

	return &testSetup{
		ctrl:               ctrl,
		ctx:                ctx,
		mockRelayer:        mockRelayer,
		mockDbus:           mockDbus,
		mockCDP:            mockCDP,
		mockExecutor:       mockExecutor,
		mockCommandHandler: mockCommandHandler,
		mockStatusPoller:   mockStatusPoller,
		mockRefresher:      mockRefresher,
		mockJSON:           mockJSON,
		mediator:           med,
		logger:             logger,
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

				ts.mockExecutor.EXPECT().
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

				// Expect IsConnected to be called twice (once for logging, once for condition check)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
					Times(2)

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

				// Expect IsConnected to be called twice (once for logging, once for condition check)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(true).
					Times(2)

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

				// Expect IsConnected to be called once for logging (condition short-circuits since connected=false)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
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

				// Expect IsConnected to be called twice (once for logging, once for condition check)
				ts.mockRelayer.EXPECT().
					IsConnected().
					Return(false).
					Times(2)

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

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

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
					Message: relayer.Message{
						TopicID: &topicID,
					},
				}

				return payload, nil
			},
		},
		{
			name: "system message missing topic ID",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				payload := relayer.Payload{
					MessageID: relayer.MESSAGE_ID_SYSTEM,
					Message: relayer.Message{
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

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

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
					Message: relayer.Message{
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

func TestMediator_HandleRelayerMessage_ProcessCommand(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (relayer.Payload, error)
	}{
		{
			name: "command processing success",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := string(commands.CMD_CONNECT)
				args := map[string]interface{}{"arg1": "value1"}
				result := map[string]interface{}{
					"type":      "RPC",
					"messageID": "test-message",
					"message":   map[string]interface{}{"result": "success"},
				}

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: relayer.Message{
						Command: &cmd,
						Request: args,
					},
				}

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Expect the command handler to process
				ts.mockCommandHandler.EXPECT().
					Process(gomock.Any(), commands.Command{
						Type:      commands.Type(cmd),
						Arguments: args,
					}).
					Return(result, nil).
					Times(1)

				// Expect the result to be sent
				ts.mockRelayer.EXPECT().
					Send(gomock.Any(), relayer.Response{
						Type:      "RPC",
						MessageID: "test-message",
						Message:   result,
					}).
					Return(nil).
					Times(1)

				return payload, nil
			},
		},
		{
			name: "command processing error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := string(commands.CMD_CONNECT)
				args := map[string]interface{}{"arg1": "value1"}
				execError := errors.New("execution failed")

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: relayer.Message{
						Command: &cmd,
						Request: args,
					},
				}

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Expect the command handler to process and return error
				ts.mockCommandHandler.EXPECT().
					Process(gomock.Any(), commands.Command{
						Type:      commands.Type(cmd),
						Arguments: args,
					}).
					Return(nil, execError).
					Times(1)

				return payload, execError
			},
		},
		{
			name: "command processing nil result",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := string(commands.CMD_CONNECT)
				args := map[string]interface{}{"arg1": "value1"}
				payload := relayer.Payload{
					MessageID: "test-message",
					Message: relayer.Message{
						Command: &cmd,
						Request: args,
					},
				}

				// Expect JSON marshal for logging
				ts.mockJSON.EXPECT().
					Marshal(gomock.Any()).
					Return([]byte("{}"), nil).
					AnyTimes()

				// Expect the command handler to process and return nil result
				ts.mockCommandHandler.EXPECT().
					Process(gomock.Any(), commands.Command{
						Type:      commands.Type(cmd),
						Arguments: args,
					}).
					Return(nil, nil).
					Times(1)

				return payload, nil
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
