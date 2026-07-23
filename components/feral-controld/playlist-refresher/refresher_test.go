package refresher_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

type testSetup struct {
	ctrl             *gomock.Controller
	ctx              context.Context
	cancel           context.CancelFunc
	mockCDP          *mocks.MockCDP
	mockStatusPoller *mocks.MockStatusPoller
	mockDP1          *mocks.MockDP1
	mockClock        *mocks.MockClock
	refresher        refresher.Refresher
}

func setup(t *testing.T) *testSetup {
	ts := setupWithLogger(t, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))

	// Most tests exercise the connected-CDP paths, so default Initialized to
	// true. Headless tests build their own setup via setupWithLogger and stub
	// Initialized themselves.
	ts.mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	return ts
}

func setupWithLogger(t *testing.T, logger *zap.Logger) *testSetup {
	ctrl := gomock.NewController(t)
	ctx, cancel := context.WithCancel(context.Background())

	// Dependencies
	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	refresher := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), mockClock, logger)

	return &testSetup{
		ctrl:             ctrl,
		ctx:              ctx,
		cancel:           cancel,
		mockCDP:          mockCDP,
		mockStatusPoller: mockStatusPoller,
		mockDP1:          mockDP1,
		mockClock:        mockClock,
		refresher:        refresher,
	}
}

func (ts *testSetup) teardown() {
	ts.cancel()
	ts.ctrl.Finish()
}

// Helper function to create a mock player status
func createMockPlayerStatus(command string, playlistURL *string, playlist *dp1.Playlist) *status.PlayerStatus {
	return &status.PlayerStatus{
		Command:     command,
		PlaylistURL: playlistURL,
		Playlist:    playlist,
		Index:       nil,
		IsPaused:    nil,
	}
}

// Helper function to create a mock playlist
func createMockPlaylist() *dp1.Playlist {
	return &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    "Test Item 1",
					Source:   "http://example.com/video1.mp4",
					Duration: float64Ptr(300),
					License:  "open",
				},
			},
		},
		DynamicQueries: []dp1.LegacyDynamicQuery{
			{
				Endpoint: "https://indexer.feralfile.com/graphql",
				Params: map[string]string{
					"limit":  "50",
					"offset": "0",
				},
			},
		},
	}
}

// Helper function to create a mock playlist without dynamic queries
func createMockPlaylistNoDynamic() *dp1.Playlist {
	return &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    "Test Item 1",
					Source:   "http://example.com/video1.mp4",
					Duration: float64Ptr(300),
					License:  "open",
				},
			},
		},
		DynamicQueries: []dp1.LegacyDynamicQuery{},
	}
}

// createMockPlaylistSpecDynamic is a playlist with only DP-1 dynamicQuery (no legacy dynamicQueries).
func createMockPlaylistSpecDynamic() *dp1.Playlist {
	return &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    "Test Item 1",
					Source:   "http://example.com/video1.mp4",
					Duration: float64Ptr(300),
					License:  "open",
				},
			},
			DynamicQuery: &playlists.DynamicQuery{
				Profile:  dp1playlist.ProfileGraphQLV1,
				Endpoint: "https://indexer.example/graphql",
				Query:    `query { items(limit: {{limit}}, offset: {{offset}}) { id title source } }`,
				ResponseMapping: playlists.ResponseMapping{
					ItemsPath:  "data.items",
					ItemSchema: "dp1/1.0",
				},
			},
		},
	}
}

func float64Ptr(f float64) *float64 {
	return &f
}

// Helper function to set up common mock expectations for background goroutine
func setupBackgroundMocks(ts *testSetup) {
	// Create a mock ticker
	mockTicker := mocks.NewMockTicker(ts.ctrl)
	tickerChan := make(chan time.Time, 1)

	// Mock the ticker's C() method to return our controllable channel
	mockTicker.EXPECT().
		C().
		Return(tickerChan).
		AnyTimes()

	// Mock the ticker's Stop() method
	mockTicker.EXPECT().
		Stop().
		AnyTimes()

	// Expect clock to create ticker
	ts.mockClock.EXPECT().
		NewTicker(gomock.Any()).
		Return(mockTicker).
		AnyTimes()
}

