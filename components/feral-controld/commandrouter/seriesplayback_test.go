package commandrouter_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/seriesplayback"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func newSeriesPlaybackStore(t *testing.T) *seriesplayback.Store {
	t.Helper()
	store, err := seriesplayback.Open(filepath.Join(t.TempDir(), "series-playback.json"))
	require.NoError(t, err)
	return store
}

func TestCommandHandler_GetSeriesPlayback_UnavailableWhenNotWired(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_SERIES_PLAYBACK, Arguments: map[string]interface{}{}})

	require.NoError(t, err)
	m := result.(map[string]interface{})
	assert.Equal(t, false, m["ok"])
	assert.Equal(t, "seriesPlaybackUnavailable", m["error"])
}

func TestCommandHandler_GetSeriesPlayback_DefaultsToFollowTheDocument(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSeriesPlayback(ts.handler, newSeriesPlaybackStore(t), ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_SERIES_PLAYBACK, Arguments: map[string]interface{}{}})

	require.NoError(t, err)
	m := result.(map[string]interface{})
	assert.Equal(t, true, m["ok"])
	assert.Nil(t, m["seriesPlayback"])
}

func TestCommandHandler_GetSeriesPlayback_RejectsANonEmptyRequest(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSeriesPlayback(ts.handler, newSeriesPlaybackStore(t), ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_SERIES_PLAYBACK, Arguments: map[string]interface{}{"a": 1}})

	require.NoError(t, err)
	m := result.(map[string]interface{})
	assert.Equal(t, false, m["ok"])
	assert.Equal(t, "invalidRequest", m["error"])
}

func TestCommandHandler_SetSeriesPlayback_PersistsAndReportsTheMode(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSeriesPlayback(ts.handler, newSeriesPlaybackStore(t), ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	m := result.(map[string]interface{})
	require.Equal(t, true, m["ok"])
	assert.Equal(t, "first", m["seriesPlayback"])

	// A later get reports exactly what was just set — no series was being
	// displayed, so there is nothing to re-display (see the dedicated re-display
	// test below), but the setting itself must still be durable.
	getResult, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_GET_SERIES_PLAYBACK, Arguments: map[string]interface{}{}})
	require.NoError(t, err)
	assert.Equal(t, "first", getResult.(map[string]interface{})["seriesPlayback"])
}

func TestCommandHandler_SetSeriesPlayback_NullClearsBackToFollowTheDocument(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSeriesPlayback(ts.handler, newSeriesPlaybackStore(t), ts.logger)

	_, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "full"},
	})
	require.NoError(t, err)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": nil},
	})
	require.NoError(t, err)
	m := result.(map[string]interface{})
	require.Equal(t, true, m["ok"])
	assert.Nil(t, m["seriesPlayback"])
}

func TestCommandHandler_SetSeriesPlayback_RejectsAnUnrecognizedMode(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSeriesPlayback(ts.handler, newSeriesPlaybackStore(t), ts.logger)

	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "shuffle"},
	})
	require.NoError(t, err)
	m := result.(map[string]interface{})
	assert.Equal(t, false, m["ok"])
	assert.Equal(t, "invalidRequest", m["error"])
}

// castedItems digs every playlist item out of the CDP expression the
// handler sent, which has the shape window.handleCDPRequest(<command JSON>).
func castedItems(t *testing.T, expression string) []map[string]interface{} {
	t.Helper()
	const prefix = "window.handleCDPRequest("
	require.True(t, strings.HasPrefix(expression, prefix), "unexpected CDP expression: %s", expression)
	require.True(t, strings.HasSuffix(expression, ")"), "unexpected CDP expression: %s", expression)

	var payload struct {
		Request struct {
			DP1Call struct {
				Items []map[string]interface{} `json:"items"`
			} `json:"dp1_call"`
		} `json:"request"`
	}
	require.NoError(t, json.Unmarshal([]byte(strings.TrimSuffix(strings.TrimPrefix(expression, prefix), ")")), &payload))
	return payload.Request.DP1Call.Items
}

func seriesEditionItem(id string) dp1playlist.PlaylistItem {
	return dp1playlist.PlaylistItem{
		ID:     id,
		Source: "https://example.com/" + id + ".mp4",
	}
}

// AC1/AC3/AC5 of feral-file/ffos-user#425, end to end at the command-handler
// level: a static dp1_call display request whose series item resolves to a group of
// editions is narrowed before it ever reaches the player (no device
// override yet, so the default is full — every edition); changing the
// device setting then re-displays the SAME series narrowed to its
// representative, without the test's mocked catalog call firing a second
// time (ResolveSeriesCastPlan itself is mocked here — the dp1 package's own
// no-second-catalog-call guarantee is pinned separately in
// dp1/seriesplayback_test.go — but the cache that makes the re-display possible
// AT ALL, and the re-display actually happening, are this handler's job).
func TestCommandHandler_SetSeriesPlayback_RecastsTheCurrentlyDisplayedSeries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	// Representative is deliberately a different item than editions[0]: it
	// pins that the re-display narrows to the DOCUMENT's own representative
	// (feralfile-bot F1, 2026-10-30), not whichever edition the catalog
	// happened to return first.
	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(initialPlan).
		Times(1)
	// The re-display's rebuilt document carries concrete editions, not a
	// seriesRegistry item, so the real resolveSeriesCastPlan would see no
	// series groups at all here — a passthrough is the correct stand-in.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	var expressions []string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			expressions = append(expressions, expr)
			return playerOkResponse(), nil
		}).
		Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)
	require.Len(t, expressions, 1)
	assert.Len(t, castedItems(t, expressions[0]), 3, "no device override yet; the default (full) displays every edition")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])

	require.Len(t, expressions, 2, "setSeriesPlayback must re-display the currently displayed series")
	narrowed := castedItems(t, expressions[1])
	require.Len(t, narrowed, 1, "the new device override (first) must narrow the re-display to the representative")
	assert.Equal(t, "item-series", narrowed[0]["id"], "first must display the document's own representative, not the catalog's first-returned edition")
}

