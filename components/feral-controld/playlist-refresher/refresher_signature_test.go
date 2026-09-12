package refresher_test

// The refresher's share of DP-1 signature verification
// (feral-file/ffos-user#307): it re-publishes the verdict of what it
// re-pushes, and publishes nothing for the cached-copy fallback, which
// carries no verdict.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func signedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "sigverify", "testdata", "feed-dd875fbe.json"))
	require.NoError(t, err)
	return raw
}

// TestRefresher_SoftRefresh_ReconcilesWithoutAttestingOnAcceptance: a soft
// refresh (refresh:true) may leave the old item on screen, so the slot is
// reconciled, not overwritten. Here the URL-only slot held "unsigned" and
// the refreshed document is valid: the poller cannot tell the two apart, so
// the slot is cleared rather than attesting valid for whichever shows.
func TestRefresher_SoftRefresh_ReconcilesWithoutAttestingOnAcceptance(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupBackgroundMocks(ts)
	active := &sigverify.Active{}
	refresher.SetSignatureVerification(ts.refresher, active, zaptest.NewLogger(t))

	playlistURL := "http://example.com/playlist.json"
	active.Set("", playlistURL, sigverify.StatusUnsigned)
	verdict := sigverify.Verify(signedFixture(t))
	require.Equal(t, sigverify.StatusValid, verdict.Status)
	playlist := createMockPlaylistNoDynamic()
	playlist.ID = ""
	playlist.Verification = &verdict

	sent := make(chan struct{}, 1)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).AnyTimes()
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(_ string, params map[string]any) (any, error) {
		assert.Contains(t, params["expression"].(string), `"refresh":true`, "this pass must be a soft refresh")
		if _, stillAttested := active.Lookup("", playlistURL); stillAttested {
			t.Error("the slot must be reconciled BEFORE the send goes out")
		}
		select {
		case sent <- struct{}{}:
		default:
		}
		return playerOKResponse(), nil
	}).AnyTimes()

	ts.refresher.Start()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never re-pushed")
	}
	time.Sleep(200 * time.Millisecond)
	ts.refresher.Stop()

	_, found := active.Lookup("", playlistURL)
	assert.False(t, found, "a changed verdict on a soft refresh must not be attested on acceptance")
}

// TestRefresher_SoftRefresh_SameVerdictKeepsAnnotation: when the refreshed
// document carries the same verdict the slot already holds, the annotation
// is kept — it is true of either document.
func TestRefresher_SoftRefresh_SameVerdictKeepsAnnotation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupBackgroundMocks(ts)
	active := &sigverify.Active{}
	refresher.SetSignatureVerification(ts.refresher, active, zaptest.NewLogger(t))

	playlistURL := "http://example.com/playlist.json"
	active.Set("pl-refreshed", playlistURL, sigverify.StatusValid)
	verdict := sigverify.Verify(signedFixture(t))
	require.Equal(t, sigverify.StatusValid, verdict.Status)
	playlist := createMockPlaylistNoDynamic()
	playlist.ID = "pl-refreshed"
	playlist.Verification = &verdict

	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).AnyTimes()
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOKResponse(), nil).AnyTimes()

	ts.refresher.Start()
	require.Eventually(t, func() bool {
		_, ok := active.Lookup("pl-refreshed", playlistURL)
		return ok
	}, 2*time.Second, 10*time.Millisecond)
	ts.refresher.Stop()

	st, _ := active.Lookup("", playlistURL)
	assert.Equal(t, sigverify.StatusValid, st)
}

