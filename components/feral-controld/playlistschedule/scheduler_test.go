package playlistschedule_test

import (
	"context"
	"strings"
	"sync"
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
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
)

func TestPrepare_WithoutDisplayAt_PassthroughAndClearsCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	// Seed a scheduled playlist first so we can prove a later unscheduled cast clears it.
	seed := displayAtPlaylist(
		item("a", "2026-07-21T00:00:00"),
		item("b", "2026-07-23T00:00:00"),
	)
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()
	_ = sched.Prepare(seed)
	require.True(t, sched.HasCache())

	plain := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "plain",
			Items: []dp1playlist.PlaylistItem{item("x", "")},
		},
	}
	got := sched.Prepare(plain)
	assert.Equal(t, plain, got)
	assert.False(t, sched.HasCache())
}

func TestPrepare_DisplayAtWithoutByDisplayAt_PassthroughAndClearsCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.Prepare(displayAtPlaylist(
		item("old", "2026-07-22T00:00:00Z"),
		item("new", "2026-07-23T00:00:00Z"),
	))
	require.True(t, sched.HasCache())

	unscheduled := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Metadata only",
			Items: []dp1playlist.PlaylistItem{
				item("old", "2026-07-22T00:00:00Z"),
				item("new", "2026-07-23T00:00:00Z"),
			},
		},
	}
	got := sched.Prepare(unscheduled)
	assert.Equal(t, unscheduled, got)
	assert.False(t, sched.HasCache())
}

func TestPrepare_DisplayAt_FiltersActiveSetAndPreservesOrder(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.FixedZone("UTC-4", -4*3600)
	now := time.Date(2026, 7, 22, 14, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	full := displayAtPlaylist(
		item("intro", ""),
		item("a", "2026-07-21T00:00:00"),
		item("b", "2026-07-22T00:00:00"),
		item("c", "2026-07-22T00:00:00"),
		item("outro", ""),
		item("d", "2026-07-23T00:00:00"),
	)

	active := sched.Prepare(full)
	require.NotNil(t, active)
	require.Len(t, active.Items, 4)
	assert.Equal(t, []string{"intro", "b", "c", "outro"}, itemIDs(active.Items))
	assert.True(t, sched.HasCache())
	// Full playlist remains cached for later timer/wake recomputation.
	assert.Len(t, full.Items, 6)
}

func TestPrepare_TimezoneLessDisplayAtUsesDeviceLocal(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	// Device is still on Jul 21 locally while UTC has already crossed into Jul 22.
	loc := time.FixedZone("UTC-4", -4*3600)
	now := time.Date(2026, 7, 21, 22, 0, 0, 0, loc) // == 2026-07-22T02:00:00Z
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	full := displayAtPlaylist(
		item("day21", "2026-07-21T00:00:00"),
		item("day22", "2026-07-22T00:00:00"),
	)
	active := sched.Prepare(full)
	require.Len(t, active.Items, 1)
	assert.Equal(t, "day21", active.Items[0].ID)
}

func TestPrepare_AbsoluteTimezoneDisplayAt(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.FixedZone("UTC+7", 7*3600)
	now := time.Date(2026, 7, 22, 8, 0, 0, 0, loc) // 01:00Z
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	full := displayAtPlaylist(
		item("utc", "2026-07-22T00:00:00Z"),
		item("future", "2026-07-22T12:00:00Z"),
	)
	active := sched.Prepare(full)
	require.Len(t, active.Items, 1)
	assert.Equal(t, "utc", active.Items[0].ID)
}

func TestTimerFires_RecomputesAndPushesActiveSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	t0 := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	t1 := time.Date(2026, 7, 23, 0, 0, 0, 0, loc)

	var mu sync.Mutex
	now := t0
	clock.EXPECT().Now().DoAndReturn(func() time.Time {
		mu.Lock()
		defer mu.Unlock()
		return now
	}).AnyTimes()

	sleepStarted := make(chan struct{})
	releaseSleep := make(chan struct{})
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, d time.Duration) error {
			// First arm waits for Jul 23; later arms (after recompute) are canceled.
			select {
			case <-sleepStarted:
			default:
				close(sleepStarted)
			}
			select {
			case <-releaseSleep:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		},
	).AnyTimes()

	pushed := make(chan struct{}, 1)
	cdpMock.EXPECT().Initialized().Return(true).AnyTimes()
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.Contains(t, expr, `"action":"now_display"`)
			assert.NotContains(t, expr, `"refresh":true`)
			assert.Contains(t, expr, "day23")
			pushed <- struct{}{}
			return map[string]interface{}{"ok": true}, nil
		},
	).Times(1)

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	full := displayAtPlaylist(
		item("day22", "2026-07-22T00:00:00Z"),
		item("day23", "2026-07-23T00:00:00Z"),
	)
	active := sched.Prepare(full)
	require.Equal(t, []string{"day22"}, itemIDs(active.Items))

	<-sleepStarted
	mu.Lock()
	now = t1
	mu.Unlock()
	close(releaseSleep)

	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for displayAt timer push")
	}
}

func TestRecomputeNow_WakePathForceCastsNowDisplay(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	cdpMock.EXPECT().Initialized().Return(true).Times(1)
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			// displayAt cutover must force-cast: now_display without refresh so
			// the player does not defer until the current artwork duration ends.
			assert.Contains(t, expr, `"action":"now_display"`)
			assert.NotContains(t, expr, `"refresh":true`)
			assert.Contains(t, expr, "day22")
			return map[string]interface{}{"ok": true}, nil
		},
	).Times(1)

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.Prepare(displayAtPlaylist(
		item("day22", "2026-07-22T00:00:00Z"),
		item("day23", "2026-07-23T00:00:00Z"),
	))
	sched.RecomputeNow(context.Background())
}

