package commandrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
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
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": item.Source},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "queued", resp["status"])
	assert.Equal(t, item.Source, resp["source"])
}

// TestCommandHandler_DownloadPlaylistItem_InlinePlaylistPersistsBodyWithEmptySourceURL
// pins the distinction an earlier version of this handler got wrong: a
// dp1_call (inline) download has no source URL to index BY, but its
// playlist body must still be persisted. The empty sourceURL argument is
// the whole contract — Service skips only the URL index on "", never the
// body save — and it mirrors
// TestCommandHandler_DownloadPlaylist_InlinePlaylistPassesEmptySourceURL's
// contract for the bulk path, which has always saved unconditionally.
//
// What the earlier "skip the whole call for inline" behavior actually
// broke was reversibility, not the offline fallback: ClearPlaylist loads
// the record BY ID to enumerate member sources, so with nothing saved it
// failed ErrPlaylistNotFound and left the cached items clearable only
// one-by-one by source.
func TestCommandHandler_DownloadPlaylistItem_InlinePlaylistPersistsBodyWithEmptySourceURL(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	playlistMap := map[string]interface{}{"id": "playlist-1", "items": []interface{}{map[string]interface{}{"id": "item-1", "source": item.Source}}}
	playlistBytes := []byte(`{"id":"playlist-1","items":[{"id":"item-1","source":"https://example.com/index.html"}]}`)
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}
	marshaled := []byte(`{"id":"playlist-1","resolved":true}`)

	ts.mockJSON.EXPECT().Marshal(playlistMap).Return(playlistBytes, nil).Times(1)
	ts.mockJSON.EXPECT().Unmarshal(playlistBytes, gomock.Any()).DoAndReturn(func(_ []byte, v interface{}) error {
		p := v.(**dp1.Playlist)
		*p = playlist
		return nil
	}).Times(1)
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(uint64(7)).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	// Empty sourceURL, and the SAMPLED generation — not a fresh read —
	// so a ClearPlaylist racing the download still wins.
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), "", uint64(7)).
		Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"dp1_call": playlistMap, "source": item.Source},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
}

// TestCommandHandler_DownloadPlaylistItem_InlineItemPersistsBody covers
// the case the body matters MOST for: an item whose bytes are inline in
// the playlist (a data: source), so DownloadItem queues nothing and
// returns ErrItemInlineNotQueued. The item is already "offline" only in
// the sense that this body holds it — dropping the body on the outcome
// that reports success is what would make that claim false.
func TestCommandHandler_DownloadPlaylistItem_InlineItemPersistsBody(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	source := "data:text/html;base64,PGh0bWw+"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: source}
	playlistMap := map[string]interface{}{"id": "playlist-1", "items": []interface{}{map[string]interface{}{"id": "item-1", "source": source}}}
	playlistBytes := []byte(`{"id":"playlist-1","items":[{"id":"item-1","source":"` + source + `"}]}`)
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}
	marshaled := []byte(`{"id":"playlist-1"}`)

	ts.mockJSON.EXPECT().Marshal(playlistMap).Return(playlistBytes, nil).Times(1)
	ts.mockJSON.EXPECT().Unmarshal(playlistBytes, gomock.Any()).DoAndReturn(func(_ []byte, v interface{}) error {
		p := v.(**dp1.Playlist)
		*p = playlist
		return nil
	}).Times(1)
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(uint64(0)).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(offlinecache.ErrItemInlineNotQueued).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), "", uint64(0)).
		Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"dp1_call": playlistMap, "source": source},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	require.Equal(t, "not_queued_inline", resp["status"])
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
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": item.Source},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "queued", resp["status"])
}

func TestCommandHandler_DownloadPlaylistItem_MissingSource(t *testing.T) {
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
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": "https://example.com/never-in-playlist.html"},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "not_found")
}

func TestCommandHandler_DownloadPlaylistItem_ResolveFailure(t *testing.T) {
	ts, _ := setupOfflineCache(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"source": "https://example.com/index.html"}, // neither playlistUrl nor dp1_call
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
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": item.Source},
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
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": item.Source},
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

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"source": "https://example.com/item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "https://example.com/item-1", resp["source"])
}