// TestCommandHandler_SetSeriesPlayback_RecastToRandomPushesTheChosenPickToTheRecorder
// pins feral-file/ffos-user#459 (F6 reopened): recastForMode's own re-narrow
// is the ONLY place a setSeriesPlayback-driven switch into random mode ever
// makes its pick — the re-entrant Process call it issues resolves no series
// groups at all (the rebuilt document carries no seriesRegistry marker), so
// that call's own copy of this same push (handler.go) reports nothing. If
// recastForMode did not also push, playlist-refresher would have no way to
// learn this pick other than reading the live display next tick — exactly
// the read rememberedWithheld (F8) can skip.
func TestCommandHandler_SetSeriesPlayback_RecastToRandomPushesTheChosenPickToTheRecorder(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	var recorded []dp1.RandomGroupPick
	commandrouter.SetAcceptedPickRecorder(handler, func(picks []dp1.RandomGroupPick) {
		recorded = append(recorded, picks...)
	}, logger)

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "387"}
	editions := []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")}
	group := &dp1.ResolvedSeriesGroup{
		Key:            key,
		Representative: seriesEditionItem("item-series"),
		Editions:       editions,
		Resolved:       true,
		ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(initialPlan).
		Times(1)
	// Same passthrough stand-in as the sibling test above: the rebuilt
	// re-display document carries concrete editions, not a seriesRegistry
	// item, so it resolves no groups.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	var expressions []string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			expressions = append(expressions, expr)
			return playerOkResponse(), nil
		}).
		Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)
	require.Len(t, expressions, 1)
	assert.Empty(t, recorded, "no device override yet: the default (full) mode makes no random choice to push")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "random"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])
	require.Len(t, expressions, 2, "setSeriesPlayback must re-display the currently displayed series")

	narrowed := castedItems(t, expressions[1])
	require.Len(t, narrowed, 1, "random narrows a distinct-editions group to one picked edition")

	require.Len(t, recorded, 1, "recastForMode's own re-narrow must push the pick it just made")
	assert.Equal(t, key, recorded[0].Key)
	assert.Equal(t, narrowed[0]["source"], recorded[0].Source, "the pushed pick must be the exact edition the re-display actually sent")
	assert.Equal(t, dp1.SeriesArtworkModelMultiUnique, recorded[0].ArtworkModel)
}

// TestCommandHandler_DisplayPlaylist_PushesTheAcceptedRandomPickOnTheInitialCast
// covers the OTHER call site of the same push (handler.go, not
// recastForMode): a static dp1_call display request that already resolves under a
// device-level random override on its very first display, with no
// setSeriesPlayback re-display involved at all.
func TestCommandHandler_DisplayPlaylist_PushesTheAcceptedRandomPickOnTheInitialCast(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	var recorded []dp1.RandomGroupPick
	commandrouter.SetAcceptedPickRecorder(handler, func(picks []dp1.RandomGroupPick) {
		recorded = append(recorded, picks...)
	}, logger)

	// Nothing displayed yet, so this just persists the override; recastForMode
	// no-ops (nothing tracked to re-narrow).
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "random"},
	})
	require.NoError(t, err)
	assert.Empty(t, recorded, "no series displayed yet: nothing to push")

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "387"}
	group := &dp1.ResolvedSeriesGroup{
		Key:            key,
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
		ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
	}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(plan).
		Times(1)

	var expression string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expression, _ = params["expression"].(string)
			return playerOkResponse(), nil
		}).
		Times(1)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err = handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)

	narrowed := castedItems(t, expression)
	require.Len(t, narrowed, 1, "random narrows a distinct-editions group to one picked edition")

	require.Len(t, recorded, 1, "the initial display request's own accepted pick must be pushed")
	assert.Equal(t, key, recorded[0].Key)
	assert.Equal(t, narrowed[0]["source"], recorded[0].Source)
}

