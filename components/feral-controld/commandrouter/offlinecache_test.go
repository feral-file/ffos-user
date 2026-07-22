package commandrouter_test

import (
	"encoding/json"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
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

// TestCommandHandler_DownloadPlaylistItem_Success also pins the
// downloadPlaylistItem-by-URL offline-fallback fix: resolving the item
// via playlistUrl must ALSO index the resolved playlist by that URL
// (Service.IndexPlaylistForOfflineDisplay), not only queue the one
// requested item — otherwise a device that only ever called
// downloadPlaylistItem for this URL would have no
// displayPlaylist-by-URL offline fallback at all once offline, even
// though this item is now genuinely cached (see that method's doc).
func TestCommandHandler_DownloadPlaylistItem_Success(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}
	marshaled := []byte(`{"id":"playlist-1"}`)

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), playlistURL).Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "itemId": "item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "queued", resp["status"])
	assert.Equal(t, "item-1", resp["itemId"])
}

// TestCommandHandler_DownloadPlaylistItem_InlinePlaylistSkipsIndexing
// pins that a dp1_call (inline) download has no source URL to index by
// — mirroring TestCommandHandler_DownloadPlaylist_InlinePlaylistPassesEmptySourceURL's
// contract for downloadPlaylist. Deliberately no
// IndexPlaylistForOfflineDisplay expectation: gomock's strict
// controller fails this test if the handler calls it anyway.
func TestCommandHandler_DownloadPlaylistItem_InlinePlaylistSkipsIndexing(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	playlistMap := map[string]interface{}{"id": "playlist-1", "items": []interface{}{map[string]interface{}{"id": "item-1", "source": item.Source}}}
	playlistBytes := []byte(`{"id":"playlist-1","items":[{"id":"item-1","source":"https://example.com/index.html"}]}`)
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}

	ts.mockJSON.EXPECT().Marshal(playlistMap).Return(playlistBytes, nil).Times(1)
	ts.mockJSON.EXPECT().Unmarshal(playlistBytes, gomock.Any()).DoAndReturn(func(_ []byte, v interface{}) error {
		p := v.(**dp1.Playlist)
		*p = playlist
		return nil
	}).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"dp1_call": playlistMap, "itemId": "item-1"},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
}

// TestCommandHandler_DownloadPlaylistItem_IndexingFailureStillReportsSuccess
// pins that a best-effort indexing failure must never turn an
// already-successful item queue into an error response to the caller —
// the requested item genuinely IS queued/cached either way; only the
// separate displayPlaylist-by-URL offline fallback degrades.
func TestCommandHandler_DownloadPlaylistItem_IndexingFailureStillReportsSuccess(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}
	marshaled := []byte(`{"id":"playlist-1"}`)

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), playlistURL).
		Return(offlinecache.ErrServiceNotStarted).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "itemId": "item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "queued", resp["status"])
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
	mockOfflineCache.EXPECT().DownloadPlaylist(ts.ctx, json.RawMessage(marshaled), playlistURL).Return(1, 2, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, 2, resp["total"])
	assert.Equal(t, 1, resp["softwareCount"])
}

// TestCommandHandler_DownloadPlaylist_InlinePlaylistPassesEmptySourceURL
// pins that a dp1_call (inline) download has no source URL to index by:
// Service.DownloadPlaylist's third argument must be "" so
// CachedPlaylistForURL never spuriously resolves for some URL this
// playlist was never actually downloaded from.
func TestCommandHandler_DownloadPlaylist_InlinePlaylistPassesEmptySourceURL(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistMap := map[string]interface{}{"id": "playlist-1", "items": []interface{}{}}
	playlistBytes := []byte(`{"id":"playlist-1","items":[]}`)
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1"}}
	marshaledResolved := []byte(`{"id":"playlist-1"}`)

	ts.mockJSON.EXPECT().Marshal(playlistMap).Return(playlistBytes, nil).Times(1)
	ts.mockJSON.EXPECT().Unmarshal(playlistBytes, gomock.Any()).DoAndReturn(func(_ []byte, v interface{}) error {
		p := v.(**dp1.Playlist)
		*p = playlist
		return nil
	}).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaledResolved, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadPlaylist(ts.ctx, json.RawMessage(marshaledResolved), "").Return(0, 0, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST,
		Arguments: map[string]any{"dp1_call": playlistMap},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
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
	mockOfflineCache.EXPECT().DownloadPlaylist(ts.ctx, json.RawMessage(marshaled), playlistURL).Return(0, 0, assertError("disk error")).Times(1)

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

// TestCommandHandler_ClearPlaylistItemCache_ResyncsKioskReplayScope is the
// regression test pinning that clearing an item's cache must resync
// replayer's live Fetch-interception scope: without this resync, an item
// cleared while it is the one currently displayed
// would keep serving from (now-deleted) stale blob entries instead of
// either playing correctly or falling back to the network cleanly.
func TestCommandHandler_ClearPlaylistItemCache_ResyncsKioskReplayScope(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("item-1").Return(nil).Times(1)

	playlistURL := "https://example.com/playlist.json"
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "item-1"}, {ID: "item-2"},
	}}}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &playlistURL,
	}, nil).Times(1)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-1", "item-2"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "item-1", resp["itemId"])
}