func TestCommandHandler_ClearPlaylistItemCache_MissingSource(t *testing.T) {
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

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(nil).Times(1)

	playlistURL := "https://example.com/playlist.json"
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "item-1", Source: "https://example.com/item-1"}, {ID: "item-2", Source: "https://example.com/item-2"},
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
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"https://example.com/item-1", "https://example.com/item-2"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"source": "https://example.com/item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "https://example.com/item-1", resp["source"])
}

// TestCommandHandler_ClearPlaylistItemCache_SkipsResyncWhenNotDisplayingPlaylist
// pins that resyncKioskReplayScopeToCurrentDisplay is a no-op (no SyncPlaylist
// call at all) when the player is not currently on a displayPlaylist
// command — nothing to resync.
func TestCommandHandler_ClearPlaylistItemCache_SkipsResyncWhenNotDisplayingPlaylist(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(nil).Times(1)
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
		Arguments: map[string]any{"source": "https://example.com/item-1"},
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

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(nil).Times(1)
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(nil, assertError("cdp not ready")).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"source": "https://example.com/item-1"},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "https://example.com/item-1", resp["source"])
}

// TestCommandHandler_ClearPlaylistCache_ResyncsKioskReplayScope mirrors
// TestCommandHandler_ClearPlaylistItemCache_ResyncsKioskReplayScope for the
// whole-playlist clear path.
func TestCommandHandler_ClearPlaylistCache_ResyncsKioskReplayScope(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearPlaylist("playlist-1").Return(nil).Times(1)

	inlinePlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{ID: "item-a", Source: "https://example.com/item-a"}}}}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:  string(commands.CMD_DISPLAY_PLAYLIST),
		Playlist: inlinePlaylist,
	}, nil).Times(1)

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"https://example.com/item-a"}).Return(nil).Times(1)
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

	inlinePlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{ID: "item-a", Source: "https://example.com/item-a"}}}}
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

// lockStepKioskReplay is a hand-rolled offlinecache.KioskReplay (a real
// mutex, not gomock) for deterministic clear-versus-display interleaving
// tests. Every LockPlayback attempt signals lockAttempts BEFORE blocking
// on the mutex, so a test goroutine standing in for an in-flight
// displayPlaylist critical section can hold the lock until it observes
// the resync arrive at it — no sleeps, no timing.
type lockStepKioskReplay struct {
	mu           sync.Mutex
	gen          atomic.Uint64
	lockAttempts chan struct{}
	syncCalls    chan []string
}

func newLockStepKioskReplay() *lockStepKioskReplay {
	return &lockStepKioskReplay{
		lockAttempts: make(chan struct{}, 4),
		syncCalls:    make(chan []string, 1),
	}
}

func (f *lockStepKioskReplay) AttachOnReconnect(context.Context) error { return nil }
func (f *lockStepKioskReplay) SyncPlaylist(_ context.Context, ids []string) error {
	f.syncCalls <- ids
	return nil
}
func (f *lockStepKioskReplay) LockPlayback() {
	select {
	case f.lockAttempts <- struct{}{}:
	default:
	}
	f.mu.Lock()
}
func (f *lockStepKioskReplay) UnlockPlayback()            { f.mu.Unlock() }
func (f *lockStepKioskReplay) PlaybackGeneration() uint64 { return f.gen.Load() }
func (f *lockStepKioskReplay) MarkPlaybackChanged()       { f.gen.Add(1) }