// TestCommandHandler_SetSeriesPlayback_RecastOfAPlaylistURLCastDoesNotReResolveTheDynamicQuery
// is run-reviewer's F1 on PR #454 (feral-file/ffos-user#452): before this
// fix, a display request whose ORIGIN was dynamic (playlistUrl or a dp1_call carrying a
// dynamicQuery) stored a shell that still carried the original DynamicQuery,
// so recastForMode's rebuilt document still had it too, and re-entering
// Process saw HasDynamicContent() true and called ProcessDynamicPlaylistForCast
// again instead of the documented network-free re-narrow. Pinned by NOT
// mocking ProcessDynamicPlaylistForCast at all: a regression calls it and
// gomock fails the test immediately with "unexpected call", rather than this
// test having to assert a call count.
func TestCommandHandler_SetSeriesPlayback_RecastOfAPlaylistURLCastDoesNotReResolveTheDynamicQuery(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	const url = "https://feed.example/exhibition.json"

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b")},
		Resolved:       true,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	dynamicOriginPlaylist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID:    "playlist-1",
			Items: []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b")},
			DynamicQuery: &playlists.DynamicQuery{
				Profile:  dp1playlist.ProfileGraphQLV1,
				Endpoint: "https://example.com/graphql",
				Query:    `query { items(limit: {{limit}}, offset: {{offset}}) { id source } }`,
				ResponseMapping: playlists.ResponseMapping{
					ItemsPath:  "data.items",
					ItemSchema: "dp1/1.0",
				},
			},
		},
	}
	// The one and only call this display request's origin ever gets. Deliberately no
	// EXPECT() for ProcessDynamicPlaylistForCast at all — see the test doc.
	mockDP1.EXPECT().
		ProcessPlaylistURLForCast(gomock.Any(), url, gomock.Any()).
		Return(dynamicOriginPlaylist, initialPlan, nil).
		Times(1)
	// The re-display's rebuilt document, once DynamicQuery is cleared, decodes
	// as an ordinary static dp1_call carrying concrete editions (no
	// seriesRegistry item left to resolve) — this is that branch's call.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": url},
	})
	require.NoError(t, err)

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"],
		"the re-display must succeed locally, never reaching ProcessDynamicPlaylistForCast")
}

// TestCommandHandler_SetSeriesPlayback_RecastOfAScheduledPlaylistURLCastPreservesTheSchedulerSource
// is feralfile-bot's F1 on PR #454: rememberSeriesCast retained the resolved
// items but not the originating playlistschedule.Source, so
// recastForMode's rebuilt document — always classified as a static dp1_call
// (see the DynamicQuery-clearing fix just above) — reached the scheduler's
// PrepareWithSource with a ZERO-VALUE source, silently resetting the
// scheduler's durable playlistUrl the moment the display request also carries a
// displayAt schedule (PrepareWithSource only writes s.source for a
// scheduled result — see HasDisplayAtSchedule's doc). A real
// playlistschedule.Scheduler is wired (not mocked) so this pins the actual
// stored Source, not just that a re-display happened.
func TestCommandHandler_SetSeriesPlayback_RecastOfAScheduledPlaylistURLCastPreservesTheSchedulerSource(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t)
	ctx := context.Background()
	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()

	sched := playlistschedule.New(ctx, mocks.CommandSender{CDP: mockCDP}, mockClock, func() *time.Location { return loc }, logger)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, sched, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	const url = "https://feed.example/scheduled-series.json"
	// In the past relative to `now` so the display request's single item is the active
	// cohort immediately, never the future-only deferred case.
	representative := dp1playlist.PlaylistItem{
		ID: "item-series", Source: "https://example.com/item-series.mp4",
		DisplayAt: strPtr("2026-07-21T00:00:00Z"),
	}
	group := &dp1.ResolvedSeriesGroup{
		Representative: representative,
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b")},
		Resolved:       true,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	fetchedPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-1",
		Items: []dp1playlist.PlaylistItem{representative},
	}}

	mockDP1.EXPECT().
		ProcessPlaylistURLForCast(gomock.Any(), url, gomock.Any()).
		Return(fetchedPlaylist, initialPlan, nil).
		Times(1)
	// The re-display's rebuilt document, once narrowed to the representative,
	// carries a concrete edition (no seriesRegistry item left to resolve) —
	// this is that branch's call.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	_, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": url},
	})
	require.NoError(t, err)
	require.True(t, sched.HasCache())
	wantSource := playlistschedule.Source{PlaylistURL: url, ContentContext: "curated"}
	require.Equal(t, wantSource, sched.Source(), "the initial scheduled display request must record its originating playlistUrl")

	result, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])

	assert.Equal(t, wantSource, sched.Source(),
		"recastForMode's re-display must preserve the original playlistUrl source, not reset the scheduler to a zero-value static source")
}

// TestCommandHandler_SetSeriesPlayback_RecastOfAnUnscheduledPlaylistURLCastPreservesTheRefreshSource
// is feralfile-bot's F1 on PR #454's SECOND review (2026-11-10 14:15): the
// first F1 fix (the test above) only restored the scheduler's own `source`
// field, which PrepareWithSource persists only for a scheduled result (see
// its own doc) — an UNSCHEDULED series still re-display as a static dp1_call
// with no playlistUrl of its own, and nothing retained the original source
// anywhere the refresher could read it. A real playlistschedule.Scheduler is
// wired (not mocked) so this pins the actual retained value, not just that a
// re-display happened.
func TestCommandHandler_SetSeriesPlayback_RecastOfAnUnscheduledPlaylistURLCastPreservesTheRefreshSource(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t)
	ctx := context.Background()
	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()

	sched := playlistschedule.New(ctx, mocks.CommandSender{CDP: mockCDP}, mockClock, func() *time.Location { return loc }, logger)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, sched, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	const url = "https://feed.example/unscheduled-series.json"
	// No DisplayAt on any item: this display request never arms the displayAt scheduler,
	// which is exactly the gap the scheduled test above does not cover.
	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b")},
		Resolved:       true,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	fetchedPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-1",
		Items: []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b")},
	}}

	mockDP1.EXPECT().
		ProcessPlaylistURLForCast(gomock.Any(), url, gomock.Any()).
		Return(fetchedPlaylist, initialPlan, nil).
		Times(1)
	// The re-display's rebuilt document, once narrowed to the representative,
	// carries a concrete edition (no seriesRegistry item left to resolve) —
	// this is that branch's call.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	_, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": url},
	})
	require.NoError(t, err)
	require.False(t, sched.HasCache(), "an unscheduled display request must never arm the displayAt scheduler")
	wantSource := playlistschedule.Source{PlaylistURL: url, ContentContext: "curated"}
	require.Equal(t, wantSource, sched.RefreshSource("playlist-1"),
		"the initial unscheduled display request must retain its originating playlistUrl for the refresher")

	result, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])

	assert.Equal(t, wantSource, sched.RefreshSource("playlist-1"),
		"recastForMode's re-display must still leave the refresher able to find the original playlistUrl source, "+
			"not nothing at all (the scheduler's own `source` field stays zero for an unscheduled result)")
}