func TestPrepare_DateOnlyDisplayAt_NotEligible(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	// §3.5.2: date-only is rejected — present but unresolvable, not evergreen.
	active := sched.Prepare(displayAtPlaylist(
		item("bad-date", "2026-07-21"),
		item("ok", "2026-07-22T00:00:00Z"),
		item("intro", ""),
	))
	require.Equal(t, []string{"ok", "intro"}, itemIDs(active.Items))
}

func TestPrepare_AllFuture_ReturnsOnlyEvergreen(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	active := sched.Prepare(displayAtPlaylist(
		item("intro", ""),
		item("future", "2026-07-22T00:00:00Z"),
	))
	require.Equal(t, []string{"intro"}, itemIDs(active.Items))
}

func displayAtPlaylist(items ...dp1playlist.PlaylistItem) *dp1.Playlist {
	return &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Daily",
			Items: items,
			Schedule: &playlists.Schedule{
				ByDisplayAt: true,
			},
		},
	}
}

func item(id, displayAt string) dp1playlist.PlaylistItem {
	it := dp1playlist.PlaylistItem{
		ID:     id,
		Title:  id,
		Source: "https://example.com/" + id + ".html",
	}
	// Empty string keeps DisplayAt nil (evergreen). Non-empty sets a present
	// pointer; date-only values are invalid per §3.5.2 and not evergreen.
	if displayAt != "" {
		da := displayAt
		it.DisplayAt = &da
	}
	return it
}

func itemIDs(items []dp1playlist.PlaylistItem) []string {
	out := make([]string, len(items))
	for i, it := range items {
		out[i] = it.ID
	}
	return out
}

func TestRecomputeNow_PushesNewerCacheWhenPrepareWinsPushLockRace(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	cdpMock.EXPECT().Initialized().Return(true).AnyTimes()
	// After the cast releases pushMu, recompute must push the *new* cache, not
	// the superseded "old" snapshot.
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr := params["expression"].(string)
			assert.Contains(t, expr, `"id":"new"`)
			assert.NotContains(t, expr, `"id":"old"`)
			return map[string]interface{}{"ok": true}, nil
		},
	).Times(1)

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.Prepare(displayAtPlaylist(item("old", "2026-07-22T00:00:00Z")))

	started := make(chan struct{})
	releaseCast := make(chan struct{})
	doneRecompute := make(chan struct{})

	go func() {
		sched.WithPlayerPush(func() {
			close(started)
			<-releaseCast
		})
	}()
	<-started

	go func() {
		sched.RecomputeNow(context.Background())
		close(doneRecompute)
	}()

	// Let RecomputeNow block on pushMu, then supersede the cache.
	time.Sleep(50 * time.Millisecond)
	_ = sched.Prepare(displayAtPlaylist(item("new", "2026-07-22T00:00:00Z")))
	close(releaseCast)

	select {
	case <-doneRecompute:
	case <-time.After(2 * time.Second):
		t.Fatal("RecomputeNow did not return")
	}
}

func TestPrepare_AllFutureNoEvergreen_EmptyActiveSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	now := time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	active := sched.Prepare(displayAtPlaylist(
		item("future", "2026-07-22T00:00:00Z"),
	))
	require.NotNil(t, active)
	assert.Empty(t, active.Items)
	assert.True(t, sched.HasCache())
}

func TestClear_StopsTimerAndDropsCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.Prepare(displayAtPlaylist(
		item("day22", "2026-07-22T00:00:00Z"),
		item("day23", "2026-07-23T00:00:00Z"),
	))
	require.True(t, sched.HasCache())

	sched.Clear()
	assert.False(t, sched.HasCache())

	cdpMock.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)
	sched.RecomputeNow(context.Background())
}

func TestClearThenWithPlayerPush_BlocksInFlightRecomputeFromOverwriting(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	enteredSend := make(chan struct{})
	releaseSend := make(chan struct{})
	var mu sync.Mutex
	var enteredOnce sync.Once
	var pushedIDs []string

	cdpMock.EXPECT().Initialized().Return(true).AnyTimes()
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr := params["expression"].(string)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case strings.Contains(expr, `"id":"old"`):
				pushedIDs = append(pushedIDs, "old")
				enteredOnce.Do(func() { close(enteredSend) })
				<-releaseSend
			case strings.Contains(expr, "displayDefaultPlaylist"):
				pushedIDs = append(pushedIDs, "default")
			default:
				pushedIDs = append(pushedIDs, "other")
			}
			return map[string]interface{}{"ok": true}, nil
		},
	).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.Prepare(displayAtPlaylist(item("old", "2026-07-22T00:00:00Z")))

	doneRecompute := make(chan struct{})
	go func() {
		sched.RecomputeNow(context.Background())
		close(doneRecompute)
	}()
	<-enteredSend

	// While recompute holds pushMu inside Send, clear+default must wait, then
	// run after — and recompute must not push again after clear.
	defaultDone := make(chan struct{})
	go func() {
		sched.ClearThenWithPlayerPush(func() {
			// Simulate displayDefaultPlaylist CDP under the same lock.
			_, _ = cdpMock.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
				"expression": `window.handleCDPRequest({"command":"displayDefaultPlaylist","request":{}})`,
			})
		})
		close(defaultDone)
	}()

	time.Sleep(50 * time.Millisecond)
	close(releaseSend)

	select {
	case <-doneRecompute:
	case <-time.After(2 * time.Second):
		t.Fatal("recompute did not finish")
	}
	select {
	case <-defaultDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ClearThenWithPlayerPush did not finish")
	}

	assert.False(t, sched.HasCache())
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, pushedIDs)
	assert.Equal(t, "default", pushedIDs[len(pushedIDs)-1], "default must win after clear")
}
