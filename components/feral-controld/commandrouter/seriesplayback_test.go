package commandrouter_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
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

	group := &dp1.ResolvedSeriesGroup{
		Editions: []dp1playlist.PlaylistItem{seriesEditionItem("a"), seriesEditionItem("b"), seriesEditionItem("c")},
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
	assert.Equal(t, "a", narrowed[0]["id"])
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