// TestCommandHandler_SetSeriesPlayback_RecastOfAnUnscheduledDynamicPlaylistCastPreservesTheRefreshSource
// is the dynamic-origin sibling of the URL test above: a dp1_call carrying a
// top-level dynamicQuery hits the exact same static re-display path once
// narrowed (DynamicQuery is cleared before re-entry, see recastForMode's
// doc), so it needs the same retained refresh source, not just the
// playlistUrl shape (feralfile-bot F1 on PR #454's second review, 2026-11-10
// 14:15, generalized per "fix the mechanism, not the example").
func TestCommandHandler_SetSeriesPlayback_RecastOfAnUnscheduledDynamicPlaylistCastPreservesTheRefreshSource(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t)
	ctx := context.Background()
	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()

	sched := playlistschedule.New(ctx, mocks.CommandSender{CDP: mockCDP}, mockClock, func() *time.Location { return loc }, logger)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, sched, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	dynamicDoc := dp1playlist.Playlist{
		DPVersion: "1.0.0",
		ID:        "playlist-2",
		Items:     []dp1playlist.PlaylistItem{},
		DynamicQuery: &playlists.DynamicQuery{
			Profile:  dp1playlist.ProfileGraphQLV1,
			Endpoint: "https://example.com/graphql",
			Query:    `query { items(limit: {{limit}}, offset: {{offset}}) { id source } }`,
			ResponseMapping: playlists.ResponseMapping{
				ItemsPath:  "data.items",
				ItemSchema: "dp1/1.0",
			},
		},
	}
	raw, err := json.Marshal(dynamicDoc)
	require.NoError(t, err)
	var dp1Call map[string]interface{}
	require.NoError(t, json.Unmarshal(raw, &dp1Call))

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b")},
		Resolved:       true,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	resolvedPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-2",
		Items: []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b")},
	}}

	// The one and only call this display request's origin ever gets. Deliberately no
	// second EXPECT() for ProcessDynamicPlaylistForCast: a regression
	// re-resolving the dynamic query on the re-display fails this test
	// immediately with "unexpected call", rather than needing a call count.
	mockDP1.EXPECT().
		ProcessDynamicPlaylistForCast(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(resolvedPlaylist, initialPlan, nil).
		Times(1)
	// The re-display's rebuilt document, once DynamicQuery is cleared, decodes
	// as an ordinary static dp1_call carrying concrete editions — this is
	// that branch's call.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	_, err = handler.Process(ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": dp1Call},
	})
	require.NoError(t, err)
	require.False(t, sched.HasCache(), "an unscheduled display request must never arm the displayAt scheduler")

	retained := sched.RefreshSource("playlist-2")
	require.NotNil(t, retained.DynamicPlaylist, "the initial unscheduled dynamic display request must retain its originating dynamic document for the refresher")
	assert.True(t, retained.DynamicPlaylist.HasDynamicContent(), "the retained source must keep the original dynamicQuery for a later refresh to re-resolve")

	result, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])

	retainedAfterRecast := sched.RefreshSource("playlist-2")
	require.NotNil(t, retainedAfterRecast.DynamicPlaylist,
		"recastForMode's re-display must still leave the refresher able to find the original dynamic document")
	assert.True(t, retainedAfterRecast.DynamicPlaylist.HasDynamicContent())
}

