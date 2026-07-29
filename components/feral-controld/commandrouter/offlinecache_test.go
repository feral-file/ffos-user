package commandrouter_test

import (
	"encoding/json"
	"fmt"
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
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, nil, nil, ts.mockJSON, ts.logger)
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

	// The exact generation sampled BEFORE the download must be threaded
	// UNCHANGED into the index write, so the service can detect a
	// ClearPlaylist that raced this download+index sequence — see
	// Service.CurrentPlaylistClearGeneration's doc. A distinctive value
	// (not 0) pins that the handler passes the sampled generation through
	// rather than a hardcoded/default one.
	const sampledGen = uint64(7)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(sampledGen).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), playlistURL, sampledGen).Return(nil).Times(1)

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
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(uint64(0)).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), playlistURL, uint64(0)).
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
	// Sampled (playlistUrl present) before the download even though the
	// download fails below and no index write ever happens.
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(uint64(0)).Times(1)
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
	// This playlist has no ID; the handler still samples (playlistUrl is
	// present) before the download fails below.
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("").Return(uint64(0)).Times(1)
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
	assert.Equal(t, 1, resp["queuedCount"])
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

// TestCommandHandler_ClearPlaylistItemCache_Success also covers the
// cancel-a-queued-first-time-download case: Service reports that as a plain
// nil (see ClearItem's doc for why it is a success, not not_found), so the
// client sees this same settled ok:true response rather than a
// non-retryable error for a clear that really did cancel work.
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
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-1", "item-2"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

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
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	// No SyncPlaylist expectation: gomock fails the test if one occurs.
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

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
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

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
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-a"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_CACHE,
		Arguments: map[string]any{"playlistId": "playlist-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "playlist-1", resp["playlistId"])
}

// TestCommandHandler_ClearPlaylistCache_ResyncSkipsWhenPlaybackGenerationAdvanced
// is the regression test for the resync TOCTOU: the corrective resync
// reads the kiosk's current playlist OUTSIDE the playback lock, then
// applies the derived scope INSIDE it. If a concurrent displayPlaylist
// switches the kiosk to a different playlist under the lock in between,
// the resync's snapshot is stale and MUST NOT be applied — otherwise it
// installs the old playlist's scope over the newly-displayed one. Here the
// mocked PlaybackGeneration returns a different value on the post-lock
// re-check than on the pre-resolve sample, standing in for that
// concurrent authoritative display, so SyncPlaylist must never be called
// (an unexpected call fails the strict mock). The clear itself still
// succeeds regardless.
func TestCommandHandler_ClearPlaylistCache_ResyncSkipsWhenPlaybackGenerationAdvanced(t *testing.T) {
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
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	// First call (pre-resolve sample) returns 0; the second call (post-
	// lock guard check) returns 1, simulating a concurrent authoritative
	// display that committed a new scope while the resync was resolving.
	var gen uint64
	mockKioskReplay.EXPECT().PlaybackGeneration().
		DoAndReturn(func() uint64 {
			cur := gen
			gen++
			return cur
		}).Times(2)
	// No SyncPlaylist expectation: the guard must skip it. A call here
	// would be an unexpected mock invocation and fail the test.
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

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
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"item-1", "item-2"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

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
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	// No SyncPlaylist expectation: gomock fails the test if one occurs.
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "item-1"},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
}

// TestCommandHandler_ClearPlaylistItemCache_NotFound pins the other side of
// the same contract: an id the device never heard of is still the
// non-retryable not_found it always was. That mapping is exactly why
// Service must NOT return ErrItemNotFound for a clear that canceled a
// queued first-time download — the client would be told, non-retryably,
// that nothing happened.
func TestCommandHandler_ClearPlaylistItemCache_NotFound(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("missing").Return(offlinecache.ErrItemNotFound).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"itemId": "missing"},
	})

	require.NoError(t, err)
	resp := assertErrorResponse(t, result, "not_found")
	assert.Equal(t, false, resp["error"].(map[string]any)["retryable"])
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

	totals := offlinecache.StatusTotals{Total: 1, Ready: 1}
	diskUsed := int64(1024)
	snapshot := offlinecache.StatusSnapshot{
		Items:         []offlinecache.ItemStatus{{ItemID: "item-1", State: offlinecache.StateReady}},
		Totals:        &totals,
		DiskUsedBytes: &diskUsed,
	}
	mockOfflineCache.EXPECT().Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1", "item-2"}}).
		Return(snapshot, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{"itemIds": []interface{}{"item-1", "item-2"}},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, snapshot.Items, resp["items"])
	assert.Equal(t, snapshot.Totals, resp["totals"])
	assert.EqualValues(t, 1024, resp["diskUsed"])
	// Last page: the paging fields must be absent, not present-and-empty,
	// so a client cannot follow a cursor that does not exist.
	assert.NotContains(t, resp, "nextCursor")
	assert.NotContains(t, resp, "truncated")
}

