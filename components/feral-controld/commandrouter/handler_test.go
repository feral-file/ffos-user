package commandrouter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	dp1playlist "github.com/display-protocol/dp1-validator/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
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
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, mockJSON, logger)

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

// Helper function
func stringPtr(s string) *string {
	return &s
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

func TestCommandHandler_Process_DisplayPlaylist_WithURL(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.CMD_DISPLAY_PLAYLIST
	playlistURL := "https://example.com/playlist.json"
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
	cdpResult := map[string]interface{}{"result": "success"}

	command := commands.Command{
		Type: cmd,
		Arguments: map[string]interface{}{
			"playlistUrl": playlistURL,
		},
	}

	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, true).
		Return(mockPlaylist, nil).
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

func TestCommandHandler_Process_DisplayPlaylist_WithPlaylistObject(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.CMD_DISPLAY_PLAYLIST
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
					Title:    stringPtr("Test Item"),
					Source:   "https://example.com/video.mp4",
					Duration: 300,
					License:  "open",
				},
			},
		},
	}
	cdpResult := map[string]interface{}{"result": "success"}

	command := commands.Command{
		Type: cmd,
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

func TestCommandHandler_Process_DisplayPlaylist_WithDynamicQueries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cmd := commands.CMD_DISPLAY_PLAYLIST
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
					ID:       "item1",
					Source:   "https://example.com/video.mp4",
					Duration: 300,
				},
			},
		},
	}
	cdpResult := map[string]interface{}{"result": "success"}

	command := commands.Command{
		Type: cmd,
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