// feralfile-bot F7, 2026-11-02 (plus the F1 finding from this fix's own
// review, PR #426): applySeriesPlayback persists the requested mode and
// releases the store lock before recastForMode's re-entrant Process call
// spends seconds in network-bound source preflight, during which
// lastSeriesCast.valid is temporarily false (rememberSeriesCast already ran
// for the rebuilt, group-less document). A second, concurrent
// setSeriesPlayback arriving in that window must not see !valid and silently
// skip its own re-display while still reporting {"ok": true} (F7) — but it also
// must not be forced to BLOCK until the first fully finishes, which would
// hold the command gate's shared heavy-tier dispatch weight for the sum of
// every stacked call's preflight instead of at most two (F1). This pins
// both: the second request returns promptly (persist, then enqueue as a
// pending follow-up, per scheduleRecast's coalescing), and the in-flight
// recast still performs exactly one more pass for the latest mode once it
// finishes, so the final re-display matches the final persisted setting.
func TestCommandHandler_SetSeriesPlayback_ConcurrentRequestsCoalesceWithoutLosingTheLatestSetting(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(initialPlan).
		Times(1)
	// The re-display's rebuilt document carries concrete editions, not a
	// seriesRegistry item, so every later ResolveSeriesCastPlan call (both
	// recasts below) sees no series groups — a passthrough is correct.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	var mu sync.Mutex
	var expressions []string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			mu.Lock()
			expressions = append(expressions, expr)
			mu.Unlock()
			return playerOkResponse(), nil
		}).
		Times(3) // initial display request + the "first" recast + the "full" recast
	mockStatusPoller.EXPECT().ForceRefresh().Times(3)

	// probeCalls counts every Preflight invocation: 1 = the initial display request's
	// own preflight, 2 = setSeriesPlayback("first")'s own recast preflight
	// (the window this test targets), 3 = the coalesced follow-up recast's
	// preflight, run by the SAME goroutine once call 2 returns. Only call 2
	// stalls; the follow-up (3) must not, since nothing else is waiting on it.
	probeCalls := 0
	var secondResult interface{}
	secondDone := make(chan struct{})
	commandrouter.SetMediaHealth(handler, &stallingProber{during: func() {
		probeCalls++
		if probeCalls != 2 {
			return
		}
		go func() {
			result, err := handler.Process(context.Background(), commands.Command{
				Type:      commands.CMD_SET_SERIES_PLAYBACK,
				Arguments: map[string]interface{}{"seriesPlayback": "full"},
			})
			require.NoError(t, err)
			secondResult = result
			close(secondDone)
		}()
		select {
		case <-secondDone:
			// Expected: scheduleRecast sees a recast already running, records
			// "full" as the pending mode, and returns immediately instead of
			// either racing recastForMode directly (F7) or blocking behind
			// this whole in-flight preflight (F1).
		case <-time.After(2 * time.Second):
			t.Fatal("setSeriesPlayback(\"full\") did not return promptly — it must enqueue as a pending follow-up rather than block behind the in-flight recast")
		}
	}}, logger)

	initialPlaylist := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": initialPlaylist},
	})
	require.NoError(t, err)

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])

	<-secondDone
	assert.Equal(t, true, secondResult.(map[string]interface{})["ok"])
	assert.Equal(t, "full", secondResult.(map[string]interface{})["seriesPlayback"])

	mu.Lock()
	require.Len(t, expressions, 3, "the coalesced follow-up for \"full\" must have actually run, not been silently skipped")
	last := castedItems(t, expressions[2])
	mu.Unlock()
	assert.Len(t, last, 3, "the LAST re-display must carry the LAST saved setting (full, all three editions)")

	getResult, err := handler.Process(context.Background(), commands.Command{Type: commands.CMD_GET_SERIES_PLAYBACK, Arguments: map[string]interface{}{}})
	require.NoError(t, err)
	assert.Equal(t, "full", getResult.(map[string]interface{})["seriesPlayback"], "the persisted setting must match what is actually on screen")
}

// feralfile-bot F2, 2026-10-30: once displayDefaultPlaylist has (possibly)
// replaced a resolved series on screen, controld can no longer prove a
// series is still there — the exact uncertainty clearVerdictForDefaultPlayback
// already carries for the verdict. A later setSeriesPlayback must then be a
// pure persist, never a re-display of the stale series over whatever is now
// actually showing.
func TestCommandHandler_SetSeriesPlayback_DefaultPlaybackInvalidatesTheSeriesCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(initialPlan).
		Times(1)

	// Exactly two CDP sends: the initial series display request, then
	// displayDefaultPlaylist. setSeriesPlayback below must NOT trigger a
	// third — recastForMode must see an invalidated cache.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)

	_, err = handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
		Arguments: map[string]interface{}{},
	})
	require.NoError(t, err)

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist")
}

// deadSourcesProber answers VerdictDead (HTTP 404) for every source in dead
// and ProbeAlive for everything else. during, if set, runs inside the first
// probe only — i.e. while the first display's preflight is still pending.
type deadSourcesProber struct {
	dead   map[string]bool
	during func()
	probes int
}

func (p *deadSourcesProber) HintArgument([]dp1playlist.PlaylistItem) interface{} { return nil }

func (p *deadSourcesProber) Report([]dp1playlist.PlaylistItem, []int) ([]mediahealth.ReportItem, error) {
	return nil, nil
}

func (p *deadSourcesProber) Preflight(_ context.Context, sources []string) []mediahealth.ProbeResult {
	p.probes++
	if p.probes == 1 && p.during != nil {
		p.during()
	}
	results := make([]mediahealth.ProbeResult, len(sources))
	for i, source := range sources {
		results[i] = mediahealth.ProbeResult{Source: source, Verdict: mediahealth.VerdictAlive, Status: 200}
		if p.dead[source] {
			results[i] = mediahealth.ProbeResult{Source: source, Verdict: mediahealth.VerdictDead, Status: 404}
		}
	}
	return results
}

// PR #429 review F2: a series display request the preflight rejects must not stay
// remembered. Every catalog edition 404s while the representative is
// reachable, so the full display request is rejected before reaching the player — but
// a later setSeriesPlayback("first") narrows to the representative alone,
// which WOULD pass the preflight. Had the rejected plan stayed cached, the
// setting change would replace whatever is really on screen with a series
// that never played. The setting itself must still persist.
func TestCommandHandler_SetSeriesPlayback_RejectedCastIsNotRecast(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	editions := []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")}
	dead := map[string]bool{}
	for _, edition := range editions {
		dead[edition.Source] = true
	}
	commandrouter.SetMediaHealth(handler, &deadSourcesProber{dead: dead}, logger)

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       editions,
		Resolved:       true,
	}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}).
		Times(1)
	// No CDP Send and no ForceRefresh expectations: neither the rejected
	// display request nor the setting change may reach the player.

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	var unreachable *commandrouter.SourceUnreachableError
	require.ErrorAs(t, err, &unreachable, "every edition is dead, so the full display request must be rejected")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist")
	assert.Equal(t, "first", result.(map[string]interface{})["seriesPlayback"])
}

