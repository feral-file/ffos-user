package commandrouter_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
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
	// displayed, so there is nothing to re-cast (see the dedicated re-cast
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
// level: a static dp1_call cast whose series item resolves to a group of
// editions is narrowed before it ever reaches the player (no device
// override yet, so the default is full — every edition); changing the
// device setting then re-casts the SAME series narrowed to its
// representative, without the test's mocked catalog call firing a second
// time (ResolveSeriesCastPlan itself is mocked here — the dp1 package's own
// no-second-catalog-call guarantee is pinned separately in
// dp1/seriesplayback_test.go — but the cache that makes the re-cast possible
// AT ALL, and the re-cast actually happening, are this handler's job).
func TestCommandHandler_SetSeriesPlayback_RecastsTheCurrentlyDisplayedSeries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
	commandrouter.SetSeriesPlayback(handler, newSeriesPlaybackStore(t), logger)

	// Representative is deliberately a different item than editions[0]: it
	// pins that the re-cast narrows to the DOCUMENT's own representative
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
	// The re-cast's rebuilt document carries concrete editions, not a
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
	assert.Len(t, castedItems(t, expressions[0]), 3, "no device override yet; the default (full) casts every edition")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"])

	require.Len(t, expressions, 2, "setSeriesPlayback must re-cast the currently displayed series")
	narrowed := castedItems(t, expressions[1])
	require.Len(t, narrowed, 1, "the new device override (first) must narrow the re-cast to the representative")
	assert.Equal(t, "item-series", narrowed[0]["id"], "first must cast the document's own representative, not the catalog's first-returned edition")
}

// TestCommandHandler_SetSeriesPlayback_RecastOfAPlaylistURLCastDoesNotReResolveTheDynamicQuery
// is run-reviewer's F1 on PR #454 (feral-file/ffos-user#452): before this
// fix, a cast whose ORIGIN was dynamic (playlistUrl or a dp1_call carrying a
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
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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
	// The one and only call this cast's origin ever gets. Deliberately no
	// EXPECT() for ProcessDynamicPlaylistForCast at all — see the test doc.
	mockDP1.EXPECT().
		ProcessPlaylistURLForCast(gomock.Any(), url, gomock.Any()).
		Return(dynamicOriginPlaylist, initialPlan, nil).
		Times(1)
	// The re-cast's rebuilt document, once DynamicQuery is cleared, decodes
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
		"the re-cast must succeed locally, never reaching ProcessDynamicPlaylistForCast")
}

// feralfile-bot F7, 2026-11-02 (plus the F1 finding from this fix's own
// review, PR #426): applySeriesPlayback persists the requested mode and
// releases the store lock before recastForMode's re-entrant Process call
// spends seconds in network-bound source preflight, during which
// lastSeriesCast.valid is temporarily false (rememberSeriesCast already ran
// for the rebuilt, group-less document). A second, concurrent
// setSeriesPlayback arriving in that window must not see !valid and silently
// skip its own re-cast while still reporting {"ok": true} (F7) — but it also
// must not be forced to BLOCK until the first fully finishes, which would
// hold the command gate's shared heavy-tier dispatch weight for the sum of
// every stacked call's preflight instead of at most two (F1). This pins
// both: the second request returns promptly (persist, then enqueue as a
// pending follow-up, per scheduleRecast's coalescing), and the in-flight
// recast still performs exactly one more pass for the latest mode once it
// finishes, so the final re-cast matches the final persisted setting.
func TestCommandHandler_SetSeriesPlayback_ConcurrentRequestsCoalesceWithoutLosingTheLatestSetting(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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
	// The re-cast's rebuilt document carries concrete editions, not a
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
		Times(3) // initial cast + the "first" recast + the "full" recast
	mockStatusPoller.EXPECT().ForceRefresh().Times(3)

	// probeCalls counts every Preflight invocation: 1 = the initial cast's
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
	assert.Len(t, last, 3, "the LAST re-cast must carry the LAST saved setting (full, all three editions)")

	getResult, err := handler.Process(context.Background(), commands.Command{Type: commands.CMD_GET_SERIES_PLAYBACK, Arguments: map[string]interface{}{}})
	require.NoError(t, err)
	assert.Equal(t, "full", getResult.(map[string]interface{})["seriesPlayback"], "the persisted setting must match what is actually on screen")
}

