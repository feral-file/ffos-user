package commandrouter_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
)

func TestCommandHandler_Process_DisplayPlaylist_FiltersByDisplayAt(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error {
			<-c.Done()
			return c.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location {
		return loc
	}, logger)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, sched, mockJSON, logger)

	playlistURL := "https://example.com/daily.json"
	full := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title:    "Daily",
			Schedule: &playlists.Schedule{ByDisplayAt: true},
			Items: []dp1playlist.PlaylistItem{
				{ID: "day21", Title: "Day 21", Source: "https://example.com/21.html", DisplayAt: "2026-07-21T00:00:00Z"},
				{ID: "day22", Title: "Day 22", Source: "https://example.com/22.html", DisplayAt: "2026-07-22T00:00:00Z"},
				{ID: "day23", Title: "Day 23", Source: "https://example.com/23.html", DisplayAt: "2026-07-23T00:00:00Z"},
			},
		},
	}

	mockDP1.EXPECT().ProcessPlaylistURL(ctx, playlistURL, true).Return(full, nil)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, ok := params["expression"].(string)
			require.True(t, ok)
			// Extract JSON payload inside window.handleCDPRequest(...)
			const prefix = "window.handleCDPRequest("
			require.Contains(t, expr, prefix)
			payload := expr[len(prefix) : len(expr)-1]
			var cmd commands.Command
			require.NoError(t, json.Unmarshal([]byte(payload), &cmd))
			pl, ok := cmd.Arguments["dp1_call"].(map[string]interface{})
			require.True(t, ok, "dp1_call should be present")
			items, ok := pl["items"].([]interface{})
			require.True(t, ok)
			require.Len(t, items, 1)
			item0 := items[0].(map[string]interface{})
			assert.Equal(t, "day22", item0["id"])
			return playerOkResponse(), nil
		},
	)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	_, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})
	require.NoError(t, err)
	assert.True(t, sched.HasCache())
}

type trackingScheduler struct {
	inner        playlistschedule.Scheduler
	pushCalls    int
	clearCalls   int
	clearThenFn  int
	lastPrepared *dp1.Playlist
}

func (t *trackingScheduler) Prepare(p *dp1.Playlist) *dp1.Playlist {
	t.lastPrepared = p
	return t.inner.Prepare(p)
}
func (t *trackingScheduler) RecomputeNow(ctx context.Context) { t.inner.RecomputeNow(ctx) }
func (t *trackingScheduler) Clear() {
	t.clearCalls++
	t.inner.Clear()
}
func (t *trackingScheduler) ClearThenWithPlayerPush(fn func()) {
	t.clearThenFn++
	t.inner.ClearThenWithPlayerPush(fn)
}
func (t *trackingScheduler) WithPlayerPush(fn func()) {
	t.pushCalls++
	t.inner.WithPlayerPush(fn)
}
func (t *trackingScheduler) HasCache() bool { return t.inner.HasCache() }
func (t *trackingScheduler) Stop()          { t.inner.Stop() }

func TestCommandHandler_Process_DisplayPlaylist_UsesWithPlayerPush(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error {
			<-c.Done()
			return c.Err()
		},
	).AnyTimes()

	inner := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location {
		return time.UTC
	}, logger)
	track := &trackingScheduler{inner: inner}
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, track, mockJSON, logger)

	playlistURL := "https://example.com/daily.json"
	full := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title:    "Daily",
			Schedule: &playlists.Schedule{ByDisplayAt: true},
			Items: []dp1playlist.PlaylistItem{
				{ID: "day22", Title: "Day 22", Source: "https://example.com/22.html", DisplayAt: "2026-07-22T00:00:00Z"},
			},
		},
	}
	mockDP1.EXPECT().ProcessPlaylistURL(ctx, playlistURL, true).Return(full, nil)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	_, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": playlistURL},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, track.pushCalls, "displayPlaylist must serialize CDP via WithPlayerPush")
}

func TestCommandHandler_Process_DisplayDefaultPlaylist_ClearsDisplayAtCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockExecutor := mocks.NewMockExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error {
			<-c.Done()
			return c.Err()
		},
	).AnyTimes()

	inner := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location {
		return time.UTC
	}, logger)
	track := &trackingScheduler{inner: inner}
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, track, mockJSON, logger)

	_ = track.Prepare(&dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Schedule: &playlists.Schedule{ByDisplayAt: true},
			Items: []dp1playlist.PlaylistItem{
				{ID: "day22", Source: "https://example.com/22.html", DisplayAt: "2026-07-22T00:00:00Z"},
			},
		},
	})
	require.True(t, track.HasCache())

	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	_, err := handler.Process(ctx, commands.Command{
		Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
		Arguments: map[string]interface{}{},
	})
	require.NoError(t, err)
	assert.Equal(t, 1, track.clearThenFn, "default playlist must ClearThenWithPlayerPush")
	assert.False(t, track.HasCache())

	// Recompute after default must not resurrect the old Daily cast.
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)
	track.RecomputeNow(ctx)
}