func TestCommandHandler_GetOfflineCacheStatus_NoFilter(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	snapshot := offlinecache.StatusSnapshot{}
	mockOfflineCache.EXPECT().Status(offlinecache.StatusRequest{}).Return(snapshot, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	// Totals/diskUsed are pointers on the snapshot; a nil one must be
	// omitted rather than marshaled as null.
	assert.NotContains(t, resp, "totals")
	assert.NotContains(t, resp, "diskUsed")
}

func TestCommandHandler_GetOfflineCacheStatus_PagingArguments(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	snapshot := offlinecache.StatusSnapshot{
		Items:      []offlinecache.ItemStatus{{ItemID: "item-9", State: offlinecache.StateReady}},
		NextCursor: "item-9",
		Truncated:  true,
	}
	// limit arrives as a float64 here, the way encoding/json decodes
	// every JSON number.
	mockOfflineCache.EXPECT().Status(offlinecache.StatusRequest{Limit: 25, Cursor: "item-8"}).
		Return(snapshot, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type: commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{
			"limit":  float64(25),
			"cursor": "item-8",
		},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "item-9", resp["nextCursor"])
	assert.Equal(t, true, resp["truncated"])
}

func TestCommandHandler_GetOfflineCacheStatus_TotalsOnly(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	totals := offlinecache.StatusTotals{Total: 3, Ready: 2, Failed: 1}
	diskUsed := int64(4096)
	mockOfflineCache.EXPECT().Status(offlinecache.StatusRequest{TotalsOnly: true}).
		Return(offlinecache.StatusSnapshot{
			Items:         []offlinecache.ItemStatus{},
			Totals:        &totals,
			DiskUsedBytes: &diskUsed,
		}, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{"totalsOnly": true},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Empty(t, resp["items"])
	assert.Equal(t, &totals, resp["totals"])
}

func TestCommandHandler_GetOfflineCacheStatus_RejectsInvalidArguments(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
	}{
		{
			// The regression this closes: a bare string used to fall
			// through to "report on every item" instead of erroring.
			name: "itemIds not an array",
			args: map[string]any{"itemIds": "item-1"},
		},
		{
			name: "itemIds holds a non-string",
			args: map[string]any{"itemIds": []interface{}{"item-1", 7}},
		},
		{
			name: "itemIds holds an empty string",
			args: map[string]any{"itemIds": []interface{}{""}},
		},
		{
			name: "itemIds over the per-request cap",
			args: map[string]any{"itemIds": tooManyItemIDs()},
		},
		{
			name: "limit is not a number",
			args: map[string]any{"limit": "25"},
		},
		{
			name: "limit is fractional",
			args: map[string]any{"limit": 2.5},
		},
		{
			name: "limit is negative",
			args: map[string]any{"limit": float64(-1)},
		},
		{
			name: "cursor is not a string",
			args: map[string]any{"cursor": 7},
		},
		{
			name: "cursor is empty",
			args: map[string]any{"cursor": ""},
		},
		{
			name: "totalsOnly is not a boolean",
			args: map[string]any{"totalsOnly": "yes"},
		},
		{
			name: "totalsOnly combined with cursor",
			args: map[string]any{"totalsOnly": true, "cursor": "item-1"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, mockOfflineCache := setupOfflineCache(t)
			defer ts.teardown()

			// The whole point of validating here is that the service is
			// never asked to do the work.
			mockOfflineCache.EXPECT().Status(gomock.Any()).Times(0)

			result, err := ts.handler.Process(ts.ctx, commands.Command{
				Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
				Arguments: tt.args,
			})

			require.NoError(t, err)
			assertErrorResponse(t, result, "invalid_request")
		})
	}
}

func tooManyItemIDs() []interface{} {
	ids := make([]interface{}, offlinecache.MaxStatusItemIDs+1)
	for i := range ids {
		ids[i] = fmt.Sprintf("item-%d", i)
	}
	return ids
}

func TestCommandHandler_GetOfflineCacheStatus_ServiceError(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().Status(offlinecache.StatusRequest{}).
		Return(offlinecache.StatusSnapshot{}, assertError("disk error")).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "offline_cache_error")
}

type assertError string

func (e assertError) Error() string { return string(e) }

// TestCommandHandler_DownloadPlaylistItem_ClearWonIsReportedNotAcked pins
// the command-level half of the false-"queued" fix: when a concurrent
// clear wins, downloadPlaylistItem must NOT answer with the flat
// ok:true/status:"queued" it returns on the happy path, because nothing
// was queued and nothing ever will be. It is a retryable busy — re-issuing
// once the clear has settled queues normally.
func TestCommandHandler_DownloadPlaylistItem_ClearWonIsReportedNotAcked(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(uint64(0)).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).
		Return(offlinecache.ErrClearedDuringDownload).Times(1)
	// The URL index write must not happen: it is deliberately downstream
	// of a SUCCESSFUL queue, and this download queued nothing.
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"itemId": "item-1", "playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	body := assertErrorResponse(t, result, "busy")
	errBody, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, errBody["retryable"], "the clear has settled by the time the client retries, so this must be retryable")
}
