package playlistschedule_test

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestPrepare_DisplayAtWithoutWrapper_FiltersActiveSet(t *testing.T) {
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

	withoutSchedule := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Schedule-free displayAt",
			Items: []dp1playlist.PlaylistItem{
				item("old", "2026-07-22T00:00:00Z"),
				item("new", "2026-07-23T00:00:00Z"),
			},
		},
	}
	got := sched.Prepare(withoutSchedule)
	require.NotNil(t, got)
	assert.Equal(t, []string{"old"}, itemIDs(got.Items))
	assert.True(t, sched.HasCache())
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

func TestPrepare_DisplayAt_PersistsSourceAndClearDeletes(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	store := &memoryStore{}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.NewWithStore(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, store, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	full := displayAtPlaylist(
		item("today", "2026-07-22T00:00:00Z"),
		item("tomorrow", "2026-07-23T00:00:00Z"),
	)
	playlistURL := "https://example.com/daily.json"
	active := sched.PrepareWithSource(full, playlistschedule.Source{PlaylistURL: playlistURL})
	require.Equal(t, []string{"today"}, itemIDs(active.Items))
	assert.True(t, store.saved.IsZero(), "Prepare must not persist before the player accepts the cast")

	sched.Commit()
	require.False(t, store.saved.IsZero())
	assert.Equal(t, playlistURL, store.saved.PlaylistURL)

	sched.Clear()
	assert.False(t, store.cleared, "Clear must not persist before the player accepts the replacement")
	assert.False(t, store.saved.IsZero(), "durable cache must still reflect the last accepted scheduled cast source")

	sched.Commit()
	assert.True(t, store.cleared)
	assert.True(t, store.saved.IsZero())
}

func TestPrepare_NonScheduledClearRequiresCommit(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	store := &memoryStore{}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.NewWithStore(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, store, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.PrepareWithSource(
		displayAtPlaylist(item("today", "2026-07-22T00:00:00Z")),
		playlistschedule.Source{PlaylistURL: "https://example.com/daily.json"},
	)
	sched.Commit()
	require.False(t, store.saved.IsZero())

	plain := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Title: "Plain",
		Items: []dp1playlist.PlaylistItem{item("plain", "")},
	}}
	active := sched.Prepare(plain)
	require.Equal(t, []string{"plain"}, itemIDs(active.Items))
	assert.False(t, sched.HasCache())
	assert.False(t, store.cleared, "non-scheduled replacement must not clear durable cache before player acceptance")
	assert.False(t, store.saved.IsZero())

	sched.Commit()
	assert.True(t, store.cleared)
	assert.True(t, store.saved.IsZero())
}

