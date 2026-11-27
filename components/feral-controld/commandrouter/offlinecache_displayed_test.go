package commandrouter_test

import (
	"encoding/json"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// These tests pin downloadPlaylistItem's `displayed` selector
// (feral-file/feral-file#3633): a controller that never held the document
// names the on-screen item by index (and id), and the device resolves it
// from its own full player status.

func displayedArgs(index any, id string) map[string]any {
	sel := map[string]any{"index": index}
	if id != "" {
		sel["id"] = id
	}
	return map[string]any{"displayed": sel}
}

func intPtr(i int) *int { return &i }

// A URL cast (the default playlist included) is handed to the existing
// path as the exact request a phone holding the URL would send, so the
// record, its series sidecar and the URL index are written exactly as
// today and never from the status list.
func TestCommandHandler_DownloadPlaylistItem_Displayed_URLCastUsesTheExistingPath(t *testing.T) {
	for _, castCommand := range []commands.Type{commands.CMD_DISPLAY_PLAYLIST, commands.CMD_DISPLAY_DEFAULT_PLAYLIST} {
		t.Run(castCommand.String(), func(t *testing.T) {
			ts, mockOfflineCache := setupOfflineCache(t)
			defer ts.teardown()

			playlistURL := "https://example.com/playlist.json"
			a := dp1playlist.PlaylistItem{ID: "item-a", Source: "https://example.com/a.html"}
			b := dp1playlist.PlaylistItem{ID: "item-b", Source: "https://example.com/b.html"}
			playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-1", Items: []dp1playlist.PlaylistItem{a, b}}}
			marshaled := []byte(`{"id":"playlist-1"}`)

			ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
				Command:     string(castCommand),
				PlaylistURL: &playlistURL,
				Index:       intPtr(0),
				Items:       &[]dp1playlist.PlaylistItem{a, b},
			}, nil).Times(1)
			ts.mockDP1.EXPECT().FetchPlaylist(ts.ctx, playlistURL).Return(playlist, json.RawMessage(nil), nil).Times(1)
			ts.mockDP1.EXPECT().ResolveSeriesCastPlan(ts.ctx, *playlist, json.RawMessage(nil), (*dp1.SeriesPlaybackMode)(nil), 0).DoAndReturn(passthroughCastPlan).Times(1)
			mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-1").Return(uint64(4)).Times(1)
			mockOfflineCache.EXPECT().DownloadItem(ts.ctx, b).Return(nil).Times(1)
			ts.mockJSON.EXPECT().Marshal(playlist).Return(marshaled, nil).Times(1)
			mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(json.RawMessage(marshaled), playlistURL, uint64(4), gomock.Any()).Return(nil).Times(1)

			result, err := ts.handler.Process(ts.ctx, commands.Command{
				Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
				Arguments: displayedArgs(float64(1), "item-b"),
			})

			require.NoError(t, err)
			resp := assertOkResponse(t, result)
			assert.Equal(t, "queued", resp["status"])
			assert.Equal(t, b.Source, resp["source"], "the reply is how a controller without the document learns the cache identity")
		})
	}
}

// An inline or dynamic cast queues the on-screen item and writes NO
// playlist record: a record rebuilt from the status list (already
// narrowed and filtered) would replace a fuller one with the same id and
// orphan its members (plan rev 3, review round 2 finding A).
func TestCommandHandler_DownloadPlaylistItem_Displayed_InlineCastWritesNoRecord(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/1.html"}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:  string(commands.CMD_DISPLAY_PLAYLIST),
		Playlist: &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "inline-1", Items: []dp1playlist.PlaylistItem{item}}},
		Index:    intPtr(0),
		Items:    &[]dp1playlist.PlaylistItem{item},
	}, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(nil).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	mockOfflineCache.EXPECT().DownloadPlaylist(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)
	ts.mockDP1.EXPECT().FetchPlaylist(gomock.Any(), gomock.Any()).Times(0)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: displayedArgs(float64(0), "item-1"),
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "queued", resp["status"])
	assert.Equal(t, item.Source, resp["source"])
}

// A data: item's bytes already live in the playlist the player persists,
// so nothing is queued and nothing is saved.
func TestCommandHandler_DownloadPlaylistItem_Displayed_InlineDataItemIsNotQueued(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "data:image/png;base64,iVBORw0KGgo="}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command: string(commands.CMD_DISPLAY_PLAYLIST),
		Index:   intPtr(0),
		Items:   &[]dp1playlist.PlaylistItem{item},
	}, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(offlinecache.ErrItemInlineNotQueued).Times(1)
	mockOfflineCache.EXPECT().IndexPlaylistForOfflineDisplay(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).Times(0)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: displayedArgs(float64(0), "item-1"),
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, "not_queued_inline", resp["status"])
	assert.Equal(t, item.Source, resp["source"])
}