// TestCommandHandler_ClearPlaylistItemCache_SkipsResyncWhenNotDisplayingPlaylist
// pins that resyncKioskReplayScopeToCurrentDisplay is a no-op (no SyncPlaylist
// call at all) when the player is not currently on a displayPlaylist
// command — nothing to resync.
func TestCommandHandler_ClearPlaylistItemCache_SkipsResyncWhenNotDisplayingPlaylist(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("item-1").Return(nil).Times(1)
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command: "someOtherCommand",
	}, nil).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	// No SyncPlaylist expectation: gomock fails the test if one occurs.
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
}

// TestCommandHandler_ClearPlaylistItemCache_ResyncFailureDoesNotBlockResponse
// pins that a resync failure (here, FetchPlayerStatus erroring) never turns
// an already-successful clear into a reported error.
func TestCommandHandler_ClearPlaylistItemCache_ResyncFailureDoesNotBlockResponse(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("item-1").Return(nil).Times(1)
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(nil, assertError("cdp not ready")).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "item-1", resp["itemId"])
}

// TestCommandHandler_ClearPlaylistCache_ResyncsKioskReplayScope mirrors
// TestCommandHandler_ClearPlaylistItemCache_ResyncsKioskReplayScope for the
// whole-playlist clear path.
func TestCommandHandler_ClearPlaylistCache_ResyncsKioskReplayScope(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearPlaylist("playlist-1").Return(nil).Times(1)

	inlinePlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{ID: "item-a"}}}}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:  string(commands.CMD_DISPLAY_PLAYLIST),
		Playlist: inlinePlaylist,
	}, nil).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-a"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_CACHE,
		Arguments: map[string]any{"playlistId": "playlist-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "playlist-1", resp["playlistId"])
}

// TestCommandHandler_ClearPlaylistItemCache_ActiveCaptureReturnsBusy is
// the RPC-shape regression test for offlineCacheErrorResponse's
// ErrItemBusy mapping: the mobile app must see a
// distinct, retryable "busy" code rather than the generic
// "offline_cache_error" bucket, so it knows to retry shortly instead of
// treating this like an unexpected/non-retryable failure.
func TestCommandHandler_ClearPlaylistItemCache_ActiveCaptureReturnsBusy(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("item-1").Return(offlinecache.ErrItemBusy).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	resp := assertErrorResponse(t, result, "busy")
	assert.Equal(t, true, resp["error"].(map[string]any)["retryable"])
}

func TestCommandHandler_ClearPlaylistCache_ActiveCaptureReturnsBusy(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearPlaylist("playlist-1").Return(offlinecache.ErrItemBusy).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_CACHE,
		Arguments: map[string]any{"playlistId": "playlist-1"},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "busy")
}

// TestCommandHandler_ClearPlaylistItemCache_ResyncFallsBackToCachedPlaylistWhenOffline
// is the regression test for resolveDisplayedPlaylist's cached-URL
// fallback: a device offline and displaying a playlist through
// displayPlaylist's own playlistUrl->cache fallback must still be able to
// resync replay scope after a clear, instead of resyncing silently
// failing every time because live ProcessPlaylistURL always errors while
// offline.
func TestCommandHandler_ClearPlaylistItemCache_ResyncFallsBackToCachedPlaylistWhenOffline(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("item-1").Return(nil).Times(1)

	playlistURL := "https://example.com/playlist.json"
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &playlistURL,
	}, nil).Times(1)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(nil, assertError("no network")).Times(1)

	cachedRawBytes := []byte(`{"id":"playlist-1","items":[{"id":"item-1"},{"id":"item-2"}]}`)
	cachedPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-1",
		Items: []dp1playlist.PlaylistItem{{ID: "item-1"}, {ID: "item-2"}},
	}}
	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).Return(json.RawMessage(cachedRawBytes), nil).Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(cachedRawBytes, gomock.Any()).
		DoAndReturn(func(_ []byte, v interface{}) error {
			p := v.(**dp1.Playlist)
			*p = cachedPlaylist
			return nil
		}).
		Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-1", "item-2"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
}

// TestCommandHandler_ClearPlaylistItemCache_ResyncSkipsWhenNoCachedFallbackEither
// pins the "give up quietly" side of the same fallback: if there is
// nothing cached for the URL either (or offline caching support for it
// otherwise fails), the resync must still not touch SyncPlaylist and must
// still not turn the already-successful clear into an error response.
func TestCommandHandler_ClearPlaylistItemCache_ResyncSkipsWhenNoCachedFallbackEither(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("item-1").Return(nil).Times(1)

	playlistURL := "https://example.com/playlist.json"
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &playlistURL,
	}, nil).Times(1)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(nil, assertError("no network")).Times(1)
	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).
		Return(json.RawMessage(nil), offlinecache.ErrPlaylistNotFound).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	// No SyncPlaylist expectation: gomock fails the test if one occurs.
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
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