// TestCommandHandler_ClearItemCache_ResyncWaitsOutInFlightDisplayScopeInstall
// is the deterministic clear-versus-display interleaving regression test
// for the resync's generation sample being taken UNDER the playback lock.
//
// The hazard (feral-file/ffos-user#229 review finding): a displayPlaylist
// sync+send critical section already holds the playback lock when the
// clear lands — its SyncPlaylist store reads PREDATE the clear's
// deletion, so the scope it is installing references just-deleted
// records/blobs, and it publishes its generation bump only inside that
// section. If the resync sampled the generation WITHOUT the lock, it
// could read the pre-bump value, then observe the bump at the post-lock
// re-check and defer to that stale scope — leaving replay pointed at
// deleted blobs until the next refresher pass. Sampling under the lock
// forces the resync to wait that section out, so its baseline includes
// the bump and the resync proceeds to recompute scope from post-clear
// store state.
//
// The interleaving is deterministic in both directions: with the locked
// sample, the resync's first LockPlayback attempt signals the
// display-section goroutine, which bumps the generation and releases —
// the resync then proceeds and MUST call SyncPlaylist. With an unlocked
// sample (the regression), the sample reads the pre-bump generation
// immediately, the first lock attempt only happens at the re-check, and
// the re-check sees the bumped generation and bails — the missing
// SyncPlaylist call fails the test.
func TestCommandHandler_ClearItemCache_ResyncWaitsOutInFlightDisplayScopeInstall(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(nil).Times(1)

	playlistURL := "https://example.com/playlist.json"
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "item-1", Source: "https://example.com/item-1"}, {ID: "item-2", Source: "https://example.com/item-2"},
	}}}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &playlistURL,
	}, nil).Times(1)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)

	fake := newLockStepKioskReplay()
	// The in-flight display critical section: lock held from BEFORE the
	// clear starts (its store reads therefore predate the deletion), the
	// authoritative generation bump published inside the section, and the
	// lock released only once the resync is observed waiting on it.
	fake.mu.Lock()
	displayDone := make(chan struct{})
	go func() {
		defer close(displayDone)
		<-fake.lockAttempts
		fake.gen.Add(1) // MarkPlaybackChanged, as the display path does under the lock
		fake.mu.Unlock()
	}()

	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, fake, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"source": "https://example.com/item-1"},
	})

	require.NoError(t, err)
	assertOkResponse(t, result)
	<-displayDone

	select {
	case ids := <-fake.syncCalls:
		assert.Equal(t, []string{"https://example.com/item-1", "https://example.com/item-2"}, ids,
			"the resync must recompute scope from post-clear state, not defer to the pre-clear install")
	default:
		t.Fatal("resync deferred to a scope installed from pre-clear store reads: SyncPlaylist was never called, replay stays pointed at just-deleted blobs")
	}
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

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(offlinecache.ErrItemBusy).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"source": "https://example.com/item-1"},
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

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(nil).Times(1)

	playlistURL := "https://example.com/playlist.json"
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &playlistURL,
	}, nil).Times(1)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(nil, assertError("no network")).Times(1)

	cachedRawBytes := []byte(`{"id":"playlist-1","items":[{"id":"item-1","source":"https://example.com/item-1"},{"id":"item-2","source":"https://example.com/item-2"}]}`)
	cachedPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-1",
		Items: []dp1playlist.PlaylistItem{{ID: "item-1", Source: "https://example.com/item-1"}, {ID: "item-2", Source: "https://example.com/item-2"}},
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
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"https://example.com/item-1", "https://example.com/item-2"}).Return(nil).Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"source": "https://example.com/item-1"},
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

	mockOfflineCache.EXPECT().ClearItem("https://example.com/item-1").Return(nil).Times(1)

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
		Arguments: map[string]any{"source": "https://example.com/item-1"},
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

	mockOfflineCache.EXPECT().ClearItem("https://example.com/missing").Return(offlinecache.ErrItemNotFound).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
		Arguments: map[string]any{"source": "https://example.com/missing"},
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