// A clear that lands while the status is being read wins: the barrier is
// sampled before the read, and nothing is queued.
func TestCommandHandler_DownloadPlaylistItem_Displayed_InlineClearDuringReadIsBusy(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCacheRaw(t)
	defer ts.teardown()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/1.html"}
	gomock.InOrder(
		mockOfflineCache.EXPECT().ClearBarrier().Return(uint64(9)),
		ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
			Command:  string(commands.CMD_DISPLAY_PLAYLIST),
			Playlist: &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "inline-1"}},
			Index:    intPtr(0),
			Items:    &[]dp1playlist.PlaylistItem{item},
		}, nil),
		mockOfflineCache.EXPECT().ClearedSinceBarrier("inline-1", uint64(9), item.Source).Return(true),
	)
	mockOfflineCache.EXPECT().DownloadItem(gomock.Any(), gomock.Any()).Times(0)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: displayedArgs(float64(0), "item-1"),
	})

	require.NoError(t, err)
	body := assertErrorResponse(t, result, "busy")
	assert.Equal(t, true, body["error"].(map[string]any)["retryable"])
}

// The race the enqueue window itself reports is surfaced the same way the
// existing path surfaces it.
func TestCommandHandler_DownloadPlaylistItem_Displayed_InlineClearDuringEnqueueIsBusy(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/1.html"}
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command: string(commands.CMD_DISPLAY_PLAYLIST),
		Index:   intPtr(0),
		Items:   &[]dp1playlist.PlaylistItem{item},
	}, nil).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, item).Return(offlinecache.ErrClearedDuringDownload).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: displayedArgs(float64(0), "item-1"),
	})

	require.NoError(t, err)
	assertErrorResponse(t, result, "busy")
}

// Every way the request can name something that is not on screen answers
// not_found and queues nothing.
func TestCommandHandler_DownloadPlaylistItem_Displayed_NotOnDisplay(t *testing.T) {
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/1.html"}
	playing := func() *status.PlayerStatus {
		return &status.PlayerStatus{
			Command: string(commands.CMD_DISPLAY_PLAYLIST),
			Index:   intPtr(0),
			Items:   &[]dp1playlist.PlaylistItem{item},
		}
	}
	cases := []struct {
		name   string
		status *status.PlayerStatus
		args   map[string]any
	}{
		{"id differs from the item at index", playing(), displayedArgs(float64(0), "item-2")},
		{"index past the end", playing(), displayedArgs(float64(1), "")},
		{"negative index", playing(), displayedArgs(float64(-1), "")},
		{"no status", nil, displayedArgs(float64(0), "")},
		{"not a playlist cast", &status.PlayerStatus{Command: "displayArtwork", Items: &[]dp1playlist.PlaylistItem{item}}, displayedArgs(float64(0), "")},
		{"no items", &status.PlayerStatus{Command: string(commands.CMD_DISPLAY_PLAYLIST)}, displayedArgs(float64(0), "")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, mockOfflineCache := setupOfflineCache(t)
			defer ts.teardown()

			ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(tc.status, nil).Times(1)
			mockOfflineCache.EXPECT().DownloadItem(gomock.Any(), gomock.Any()).Times(0)

			result, err := ts.handler.Process(ts.ctx, commands.Command{
				Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
				Arguments: tc.args,
			})

			require.NoError(t, err)
			body := assertErrorResponse(t, result, "not_found")
			assert.Equal(t, false, body["error"].(map[string]any)["retryable"])
		})
	}
}

// A malformed selector, or one combined with another way of naming the
// item, is a client bug: invalid_request before any status read.
func TestCommandHandler_DownloadPlaylistItem_Displayed_InvalidRequest(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
	}{
		{"with source", map[string]any{"displayed": map[string]any{"index": float64(0)}, "source": "https://example.com/1.html"}},
		{"with playlistUrl", map[string]any{"displayed": map[string]any{"index": float64(0)}, "playlistUrl": "https://example.com/p.json"}},
		{"with dp1_call", map[string]any{"displayed": map[string]any{"index": float64(0)}, "dp1_call": map[string]any{}}},
		{"not an object", map[string]any{"displayed": true}},
		{"no index", map[string]any{"displayed": map[string]any{"id": "item-1"}}},
		{"fractional index", displayedArgs(1.5, "")},
		{"string index", displayedArgs("1", "")},
		{"empty id", map[string]any{"displayed": map[string]any{"index": float64(0), "id": ""}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts, mockOfflineCache := setupOfflineCache(t)
			defer ts.teardown()

			ts.mockStatusPoller.EXPECT().FetchPlayerStatus(gomock.Any()).Times(0)
			mockOfflineCache.EXPECT().DownloadItem(gomock.Any(), gomock.Any()).Times(0)

			result, err := ts.handler.Process(ts.ctx, commands.Command{
				Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
				Arguments: tc.args,
			})

			require.NoError(t, err)
			body := assertErrorResponse(t, result, "invalid_request")
			assert.Equal(t, false, body["error"].(map[string]any)["retryable"])
		})
	}
}

