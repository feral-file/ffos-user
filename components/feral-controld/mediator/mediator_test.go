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

	dp1playlist "github.com/display-protocol/dp1-validator/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/command"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

type testSetup struct {
	ctrl             *gomock.Controller
	ctx              context.Context
	mockRelayer      *mocks.MockRelayer
	mockDbus         *mocks.MockDBus
	mockCDP          *mocks.MockCDP
	mockCmd          *mocks.MockCommandHandler
	mockDP1          *mocks.MockDP1
	mockStatusPoller *mocks.MockStatusPoller
	mockClock        *mocks.MockClock
	mockJSON         *mocks.MockJSON
	mockRefresher    *mocks.MockRefresher
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
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockRefresher := mocks.NewMockRefresher(ctrl)

	med := mediator.New(
		mockRelayer,
		mockDbus,
		mockCDP,
		mockCmd,
		mockDP1,
		mockClock,
		mockJSON,
		mockRefresher,
		logger,
	)

	return &testSetup{
		ctrl:             ctrl,
		ctx:              ctx,
		mockRelayer:      mockRelayer,
		mockDbus:         mockDbus,
		mockCDP:          mockCDP,
		mockCmd:          mockCmd,
		mockDP1:          mockDP1,
		mockStatusPoller: mockStatusPoller,
		mockClock:        mockClock,
		mockJSON:         mockJSON,
		mockRefresher:    mockRefresher,
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
		{
			name: "controld command execution success",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_CONNECT // Use a real controld command
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
			name: "controld command execution error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_CONNECT // Use a real controld command
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
			name: "non-controld command forwarded to CDP",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.RelayerCmd("non-controld-cmd") // Use a non-controld command
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
			name: "display playlist command with playlistUrl",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST
				playlistUrl := "https://example.com/playlist.json"
				mockPlaylist := &dp1.Playlist{
					Playlist: dp1playlist.Playlist{
						Items: []dp1playlist.PlaylistItem{
							{
								ID:       "item1",
								Title:    stringPtr("Test Item"),
								Source:   "https://example.com/video.mp4",
								Duration: 300,
								License:  "open",
							},
						},
					},
				}
				cdpResult := map[string]interface{}{"result": "cdp-success"}

				// Expect ProcessPlaylistURL to be called
				ts.mockDP1.EXPECT().
					ProcessPlaylistURL(gomock.Any(), playlistUrl, true).
					Return(mockPlaylist, nil).
					Times(1)

				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, gomock.Any()).
					Return(cdpResult, nil).
					Times(1)

				// Expect ForceRefresh to be called
				ts.mockStatusPoller.EXPECT().
					ForceRefresh().
					Times(1)

				// Expect Sleep to be called
				ts.mockClock.EXPECT().
					Sleep(500 * time.Millisecond).
					Times(1)

				// Expect the result to be sent
				ts.mockRelayer.EXPECT().
					Send(gomock.Any(), cdpResult).
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
						Args: map[string]interface{}{
							"playlistUrl": playlistUrl,
						},
					},
				}

				return payload, nil
			},
		},
		{
			name: "display playlist command with playlist object",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST
				playlistMap := map[string]interface{}{
					"items": []interface{}{
						map[string]interface{}{
							"id":       "item1",
							"title":    "Test Item",
							"source":   "https://example.com/video.mp4",
							"duration": 300,
							"license":  "open",
						},
					},
				}
				playlistBytes := []byte(`{"items":[{"id":"item1","title":"Test Item","source":"https://example.com/video.mp4","duration":300,"license":"open"}]}`)
				mockPlaylist := &dp1.Playlist{
					Playlist: dp1playlist.Playlist{
						Items: []dp1playlist.PlaylistItem{
							{
								ID:       "item1",
								Title:    stringPtr("Test Item"),
								Source:   "https://example.com/video.mp4",
								Duration: 300,
								License:  "open",
							},
						},
					},
				}
				cdpResult := map[string]interface{}{"result": "cdp-success"}

				// Expect JSON marshal and unmarshal
				ts.mockJSON.EXPECT().
					Marshal(playlistMap).
					Return(playlistBytes, nil).
					Times(1)

				ts.mockJSON.EXPECT().
					Unmarshal(playlistBytes, gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						playlist := v.(**dp1.Playlist)
						*playlist = mockPlaylist
						return nil
					}).
					Times(1)

				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, gomock.Any()).
					Return(cdpResult, nil).
					Times(1)

				// Expect ForceRefresh to be called
				ts.mockStatusPoller.EXPECT().
					ForceRefresh().
					Times(1)

				// Expect Sleep to be called
				ts.mockClock.EXPECT().
					Sleep(500 * time.Millisecond).
					Times(1)

				// Expect the result to be sent
				ts.mockRelayer.EXPECT().
					Send(gomock.Any(), cdpResult).
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
						Args: map[string]interface{}{
							"playlist": playlistMap,
						},
					},
				}

				return payload, nil
			},
		},
		{
			name: "display playlist command with dynamic playlist",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST
				playlistMap := map[string]interface{}{
					"items": []interface{}{},
					"dynamicQueries": []interface{}{
						map[string]interface{}{
							"endpoint": "https://api.example.com/graphql",
							"params": map[string]interface{}{
								"query": "test query",
							},
						},
					},
				}
				playlistBytes := []byte(`{"items":[],"dynamicQueries":[{"endpoint":"https://api.example.com/graphql","params":{"query":"test query"}}]}`)
				mockPlaylist := &dp1.Playlist{
					Playlist: dp1playlist.Playlist{
						Items: []dp1playlist.PlaylistItem{},
					},
					DynamicQueries: []dp1.DynamicQuery{
						{
							Endpoint: "https://api.example.com/graphql",
							Params: map[string]string{
								"query": "test query",
							},
						},
					},
				}
				processedPlaylist := &dp1.Playlist{
					Playlist: dp1playlist.Playlist{
						Items: []dp1playlist.PlaylistItem{
							{
								ID:       "processed1",
								Title:    stringPtr("Processed Item"),
								Source:   "https://example.com/processed.mp4",
								Duration: 300,
								License:  "open",
							},
						},
					},
				}
				cdpResult := map[string]interface{}{"result": "cdp-success"}

				// Expect JSON marshal and unmarshal
				ts.mockJSON.EXPECT().
					Marshal(playlistMap).
					Return(playlistBytes, nil).
					Times(1)

				ts.mockJSON.EXPECT().
					Unmarshal(playlistBytes, gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						playlist := v.(**dp1.Playlist)
						*playlist = mockPlaylist
						return nil
					}).
					Times(1)

				// Expect ProcessDynamicPlaylist to be called
				ts.mockDP1.EXPECT().
					ProcessDynamicPlaylist(gomock.Any(), *mockPlaylist, true).
					Return(processedPlaylist, nil).
					Times(1)

				// Expect CDP send to be called
				ts.mockCDP.EXPECT().
					Send(cdp.METHOD_EVALUATE, gomock.Any()).
					Return(cdpResult, nil).
					Times(1)

				// Expect ForceRefresh to be called
				ts.mockStatusPoller.EXPECT().
					ForceRefresh().
					Times(1)

				// Expect Sleep to be called
				ts.mockClock.EXPECT().
					Sleep(500 * time.Millisecond).
					Times(1)

				// Expect the result to be sent
				ts.mockRelayer.EXPECT().
					Send(gomock.Any(), cdpResult).
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
						Args: map[string]interface{}{
							"playlist": playlistMap,
						},
					},
				}

				return payload, nil
			},
		},
		{
			name: "display playlist command with invalid playlistUrl",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args: map[string]interface{}{
							"playlistUrl": "", // Empty URL should cause error
						},
					},
				}

				return payload, errors.New("playlistUrl is not a string or empty")
			},
		},
		{
			name: "display playlist command with invalid playlist type",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args: map[string]interface{}{
							"playlist": "not-a-map", // Invalid type should cause error
						},
					},
				}

				return payload, errors.New("playlist is not a map")
			},
		},
		{
			name: "display playlist command with unknown payload type",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args:    map[string]interface{}{}, // No playlistUrl or playlist
					},
				}

				return payload, errors.New("unknown payload type")
			},
		},
		{
			name: "display playlist command with ProcessPlaylistURL error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST
				playlistUrl := "https://example.com/playlist.json"
				processError := errors.New("failed to process playlist URL")

				// Expect ProcessPlaylistURL to be called and return error
				ts.mockDP1.EXPECT().
					ProcessPlaylistURL(gomock.Any(), playlistUrl, true).
					Return(nil, processError).
					Times(1)

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args: map[string]interface{}{
							"playlistUrl": playlistUrl,
						},
					},
				}

				return payload, processError
			},
		},
		{
			name: "display playlist command with JSON marshal error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST
				playlistMap := map[string]interface{}{
					"items": []interface{}{},
				}
				marshalError := errors.New("failed to marshal playlist")

				// Expect JSON marshal to return error
				ts.mockJSON.EXPECT().
					Marshal(playlistMap).
					Return(nil, marshalError).
					Times(1)

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args: map[string]interface{}{
							"playlist": playlistMap,
						},
					},
				}

				return payload, marshalError
			},
		},
		{
			name: "display playlist command with JSON unmarshal error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST
				playlistMap := map[string]interface{}{
					"items": []interface{}{},
				}
				playlistBytes := []byte(`{"items":[]}`)
				unmarshalError := errors.New("failed to unmarshal playlist")

				// Expect JSON marshal to succeed
				ts.mockJSON.EXPECT().
					Marshal(playlistMap).
					Return(playlistBytes, nil).
					Times(1)

				// Expect JSON unmarshal to return error
				ts.mockJSON.EXPECT().
					Unmarshal(playlistBytes, gomock.Any()).
					Return(unmarshalError).
					Times(1)

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args: map[string]interface{}{
							"playlist": playlistMap,
						},
					},
				}

				return payload, unmarshalError
			},
		},
		{
			name: "display playlist command with ProcessDynamicPlaylist error",
			setupFunc: func(ts *testSetup) (relayer.Payload, error) {
				cmd := relayer.CMD_DISPLAY_PLAYLIST
				playlistMap := map[string]interface{}{
					"items": []interface{}{},
					"dynamicQueries": []interface{}{
						map[string]interface{}{
							"endpoint": "https://api.example.com/graphql",
							"params": map[string]interface{}{
								"query": "test query",
							},
						},
					},
				}
				playlistBytes := []byte(`{"items":[],"dynamicQueries":[{"endpoint":"https://api.example.com/graphql","params":{"query":"test query"}}]}`)
				mockPlaylist := &dp1.Playlist{
					Playlist: dp1playlist.Playlist{
						Items: []dp1playlist.PlaylistItem{},
					},
					DynamicQueries: []dp1.DynamicQuery{
						{
							Endpoint: "https://api.example.com/graphql",
							Params: map[string]string{
								"query": "test query",
							},
						},
					},
				}
				processError := errors.New("failed to process dynamic playlist")

				// Expect JSON marshal and unmarshal
				ts.mockJSON.EXPECT().
					Marshal(playlistMap).
					Return(playlistBytes, nil).
					Times(1)

				ts.mockJSON.EXPECT().
					Unmarshal(playlistBytes, gomock.Any()).
					DoAndReturn(func(data []byte, v interface{}) error {
						playlist := v.(**dp1.Playlist)
						*playlist = mockPlaylist
						return nil
					}).
					Times(1)

				// Expect ProcessDynamicPlaylist to be called and return error
				ts.mockDP1.EXPECT().
					ProcessDynamicPlaylist(gomock.Any(), *mockPlaylist, true).
					Return(nil, processError).
					Times(1)

				payload := relayer.Payload{
					MessageID: "test-message",
					Message: struct {
						Command *relayer.RelayerCmd    `json:"command,omitempty"`
						Args    map[string]interface{} `json:"request,omitempty"`
						TopicID *string                `json:"topicID,omitempty"`
					}{
						Command: &cmd,
						Args: map[string]interface{}{
							"playlist": playlistMap,
						},
					},
				}

				return payload, processError
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

// Helper function to create string pointers for test data
func stringPtr(s string) *string {
	return &s
}