// The other half of PR #431 review F2: a re-display the player refuses leaves
// the accepted series on screen, so recastForMode must restore it AS
// ACCEPTED and a later setting change must still re-display it. (On a
// successful re-display the nested settle already marks it accepted; only this
// path depends on the restore doing so.)
func TestCommandHandler_SetSeriesPlayback_FailedRecastKeepsTheDisplayedSeries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
	}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}).
		Times(1)
	// Re-displays carry concrete editions; see the passthrough's rationale in
	// TestCommandHandler_SetSeriesPlayback_RecastsTheCurrentlyDisplayedSeries.
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	// Send 1: the original display request (accepted). Send 2: the "first" re-display,
	// refused by the player. Send 3: the "full" re-display.
	var expressions []string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			expressions = append(expressions, expr)
			if len(expressions) == 2 {
				return playerNotOkResponse(), nil
			}
			return playerOkResponse(), nil
		}).
		Times(3)
	mockStatusPoller.EXPECT().ForceRefresh().AnyTimes()

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)

	for _, mode := range []string{"first", "full"} {
		result, err := handler.Process(context.Background(), commands.Command{
			Type:      commands.CMD_SET_SERIES_PLAYBACK,
			Arguments: map[string]interface{}{"seriesPlayback": mode},
		})
		require.NoError(t, err)
		assert.Equal(t, true, result.(map[string]interface{})["ok"])
	}

	require.Len(t, expressions, 3, "the series is still on screen after the refused re-display, so the next setting change must re-display it")
	assert.Len(t, castedItems(t, expressions[2]), 3, "full must re-display every edition")
}

// PR #431 review F2: the same rejected series must stay out of reach when a
// setting change overlaps its pending preflight. setSeriesPlayback("full")
// lands while the display request is still being probed; its re-display must not run on
// that unaccepted plan (it would also fail, move seq on, and restore the
// plan, leaving the original display request unable to retract it). After the display request
// rejects, setSeriesPlayback("first") must still send nothing.
func TestCommandHandler_SetSeriesPlayback_OverlappingSettingChangeDoesNotRecastAPendingCast(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	editions := []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")}
	dead := map[string]bool{}
	for _, edition := range editions {
		dead[edition.Source] = true
	}
	setMode := func(mode string) {
		result, err := handler.Process(context.Background(), commands.Command{
			Type:      commands.CMD_SET_SERIES_PLAYBACK,
			Arguments: map[string]interface{}{"seriesPlayback": mode},
		})
		require.NoError(t, err)
		assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist")
	}
	commandrouter.SetMediaHealth(handler, &deadSourcesProber{dead: dead, during: func() { setMode("full") }}, logger)

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       editions,
		Resolved:       true,
	}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}).
		Times(1)
	// No CDP Send and no ForceRefresh expectations: nothing may reach the
	// player.

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	var unreachable *commandrouter.SourceUnreachableError
	require.ErrorAs(t, err, &unreachable, "every edition is dead, so the full display request must be rejected")

	setMode("first")
}

// The player-refusal half of the same rule: a series display request the preflight
// accepts but the player answers ok:false never reached the screen either.
// err stays nil on this path, so this pins that the retraction judges the
// player's reply, not only err.
func TestCommandHandler_SetSeriesPlayback_PlayerRefusedCastIsNotRecast(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	group := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
	}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}).
		Times(1)
	// Exactly one send: the refused display request. setSeriesPlayback below must not
	// trigger a second.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerNotOkResponse(), nil).
		Times(1)
	mockStatusPoller.EXPECT().ForceRefresh().AnyTimes()

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err, "a player refusal is reported in the reply, not as an error")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist")
}

// AC5's other half: when nothing currently displayed resolved any series
// group (a fresh process, or the screen shows plain content), a setting
// change has nothing to re-display — and correctly so, since once a series is
// narrowed and displayed the player holds no marker telling controld one was ever
// there (see recastForMode's doc).
func TestCommandHandler_SetSeriesPlayback_NoOpWhenNothingDisplayedIsASeries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSeriesPlayback(ts.handler, newSeriesPlaybackStore(t), ts.logger)

	// No CDP Send expectation at all: a re-display attempt would be a test
	// failure (unexpected call).
	result, err := ts.handler.Process(ts.ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "full"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])
}

// feralfile-bot F1 on feralfile-bot F5's own fix, 2026-11-02: a series whose
// catalog lookup failed must no-op setSeriesPlayback exactly like
// TestCommandHandler_SetSeriesPlayback_NoOpWhenNothingDisplayedIsASeries
// above (dp1/seriesplayback_test.go pins the same contract from the dp1
// package's own side: HasSeriesGroups() reads false for an unresolved
// fallback). A fix for a DIFFERENT bug (F5, preserving the unresolved item's
// seriesPlayback marker across an internal rebuild) briefly made this display request
// register as "tracked" too, so setSeriesPlayback attempted a real re-display —
// and because the only "edition" an unresolved fallback has is the item
// itself, still carrying the series marker, that re-display would have
// re-entered ResolveSeriesCastPlan and re-fetched the catalog synchronously
// inside the command handler. Only ONE mockDP1.ResolveSeriesCastPlan
// expectation and ONE CDP send are registered below — a second of either
// would fail this test outright.
func TestCommandHandler_SetSeriesPlayback_NoOpWhenTheDisplayedSeriesNeverResolved(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	unresolved := &dp1.ResolvedSeriesGroup{
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("item-series")},
		Resolved:       false,
	}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(dp1.CastPlan{Segments: []dp1.CastSegment{{Group: unresolved}}}).
		Times(1)

	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(playerOkResponse(), nil).
		Times(1)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "full"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist")
}