func TestRefresher_Start_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock expectations for background goroutine
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, errors.New("test error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Test
	ts.refresher.Start()

	// Give it a moment to start
	time.Sleep(50 * time.Millisecond)

	// Stop to clean up
	ts.refresher.Stop()

	// Verify that the refresher is started (we can't easily test the goroutine directly)
	// The main test is that Start doesn't panic and returns immediately
}

func TestRefresher_Start_AlreadyStarted(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock expectations for background goroutine
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, errors.New("test error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Start first time
	ts.refresher.Start()

	// Start second time - should return early
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(50 * time.Millisecond)

	// Stop to clean up
	ts.refresher.Stop()

	// Should not panic or cause issues
}

func TestRefresher_Stop_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock expectations for background goroutine
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, errors.New("test error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to start
	time.Sleep(50 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()

	// Should not panic
}

func TestRefresher_Stop_NotStarted(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Stop without starting - should return early
	ts.refresher.Stop()

	// Should not panic
}

func TestRefresher_ConcurrentStartStop(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock expectations for any background goroutines
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, errors.New("test error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Test concurrent Start/Stop operations
	var wg sync.WaitGroup

	// Start multiple goroutines that call Start/Stop
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ts.refresher.Start()
			time.Sleep(10 * time.Millisecond)
			ts.refresher.Stop()
		}()
	}

	wg.Wait()

	// Should not panic or cause issues
}

func TestRefresher_MultipleStartStop(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Set up mock expectations for any background goroutines
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, errors.New("test error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Test multiple Start/Stop cycles
	for range 5 {
		ts.refresher.Start()
		time.Sleep(10 * time.Millisecond)
		ts.refresher.Stop()
		time.Sleep(10 * time.Millisecond)
	}

	// Should not panic or cause issues
}

// Test the core functionality with proper mock expectations
func TestRefresher_ProcessPlayingPlaylist_PlaylistURL(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupBackgroundMocks(ts)

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	// Expect status poller to return player status with playlist URL
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()

	// Expect DP1 to process playlist URL
	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()

	// Expect CDP to send the playlist
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
			expression := params["expression"].(string)
			assert.Contains(t, expression, "window.handleCDPRequest")
			assert.Contains(t, expression, "dp1_call")
			assert.Contains(t, expression, "refresh")
			return "success", nil
		}).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_SyncsKioskReplayScope(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item1"}).
		Return(nil).
		MinTimes(1)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), mockClock, logger)
	r.Start()
	time.Sleep(100 * time.Millisecond)
	r.Stop()
}

// TestRefresher_ProcessPlayingPlaylist_HoldsPlaybackLockAcrossSyncAndSend
// is the refresher-side regression test for the "replay scope and kiosk
// navigation are not serialized" hazard (its commandrouter twin lives in
// handler_test.go). Rather than pin an exact InOrder sequence — the
// refresher runs on a background goroutine that may take multiple passes —
// it records lock depth around every scope sync and CDP send and asserts
// the safety invariant directly: the playback lock (see
// offlinecache.KioskReplay.LockPlayback) is ALWAYS held (depth == 1) while
// this refresher pass syncs replay scope and re-sends the playlist, for
// every pass. A future edit that syncs or sends outside the lock trips
// the recorded violation regardless of how many passes run.
func TestRefresher_ProcessPlayingPlaylist_HoldsPlaybackLockAcrossSyncAndSend(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	// This test's CDP send returns a bare "success" string rather than a
	// realistic {"message":{"ok":true}} envelope, which
	// isPlayerResponseOk treats as NOT ok (fail-closed — see its doc),
	// so every pass here also exercises the corrective-resync path
	// below; PlaybackGeneration just needs to answer, not assert
	// anything about it.
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()

	// held tracks the playback lock depth; violation records the first
	// time a scope sync or CDP send observed the lock NOT held. Guarded
	// by mu because the assertion (in the main goroutine) reads them
	// while the refresher goroutine's mock callbacks write them.
	var mu sync.Mutex
	held := 0
	violation := ""
	observeLockHeld := func(op string) {
		mu.Lock()
		defer mu.Unlock()
		if held != 1 && violation == "" {
			violation = op + " ran while the playback lock was not held"
		}
	}

	mockKioskReplay.EXPECT().LockPlayback().Do(func() {
		mu.Lock()
		held++
		mu.Unlock()
	}).MinTimes(1)
	mockKioskReplay.EXPECT().UnlockPlayback().Do(func() {
		mu.Lock()
		held--
		mu.Unlock()
	}).MinTimes(1)
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item1"}).
		Do(func(_ context.Context, _ []string) { observeLockHeld("SyncPlaylist") }).
		Return(nil).
		MinTimes(1)
	// MarkPlaybackChanged must also run under the lock (it announces the
	// scope change the resync must defer to — see
	// KioskReplay.PlaybackGeneration), so assert lock-held for it too.
	mockKioskReplay.EXPECT().
		MarkPlaybackChanged().
		Do(func() { observeLockHeld("MarkPlaybackChanged") }).
		MinTimes(1)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Do(func(_ string, _ map[string]interface{}) { observeLockHeld("CDP send") }).
		Return("success", nil).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), mockClock, logger)
	r.Start()
	time.Sleep(100 * time.Millisecond)
	r.Stop()

	mu.Lock()
	defer mu.Unlock()
	assert.Empty(t, violation, "scope sync and CDP send must both run under the playback lock")
	assert.Zero(t, held, "every LockPlayback must be balanced by an UnlockPlayback")
}