func TestCommandHandler_GetOfflineCacheStatus_WithSources(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	totals := offlinecache.StatusTotals{Total: 1, Ready: 1}
	diskUsed := int64(1024)
	snapshot := offlinecache.StatusSnapshot{
		Items:         []offlinecache.ItemStatus{{Source: "https://example.com/item-1", State: offlinecache.StateReady}},
		Totals:        &totals,
		DiskUsedBytes: &diskUsed,
	}
	mockOfflineCache.EXPECT().Status(offlinecache.StatusRequest{Sources: []string{"https://example.com/item-1", "https://example.com/item-2"}}).
		Return(snapshot, nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{"sources": []interface{}{"https://example.com/item-1", "https://example.com/item-2"}},
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
		Items:      []offlinecache.ItemStatus{{Source: "https://example.com/item-9", State: offlinecache.StateReady}},
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
			name: "sources not an array",
			args: map[string]any{"sources": "https://example.com/item-1"},
		},
		{
			name: "sources holds a non-string",
			args: map[string]any{"sources": []interface{}{"https://example.com/item-1", 7}},
		},
		{
			name: "sources holds an empty string",
			args: map[string]any{"sources": []interface{}{""}},
		},
		{
			name: "sources over the per-request cap",
			args: map[string]any{"sources": tooManySources()},
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

// TestCommandHandler_OfflineCache_LegacyItemIDShapeIsRejectedLoudly is the
// controller-boundary contract test for the source-keying cutover: a
// client still speaking the pre-cutover `itemId`/`itemIds` shape must be
// rejected with a NON-retryable `invalid_request` on every command, never
// silently misinterpreted.
//
// "Silently misinterpreted" is the failure this pins, and it is the one
// that would actually be dangerous: an id accepted where a source belongs
// would hash to a key nothing can ever match, so the device would report
// work queued/cleared for an artwork it never touched. Failing closed
// means a stale client sees an immediate, non-retryable error naming the
// missing field instead — the honest signal that it needs the coordinated
// release.
//
// Deliberately no legacy-compat branch is being pinned here: the
// itemId-keyed contract never shipped in any release tag (the offline
// cache command family merged after v1.0.21 and is opt-in via
// offlineCache.enabled), so there is no fielded caller to preserve — see
// docs/api-design.md's current-v1 posture, whose rule 2 allows a rename
// through a coordinated release that updates all callers. This test is
// what stops a future package release from quietly loosening that
// rejection into a silent misread.
func TestCommandHandler_OfflineCache_LegacyItemIDShapeIsRejectedLoudly(t *testing.T) {
	tests := []struct {
		name string
		cmd  commands.Type
		args map[string]any
	}{
		{
			name: "downloadPlaylistItem with legacy itemId",
			cmd:  commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
			args: map[string]any{"playlistUrl": "https://example.com/playlist.json", "itemId": "work-1"},
		},
		{
			name: "clearPlaylistItemCache with legacy itemId",
			cmd:  commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE,
			args: map[string]any{"itemId": "work-1"},
		},
		{
			name: "getOfflineCacheStatus with legacy itemIds",
			cmd:  commands.CMD_GET_OFFLINE_CACHE_STATUS,
			args: map[string]any{"itemIds": []interface{}{"work-1", "work-2"}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts, mockOfflineCache := setupOfflineCache(t)
			defer ts.teardown()

			// The service must never be reached: a legacy-shape request
			// is rejected at the argument boundary, so no download is
			// queued, no cache is cleared, and no status work is done.
			mockOfflineCache.EXPECT().DownloadItem(gomock.Any(), gomock.Any()).Times(0)
			mockOfflineCache.EXPECT().ClearItem(gomock.Any()).Times(0)
			mockOfflineCache.EXPECT().Status(gomock.Any()).Times(0)

			result, err := ts.handler.Process(ts.ctx, commands.Command{Type: tt.cmd, Arguments: tt.args})
			require.NoError(t, err)

			body := assertErrorResponse(t, result, "invalid_request")
			errBody, ok := body["error"].(map[string]any)
			require.True(t, ok)
			assert.Equal(t, false, errBody["retryable"],
				"a stale client cannot fix this by retrying — it needs the coordinated release, so the error must be non-retryable")
		})
	}
}

// TestCommandHandler_GetOfflineCacheStatus_LegacyItemIDsDoNotWidenTheReport
// pins the one legacy shape that could fail OPEN rather than closed:
// `getOfflineCacheStatus` treats an absent `sources` as "report on every
// known item", so an unrecognized `itemIds` key being ignored (rather
// than rejected) would silently turn a stale client's narrow query into a
// full-store scan — the exact "a client-side typo becomes a full-store
// scan" hazard parseStatusRequest's strict validation exists to prevent.
func TestCommandHandler_GetOfflineCacheStatus_LegacyItemIDsDoNotWidenTheReport(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	// No Status expectation at all: a whole-store snapshot must never be
	// computed for a request that only named two items in the old shape.
	mockOfflineCache.EXPECT().Status(gomock.Any()).Times(0)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_GET_OFFLINE_CACHE_STATUS,
		Arguments: map[string]any{"itemIds": []interface{}{"work-1", "work-2"}},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "invalid_request")
}

func tooManySources() []interface{} {
	sources := make([]interface{}, offlinecache.MaxStatusSources+1)
	for i := range sources {
		sources[i] = fmt.Sprintf("https://example.com/item-%d", i)
	}
	return sources
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
		Arguments: map[string]any{"source": item.Source, "playlistUrl": playlistURL},
	})

	require.NoError(t, err)
	body := assertErrorResponse(t, result, "busy")
	errBody, ok := body["error"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, true, errBody["retryable"], "the clear has settled by the time the client retries, so this must be retryable")
}