// TestCommandHandler_DisplayPlaylist_DoesNotPushAWithheldRandomPickEvenWhenTheCastIsAccepted
// pins run-reviewer's consolidated F1 on feral-file/ffos-user#459's accepted-
// pick push fix (2026-11-16): a display request is "accepted" as soon as ANY of its
// items survives content-policy filtering, not only when every item does.
// A random pick that content policy itself withholds — here, the group's
// single edition is `mature` and the default policy hides mature content —
// must never be recorded as the group's accepted pick just because some
// OTHER, unrelated item in the same display request kept the overall display from being
// blocked.
func TestCommandHandler_DisplayPlaylist_DoesNotPushAWithheldRandomPickEvenWhenTheCastIsAccepted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)
	policyStore, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	commandrouter.SetContentPolicy(handler, policyStore, logger)

	var recorded []dp1.RandomGroupPick
	commandrouter.SetAcceptedPickRecorder(handler, func(picks []dp1.RandomGroupPick) {
		recorded = append(recorded, picks...)
	}, logger)

	_, err = handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "random"},
	})
	require.NoError(t, err)

	mature := contentrating.RatingMature
	matureEdition := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4", ContentRating: &mature}
	allowedExplicit := dp1playlist.PlaylistItem{ID: "explicit", Source: "https://cdn.example.com/explicit.mp4"}
	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "387"}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{
		{Item: &allowedExplicit},
		{Group: &dp1.ResolvedSeriesGroup{
			Key:            key,
			Representative: seriesEditionItem("item-series"),
			Editions:       []dp1playlist.PlaylistItem{matureEdition},
			Resolved:       true,
			ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
		}},
	}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(plan).
		Times(1)

	var expression string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expression, _ = params["expression"].(string)
			return playerOkResponse(), nil
		}).
		Times(1)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err = handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err, "the display request as a whole is accepted: the explicit item alone keeps it from being blocked")

	narrowed := castedItems(t, expression)
	require.Len(t, narrowed, 1, "the mature edition must be filtered out; only the explicit item reaches the player")
	assert.Equal(t, allowedExplicit.Source, narrowed[0]["source"])

	assert.Empty(t, recorded, "the withheld random pick must never be pushed, even though the overall display was accepted")
}

// TestCommandHandler_SetSeriesPlayback_RecastDoesNotPushWhenThePlayerRefusesTheRecast
// pins run-reviewer's consolidated F2 on this fix (2026-11-16): recastForMode
// must gate its push on the player's own ok field, not merely on the
// re-entrant Process call returning no Go error — a legitimate player
// refusal (ok:false) carries no error either.
func TestCommandHandler_SetSeriesPlayback_RecastDoesNotPushWhenThePlayerRefusesTheRecast(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	var recorded []dp1.RandomGroupPick
	commandrouter.SetAcceptedPickRecorder(handler, func(picks []dp1.RandomGroupPick) {
		recorded = append(recorded, picks...)
	}, logger)

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "387"}
	group := &dp1.ResolvedSeriesGroup{
		Key:            key,
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
		ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(initialPlan).
		Times(1)
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	callCount := 0
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, _ map[string]interface{}) (interface{}, error) {
			callCount++
			if callCount == 1 {
				return playerOkResponse(), nil
			}
			// The re-display itself: the player refuses it (ok:false, no Go
			// error) — a designed, first-class outcome.
			return playerNotOkResponse(), nil
		}).
		Times(2)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)
	assert.Empty(t, recorded, "no device override yet: the default (full) mode makes no random choice to push")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "random"},
	})
	require.NoError(t, err, "the setting change itself is a Go-level success even though the player refused the re-display")
	assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist regardless of the re-display outcome")
	assert.Equal(t, 2, callCount)

	assert.Empty(t, recorded, "a player-refused re-display must never push the pick it computed — it was never actually displayed")
}