// TestRefresher_ForceRefresh_TriggersImmediateSyncBeforeNextTick is the
// regression test pinning that offline-cache replay scope must not only
// ever be re-synced by the next displayPlaylist command or the next
// PLAYLIST_REFRESH_INTERVAL tick — up to 5 minutes after a kiosk/CDP
// reconnect (see main.go's onConnect hook). The ticker channel here never
// fires on its own, so the only way a second sync pass can happen is via
// ForceRefresh.
func TestRefresher_ForceRefresh_TriggersImmediateSyncBeforeNextTick(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	// A ticker channel that never delivers a tick: without ForceRefresh,
	// only the one initial pass from background()'s own
	// retry-until-success loop would ever run.
	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		AnyTimes()

	syncCount := make(chan struct{}, 8)
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item1"}).
		DoAndReturn(func(context.Context, []string) error {
			syncCount <- struct{}{}
			return nil
		}).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), mockClock, logger)
	r.Start()
	defer r.Stop()

	// Drain the one sync pass background()'s own initial retry-until-
	// success loop produces at Start.
	select {
	case <-syncCount:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the initial sync pass")
	}

	r.ForceRefresh()

	select {
	case <-syncCount:
	case <-time.After(2 * time.Second):
		t.Fatal("ForceRefresh did not trigger an additional sync pass")
	}
}

// TestRefresher_ForceRefresh_BeforeStartIsSafeNoOp pins that calling
// ForceRefresh before Start (or after Stop) never panics or blocks: the
// buffered channel just absorbs the signal until/unless a background loop
// is running to consume it.
func TestRefresher_ForceRefresh_BeforeStartIsSafeNoOp(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.NotPanics(t, func() {
		ts.refresher.ForceRefresh()
	})
}

