package commandrouter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

type testSetup struct {
	ctrl             *gomock.Controller
	ctx              context.Context
	mockExecutor     *mocks.MockExecutor
	mockCDP          *mocks.MockCDP
	mockDP1          *mocks.MockDP1
	mockJSON         *mocks.MockJSON
	mockStatusPoller *mocks.MockStatusPoller
	handler          commandrouter.Handler
	logger           *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, mockJSON, logger)

	return &testSetup{
		ctrl:             ctrl,
		ctx:              ctx,
		mockExecutor:     mockExecutor,
		mockCDP:          mockCDP,
		mockDP1:          mockDP1,
		mockJSON:         mockJSON,
		mockStatusPoller: mockStatusPoller,
		handler:          handler,
		logger:           logger,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

// Helper functions
func float64Ptr(f float64) *float64 {
	return &f
}

func playerOkResponse() map[string]interface{} {
	return map[string]interface{}{
		"messageID": "1",
		"message": map[string]interface{}{
			"ok": true,
		},
	}
}

func playerNotOkResponse() map[string]interface{} {
	return map[string]interface{}{
		"messageID": "1",
		"message": map[string]interface{}{
			"ok": false,
		},
	}
}

// expectDisplayPlaylistSuccess sets up mock expectations for a successful
// displayPlaylist via URL: DP1 processing, CDP send returning ok, and ForceRefresh.
func expectDisplayPlaylistSuccess(ts *testSetup, playlistURL string, playlist *dp1.Playlist) {
	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, true).
		Return(playlist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)
}

func TestCommandHandler_Process_NoCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	command := commands.Command{
		Type:      "",
		Arguments: map[string]any{},
	}

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Nil(t, result)
}

func TestCommandHandler_Process_ControldCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.CMD_CONNECT
	args := map[string]interface{}{"clientDevice": map[string]interface{}{"device_id": "test-device"}}
	execResult := map[string]interface{}{"ok": true}

	payload := commands.Command{
		Type:      cmd,
		Arguments: args,
	}

	ts.mockExecutor.EXPECT().
		Execute(ts.ctx, commands.Command{
			Type:      cmd,
			Arguments: args,
		}).
		Return(execResult, nil).
		Times(1)

	result, err := ts.handler.Process(ts.ctx, payload)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, execResult, result)
}

func TestCommandHandler_Process_ControldCommand_Error(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.CMD_SHUTDOWN
	args := map[string]interface{}{}
	execError := errors.New("failed to shutdown")

	command := commands.Command{
		Type:      cmd,
		Arguments: args,
	}

	ts.mockExecutor.EXPECT().
		Execute(ts.ctx, commands.Command{
			Type:      cmd,
			Arguments: args,
		}).
		Return(nil, execError).
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.Error(t, err)
	assert.Equal(t, execError, err)
	assert.Nil(t, result)
}

func TestCommandHandler_Process_MintPairingApprovalDisabled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_MINT_PAIRING_APPROVAL,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	resp, ok := result.(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, false, resp["ok"])
	errObj, ok := resp["error"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "not_found", errObj["code"])
}

func TestCommandHandler_Process_StartMintPairingSessionDisabled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_START_MINT_PAIRING_SESSION,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	resp, ok := result.(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, false, resp["ok"])
	errObj, ok := resp["error"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "disabled", errObj["code"])
}

func TestCommandHandler_Process_StartMintPairingSessionRoutesToMintPairing(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("startMintPairingSession"), commands.CMD_START_MINT_PAIRING_SESSION)

	args := map[string]any{"source": "controller"}
	want := map[string]any{"ok": true, "status": "started"}
	mintSvc := &fakeMintPairingService{startResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, mintSvc, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_START_MINT_PAIRING_SESSION,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, mintSvc.startArgs)
	assert.Equal(t, 1, mintSvc.startCalls)
}

func TestCommandHandler_Process_CloseMintPairingSessionDisabled(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLOSE_MINT_PAIRING_SESSION,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	resp, ok := result.(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, false, resp["ok"])
	errObj, ok := resp["error"].(map[string]any)
	assert.True(t, ok)
	assert.Equal(t, "disabled", errObj["code"])
}