func TestCommandHandler_DownloadPlaylistItem_Displayed_StatusReadFailureIsRetryable(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(nil, assertError("cdp not ready")).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(gomock.Any(), gomock.Any()).Times(0)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: displayedArgs(float64(0), ""),
	})

	require.NoError(t, err)
	body := assertErrorResponse(t, result, "resolve_failed")
	assert.Equal(t, true, body["error"].(map[string]any)["retryable"])
}

// The URL branch must keep a series' group identity (plan §5.3): the
// delegated request resolves through ResolveSeriesCastPlan and hands the
// resulting segments to the record write, so a displayed keep of one
// edition never strips the sidecar #455's offline narrowing reads.
func TestCommandHandler_DownloadPlaylistItem_Displayed_URLSeriesKeepsItsSegments(t *testing.T) {
	ts, mockOfflineCache := setupOfflineCache(t)
	defer ts.teardown()

	playlistURL := "https://example.com/series.json"
	representative := dp1playlist.PlaylistItem{ID: "item-series", Source: "https://example.com/series.html"}
	e1 := dp1playlist.PlaylistItem{ID: "edition-1", Source: "https://cdn.example.com/1.html"}
	e2 := dp1playlist.PlaylistItem{ID: "edition-2", Source: "https://cdn.example.com/2.html"}
	fetched := &dp1.Playlist{Playlist: dp1playlist.Playlist{ID: "playlist-s", Items: []dp1playlist.PlaylistItem{representative}}}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: &dp1.ResolvedSeriesGroup{
		Key:            dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "7"},
		Representative: representative,
		Editions:       []dp1playlist.PlaylistItem{e1, e2},
		Resolved:       true,
		ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
	}}}}

	// The screen shows one narrowed edition; the record must still hold both.
	ts.mockStatusPoller.EXPECT().FetchPlayerStatus(ts.ctx).Return(&status.PlayerStatus{
		Command:     string(commands.CMD_DISPLAY_PLAYLIST),
		PlaylistURL: &playlistURL,
		Index:       intPtr(0),
		Items:       &[]dp1playlist.PlaylistItem{e1},
	}, nil).Times(1)
	ts.mockDP1.EXPECT().FetchPlaylist(ts.ctx, playlistURL).Return(fetched, json.RawMessage(nil), nil).Times(1)
	ts.mockDP1.EXPECT().ResolveSeriesCastPlan(ts.ctx, *fetched, json.RawMessage(nil), (*dp1.SeriesPlaybackMode)(nil), 0).Return(plan).Times(1)
	mockOfflineCache.EXPECT().CurrentPlaylistClearGeneration("playlist-s").Return(uint64(2)).Times(1)
	mockOfflineCache.EXPECT().DownloadItem(ts.ctx, e1).Return(nil).Times(1)
	var saved *dp1.Playlist
	ts.mockJSON.EXPECT().Marshal(gomock.Any()).DoAndReturn(func(v any) ([]byte, error) {
		saved, _ = v.(*dp1.Playlist)
		return []byte(`{"id":"playlist-s"}`), nil
	}).Times(1)
	mockOfflineCache.EXPECT().
		IndexPlaylistForOfflineDisplay(gomock.Any(), playlistURL, uint64(2), gomock.Any()).
		DoAndReturn(func(_ json.RawMessage, _ string, _ uint64, segments []offlinecache.SeriesSegment) error {
			require.Len(t, segments, 1)
			require.NotNil(t, segments[0].Group)
			assert.Len(t, segments[0].Group.Editions, 2, "the sidecar keeps the full edition set, not the one on screen")
			return nil
		}).Times(1)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_DOWNLOAD_PLAYLIST_ITEM,
		Arguments: displayedArgs(float64(0), "edition-1"),
	})

	require.NoError(t, err)
	resp := assertOkResponse(t, result)
	assert.Equal(t, e1.Source, resp["source"])
	require.NotNil(t, saved)
	var savedSources []string
	for _, item := range saved.Items {
		savedSources = append(savedSources, item.Source)
	}
	assert.Subset(t, savedSources, []string{e1.Source, e2.Source},
		"the record holds every edition (plus the representative, per CastPlan.Full), so clearPlaylistCache still reaches them all")
}