// feralfile-bot F2, 2026-10-30: once displayDefaultPlaylist has (possibly)
// replaced a resolved series on screen, controld can no longer prove a
// series is still there — the exact uncertainty clearVerdictForDefaultPlayback
// already carries for the verdict. A later setSeriesPlayback must then be a
// pure persist, never a re-cast of the stale series over whatever is now
// actually showing.
func TestCommandHandler_SetSeriesPlayback_DefaultPlaybackInvalidatesTheSeriesCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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

	// Exactly two CDP sends: the initial series cast, then
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
// probe only — i.e. while the first cast's preflight is still pending.
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

// PR #429 review F2: a series cast the preflight rejects must not stay
// remembered. Every catalog edition 404s while the representative is
// reachable, so the full cast is rejected before reaching the player — but
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
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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
	// cast nor the setting change may reach the player.

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
	require.ErrorAs(t, err, &unreachable, "every edition is dead, so the full cast must be rejected")

	result, err := handler.Process(context.Background(), commands.Command{
		Type:      commands.CMD_SET_SERIES_PLAYBACK,
		Arguments: map[string]interface{}{"seriesPlayback": "first"},
	})
	require.NoError(t, err)
	assert.Equal(t, true, result.(map[string]interface{})["ok"], "the setting itself must still persist")
	assert.Equal(t, "first", result.(map[string]interface{})["seriesPlayback"])
}

// The other half of PR #431 review F2: a re-cast the player refuses leaves
// the accepted series on screen, so recastForMode must restore it AS
// ACCEPTED and a later setting change must still re-cast it. (On a
// successful re-cast the nested settle already marks it accepted; only this
// path depends on the restore doing so.)
func TestCommandHandler_SetSeriesPlayback_FailedRecastKeepsTheDisplayedSeries(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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
	// Re-casts carry concrete editions; see the passthrough's rationale in
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

	// Send 1: the original cast (accepted). Send 2: the "first" re-cast,
	// refused by the player. Send 3: the "full" re-cast.
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

	require.Len(t, expressions, 3, "the series is still on screen after the refused re-cast, so the next setting change must re-cast it")
	assert.Len(t, castedItems(t, expressions[2]), 3, "full must re-cast every edition")
}

// PR #431 review F2: the same rejected series must stay out of reach when a
// setting change overlaps its pending preflight. setSeriesPlayback("full")
// lands while the cast is still being probed; its re-cast must not run on
// that unaccepted plan (it would also fail, move seq on, and restore the
// plan, leaving the original cast unable to retract it). After the cast
// rejects, setSeriesPlayback("first") must still send nothing.
func TestCommandHandler_SetSeriesPlayback_OverlappingSettingChangeDoesNotRecastAPendingCast(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	logger := zaptest.NewLogger(t)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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
	require.ErrorAs(t, err, &unreachable, "every edition is dead, so the full cast must be rejected")

	setMode("first")
}

// The player-refusal half of the same rule: a series cast the preflight
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
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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
	// Exactly one send: the refused cast. setSeriesPlayback below must not
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
// change has nothing to re-cast — and correctly so, since once a series is
// narrowed and cast the player holds no marker telling controld one was ever
// there (see recastForMode's doc).
func TestCommandHandler_SetSeriesPlayback_NoOpWhenNothingDisplayedIsASeries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	commandrouter.SetSeriesPlayback(ts.handler, newSeriesPlaybackStore(t), ts.logger)

	// No CDP Send expectation at all: a re-cast attempt would be a test
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
// seriesPlayback marker across an internal rebuild) briefly made this cast
// register as "tracked" too, so setSeriesPlayback attempted a real re-cast —
// and because the only "edition" an unresolved fallback has is the item
// itself, still carrying the series marker, that re-cast would have
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
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, nil, wrapper.NewJSON(), logger)
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