func TestClearThenWithPlayerPush_ClearRequiresAcceptedDefault(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	store := &memoryStore{}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.NewWithStore(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, store, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	_ = sched.PrepareWithSource(
		displayAtPlaylist(item("today", "2026-07-22T00:00:00Z")),
		playlistschedule.Source{PlaylistURL: "https://example.com/daily.json"},
	)
	sched.Commit()
	require.False(t, store.saved.IsZero())

	sched.ClearThenWithPlayerPush(func() bool { return false })
	assert.True(t, sched.HasCache())
	assert.False(t, store.cleared, "rejected default must not clear durable cache")
	assert.False(t, store.saved.IsZero())

	sched.ClearThenWithPlayerPush(func() bool { return true })
	assert.False(t, sched.HasCache())
	assert.True(t, store.cleared)
	assert.True(t, store.saved.IsZero())
}

func TestStop_CancelsTimerWithoutClearingPersistedPlaylist(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	store := &memoryStore{}
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	timerCanceled := make(chan struct{})
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			close(timerCanceled)
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.NewWithStore(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, store, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	_ = sched.PrepareWithSource(
		displayAtPlaylist(
			item("today", "2026-07-22T00:00:00Z"),
			item("tomorrow", "2026-07-23T00:00:00Z"),
		),
		playlistschedule.Source{PlaylistURL: "https://example.com/daily.json"},
	)
	sched.Commit()
	require.False(t, store.saved.IsZero())

	sched.Stop()
	select {
	case <-timerCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("displayAt timer was not canceled on Stop")
	}
	assert.False(t, store.saved.IsZero(), "service shutdown must preserve restart recovery source")
	assert.False(t, store.cleared)
}

func TestNewWithStore_RestoredSourceDoesNotRecomputeUntilRefresh(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	store := &memoryStore{
		saved: playlistschedule.Source{PlaylistURL: "https://example.com/daily.json"},
	}
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	cdpMock.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	sched := playlistschedule.NewWithStore(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, store, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	require.False(t, sched.HasCache())
	require.True(t, sched.RestoredPending())

	sched.RecomputeNow(context.Background())
	sched.ResumePersisted(context.Background())
}

func TestRestore_RestoredPendingSourceOnlyKeepsDurableSource(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	playlistURL := "https://example.com/daily.json"
	store := &memoryStore{
		saved: playlistschedule.Source{PlaylistURL: playlistURL},
	}
	now := time.Date(2026, 7, 23, 1, 0, 0, 0, time.UTC)
	clock.EXPECT().Now().Return(now).AnyTimes()
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			return ctx.Err()
		},
	).AnyTimes()

	sched := playlistschedule.NewWithStore(context.Background(), cdpMock, clock, func() *time.Location {
		return time.UTC
	}, store, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	snapshot := sched.Snapshot()

	_ = sched.PrepareWithSource(
		displayAtPlaylist(item("day23", "2026-07-23T00:00:00Z")),
		playlistschedule.Source{PlaylistURL: playlistURL},
	)
	require.True(t, sched.HasCache())

	sched.Restore(snapshot)

	assert.True(t, sched.RestoredPending())
	assert.False(t, sched.HasCache())
	assert.Equal(t, playlistURL, store.saved.PlaylistURL, "failed first refresh must keep durable source for retry")
	assert.False(t, store.cleared)
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

func TestPrepare_InvalidPresentDisplayAt_StillSchedules(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	emptyDisplayAt := ""
	active := sched.Prepare(&dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{ID: "invalid", Source: "https://example.com/invalid.html", DisplayAt: &emptyDisplayAt},
				item("intro", ""),
			},
		},
	})
	require.Equal(t, []string{"intro"}, itemIDs(active.Items))
	assert.True(t, sched.HasCache())
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

type memoryStore struct {
	saved   playlistschedule.Source
	cleared bool
	err     error
}

func (m *memoryStore) Load() (*playlistschedule.Source, error) {
	if m.err != nil {
		return nil, m.err
	}
	if m.saved.IsZero() {
		return nil, nil
	}
	source := m.saved
	return &source, nil
}

func (m *memoryStore) Save(source playlistschedule.Source) error {
	if m.err != nil {
		return m.err
	}
	m.saved = source
	m.cleared = false
	return nil
}

func (m *memoryStore) Clear() error {
	if m.err != nil {
		return m.err
	}
	m.saved = playlistschedule.Source{}
	m.cleared = true
	return nil
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

func TestRecomputeNow_SnapshotsSourceWithActiveSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	oldURL := "https://example.com/old.json"
	newURL := "https://example.com/new.json"
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
	_ = sched.PrepareWithSource(
		displayAtPlaylist(item("old", "2026-07-22T00:00:00Z")),
		playlistschedule.Source{PlaylistURL: oldURL},
	)

	var mutateOnce sync.Once
	cdpMock.EXPECT().Initialized().DoAndReturn(func() bool {
		mutateOnce.Do(func() {
			_ = sched.PrepareWithSource(
				displayAtPlaylist(item("new", "2026-07-22T00:00:00Z")),
				playlistschedule.Source{PlaylistURL: newURL},
			)
		})
		return true
	}).AnyTimes()

	var sends int
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			sends++
			expr := params["expression"].(string)
			if sends == 1 {
				assert.Contains(t, expr, `"id":"old"`)
				assert.Contains(t, expr, `"playlistUrl":"`+oldURL+`"`)
				assert.NotContains(t, expr, newURL)
				return map[string]interface{}{"ok": true}, nil
			}
			assert.Contains(t, expr, `"id":"new"`)
			assert.Contains(t, expr, `"playlistUrl":"`+newURL+`"`)
			return map[string]interface{}{"ok": true}, nil
		},
	).Times(2)

	sched.RecomputeNow(context.Background())
}

// TestRecompute_RetriesAfterFailedFinalBoundaryPush pins the fix for a
// rejected cutover push at the *last* displayAt boundary: armTimerLocked
// finds no future boundary to re-arm in that case, so only an independent
// push retry can recover the cutover without waiting for an unrelated
// wake/reconnect/Prepare.
func TestRecompute_RetriesAfterFailedFinalBoundaryPush(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()

	retryScheduled := make(chan time.Duration, 1)
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, d time.Duration) error {
			retryScheduled <- d
			return nil // simulate the retry delay elapsing immediately
		},
	).Times(1)

	cdpMock.EXPECT().Initialized().Return(true).AnyTimes()
	var sends int32
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			if atomic.AddInt32(&sends, 1) == 1 {
				return map[string]interface{}{"ok": false}, nil
			}
			return map[string]interface{}{"ok": true}, nil
		},
	).Times(2)

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	// A single item already past its displayAt with nothing scheduled after
	// it: armTimerLocked has no future boundary to arm, matching the "final
	// cutover" scenario from the bug report.
	_ = sched.Prepare(displayAtPlaylist(item("today", "2026-07-22T00:00:00Z")))

	sched.RecomputeNow(context.Background())

	select {
	case <-retryScheduled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected a push retry to be armed after the rejected cutover push")
	}

	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&sends) == 2
	}, 2*time.Second, 10*time.Millisecond, "expected the armed retry to resend the cutover push")
}