// TestCommandHandler_SetSeriesPlayback_RecastDoesNotPushAGuessedSeqWhenAnotherCastInterleaves
// pins run-reviewer's consolidated F1 on pass 2 of this fix (2026-11-16):
// recastForMode captures seqBefore before releasing h.lastSeriesCast.mu and
// calling its own re-entrant Process, so an unrelated, independent
// CMD_DISPLAY_PLAYLIST display request landing in that window advances the SAME shared
// seq counter before recastForMode's own re-entrant call gets its turn —
// making seqBefore+1 wrong. This test forces exactly that interleaving (a
// plain, non-series display request fired from inside the re-entrant call's own
// ResolveSeriesCastPlan, deterministically, before that call's own
// rememberSeriesCast runs) and requires that the re-display's push either never
// happens or carries a Seq that is not simply assumed — never a stale
// seqBefore+1 recorded as if it were ground truth.
func TestCommandHandler_SetSeriesPlayback_RecastDoesNotPushAGuessedSeqWhenAnotherCastInterleaves(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	var recorded []dp1.RandomGroupPick
	commandrouter.SetAcceptedPickRecorder(handler, func(picks []dp1.RandomGroupPick) {
		recorded = append(recorded, picks...)
	}, logger)

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "387"}
	group := &dp1.ResolvedSeriesGroup{
		Key:            key,
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
		Resolved:       true,
		ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
	}
	initialPlan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(initialPlan).
		Times(1)

	// AnyTimes passthrough for every later call (A's own rebuild, and the
	// interleaved display request's own resolution): resolves no groups, like a
	// document carrying no seriesRegistry marker. On its very FIRST entry
	// — which is always A's rebuild, since that is the next
	// ResolveSeriesCastPlan call after the dedicated one above — it first
	// fires a completely unrelated CMD_DISPLAY_PLAYLIST display request to
	// completion, synchronously, before computing its own return value:
	// this reproduces the exact interleaving the finding describes,
	// deterministically, without needing real goroutines.
	entries := 0
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		DoAndReturn(func(_ interface{}, playlist dp1.Playlist, _ json.RawMessage, _ *dp1.SeriesPlaybackMode, _ int) dp1.CastPlan {
			entries++
			if entries == 1 {
				interleavedPlaylist := map[string]interface{}{
					"dpVersion": "1.0.0",
					"id":        "playlist-interleaved",
					"items":     []interface{}{map[string]interface{}{"id": "other-item", "source": "https://cdn.example.com/other.mp4"}},
				}
				_, err := handler.Process(context.Background(), commands.Command{
					Type:      commands.CMD_DISPLAY_PLAYLIST,
					Arguments: map[string]interface{}{"dp1_call": interleavedPlaylist},
				})
				require.NoError(t, err)
			}
			segments := make([]dp1.CastSegment, len(playlist.Items))
			for i := range playlist.Items {
				item := playlist.Items[i]
				segments[i] = dp1.CastSegment{Item: &item}
			}
			return dp1.CastPlan{Segments: segments}
		}).
		AnyTimes()

	var expressions []string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			expressions = append(expressions, expr)
			return playerOkResponse(), nil
		}).
		Times(3) // initial A display request + the interleaved display request + A's own re-display
	mockStatusPoller.EXPECT().ForceRefresh().Times(3)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	_, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)
	assert.Empty(t, recorded, "no device override yet: the default (full) mode makes no random choice to push")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "random"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist")
	require.Len(t, expressions, 3)

	for _, pick := range recorded {
		assert.NotEqual(t, key, pick.Key,
			"recastForMode's own re-display must never push a pick for its group under these interleaved conditions: its assumed seqBefore+1 is not the re-entrant call's real seq once another display request has landed in between")
	}
}

// TestCommandHandler_DisplayPlaylist_PushesTheAcceptedPickEvenWhenTheGroupIsDisplayAtDeferred
// pins run-reviewer's consolidated F1 on pass 4 of this fix (2026-11-16): a
// random-mode group's edition is decided once, at this display request's own Narrow
// call — a displayAt schedule only defers WHEN that decided edition
// reaches the screen, never WHAT it is. Before this fix, the push's own
// survival check read playlist.Items AFTER the scheduler narrowed it down
// to the active-now cohort, so an entirely-future display (the player is
// never even sent anything — deferred:true) dropped the pick with no
// survivor at all, leaving playlist-refresher with nothing to learn about
// a display request that was genuinely accepted.
func TestCommandHandler_DisplayPlaylist_PushesTheAcceptedPickEvenWhenTheGroupIsDisplayAtDeferred(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t)
	ctx := context.Background()
	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()

	sched := playlistschedule.New(ctx, mocks.CommandSender{CDP: mockCDP}, mockClock, func() *time.Location { return time.UTC }, logger)
	handler := commandrouter.New(mockExecutor, mocks.CommandSender{CDP: mockCDP}, mockDP1, mockStatusPoller, nil, nil, nil, sched, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	var recorded []dp1.RandomGroupPick
	commandrouter.SetAcceptedPickRecorder(handler, func(picks []dp1.RandomGroupPick) {
		recorded = append(recorded, picks...)
	}, logger)

	_, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "random"},
	})
	require.NoError(t, err)

	key := dp1.SeriesGroupKey{Chain: "evm", Address: "0xabc", SeriesID: "387"}
	futureEdition := dp1playlist.PlaylistItem{ID: "a", Source: "https://cdn.example.com/a.mp4", DisplayAt: strPtr("2026-07-22T00:00:00Z")}
	group := &dp1.ResolvedSeriesGroup{
		Key:            key,
		Representative: seriesEditionItem("item-series"),
		Editions:       []dp1playlist.PlaylistItem{futureEdition},
		Resolved:       true,
		ArtworkModel:   dp1.SeriesArtworkModelMultiUnique,
	}
	plan := dp1.CastPlan{Segments: []dp1.CastSegment{{Group: group}}}
	mockDP1.EXPECT().
		ResolveSeriesCastPlan(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(plan).
		Times(1)

	// Entirely future cohort: the player must never be sent anything.
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Times(0)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	playlistMap := map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"items": []interface{}{map[string]interface{}{
			"id":     "item-series",
			"source": "https://cdn.artworks.feralfile.io/0.mp4",
			"provenance": map[string]interface{}{
				"type":     "seriesRegistry",
				"contract": map[string]interface{}{"chain": "evm", "seriesId": 387},
			},
		}},
	}
	result, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	})
	require.NoError(t, err)
	require.Equal(t, map[string]interface{}{"message": map[string]interface{}{"ok": true, "deferred": true}}, result,
		"sanity: this display request is entirely future, so the scheduler defers it rather than sending anything")

	require.Len(t, recorded, 1, "the random pick must be pushed even though its own segment is not due to display yet")
	assert.Equal(t, key, recorded[0].Key)
	assert.Equal(t, futureEdition.Source, recorded[0].Source)
}