// TestRefresher_CachedFallback_ClearsSlot mirrors commandrouter's rule: the
// cached copy carries no verdict, so a re-push from it CLEARS the slot an
// earlier live fetch of the same URL had set — player_status must not keep
// vouching for bytes nobody verified. The cached body is the signed fixture
// itself, so a loader that judged it would flip this test.
func TestRefresher_CachedFallback_ClearsSlot(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupBackgroundMocks(ts)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	mockOfflineCache := mocks.NewMockOfflineCacheService(ts.ctrl)
	r := refresher.New(ts.ctx, ts.mockDP1, ts.mockStatusPoller, ts.mockCDP, nil, mockOfflineCache, wrapper.NewJSON(), nil, ts.mockClock, logger)
	active := &sigverify.Active{}
	refresher.SetSignatureVerification(r, active, logger)

	playlistURL := "http://example.com/playlist.json"
	active.Set("live-1", playlistURL, sigverify.StatusValid) // an earlier live pass of this URL
	cachedRaw := signedFixture(t)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(nil, errors.New("offline")).AnyTimes()
	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).Return(json.RawMessage(cachedRaw), nil).AnyTimes()
	sent := make(chan struct{}, 1)
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(string, map[string]any) (any, error) {
		select {
		case sent <- struct{}{}:
		default:
		}
		return playerOKResponse(), nil
	}).AnyTimes()

	r.Start()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never re-pushed the cached copy")
	}
	r.Stop()

	_, found := active.Lookup("live-1", playlistURL)
	assert.False(t, found, "the earlier verdict must not survive an unverified re-push of the same URL")
}

// futureOnlyRefresher builds a refresher over a REAL scheduler so a
// future-only refresh takes the empty-active-set branch (schedule replaced,
// nothing sent). The scheduler's timer is parked on ctx so no cutover fires
// on its own; the test promotes explicitly, standing in for the cutover the
// push observer performs (pinned separately in playlistschedule's tests).
func futureOnlyRefresher(t *testing.T, ts *testSetup, offlineCache *mocks.MockOfflineCacheService) (refresher.Refresher, *sigverify.Active) {
	t.Helper()
	r, active := scheduledRefresher(t, ts, offlineCache)
	// A future-only schedule is never sent by the refresher itself.
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Times(0)
	return r, active
}

// scheduledRefresher is futureOnlyRefresher without the no-send expectation,
// for schedules whose active cohort is non-empty and therefore pushed.
func scheduledRefresher(t *testing.T, ts *testSetup, offlineCache *mocks.MockOfflineCacheService) (refresher.Refresher, *sigverify.Active) {
	t.Helper()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	setupBackgroundMocks(ts)
	ts.mockClock.EXPECT().Now().Return(time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)).AnyTimes()
	ts.mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()
	sched := playlistschedule.New(ts.ctx, ts.mockCDP, ts.mockClock, func() *time.Location { return time.UTC }, logger)
	t.Cleanup(sched.Stop)
	var cache offlinecache.Service
	if offlineCache != nil {
		cache = offlineCache
	}
	r := refresher.New(ts.ctx, ts.mockDP1, ts.mockStatusPoller, ts.mockCDP, nil, cache, wrapper.NewJSON(), sched, ts.mockClock, logger)
	active := &sigverify.Active{}
	refresher.SetSignatureVerification(r, active, logger)
	sched.SetPushObserver(func(p playlistschedule.PushPhase) {
		switch p {
		case playlistschedule.PushStarting:
			active.ClearCurrent()
		case playlistschedule.PushAccepted:
			active.Promote()
		}
	})
	return r, active
}

// TestRefresher_ActiveCohortRefresh_RestagesPending: a scheduled V2 whose
// active cohort is non-empty is pushed (soft) AND replaces the schedule; the
// pending slot must follow the schedule, or V2's next cutover would promote
// the verdict a deferred V1 parked earlier.
func TestRefresher_ActiveCohortRefresh_RestagesPending(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	r, active := scheduledRefresher(t, ts, nil)

	playlistURL := "http://example.com/daily.json"
	active.Set("showing", playlistURL, sigverify.StatusValid)
	active.SetPending("v1", playlistURL, sigverify.StatusValid)
	today, tomorrow := "2026-07-22T00:00:00Z", "2026-07-23T00:00:00Z"
	v2 := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID: "v2",
		Items: []dp1playlist.PlaylistItem{
			{ID: "today", Source: "https://example.com/today", DisplayAt: &today},
			{ID: "tomorrow", Source: "https://example.com/tomorrow", DisplayAt: &tomorrow},
		},
	}}
	unsigned := sigverify.Verify([]byte(`{"dpVersion":"1.1.0","title":"v2","items":[]}`))
	v2.Verification = &unsigned

	sent := make(chan struct{}, 1)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(v2, nil).AnyTimes()
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(string, map[string]any) (any, error) {
		select {
		case sent <- struct{}{}:
		default:
		}
		return playerOKResponse(), nil
	}).AnyTimes()

	r.Start()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never pushed the active cohort")
	}
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	active.Promote() // the next cutover of V2's schedule
	st, ok := active.Lookup("v2", "")
	assert.True(t, ok, "the cutover promotes the document the schedule holds")
	assert.Equal(t, sigverify.StatusUnsigned, st)
	_, ok = active.Lookup("v1", "")
	assert.False(t, ok, "the earlier deferred verdict must not survive the replacement")
}