// TestRefresher_ProcessPlayingPlaylist_FallsBackToCachedPlaylistWhenOffline
// is the regression test pinning that this refresher path — which is what
// actually runs on every periodic pass AND on the reconnect-triggered
// ForceRefresh (main.go's onConnect hook) — must fall back to a
// previously downloaded copy of a URL playlist when live DP-1 resolution
// fails, the same way commandrouter's resolveDisplayedPlaylist already
// does. Without this fallback, this pass would return early before ever
// reaching kioskReplay.SyncPlaylist below.
func TestRefresher_ProcessPlayingPlaylist_FallsBackToCachedPlaylistWhenOffline(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockOfflineCache := mocks.NewMockOfflineCacheService(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	playlistURL := "http://example.com/playlist.json"
	cachedPlaylist := createMockPlaylistNoDynamic()
	cachedRaw, err := json.Marshal(cachedPlaylist)
	require.NoError(t, err)

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(nil, errors.New("network unreachable")).
		AnyTimes()
	mockOfflineCache.EXPECT().
		CachedPlaylistForURL(playlistURL).
		Return(json.RawMessage(cachedRaw), nil).
		AnyTimes()
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item1"}).
		Return(nil).
		MinTimes(1)
	// The device is offline (live resolution just failed), but the
	// cached copy must still be pushed to the player, exactly as a live
	// success would be.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, mockOfflineCache, wrapper.NewJSON(), mockClock, logger)
	r.Start()
	time.Sleep(100 * time.Millisecond)
	r.Stop()
}

// TestRefresher_ProcessPlayingPlaylist_NoCachedFallbackReturnsOriginalError
// pins that when there is nothing to fall back to (offline caching
// disabled, or this URL was never downloaded), processPlayingPlaylist
// still surfaces the original live DP-1 error rather than a confusing
// cache-lookup error, and never reaches SyncPlaylist/CDP for this pass.
func TestRefresher_ProcessPlayingPlaylist_NoCachedFallbackReturnsOriginalError(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockOfflineCache := mocks.NewMockOfflineCacheService(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	playlistURL := "http://example.com/playlist.json"

	statusCalls := make(chan struct{}, 8)
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		DoAndReturn(func(context.Context) (*status.PlayerStatus, error) {
			statusCalls <- struct{}{}
			return createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil
		}).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(nil, errors.New("network unreachable")).
		AnyTimes()
	mockOfflineCache.EXPECT().
		CachedPlaylistForURL(playlistURL).
		Return(nil, errors.New("offline cache: no playlist saved for this url")).
		AnyTimes()
	// Neither must ever be called: with no cached fallback available,
	// the pass must fail out before reaching sync or CDP resend.

	// The failing pass repeats via background()'s retry-until-success
	// loop (see refresher.go), which sleeps between attempts.
	mockClock.EXPECT().Sleep(gomock.Any()).AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, mockOfflineCache, wrapper.NewJSON(), mockClock, logger)
	r.Start()

	select {
	case <-statusCalls:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the initial process attempt")
	}
	r.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_KioskReplaySyncFailureDoesNotBlockRefresh(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item1"}).
		Return(errors.New("dial failed")).
		MinTimes(1)
	// The authoritative generation bump must still fire even when the
	// SyncPlaylist above errors (see KioskReplay.PlaybackGeneration).
	// Pinned to MinTimes(1) — not AnyTimes — so dropping the bump on the
	// refresher's sync-error branch fails here rather than silently
	// weakening the resync TOCTOU guard.
	mockKioskReplay.EXPECT().
		MarkPlaybackChanged().
		MinTimes(1)
	// The refresh must still proceed to CDP despite the sync failure.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), mockClock, logger)
	r.Start()
	time.Sleep(100 * time.Millisecond)
	r.Stop()
}

// playerNotOkResponse mirrors commandrouter/handler_test.go's own helper of
// the same name (kept as an independent copy — see
// refresher.isPlayerResponseOk's doc for why commandrouter and
// playlist-refresher do not share test helpers either): the shape
// ff-player's window.handleCDPRequest replies with when it rejects a
// command.
func playerNotOkResponse() map[string]interface{} {
	return map[string]interface{}{
		"messageID": "1",
		"message": map[string]interface{}{
			"ok": false,
		},
	}
}

// TestRefresher_ProcessPlayingPlaylist_CDPSendFailureRevertsKioskReplayScope
// is the regression test for the P1 finding that a CDP transport failure
// left replay's Fetch-interception scope pointed at whatever this pass had
// just resolved (via the pre-send SyncPlaylist below), even though the
// kiosk never actually received it. Since the refresher's retry loop keeps
// re-attempting a permanently-failing send (Sleep is mocked as an
// immediate no-op below, so it spins fast rather than actually waiting),
// FetchPlayerStatus/ProcessPlaylistURL are mocked to alternate between two
// distinct playlists on successive calls: this lets the test tell apart
// "the main pass's own pre-send sync" (item-new, the odd calls) from "the
// resync's independent, freshly-queried sync" (item-old, the even calls)
// without pinning a specific call count against a loop that runs an
// indeterminate number of times before the test observes success and
// stops it.
func TestRefresher_ProcessPlayingPlaylist_CDPSendFailureRevertsKioskReplayScope(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	// The retry loop's Sleep between failed attempts is mocked as an
	// immediate no-op, so it spins as fast as the mocks allow rather
	// than actually waiting PLAYER_STATUS_POLLING_INTERVAL each time.
	mockClock.EXPECT().Sleep(gomock.Any()).AnyTimes()

	url := "http://example.com/playlist.json"
	newPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-new"}},
	}}
	oldPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-old"}},
	}}

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &url, nil), nil).
		AnyTimes()

	var resolveCalls atomic.Int64
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, url, false).
		DoAndReturn(func(context.Context, string, bool) (*dp1.Playlist, error) {
			if resolveCalls.Add(1)%2 == 1 {
				return newPlaylist, nil
			}
			return oldPlaylist, nil
		}).
		AnyTimes()

	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("cdp send failed")).
		AnyTimes()

	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item-new"}).
		Return(nil).
		MinTimes(1)
	reverted := make(chan struct{}, 8)
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item-old"}).
		DoAndReturn(func(context.Context, []string) error {
			select {
			case reverted <- struct{}{}:
			default:
			}
			return nil
		}).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), mockClock, logger)
	r.Start()

	select {
	case <-reverted:
		// The revert-to-"item-old" sync ran, proving the CDP send
		// failure triggered an independent re-sync rather than leaving
		// scope pinned to the just-attempted "item-new".
	case <-time.After(2 * time.Second):
		t.Fatal("CDP send failure never triggered a revert sync to the player's currently-reported playlist")
	}

	r.Stop()
}