func TestCommandHandler_Process_CloseMintPairingSessionRoutesToMintPairing(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("closeMintPairingSession"), commands.CMD_CLOSE_MINT_PAIRING_SESSION)

	args := map[string]any{"source": "controller"}
	want := map[string]any{"ok": true, "status": "closed"}
	mintSvc := &fakeMintPairingService{closeResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, mintSvc, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLOSE_MINT_PAIRING_SESSION,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, mintSvc.closeArgs)
	assert.Equal(t, 1, mintSvc.closeCalls)
}

func TestCommandHandler_Process_MintPairingApprovalRoutesToMintPairing(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("mintPairingApprovalDecision"), commands.CMD_MINT_PAIRING_APPROVAL)

	args := map[string]any{"approvalRequestID": "mpa_1", "decision": "approve"}
	want := map[string]any{"ok": true, "status": "accepted"}
	mintSvc := &fakeMintPairingService{approvalResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, mintSvc, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_MINT_PAIRING_APPROVAL,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, mintSvc.approvalArgs)
	assert.Equal(t, 1, mintSvc.approvalCalls)
}

func TestCommandHandler_Process_ListEphemeralSessionsRoutesToSessionService(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("listEphemeralSessions"), commands.CMD_LIST_EPHEMERAL_SESSIONS)
	assert.False(t, commands.CMD_LIST_EPHEMERAL_SESSIONS.DeviceCtlCommand())

	args := map[string]any{}
	want := map[string]any{"ok": true, "sessions": []any{}}
	sessionSvc := &fakeEphemeralSessionService{listResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, sessionSvc, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_LIST_EPHEMERAL_SESSIONS,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, sessionSvc.listArgs)
	assert.Equal(t, 1, sessionSvc.listCalls)
}

func TestCommandHandler_Process_RevokeEphemeralSessionRoutesToSessionService(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.Equal(t, commands.Type("revokeEphemeralSession"), commands.CMD_REVOKE_EPHEMERAL_SESSION)
	assert.False(t, commands.CMD_REVOKE_EPHEMERAL_SESSION.DeviceCtlCommand())

	args := map[string]any{"sessionID": "session-1"}
	want := map[string]any{"ok": true, "status": "revoked"}
	sessionSvc := &fakeEphemeralSessionService{revokeResult: want}
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, sessionSvc, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_REVOKE_EPHEMERAL_SESSION,
		Arguments: args,
	})

	assert.NoError(t, err)
	assert.Equal(t, want, result)
	assert.Equal(t, args, sessionSvc.revokeArgs)
	assert.Equal(t, 1, sessionSvc.revokeCalls)
}

func TestCommandHandler_Process_NewGestureCommandsRouteToExecutor(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cases := []struct {
		name string
		cmd  commands.Type
	}{
		{name: "doubleTapGesture", cmd: commands.CMD_MOUSE_DOUBLE_TAP_EVENT},
		{name: "longPressGesture", cmd: commands.CMD_MOUSE_LONG_PRESS_EVENT},
		{name: "clickAndDragGesture", cmd: commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT},
		{name: "zoomGesture", cmd: commands.CMD_ZOOM_GESTURE},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts.mockExecutor.EXPECT().
				Execute(ts.ctx, commands.Command{
					Type:      tc.cmd,
					Arguments: map[string]interface{}{},
				}).
				Return(devicectl.CmdOK, nil).
				Times(1)

			result, err := ts.handler.Process(ts.ctx, commands.Command{
				Type:      tc.cmd,
				Arguments: map[string]interface{}{},
			})

			assert.NoError(t, err)
			assert.Equal(t, devicectl.CmdOK, result)
		})
	}
}

type fakeMintPairingService struct {
	startCalls     int
	closeCalls     int
	approvalCalls  int
	startArgs      map[string]any
	closeArgs      map[string]any
	approvalArgs   map[string]any
	startResult    any
	closeResult    any
	approvalResult any
}

func (f *fakeMintPairingService) Start(context.Context) {}

func (f *fakeMintPairingService) Stop() {}

func (f *fakeMintPairingService) HandleStartPairingSession(_ context.Context, args map[string]any) (any, error) {
	f.startCalls++
	f.startArgs = args
	return f.startResult, nil
}

func (f *fakeMintPairingService) HandleClosePairingSession(_ context.Context, args map[string]any) (any, error) {
	f.closeCalls++
	f.closeArgs = args
	return f.closeResult, nil
}