func deferredSchedulePlaylist(id string) *dp1.Playlist {
	displayAt := "2026-07-23T00:00:00Z"
	return &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    id,
		Items: []dp1playlist.PlaylistItem{{ID: "tomorrow", Source: "https://example.com/tomorrow", DisplayAt: &displayAt}},
	}}
}

// TestRefresher_FutureOnlyRefresh_RestagesPending: a deferred V1 parked its
// verdict as pending; a refresh replaces the schedule with future-only V2.
// The cutover must promote V2's verdict, not V1's — the refresh restages.
func TestRefresher_FutureOnlyRefresh_RestagesPending(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	r, active := futureOnlyRefresher(t, ts, nil)

	playlistURL := "http://example.com/daily.json"
	active.Set("showing", playlistURL, sigverify.StatusValid)
	active.SetPending("v1", playlistURL, sigverify.StatusValid)
	v2 := deferredSchedulePlaylist("v2")
	unsigned := sigverify.Verify([]byte(`{"dpVersion":"1.1.0","title":"v2","items":[]}`))
	v2.Verification = &unsigned

	passed := make(chan struct{}, 1)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).DoAndReturn(
		func(context.Context, string, bool) (*dp1.Playlist, error) {
			select {
			case passed <- struct{}{}:
			default:
			}
			return v2, nil
		}).AnyTimes()

	r.Start()
	select {
	case <-passed:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never resolved the playlist")
	}
	// Let the pass finish its (send-less) push section, then join it.
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	st, ok := active.Lookup("showing", playlistURL)
	assert.True(t, ok, "before the cutover the showing document keeps its verdict")
	assert.Equal(t, sigverify.StatusValid, st)

	active.Promote()
	st, ok = active.Lookup("", playlistURL)
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusUnsigned, st, "the cutover promotes V2, not the stale V1")
	_, ok = active.Lookup("v1", "")
	assert.False(t, ok)
}

// TestRefresher_FutureOnlyCachedFallback_ClearsOnPromote: the future-only
// replacement came from the cached copy (no verdict). Its promotion must
// clear, not inherit the showing document's verdict.
func TestRefresher_FutureOnlyCachedFallback_ClearsOnPromote(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	mockOfflineCache := mocks.NewMockOfflineCacheService(ts.ctrl)
	r, active := futureOnlyRefresher(t, ts, mockOfflineCache)

	playlistURL := "http://example.com/daily.json"
	active.Set("showing", playlistURL, sigverify.StatusValid)
	cachedRaw := []byte(`{"dpVersion":"1.1.0","id":"v2-cached","title":"t","items":[{"id":"tomorrow","source":"https://example.com/tomorrow","displayAt":"2026-07-23T00:00:00Z"}]}`)

	loaded := make(chan struct{}, 1)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(nil, errors.New("offline")).AnyTimes()
	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).DoAndReturn(func(string) (json.RawMessage, error) {
		select {
		case loaded <- struct{}{}:
		default:
		}
		return json.RawMessage(cachedRaw), nil
	}).AnyTimes()

	r.Start()
	select {
	case <-loaded:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never loaded the cached copy")
	}
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	st, ok := active.Lookup("showing", playlistURL)
	assert.True(t, ok, "before the cutover the showing document keeps its verdict")
	assert.Equal(t, sigverify.StatusValid, st)

	active.Promote()
	_, ok = active.Lookup("showing", playlistURL)
	assert.False(t, ok, "an unverified scheduled document must not inherit a verdict at cutover")
}