// TestCommandHandler_DownloadPlaylistItem_InlineItemIsNotReportedQueued
// pins the distinction between "accepted" and "queued". A data: item's
// bytes travel inside the playlist body, so DownloadItem does no work and
// no progress notification will ever be emitted for it. Reporting
// status:"queued" for that told the client to wait for something that
// never comes.
//
// The index step must still run: indexing is what persists the playlist
// body those inline bytes live in, so it matters MORE for an inline item
// than for a downloaded one.
func TestCommandHandler_DownloadPlaylistItem_InlineItemIsNotReportedQueued(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "data:image/png;base64,iVBORw0KGgo="}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}
	marshaled := []byte(`{"id":"playlist-1"}`)

	const sampledGen = uint64(3)
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(sampledGen).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).
		Return(offlinecache.ErrItemInlineNotQueued).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().
		IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), playlistURL, sampledGen).
		Return(nil).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": item.Source},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "not_queued_inline", resp["status"],
		"an inline item is accepted but nothing is queued; saying otherwise promises a notification that never arrives")
	assert.Equal(t, item.Source, resp["source"])
}

// TestCommandHandler_DownloadPlaylistItem_InlinePersistFailureIsAnError
// pins the asymmetry between the two outcomes. For a QUEUED item the save
// is best-effort — the bytes are being fetched independently, so a failed
// save costs only the by-URL fallback. For an INLINE item the save is the
// only thing that happens: the bytes live nowhere but this playlist body,
// so ok:true after a failed save would claim an offline copy that does
// not exist anywhere on disk.
//
// This is a WIRE-VISIBLE change: a request that previously answered
// ok:true/not_queued_inline now answers an error when persistence fails.
func TestCommandHandler_DownloadPlaylistItem_InlinePersistFailureIsAnError(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	source := "data:text/html;base64,PGh0bWw+"
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: source}
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{item}}}
	playlistURL := "https://example.com/playlist.json"
	marshaled := []byte(`{"id":"playlist-1"}`)

	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).Times(1)
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(uint64(0)).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(offlinecache.ErrItemInlineNotQueued).Times(1)
	ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), playlistURL, uint64(0)).
		Return(errors.New("disk full")).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": source},
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "internal")
}

// TestCommandHandler_DownloadPlaylistItem_QueuedPersistFailureStillSucceeds
// is the other half of that asymmetry, and exists so a future edit cannot
// "simplify" the inline case above into failing every outcome: the item
// really is queued, and its bytes really are being written independently
// of this record.
func TestCommandHandler_DownloadPlaylistItem_QueuedPersistFailureStillSucceeds(t *testing.T) {
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
		Return(errors.New("disk full")).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: map[string]any{"playlistUrl": playlistURL, "source": item.Source},
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	require.Equal(t, "queued", resp["status"])
}