func (f *fakeMintPairingService) HandleApprovalDecision(_ context.Context, args map[string]any) (any, error) {
	f.approvalCalls++
	f.approvalArgs = args
	return f.approvalResult, nil
}

type fakeEphemeralSessionService struct {
	listCalls    int
	revokeCalls  int
	listArgs     map[string]any
	revokeArgs   map[string]any
	listResult   any
	revokeResult any
}

func (f *fakeEphemeralSessionService) HandleListSessions(_ context.Context, args map[string]any) (any, error) {
	f.listCalls++
	f.listArgs = args
	return f.listResult, nil
}

func (f *fakeEphemeralSessionService) HandleRevokeSession(_ context.Context, args map[string]any) (any, error) {
	f.revokeCalls++
	f.revokeArgs = args
	return f.revokeResult, nil
}

func TestCommandHandler_Process_DisplayPlaylist_WithURL(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    "Test Item",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
					License:  "open",
				},
			},
		},
	}

	expectDisplayPlaylistSuccess(ts, playlistURL, mockPlaylist)

	command := commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	}

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestCommandHandler_Process_DisplayPlaylist_WithPlaylistObject(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

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
	playlistBytes := []byte(`{"items":[{"id":"item1","title":"Test Item"}]}`)
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    "Test Item",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
					License:  "open",
				},
			},
		},
	}
	cdpResult := playerOkResponse()

	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlistMap,
		},
	}

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

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

func TestCommandHandler_Process_RefreshArtwork(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	command := commands.Command{
		Type:      commands.CMD_REFRESH_ARTWORK,
		Arguments: map[string]interface{}{},
	}

	ts.mockCDP.EXPECT().
		Send("Network.clearBrowserCache", map[string]interface{}{}).
		Return(nil, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestCommandHandler_Process_DisplayPlaylist_WithDynamicQueries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

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
	playlistBytes := []byte(`{"items":[],"dynamicQueries":[]}`)
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{},
		},
		DynamicQueries: []dp1.LegacyDynamicQuery{
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
					ID:       "item1",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
				},
			},
		},
	}
	cdpResult := playerOkResponse()

	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlistMap,
		},
	}

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

	ts.mockDP1.EXPECT().
		ProcessDynamicPlaylist(ts.ctx, *mockPlaylist, true).
		Return(processedPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

// TestCommandHandler_Process_DisplayPlaylist_WithSpecDynamicQuery ensures dp1_call with only
// the DP-1 playlists extension dynamicQuery (no legacy dynamicQueries) still triggers
// ProcessDynamicPlaylist via HasDynamicContent().
func TestCommandHandler_Process_DisplayPlaylist_WithSpecDynamicQuery(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistMap := map[string]interface{}{
		"items": []interface{}{},
		"dynamicQuery": map[string]interface{}{
			"profile":  "graphql-v1",
			"endpoint": "https://api.example.com/graphql",
			"query":    `query { items(limit: {{limit}}, offset: {{offset}}) { id title source } }`,
			"responseMapping": map[string]interface{}{
				"itemsPath":  "data.items",
				"itemSchema": "dp1/1.0",
			},
		},
	}
	playlistBytes := []byte(`{"items":[]}`)
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{},
			DynamicQuery: &playlists.DynamicQuery{
				Profile:  dp1playlist.ProfileGraphQLV1,
				Endpoint: "https://api.example.com/graphql",
				Query:    `query { items(limit: {{limit}}, offset: {{offset}}) { id title source } }`,
				ResponseMapping: playlists.ResponseMapping{
					ItemsPath:  "data.items",
					ItemSchema: "dp1/1.0",
				},
			},
		},
	}
	assert.True(t, mockPlaylist.HasDynamicContent(), "fixture should be spec-only dynamic (no legacy dynamicQueries)")

	processedPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Source:   "https://example.com/video.mp4",
					Duration: float64Ptr(300),
				},
			},
		},
	}
	cdpResult := playerOkResponse()

	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlistMap,
		},
	}

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

	ts.mockDP1.EXPECT().
		ProcessDynamicPlaylist(ts.ctx, *mockPlaylist, true).
		Return(processedPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

func TestCommandHandler_Process_DisplayPlaylist_Errors(t *testing.T) {
	tests := []struct {
		name          string
		setupFunc     func(*testSetup) commands.Command
		expectedError string
	}{
		{
			name: "invalid playlistUrl type",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type: cmd,
					Arguments: map[string]interface{}{
						"playlistUrl": 123, // Invalid type
					},
				}
			},
			expectedError: "playlistUrl is not a string or empty",
		},
		{
			name: "empty playlistUrl",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type: cmd,
					Arguments: map[string]interface{}{
						"playlistUrl": "",
					},
				}
			},
			expectedError: "playlistUrl is not a string or empty",
		},
		{
			name: "invalid playlist type",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type: cmd,
					Arguments: map[string]interface{}{
						"dp1_call": "not a map", // Invalid type
					},
				}
			},
			expectedError: "playlist is not a map",
		},
		{
			name: "unknown payload type",
			setupFunc: func(ts *testSetup) commands.Command {
				cmd := commands.CMD_DISPLAY_PLAYLIST
				return commands.Command{
					Type:      cmd,
					Arguments: map[string]interface{}{}, // Neither playlistUrl nor dp1_call
				}
			},
			expectedError: "unknown payload type",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			payload := tt.setupFunc(ts)
			result, err := ts.handler.Process(ts.ctx, payload)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.expectedError)
			assert.Nil(t, result)
		})
	}
}