// TestRefresher_ForceCast_UnpublishedAcrossGenerationBump: the cold-cache
// reconstruction of a scheduled playlist is a force cast; when the page
// generation moves while the send is in flight, its accepted reply is for a
// page that is gone, so the verdict is not published — but the schedule was
// replaced, so pending is still restaged for the new page's re-push.
func TestRefresher_ForceCast_UnpublishedAcrossGenerationBump(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	r, active := scheduledRefresher(t, ts, nil)
	gen := uint64(1)
	refresher.SetSessionGeneration(r, func() uint64 { return gen }, zap.NewNop())

	playlistURL := "http://example.com/daily.json"
	today, tomorrow := "2026-07-22T00:00:00Z", "2026-07-23T00:00:00Z"
	v2 := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID: "v2",
		Items: []dp1playlist.PlaylistItem{
			{ID: "today", Source: "https://example.com/today", DisplayAt: &today},
			{ID: "tomorrow", Source: "https://example.com/tomorrow", DisplayAt: &tomorrow},
		},
	}}
	unsigned := sigverify.Verify([]byte(`{"dpVersion":"1.1.0","title":"v2","items":[]}`))
	v2.Verification = &unsigned

	sent := make(chan struct{}, 1)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(v2, nil).AnyTimes()
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(_ string, params map[string]any) (any, error) {
		assert.Contains(t, params["expression"].(string), `"now_display"`, "cold-cache reconstruction is a force cast")
		gen++ // the page reloads while the send is in flight
		select {
		case sent <- struct{}{}:
		default:
		}
		return playerOKResponse(), nil
	}).AnyTimes()

	r.Start()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never force-cast")
	}
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	_, ok := active.Lookup("v2", playlistURL)
	assert.False(t, ok, "an accepted reply from a page that is gone publishes nothing")
	active.Promote() // the new generation's re-push
	st, ok := active.Lookup("v2", playlistURL)
	assert.True(t, ok, "pending was restaged for the schedule the cache now holds")
	assert.Equal(t, sigverify.StatusUnsigned, st)
}

// TestRefresher_Strict_SkipsNonValidRefresh: under strict, a re-fetched
// document that is not proven valid is not pushed — the current artwork
// stays and the pass is a successful no-op, so policy never drives the
// startup escalation.
func TestRefresher_Strict_SkipsNonValidRefresh(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupBackgroundMocks(ts)
	active := &sigverify.Active{}
	refresher.SetSignatureVerification(ts.refresher, active, zaptest.NewLogger(t))
	refresher.SetSignatureVerificationMode(ts.refresher, func() sigverify.Mode { return sigverify.ModeStrict }, zap.NewNop())

	playlistURL := "http://example.com/playlist.json"
	active.Set("showing", playlistURL, sigverify.StatusValid)
	unsigned := sigverify.Verify([]byte(`{"dpVersion":"1.1.0","title":"t","items":[]}`))
	playlist := createMockPlaylistNoDynamic()
	playlist.ID = "refreshed-unsigned"
	playlist.Verification = &unsigned

	resolved := make(chan struct{}, 1)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).DoAndReturn(
		func(context.Context, string, bool) (*dp1.Playlist, error) {
			select {
			case resolved <- struct{}{}:
			default:
			}
			return playlist, nil
		}).AnyTimes()
	// No CDP Send expectation: a push is an unexpected call.

	ts.refresher.Start()
	select {
	case <-resolved:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never resolved the playlist")
	}
	time.Sleep(200 * time.Millisecond)
	ts.refresher.Stop()

	st, ok := active.Lookup("showing", playlistURL)
	assert.True(t, ok, "the current artwork keeps playing and keeps its verdict")
	assert.Equal(t, sigverify.StatusValid, st)
}

// TestRefresher_SetSignatureVerification_ForeignImplementationIsLeftAlone
// pins the setter's contract: a non-concrete Refresher logs and is untouched
// rather than panicking.
func TestRefresher_SetSignatureVerification_ForeignImplementationIsLeftAlone(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	foreign := mocks.NewMockRefresher(ctrl)

	assert.NotPanics(t, func() {
		refresher.SetSignatureVerification(foreign, &sigverify.Active{}, zap.NewNop())
	})
}