// TestClear_CancelsPendingPushRetry proves a retry armed by a failed push
// cannot fire after Clear replaces the schedule — otherwise it could push a
// stale playlist over whatever replaced it.
func TestClear_CancelsPendingPushRetry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()

	retryCanceled := make(chan struct{})
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ time.Duration) error {
			<-ctx.Done()
			close(retryCanceled)
			return ctx.Err()
		},
	).Times(1)

	cdpMock.EXPECT().Initialized().Return(true).Times(1)
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(
		map[string]interface{}{"ok": false}, nil,
	).Times(1)

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.Prepare(displayAtPlaylist(item("today", "2026-07-22T00:00:00Z")))
	sched.RecomputeNow(context.Background())

	sched.Clear()

	select {
	case <-retryCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("expected Clear to cancel the pending push retry")
	}
}

// TestPrepare_ResetsPushRetryStateForNewSchedule pins that a new Prepare
// cancels a still-pending retry from the previous schedule's failed cutover
// and does not let the new schedule inherit its backoff/attempt count. Without
// this, a run of failures on an old (possibly now-irrelevant) schedule could
// leave a brand-new schedule starting retries at an inflated backoff, or
// already past the give-up threshold.
func TestPrepare_ResetsPushRetryStateForNewSchedule(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	clock := mocks.NewMockClock(ctrl)
	cdpMock := mocks.NewMockCDP(ctrl)
	loc := time.UTC
	now := time.Date(2026, 7, 22, 12, 0, 0, 0, loc)
	clock.EXPECT().Now().Return(now).AnyTimes()

	waitDurations := make(chan time.Duration, 2)
	clock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(ctx context.Context, d time.Duration) error {
			waitDurations <- d
			<-ctx.Done()
			return ctx.Err()
		},
	).Times(2)

	cdpMock.EXPECT().Initialized().Return(true).AnyTimes()
	cdpMock.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(
		map[string]interface{}{"ok": false}, nil,
	).Times(2)

	sched := playlistschedule.New(context.Background(), cdpMock, clock, func() *time.Location {
		return loc
	}, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	_ = sched.Prepare(displayAtPlaylist(item("old", "2026-07-22T00:00:00Z")))
	sched.RecomputeNow(context.Background())

	select {
	case d := <-waitDurations:
		assert.Equal(t, 5*time.Second, d, "first failed cutover should retry at the base backoff")
	case <-time.After(2 * time.Second):
		t.Fatal("expected the first push retry to be armed")
	}

	// A new schedule must cancel the stale retry and not inherit its backoff
	// state, even though the previous retry is still "pending" (blocked
	// above on its now-canceled context).
	_ = sched.Prepare(displayAtPlaylist(item("new", "2026-07-22T00:00:00Z")))
	sched.RecomputeNow(context.Background())

	select {
	case d := <-waitDurations:
		assert.Equal(t, 5*time.Second, d,
			"new schedule's first failed cutover must restart backoff at the base delay, "+
				"not inherit the old schedule's attempt count")
	case <-time.After(2 * time.Second):
		t.Fatal("expected the new schedule's push retry to be armed at the base backoff")
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
			case strings.Contains(expr, `"id":"replacement"`):
				pushedIDs = append(pushedIDs, "replacement")
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

	// While recompute holds pushMu inside Send, replacement must wait, then
	// run after — and recompute must not push again after clear.
	replacementDone := make(chan struct{})
	go func() {
		sched.ClearThenWithPlayerPush(func() bool {
			// Simulate a scheduler-owned replacement CDP send under the same lock.
			_, _ = cdpMock.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
				"expression": `window.handleCDPRequest({"command":"displayPlaylist","request":{"dp1_call":{"items":[{"id":"replacement"}]}}})`,
			})
			return true
		})
		close(replacementDone)
	}()

	time.Sleep(50 * time.Millisecond)
	close(releaseSend)

	select {
	case <-doneRecompute:
	case <-time.After(2 * time.Second):
		t.Fatal("recompute did not finish")
	}
	select {
	case <-replacementDone:
	case <-time.After(2 * time.Second):
		t.Fatal("ClearThenWithPlayerPush did not finish")
	}

	assert.False(t, sched.HasCache())
	mu.Lock()
	defer mu.Unlock()
	require.NotEmpty(t, pushedIDs)
	assert.Equal(t, "replacement", pushedIDs[len(pushedIDs)-1], "replacement must win after clear")
}