// TestRefresher_ProcessPlayingPlaylist_PlayerRejectionRevertsKioskReplayScope
// mirrors the CDP-send-failure regression above for the other failure
// shape: the CDP send itself succeeds at the transport level, but the
// player replies ok:false (rejecting the refreshed playlist), which must
// revert scope the same way. Unlike the transport-failure test, err stays
// nil here (see processPlayingPlaylist's doc), so background()'s
// retry-until-success loop treats this single pass as done and moves on
// to the (never-firing, in this test) ticker — making the exact call
// sequence below deterministic enough to pin with gomock.InOrder.
func TestRefresher_ProcessPlayingPlaylist_PlayerRejectionRevertsKioskReplayScope(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	url := "http://example.com/playlist.json"
	newPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-new"}},
	}}
	oldPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-old"}},
	}}

	gomock.InOrder(
		mockStatusPoller.EXPECT().
			FetchPlayerStatus(ctx).
			Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &url, nil), nil),
		mockDP1.EXPECT().
			ProcessPlaylistURL(ctx, url, false).
			Return(newPlaylist, nil),
		mockKioskReplay.EXPECT().
			SyncPlaylist(ctx, []string{"item-new"}).
			Return(nil),
		mockCDP.EXPECT().
			Send(cdp.METHOD_EVALUATE, gomock.Any()).
			Return(playerNotOkResponse(), nil),
		// Only reached via the deferred revert: FetchPlayerStatus/
		// ProcessPlaylistURL/SyncPlaylist run a SECOND time,
		// independently, after the player rejection above.
		mockStatusPoller.EXPECT().
			FetchPlayerStatus(ctx).
			Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &url, nil), nil),
		mockDP1.EXPECT().
			ProcessPlaylistURL(ctx, url, false).
			Return(oldPlaylist, nil),
		mockKioskReplay.EXPECT().
			SyncPlaylist(ctx, []string{"item-old"}).
			Return(nil),
	)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), mockClock, logger)
	r.Start()
	time.Sleep(150 * time.Millisecond)
	r.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_DynamicPlaylist(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupBackgroundMocks(ts)

	mockPlaylist := createMockPlaylist()

	// Expect status poller to return player status with dynamic playlist
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, mockPlaylist), nil).
		AnyTimes()

	// Expect DP1 to process dynamic playlist
	ts.mockDP1.EXPECT().
		ProcessDynamicPlaylist(ts.ctx, *mockPlaylist, false).
		Return(mockPlaylist, nil).
		AnyTimes()

	// Expect CDP to send the playlist
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_SpecDynamicQueryOnly(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupBackgroundMocks(ts)
	mockPlaylist := createMockPlaylistSpecDynamic()
	assert.True(t, mockPlaylist.HasDynamicContent(), "spec-only dynamicQuery should trigger refresh path")
	// Expect status poller to return player status with spec dynamic playlist (no legacy dynamicQueries)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, mockPlaylist), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().
		ProcessDynamicPlaylist(ts.ctx, *mockPlaylist, false).
		Return(mockPlaylist, nil).
		AnyTimes()
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		AnyTimes()
	ts.refresher.Start()
	time.Sleep(100 * time.Millisecond)
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_NoDynamicQueries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupBackgroundMocks(ts)

	mockPlaylist := createMockPlaylistNoDynamic()

	// Expect status poller to return player status with playlist but no dynamic queries
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, mockPlaylist), nil).
		AnyTimes()

	// Should not call DP1.ProcessDynamicPlaylist since there are no dynamic queries
	// Should not call CDP.Send since there's nothing to process

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

