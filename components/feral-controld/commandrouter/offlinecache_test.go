package commandrouter_test

import (
	"encoding/json"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

// setupOfflineCache builds a handler with a mocked offlinecache.Service so
// the 5 pre-CDP offline-cache commands can be exercised without touching
// CDP, the executor, or mint pairing.
func setupOfflineCache(t *testing.T) (*testSetup, *mocks.MockOfflineCacheService) {
	ts := setup(t)
	mockOfflineCache := mocks.NewMockOfflineCacheService(ts.ctrl)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, nil, ts.mockJSON, ts.logger)
	return ts, mockOfflineCache
}

func assertOkResponse(t *testing.T, result interface{}) map[string]any {
	t.Helper()
	resp, ok := result.(map[string]any)
	require.True(t, ok, "result must be a map[string]any")
	assert.Equal(t, true, resp["ok"])
	return resp
}

func assertErrorResponse(t *testing.T, result interface{}, wantCode string) map[string]any {
	t.Helper()
	resp, ok := result.(map[string]any)
	require.True(t, ok, "result must be a map[string]any")
	assert.Equal(t, false, resp["ok"])
	errObj, ok := resp["error"].(map[string]any)
	require.True(t, ok, "error field must be a map[string]any")
	assert.Equal(t, wantCode, errObj["code"])
	return resp
}

func TestCommandHandler_OfflineCache_AllCommandsDisabledWhenServiceNil(t *testing.T) {
	ts := setup(t) // setup() wires offlineCache as nil
	defer ts.teardown()

	cmds := []commands.Type{
		commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		commands.CMD_DOWNLOAD_PLAYLIST,
		commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		commands.CMD_CLEAR_PLAYLIST_CACHE,
		commands.CMD_GET_OFFLINE_CACHE_STATUS,
	}
	for _, cmd := range cmds {
		t.Run(cmd.String(), func(t *testing.T) {
			result, err := ts.handler.Process(ts.ctx, commands.Command{Type: cmd, Arguments: map[string]any{}})
			require.NoError(t, err)
			assertErrorResponse(t, result, "disabled")
		})
	}
}

func TestCommandHandler_DownloadPlaylistItem_Success(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "itemId": "item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "queued", resp["status"])
	assert.Equal(t, "item-1", resp["itemId"])
}

func TestCommandHandler_DownloadPlaylistItem_MissingItemID(t *testing.T) {
	ts, _ := setupOfflineCache(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": "https://example.com/playlist.json"},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "invalid_request")
}

func TestCommandHandler_DownloadPlaylistItem_ItemNotInPlaylist(t *testing.T) {
	ts, _ := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{}}}
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "itemId": "missing-item"},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "not_found")
}

func TestCommandHandler_DownloadPlaylistItem_ResolveFailure(t *testing.T) {
	ts, _ := setupOfflineCache(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"itemId": "item-1"}, // neither playlistUrl nor dp1_call
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "resolve_failed")
}

func TestCommandHandler_DownloadPlaylistItem_ServiceNotStarted(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(offlinecache.ErrServiceNotStarted).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "itemId": "item-1"},
	})

	require.NoError(t, err)
	// Same "disabled" shape as h.offlineCache == nil (see
	// TestCommandHandler_OfflineCache_AllCommandsDisabledWhenServiceNil):
	// a startup failure is not something a client can fix by retrying.
	assertErrorResponse(t, result, "disabled")
}

func TestCommandHandler_DownloadPlaylistItem_UnsupportedMediaClass(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{item}}}

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(offlinecache.ErrUnsupportedMediaClass).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "itemId": "item-1"},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "unsupported_media")
}

func TestCommandHandler_DownloadPlaylist_Success(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{
		{ID: "item-1", Source: "https://example.com/index.html"},
		{ID: "item-2", Source: "https://example.com/video.mp4"},
	}}}
	marshaled := []byte(`{"id":"playlist-1"}`)

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadPlaylist(ts.ctx, json.RawMessage(marshaled)).Return(1, 2, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, 2, resp["total"])
	assert.Equal(t, 1, resp["softwareCount"])
}

func TestCommandHandler_DownloadPlaylist_ResolveFailure(t *testing.T) {
	ts, _ := setupOfflineCache(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST,
		Arguments: map[string]any{},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "resolve_failed")
}

func TestCommandHandler_DownloadPlaylist_ServiceError(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1"}}
	marshaled := []byte(`{"id":"playlist-1"}`)

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadPlaylist(ts.ctx, json.RawMessage(marshaled)).Return(0, 0, assertError("disk error")).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "offline_cache_error")
}

func TestCommandHandler_ClearPlaylistItemCache_Success(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("item-1").Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "item-1", resp["itemId"])
}

func TestCommandHandler_ClearPlaylistItemCache_MissingItemID(t *testing.T) {
	ts, _ := setupOfflineCache(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "invalid_request")
}

func TestCommandHandler_ClearPlaylistCache_Success(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearPlaylist("playlist-1").Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_CACHE,
		Arguments: map[string]any{"playlistId": "playlist-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "playlist-1", resp["playlistId"])
}

func TestCommandHandler_ClearPlaylistCache_NotFound(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearPlaylist("missing").Return(offlinecache.ErrPlaylistNotFound).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_CACHE,
		Arguments: map[string]any{"playlistId": "missing"},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "not_found")
}

func TestCommandHandler_GetOfflineCacheStatus_WithItemIDs(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	snapshot := offlinecache.StatusSnapshot{
		Items:         []offlinecache.ItemStatus{{ItemID: "item-1", State: offlinecache.StateReady}},
		Totals:        offlinecache.StatusTotals{Total: 1, Ready: 1},
		DiskUsedBytes: 1024,
	}
	mockOfflineCache.EXPECT().Status([]string{"item-1", "item-2"}).Return(snapshot, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{"itemIds": []interface{}{"item-1", "item-2"}},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, snapshot.Items, resp["items"])
	assert.Equal(t, snapshot.Totals, resp["totals"])
	assert.EqualValues(t, 1024, resp["diskUsed"])
}

func TestCommandHandler_GetOfflineCacheStatus_NoFilter(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	snapshot := offlinecache.StatusSnapshot{}
	mockOfflineCache.EXPECT().Status(nil).Return(snapshot, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
}

func TestCommandHandler_GetOfflineCacheStatus_ServiceError(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().Status(nil).Return(offlinecache.StatusSnapshot{}, assertError("disk error")).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "offline_cache_error")
}

type assertError string

func (e assertError) Error() string { return string(e) }