func TestCommandHandler_Process_NonControldCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.Type("someCustomCommand")
	args := map[string]interface{}{"key": "value"}
	cdpResult := map[string]interface{}{"result": "success"}

	payload := commands.Command{
		Type:      cmd,
		Arguments: args,
	}

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	result, err := ts.handler.Process(ts.ctx, payload)

	assert.NoError(t, err)
	assert.Equal(t, cdpResult, result)
}

// --- Playback metrics tests ---

func TestCommandHandler_Metrics_DisplayPlaylist_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	expectDisplayPlaylistSuccess(ts, playlistURL, mockPlaylist)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal(), "attempt counter should increment")
	assert.Equal(t, beforeFailures, status.PlaybackStartFailures(), "failure counter should not increment on success")
}

func TestCommandHandler_Metrics_DisplayPlaylist_ControldError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{}, // neither playlistUrl nor dp1_call
	})

	assert.Error(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures(), "failure should be recorded for controld-side error")
}

func TestCommandHandler_Metrics_DisplayPlaylist_CDPFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, true).
		Return(mockPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("CDP write error")).
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.Error(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures())
}

func TestCommandHandler_Metrics_PlayerResponseNotOk(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, true).
		Return(mockPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerNotOkResponse(), nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err, "Process itself succeeds; failure is only in the metric")
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures(), "failure should be recorded when player responds with ok: false")
}

func TestCommandHandler_Metrics_PlayerResponseMissingMessage(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	mockPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{{ID: "item1"}},
		},
	}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, true).
		Return(mockPlaylist, nil).
		Times(1)

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"unexpected": "shape"}, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})

	assert.NoError(t, err)
	assert.Equal(t, beforeAttempts+1, status.PlaybackStartTotal())
	assert.Equal(t, beforeFailures+1, status.PlaybackStartFailures(), "failure should be recorded when response has no message.ok")
}

func TestCommandHandler_Metrics_DisplayDefaultPlaylist_NoMetrics(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cdpResult := map[string]interface{}{"result": "success"}

	beforeAttempts := status.PlaybackStartTotal()
	beforeFailures := status.PlaybackStartFailures()

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(cdpResult, nil).
		Times(1)

	ts.mockStatusPoller.EXPECT().
		ForceRefresh().
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
		Arguments: map[string]interface{}{},
	})

	assert.NoError(t, err)
	assert.Equal(t, beforeAttempts, status.PlaybackStartTotal(), "displayDefaultPlaylist should not record metrics")
	assert.Equal(t, beforeFailures, status.PlaybackStartFailures(), "displayDefaultPlaylist should not record metrics")
}

func TestCommandHandler_Metrics_NonPlaybackCommand_NoMetrics(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	beforeAttempts := status.PlaybackStartTotal()

	ts.mockExecutor.EXPECT().
		Execute(ts.ctx, gomock.Any()).
		Return(nil, errors.New("some error")).
		Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_SHUTDOWN,
		Arguments: map[string]interface{}{},
	})

	assert.Error(t, err)
	assert.Equal(t, beforeAttempts, status.PlaybackStartTotal(), "non-playback command should not increment attempt counter")
}