// TestRefresher_ProcessPlayingPlaylist_StaticInlinePlaylistStillSyncsKioskReplayScope
// is the regression test pinning that the static (non-dynamic)
// inline-playlist branch must not return before ever reaching the
// kioskReplay.SyncPlaylist call below it. A static playlist can loop on
// screen indefinitely while a background downloadPlaylistItem/
// downloadPlaylist completes, or a cache gets cleared — the periodic
// refresher is the only path that would otherwise notice that for a
// playlist with nothing dynamic to re-resolve. This must still skip
// ProcessDynamicPlaylist and the CDP resend (the original, intentional
// "do not re-send an unchanged static playlist" behavior).
func TestRefresher_ProcessPlayingPlaylist_StaticInlinePlaylistStillSyncsKioskReplayScope(t *testing.T) {
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().MarkPlaybackChanged().AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()

	mockPlaylist := createMockPlaylistNoDynamic()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, mockPlaylist), nil).
		AnyTimes()
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"item1"}).
		Return(nil).
		MinTimes(1)
	// Neither must ever be called: a static playlist has nothing dynamic
	// to re-resolve, and it must not be re-sent to CDP on every refresh
	// pass (gomock fails the test if either occurs).

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), mockClock, logger)
	r.Start()
	time.Sleep(100 * time.Millisecond)
	r.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_WrongCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupBackgroundMocks(ts)

	// Expect status poller to return player status with wrong command
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_SHUTDOWN), nil, nil), nil).
		AnyTimes()

	// Should not call DP1 or CDP since command is not CMD_DISPLAY_PLAYLIST

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_NoPlaylistData(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// An empty player status is nothing-to-refresh: the startup loop succeeds
	// and settles into the ticker (no retry Sleep).
	setupBackgroundMocks(ts)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, nil), nil).
		AnyTimes()

	// Should not call DP1 or CDP since there's no playlist data

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_NilPlayerStatus(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupBackgroundMocks(ts)

	// Expect status poller to return nil player status
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, nil).
		AnyTimes()

	// Should not call DP1 or CDP since player status is nil

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_StatusPollerError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect status poller to return error
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, errors.New("status poller error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Should not call DP1 or CDP since status poller failed

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_DP1ProcessPlaylistURLError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "http://example.com/playlist.json"

	// Expect status poller to return player status with playlist URL
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()

	// Expect DP1 to fail processing playlist URL
	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(nil, errors.New("dp1 processing error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Should not call CDP since DP1 failed

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_DP1ProcessDynamicPlaylistError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockPlaylist := createMockPlaylist()

	// Expect status poller to return player status with dynamic playlist
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, mockPlaylist), nil).
		AnyTimes()

	// Expect DP1 to fail processing dynamic playlist
	ts.mockDP1.EXPECT().
		ProcessDynamicPlaylist(ts.ctx, *mockPlaylist, false).
		Return(nil, errors.New("dp1 dynamic processing error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Should not call CDP since DP1 failed

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_CDPSendError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	// Expect status poller to return player status with playlist URL
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()

	// Expect DP1 to process playlist URL successfully
	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()

	// Expect CDP to fail sending
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, errors.New("cdp send error")).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_ProcessPlayingPlaylist_InvalidPlayerStatus(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// A displayPlaylist status with no playlist payload is the unconfigured
	// state: treated as success, so the loop settles into the ticker.
	setupBackgroundMocks(ts)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, nil), nil).
		AnyTimes()

	// Should not call DP1 or CDP since there's no valid playlist data

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_SendCDPRequest_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupBackgroundMocks(ts)

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	// Expect status poller to return player status with playlist URL
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()

	// Expect DP1 to process playlist URL
	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()

	// Expect CDP to send the playlist with proper payload structure
	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(method string, params map[string]interface{}) (interface{}, error) {
			// Verify the method
			assert.Equal(t, cdp.METHOD_EVALUATE, method)

			// Verify the expression contains the expected structure
			expression := params["expression"].(string)
			assert.Contains(t, expression, "window.handleCDPRequest")
			assert.Contains(t, expression, "dp1_call")
			assert.Contains(t, expression, "refresh")

			return "success", nil
		}).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_Background_ContextCancellation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a mock ticker with a controllable channel
	mockTicker := mocks.NewMockTicker(ts.ctrl)
	tickerChan := make(chan time.Time, 1)

	// Mock the ticker's C() method to return our controllable channel
	mockTicker.EXPECT().
		C().
		Return(tickerChan).
		AnyTimes()

	// Expect ticker to be stopped exactly twice:
	// 1. Once by the defer statement when the goroutine exits
	// 2. Once explicitly when context is canceled
	mockTicker.EXPECT().
		Stop().
		Times(2)

	// Expect clock to create ticker
	ts.mockClock.EXPECT().
		NewTicker(gomock.Any()).
		Return(mockTicker).
		Times(1)

	// Create a playlist URL for the test
	playlistURL := "https://example.com/playlist.json"

	// Expect status poller to return player status (needed for processPlayingPlaylist to succeed)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()

	// Expect DP1 to process playlist (needed for processPlayingPlaylist to succeed)
	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(createMockPlaylist(), nil).
		AnyTimes()

	// Expect CDP to send request (needed for processPlayingPlaylist to succeed)
	ts.mockCDP.EXPECT().
		Send(gomock.Any(), gomock.Any()).
		Return(nil, nil).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process the initial playlist and create the ticker
	time.Sleep(100 * time.Millisecond)

	// Cancel the context - this should trigger the ticker.Stop() call
	ts.cancel()

	// Give it a moment to process the cancellation
	time.Sleep(50 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

func TestRefresher_Background_DoneChannel(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a mock ticker with a controllable channel
	mockTicker := mocks.NewMockTicker(ts.ctrl)
	tickerChan := make(chan time.Time, 1)

	// Mock the ticker's C() method to return our controllable channel
	mockTicker.EXPECT().
		C().
		Return(tickerChan).
		AnyTimes()

	// Expect ticker to be stopped exactly twice:
	// 1. Once by the defer statement when the goroutine exits
	// 2. Once explicitly when done channel is triggered
	mockTicker.EXPECT().
		Stop().
		Times(2)

	// Expect clock to create ticker
	ts.mockClock.EXPECT().
		NewTicker(gomock.Any()).
		Return(mockTicker).
		Times(1)

	// Create a playlist URL for the test
	playlistURL := "https://example.com/playlist.json"

	// Expect status poller to return player status (needed for processPlayingPlaylist to succeed)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()

	// Expect DP1 to process playlist (needed for processPlayingPlaylist to succeed)
	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(createMockPlaylist(), nil).
		AnyTimes()

	// Expect CDP to send request (needed for processPlayingPlaylist to succeed)
	ts.mockCDP.EXPECT().
		Send(gomock.Any(), gomock.Any()).
		Return(nil, nil).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process the initial playlist and create the ticker
	time.Sleep(100 * time.Millisecond)

	// Stop the refresher (this sends the done signal)
	ts.refresher.Stop()

	// Give it a moment to process the done signal
	time.Sleep(50 * time.Millisecond)
}

// TestRefresher_HeadlessBoot_CDPNotInitialized pins the headless expected-state
// contract: with CDP uninitialized (headless boot, Chromium intentionally not
// running) the refresher must not touch the status poller, DP1, or CDP.Send,
// and must not emit Error-level logs — it just retries quietly.
func TestRefresher_HeadlessBoot_CDPNotInitialized(t *testing.T) {
	core, observed := observer.New(zap.DebugLevel)
	ts := setupWithLogger(t, zap.New(core))
	defer ts.teardown()

	// CDP stays down for the whole test.
	ts.mockCDP.EXPECT().Initialized().Return(false).MinTimes(2)

	// Quiet retry loop: sleep between passes.
	ts.mockClock.EXPECT().
		Sleep(refresher.PLAYER_STATUS_POLLING_INTERVAL).
		AnyTimes()

	// No expectations on FetchPlayerStatus / DP1 / CDP.Send: any such call is an
	// unexpected-call failure, proving the guard short-circuits before them.

	ts.refresher.Start()
	time.Sleep(100 * time.Millisecond)
	ts.refresher.Stop()

	for _, entry := range observed.All() {
		assert.Less(t, entry.Level, zap.ErrorLevel,
			"headless CDP absence must not log at Error level: %s", entry.Message)
	}
}

// TestRefresher_HeadlessBoot_CDPConnectsLater verifies the startup loop keeps
// retrying while CDP is absent and performs the initial refresh as soon as the
// connection appears, rather than giving up or deferring to the 5-minute tick.
func TestRefresher_HeadlessBoot_CDPConnectsLater(t *testing.T) {
	ts := setupWithLogger(t, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	defer ts.teardown()

	setupBackgroundMocks(ts)

	gomock.InOrder(
		ts.mockCDP.EXPECT().Initialized().Return(false).Times(2),
		ts.mockCDP.EXPECT().Initialized().Return(true).AnyTimes(),
	)

	ts.mockClock.EXPECT().
		Sleep(refresher.PLAYER_STATUS_POLLING_INTERVAL).
		Times(2)

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()

	fetched := make(chan struct{}, 1)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		DoAndReturn(func(ctx context.Context) (*status.PlayerStatus, error) {
			select {
			case fetched <- struct{}{}:
			default:
			}
			return createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil
		}).
		AnyTimes()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURL(ts.ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()

	ts.mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		AnyTimes()

	ts.refresher.Start()

	select {
	case <-fetched:
		// Initial refresh ran once CDP reported initialized.
	case <-time.After(2 * time.Second):
		t.Fatal("initial refresh never ran after CDP became initialized")
	}

	ts.refresher.Stop()
}

func TestRefresher_Background_RetryLogic(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect status poller to fail at least 2 times to verify retry logic
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(nil, errors.New("temporary error")).
		MinTimes(2)

	// Expect Sleep to be called during retry logic (once after each failed attempt)
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process and retry
	time.Sleep(500 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

// TestRefresher_ProcessPlayingPlaylist_EmptyPlayerIsNotAnError: a fresh-boot
// player reports displayPlaylist with neither a playlist URL nor an inline
// playlist (nothing assigned yet). That is a normal state, not a failure: no
// Error logs, no DP1/CDP traffic, and the startup loop must settle into the
// refresh ticker instead of spinning at the 5-second polling cadence forever.
func TestRefresher_ProcessPlayingPlaylist_EmptyPlayerIsNotAnError(t *testing.T) {
	core, observed := observer.New(zap.ErrorLevel)
	ts := setupWithLogger(t, zap.New(core))
	defer ts.teardown()
	ts.mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	// Ticker mocks only: no clock.Sleep expectation, so a regression to the
	// error-and-retry path fails the test on the unexpected Sleep call.
	setupBackgroundMocks(ts)

	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, nil), nil).
		AnyTimes()
	// No DP1 or CDP Send expectations: nothing must be processed or sent.

	ts.refresher.Start()
	time.Sleep(100 * time.Millisecond)
	ts.refresher.Stop()

	assert.Zero(t, observed.Len(), "empty player state must not log Error-level entries")
}
