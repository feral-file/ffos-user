package refresher_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strings"
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
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
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

	refresher := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), nil, mockClock, logger)

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

// playerOKResponse is the CDP body the player returns on an accepted refresh.
func playerOKResponse() map[string]interface{} {
	return map[string]interface{}{"message": map[string]interface{}{"ok": true}}
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
		SleepContext(gomock.Any(), gomock.Any()).
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
		SleepContext(gomock.Any(), gomock.Any()).
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
		SleepContext(gomock.Any(), gomock.Any()).
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

func TestRefresher_StopCancelsInitialRetryLoop(t *testing.T) {
	ts := setupWithLogger(t, zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	defer ts.teardown()

	ts.mockCDP.EXPECT().Initialized().Return(false).AnyTimes()

	enteredSleep := make(chan struct{})
	sleepReturned := make(chan struct{})
	var enteredOnce sync.Once
	var returnedOnce sync.Once
	ts.mockClock.EXPECT().
		SleepContext(gomock.Any(), refresher.PLAYER_STATUS_POLLING_INTERVAL).
		DoAndReturn(func(ctx context.Context, _ time.Duration) error {
			enteredOnce.Do(func() { close(enteredSleep) })
			<-ctx.Done()
			returnedOnce.Do(func() { close(sleepReturned) })
			return ctx.Err()
		}).
		AnyTimes()

	ts.refresher.Start()
	select {
	case <-enteredSleep:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher did not enter initial retry sleep")
	}

	ts.refresher.Stop()
	select {
	case <-sleepReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop did not cancel initial retry sleep")
	}
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
		SleepContext(gomock.Any(), gomock.Any()).
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
		SleepContext(gomock.Any(), gomock.Any()).
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
			return playerOKResponse(), nil
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
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

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
		SyncPlaylist(ctx, []string{"http://example.com/video1.mp4"}).
		Return(nil).
		MinTimes(1)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), nil, mockClock, logger)
	r.Start()
	time.Sleep(100 * time.Millisecond)
	r.Stop()
}

// TestRefresher_ProcessPlayingPlaylist_SkipsScopeSyncWhenAuthorityChangesMidResolution
// is the regression test for the scope-overwrite hole the refresher's
// authority guard closes (feral-file/ffos-user#229 review finding): the
// pass samples the scheduler's AuthorityToken, then does slow URL/dynamic
// resolution; if a scheduler-owned cast lands in that window, the send is
// already skipped on the token mismatch — but the scope sync used to run
// anyway, overwriting (under the same playback lock, after it ran) the
// scope that cast just installed, and leaving interception pointed at the
// previous playlist until the next pass. The guard must skip BOTH.
//
// The interleaving is deterministic: the DP1 resolution mock — the exact
// step the token window spans — stands in for the concurrent cast by
// bumping playlist authority (a real scheduler's PrepareWithSource)
// before returning. The strict kiosk-replay mock then pins the fix: any
// SyncPlaylist (or CDP send, also unexpected) fails the test, while the
// MinTimes(1) resolution expectation proves passes genuinely ran into
// the guard rather than short-circuiting earlier.
func TestRefresher_ProcessPlayingPlaylist_SkipsScopeSyncWhenAuthorityChangesMidResolution(t *testing.T) {
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
	// No SyncPlaylist / MarkPlaybackChanged expectations: either call is an
	// unexpected mock invocation and fails the test.
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	// No mockCDP.Send expectation either: the pass must not reach the send.

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()
	mockClock.EXPECT().Now().Return(time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)).AnyTimes()

	sched := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC }, logger)

	playlistURL := "http://example.com/playlist.json"
	mockPlaylist := createMockPlaylist()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		DoAndReturn(func(context.Context, string, bool) (*dp1.Playlist, error) {
			// The concurrent scheduler-owned cast: bumps AuthorityToken
			// exactly inside the pass's sample→guard window.
			sched.PrepareWithSource(mockPlaylist, playlistschedule.Source{})
			return mockPlaylist, nil
		}).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), sched, mockClock, logger)
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
	// playerresponse.OK treats as NOT ok (fail-closed),
	// so every pass here also exercises the corrective-resync path
	// below; PlaybackGeneration just needs to answer, not assert
	// anything about it.
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	mockTicker := mocks.NewMockTicker(ctrl)
	mockTicker.EXPECT().C().Return(make(chan time.Time, 1)).AnyTimes()
	mockTicker.EXPECT().Stop().AnyTimes()
	mockClock.EXPECT().NewTicker(gomock.Any()).Return(mockTicker).AnyTimes()
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

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
		SyncPlaylist(ctx, []string{"http://example.com/video1.mp4"}).
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
		Return(map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), nil, mockClock, logger)
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
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

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
		Return(map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil).
		AnyTimes()

	syncCount := make(chan struct{}, 8)
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"http://example.com/video1.mp4"}).
		DoAndReturn(func(context.Context, []string) error {
			syncCount <- struct{}{}
			return nil
		}).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), nil, mockClock, logger)
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
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

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
		SyncPlaylist(ctx, []string{"http://example.com/video1.mp4"}).
		Return(nil).
		MinTimes(1)
	// The device is offline (live resolution just failed), but the
	// cached copy must still be pushed to the player, exactly as a live
	// success would be.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, mockOfflineCache, wrapper.NewJSON(), nil, mockClock, logger)
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
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

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
	// This pass deliberately fails, so the background loop reaches develop's
	// retry pacing before the first success.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, mockOfflineCache, wrapper.NewJSON(), nil, mockClock, logger)
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
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

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
		SyncPlaylist(ctx, []string{"http://example.com/video1.mp4"}).
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
		Return(map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), nil, mockClock, logger)
	r.Start()
	time.Sleep(100 * time.Millisecond)
	r.Stop()
}

// playerNotOkResponse mirrors commandrouter/handler_test.go's own helper of
// the same name (kept as an independent copy — see
// playerresponse.OK's doc for why commandrouter and
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
	// This pass deliberately fails, so the background loop reaches develop's
	// retry pacing before the first success.
	// Terminal: this pass deliberately fails, and the revert under test
	// has already run by the time the loop reaches its retry pacing. Ending
	// the loop here keeps the single-pass mock expectations below exact
	// instead of letting a zero-delay retry spin past them.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(context.Canceled).AnyTimes()

	url := "http://example.com/playlist.json"
	newPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-new", Source: "https://example.com/item-new"}},
	}}
	oldPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-old", Source: "https://example.com/item-old"}},
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
		SyncPlaylist(ctx, []string{"https://example.com/item-new"}).
		Return(nil).
		MinTimes(1)
	reverted := make(chan struct{}, 8)
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"https://example.com/item-old"}).
		DoAndReturn(func(context.Context, []string) error {
			select {
			case reverted <- struct{}{}:
			default:
			}
			return nil
		}).
		MinTimes(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), nil, mockClock, logger)
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
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	// Terminal: this pass deliberately fails, and the revert under test
	// has already run by the time the loop reaches its retry pacing. Ending
	// the loop here keeps the single-pass mock expectations below exact
	// instead of letting a zero-delay retry spin past them.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(context.Canceled).AnyTimes()

	url := "http://example.com/playlist.json"
	newPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-new", Source: "https://example.com/item-new"}},
	}}
	oldPlaylist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Items: []dp1playlist.PlaylistItem{{ID: "item-old", Source: "https://example.com/item-old"}},
	}}

	gomock.InOrder(
		mockStatusPoller.EXPECT().
			FetchPlayerStatus(ctx).
			Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &url, nil), nil),
		mockDP1.EXPECT().
			ProcessPlaylistURL(ctx, url, false).
			Return(newPlaylist, nil),
		mockKioskReplay.EXPECT().
			SyncPlaylist(ctx, []string{"https://example.com/item-new"}).
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
			SyncPlaylist(ctx, []string{"https://example.com/item-old"}).
			Return(nil),
	)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), nil, mockClock, logger)
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
		Return(playerOKResponse(), nil).
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
		Return(playerOKResponse(), nil).
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
	// develop's retry pacing before the first successful pass: these tests
	// exercise a single pass, so the sleep is incidental and simply allowed.
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	mockPlaylist := createMockPlaylistNoDynamic()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, mockPlaylist), nil).
		AnyTimes()
	mockKioskReplay.EXPECT().
		SyncPlaylist(ctx, []string{"http://example.com/video1.mp4"}).
		Return(nil).
		MinTimes(1)
	// Neither must ever be called: a static playlist has nothing dynamic
	// to re-resolve, and it must not be re-sent to CDP on every refresh
	// pass (gomock fails the test if either occurs).

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockKioskReplay, nil, wrapper.NewJSON(), nil, mockClock, logger)
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
		SleepContext(gomock.Any(), gomock.Any()).
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
		SleepContext(gomock.Any(), gomock.Any()).
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
		SleepContext(gomock.Any(), gomock.Any()).
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
		SleepContext(gomock.Any(), gomock.Any()).
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

			return playerOKResponse(), nil
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
		Return(playerOKResponse(), nil).
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
		Return(playerOKResponse(), nil).
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
		SleepContext(gomock.Any(), refresher.PLAYER_STATUS_POLLING_INTERVAL).
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
		SleepContext(gomock.Any(), refresher.PLAYER_STATUS_POLLING_INTERVAL).
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
		Return(playerOKResponse(), nil).
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
		SleepContext(gomock.Any(), gomock.Any()).
		AnyTimes()

	// Start the refresher
	ts.refresher.Start()

	// Give it a moment to process and retry
	time.Sleep(500 * time.Millisecond)

	// Stop the refresher
	ts.refresher.Stop()
}

type fakePlaylistScheduler struct {
	mu              sync.Mutex
	hasCache        bool
	restoredPending bool
	cacheOnPrepare  bool
	token           uint64
	recomputes      int
	resumePersisted int
	prepares        int
	pushCalls       int
	restores        int
	commits         int
	source          playlistschedule.Source
	restored        chan struct{}
}

func (f *fakePlaylistScheduler) Prepare(playlist *dp1.Playlist) *dp1.Playlist {
	return f.PrepareWithSource(playlist, playlistschedule.Source{})
}
func (f *fakePlaylistScheduler) PrepareWithSource(playlist *dp1.Playlist, source playlistschedule.Source) *dp1.Playlist {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prepares++
	f.token++
	f.source = source
	if f.cacheOnPrepare {
		f.hasCache = true
	}
	return playlist
}
func (f *fakePlaylistScheduler) RecomputeNow(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.recomputes++
}
func (f *fakePlaylistScheduler) ResumePersisted(context.Context) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.resumePersisted++
	f.recomputes++
	f.restoredPending = false
	f.token++
}
func (f *fakePlaylistScheduler) Clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hasCache = false
	f.token++
}
func (f *fakePlaylistScheduler) ClearThenWithPlayerPush(fn func() bool) {
	f.mu.Lock()
	f.hasCache = false
	f.token++
	f.mu.Unlock()
	fn()
}
func (f *fakePlaylistScheduler) WithPlayerPush(fn func()) {
	f.mu.Lock()
	f.pushCalls++
	f.mu.Unlock()
	fn()
}
func (f *fakePlaylistScheduler) AuthorityToken() uint64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.token
}
func (f *fakePlaylistScheduler) Commit() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.commits++
}
func (f *fakePlaylistScheduler) AdvanceAuthority() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.token++
}
func (f *fakePlaylistScheduler) Snapshot() playlistschedule.Snapshot {
	return playlistschedule.Snapshot{}
}
func (f *fakePlaylistScheduler) Restore(playlistschedule.Snapshot) {
	f.mu.Lock()
	f.restores++
	f.token++
	restored := f.restored
	f.mu.Unlock()
	if restored != nil {
		select {
		case restored <- struct{}{}:
		default:
		}
	}
}
func (f *fakePlaylistScheduler) HasCache() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.hasCache
}
func (f *fakePlaylistScheduler) RestoredPending() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restoredPending
}
func (f *fakePlaylistScheduler) SourceMatches(source playlistschedule.Source) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.source.Matches(source)
}
func (f *fakePlaylistScheduler) Source() playlistschedule.Source {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.source
}
func (f *fakePlaylistScheduler) Stop() { f.Clear() }
func (f *fakePlaylistScheduler) preparesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.prepares
}
func (f *fakePlaylistScheduler) pushCallsCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pushCalls
}
func (f *fakePlaylistScheduler) recomputesCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.recomputes
}
func (f *fakePlaylistScheduler) resumePersistedCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.resumePersisted
}
func (f *fakePlaylistScheduler) restoresCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restores
}

func TestRefresher_TransientURLError_RecomputesFromDisplayAtCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	playlistURL := "https://example.com/daily.json"
	fakeSched := &fakePlaylistScheduler{
		hasCache: true,
		source:   playlistschedule.Source{PlaylistURL: playlistURL},
	}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(nil, &url.Error{Op: "Get", URL: playlistURL, Err: &net.DNSError{Name: "example.com", IsTemporary: true}}).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	assert.GreaterOrEqual(t, fakeSched.recomputesCount(), 1, "transient fetch should recompute from cache")
}

func TestRefresher_SchedulerOwnedFutureSourceDoesNotUseStalePlayerStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	futureURL := "https://example.com/future.json"
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(timerCtx context.Context, _ time.Duration) error {
			<-timerCtx.Done()
			return timerCtx.Err()
		},
	).AnyTimes()
	scheduler := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC },
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	// Seed B exactly as the accepted cast path does. Its first item is future-only,
	// so the player remains on stale A and the scheduler owns B's source.
	require.Empty(t, scheduler.PrepareWithSource(futureOnlyPlaylist(now.Add(time.Hour)), playlistschedule.Source{PlaylistURL: futureURL}).Items)

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	// The accepted future-only schedule is B, while the player is still
	// displaying A. Its status must not be consulted: B is scheduler-owned
	// authority until a newer cast/default replaces it.
	mockStatusPoller.EXPECT().FetchPlayerStatus(gomock.Any()).Times(0)
	resolved := make(chan struct{}, 1)
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, futureURL, false).
		DoAndReturn(func(context.Context, string, bool) (*dp1.Playlist, error) {
			select {
			case resolved <- struct{}{}:
			default:
			}
			return futureOnlyPlaylist(now.Add(time.Hour)), nil
		}).
		AnyTimes()
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), scheduler, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	select {
	case <-resolved:
	case <-time.After(time.Second):
		t.Fatal("scheduler-owned future source was not refreshed")
	}
	require.Eventually(t, func() bool {
		return scheduler.HasCache() && scheduler.Source().PlaylistURL == futureURL
	}, time.Second, 10*time.Millisecond)
	r.Stop()
	scheduler.Stop()

	assert.Equal(t, futureURL, scheduler.Source().PlaylistURL, "stale player status must not replace the future schedule")
}

func TestRefresher_MalformedPlaylistError_DoesNotUseDisplayAtCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(nil, errors.New("invalid character 'x' looking for beginning of value")).
		AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	assert.Equal(t, 0, fakeSched.recomputesCount(), "data/parse errors must not degrade to cache")
}

func TestRefresher_TransientURLError_WithoutCache_SurfacesError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: false}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(nil, &url.Error{Op: "Get", URL: playlistURL, Err: &net.DNSError{Name: "example.com", IsTemporary: true}}).
		AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	assert.Equal(t, 0, fakeSched.recomputesCount(), "without cache, transient errors must not recompute")
}

func TestRefresher_TransientURLError_RestoredPendingDoesNotResumeCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	playlistURL := "https://example.com/plain.json"
	fakeSched := &fakePlaylistScheduler{
		hasCache:        true,
		restoredPending: true,
		source:          playlistschedule.Source{PlaylistURL: playlistURL},
	}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(nil, &url.Error{Op: "Get", URL: playlistURL, Err: &net.DNSError{Name: "example.com", IsTemporary: true}}).
		AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(200 * time.Millisecond)
	r.Stop()

	assert.Equal(t, 0, fakeSched.resumePersistedCount(), "pending source must wait for a successful fetch before scheduler resumes")
	assert.Equal(t, 0, fakeSched.recomputesCount(), "fetch failure should leave current player artwork unchanged")
}

// TestRefresher_StartupLoop_EscalatesToForceCastAfterRepeatedPlayerRejection
// pins the fix for a soft refresh that is permanently rejected because the
// player has no active playlist to refresh, while the scheduler cache is
// already warm. PrepareWithSource's own force-cast escape
// (scheduler.HasCache() && (!hadDisplayAtCache || hadRestoredPending)) does
// not fire in that state (the cache was warm both before and after
// PrepareWithSource), so without escalation the startup loop would retry the
// same rejected soft refresh at PLAYER_STATUS_POLLING_INTERVAL forever
// instead of ever reaching the ticker.
func TestRefresher_StartupLoop_EscalatesToForceCastAfterRepeatedPlayerRejection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	playlistURL := "https://example.com/daily.json"
	mockPlaylist := createMockPlaylist()
	fakeSched := &fakePlaylistScheduler{
		hasCache: true,
		source:   playlistschedule.Source{PlaylistURL: playlistURL},
	}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	mockStatusPoller.EXPECT().FetchPlayerStatus(gomock.Any()).Times(0)
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(mockPlaylist, nil).
		AnyTimes()

	mockClock.EXPECT().
		SleepContext(gomock.Any(), refresher.PLAYER_STATUS_POLLING_INTERVAL).
		Return(nil).
		AnyTimes()

	var rejectedSends int32
	forceCastSucceeded := make(chan struct{})
	var forceCastOnce sync.Once
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr := params["expression"].(string)
			if strings.Contains(expr, `"action":"now_display"`) {
				forceCastOnce.Do(func() { close(forceCastSucceeded) })
				return map[string]interface{}{"ok": true}, nil
			}
			atomic.AddInt32(&rejectedSends, 1)
			return map[string]interface{}{"ok": false}, nil
		}).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()

	select {
	case <-forceCastSucceeded:
	case <-time.After(2 * time.Second):
		t.Fatal("expected the startup loop to escalate to a forced now_display cast")
	}
	r.Stop()

	assert.GreaterOrEqual(t, atomic.LoadInt32(&rejectedSends), int32(2),
		"expected repeated rejected soft refreshes before the loop escalated to force-cast")
}

func TestRefresher_URLRefresh_PreparesAndUsesWithPlayerPush(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: false}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	pl := createMockPlaylist()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(pl, nil).
		AnyTimes()
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.GreaterOrEqual(t, fakeSched.preparesCount(), 1, "refresh must call Prepare")
	assert.GreaterOrEqual(t, fakeSched.pushCallsCount(), 1, "refresh must send via WithPlayerPush")
}

func TestRefresher_URLRefresh_SkipsObsoleteResultAfterAuthorityChange(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	pl := createMockPlaylist()
	resolveStarted := make(chan struct{})
	releaseResolve := make(chan struct{})
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		DoAndReturn(func(context.Context, string, bool) (*dp1.Playlist, error) {
			select {
			case <-resolveStarted:
			default:
				close(resolveStarted)
			}
			<-releaseResolve
			return pl, nil
		}).
		AnyTimes()
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	<-resolveStarted

	// Simulate a newer accepted cast or displayDefaultPlaylist while the old
	// refresh is still resolving. Once it gets the push lock it must discard
	// the obsolete result instead of sending it to the player.
	fakeSched.AdvanceAuthority()
	close(releaseResolve)

	time.Sleep(100 * time.Millisecond)
	r.Stop()

	assert.Zero(t, fakeSched.preparesCount(), "obsolete refresh must not replace scheduler cache")
	assert.GreaterOrEqual(t, fakeSched.pushCallsCount(), 1, "obsolete refresh must re-check under the push lock")
}

func TestRefresher_URLRefresh_SkipsObsoleteStatusAfterAuthorityChange(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	statusReadStarted := make(chan struct{})
	releaseStatusRead := make(chan struct{})
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		DoAndReturn(func(context.Context) (*status.PlayerStatus, error) {
			select {
			case <-statusReadStarted:
			default:
				close(statusReadStarted)
			}
			<-releaseStatusRead
			return createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil
		}).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(createMockPlaylist(), nil).
		AnyTimes()
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	<-statusReadStarted

	// The status response was requested before this newer cast/default took
	// authority. The refresh result must be discarded even if URL resolution
	// itself happens after the newer authority is visible.
	fakeSched.AdvanceAuthority()
	close(releaseStatusRead)

	time.Sleep(100 * time.Millisecond)
	r.Stop()

	assert.Zero(t, fakeSched.preparesCount(), "obsolete status snapshot must not replace scheduler cache")
	assert.GreaterOrEqual(t, fakeSched.pushCallsCount(), 1, "obsolete status must re-check under the push lock")
}

func TestRefresher_DisplayAtCacheRebuild_ForceCasts(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{cacheOnPrepare: true, restored: make(chan struct{}, 1)}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	pl := createMockPlaylist()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(pl, nil).
		AnyTimes()
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.Contains(t, expr, `"action":"now_display"`)
			assert.Contains(t, expr, `"playlistUrl":"`+playlistURL+`"`)
			assert.NotContains(t, expr, `"refresh":true`)
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.GreaterOrEqual(t, fakeSched.preparesCount(), 1, "refresh must rebuild scheduler cache")
	assert.GreaterOrEqual(t, fakeSched.pushCallsCount(), 1, "refresh must still serialize the send")
}

func TestRefresher_DisplayAtRestoredPending_ForceCastsFirstValidatedRefresh(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	boundary := now.Add(time.Minute)
	var clockMu sync.Mutex
	clockNow := now
	timerWake := make(chan struct{})
	mockClock.EXPECT().Now().DoAndReturn(func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clockNow
	}).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(timerCtx context.Context, _ time.Duration) error {
			select {
			case <-timerWake:
				return nil
			case <-timerCtx.Done():
				return timerCtx.Err()
			}
		},
	).AnyTimes()

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	persistedURL := "https://example.com/persisted-b.json"
	store := &sourceStore{source: playlistschedule.Source{PlaylistURL: persistedURL}}
	scheduler := playlistschedule.NewWithStore(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC }, store,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	require.True(t, scheduler.RestoredPending())
	require.False(t, scheduler.HasCache(), "restored startup state has a source but no in-memory full playlist")
	// On restart the player can still report stale A. The persisted B source
	// must be selected before status is read, so A cannot replace it.
	mockStatusPoller.EXPECT().FetchPlayerStatus(gomock.Any()).Times(0)
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, persistedURL, false).
		Return(futureOnlyPlaylist(boundary), nil).
		AnyTimes()
	pushed := make(chan struct{}, 1)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			assert.Contains(t, expr, `"action":"now_display"`)
			assert.Contains(t, expr, `"playlistUrl":"`+persistedURL+`"`)
			assert.NotContains(t, expr, `"refresh":true`)
			pushed <- struct{}{}
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).
		Times(1)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), scheduler, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	require.Eventually(t, func() bool {
		return scheduler.HasCache() && !scheduler.RestoredPending()
	}, time.Second, 10*time.Millisecond)
	clockMu.Lock()
	clockNow = boundary
	clockMu.Unlock()
	close(timerWake)
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		t.Fatal("restored future-only schedule was not pushed at its displayAt boundary")
	}
	r.Stop()
	scheduler.Stop()

	assert.Equal(t, persistedURL, scheduler.Source().PlaylistURL, "restart recovery must keep the persisted schedule armed")
}

func futureOnlyPlaylist(displayAt time.Time) *dp1.Playlist {
	value := displayAt.UTC().Format(time.RFC3339)
	return &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{
		ID:        "future-b",
		Title:     "Future B",
		Source:    "https://example.com/future-b.html",
		DisplayAt: &value,
	}}}}
}

type sourceStore struct {
	source playlistschedule.Source
}

func (s *sourceStore) Load() (*playlistschedule.Source, error) {
	if s.source.IsZero() {
		return nil, nil
	}
	source := s.source
	return &source, nil
}

func (s *sourceStore) Save(source playlistschedule.Source) error {
	s.source = source
	return nil
}

func (s *sourceStore) Clear() error {
	s.source = playlistschedule.Source{}
	return nil
}

func TestRefresher_StaticInlineDisplayAt_DoesNotOverwriteSchedulerCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: true, cacheOnPrepare: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	displayAt := "2026-07-22T00:00:00Z"
	pl := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Daily",
			Items: []dp1playlist.PlaylistItem{
				{
					ID:        "active",
					Title:     "Active",
					Source:    "https://example.com/active.html",
					DisplayAt: &displayAt,
				},
			},
		},
	}
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, pl), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessDynamicPlaylist(gomock.Any(), gomock.Any(), gomock.Any()).
		Times(0)
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.Zero(t, fakeSched.preparesCount(), "player status only has the filtered active set and must not replace the full cache")
	assert.Zero(t, fakeSched.resumePersistedCount(), "steady-state inline displayAt status must not force-cast on every refresh tick")
	assert.Zero(t, fakeSched.pushCallsCount(), "static inline status must not trigger a refresher send")
}

func TestRefresher_StaticInlineRestoredPending_DoesNotResumeWithoutPlayerIdentity(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: true, restoredPending: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	displayAt := "2026-07-22T00:00:00Z"
	pl := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Daily",
			Items: []dp1playlist.PlaylistItem{
				{
					ID:        "active",
					Title:     "Active",
					Source:    "https://example.com/active.html",
					DisplayAt: &displayAt,
				},
			},
		},
	}
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, pl), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessDynamicPlaylist(gomock.Any(), gomock.Any(), gomock.Any()).
		Times(0)
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.Zero(t, fakeSched.preparesCount(), "player status only has the filtered active set and must not replace the full cache")
	assert.Zero(t, fakeSched.resumePersistedCount(), "static inline status has no refreshable source identity and must not validate persisted scheduler ownership")
	assert.Zero(t, fakeSched.pushCallsCount(), "static inline status must not trigger a refresher send")
}

func TestRefresher_StaticInlineRestoredPending_DoesNotResumeEmptyActiveSet(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: true, restoredPending: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	emptyActive := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Daily",
			Items: nil,
		},
	}
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, emptyActive), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessDynamicPlaylist(gomock.Any(), gomock.Any(), gomock.Any()).
		Times(0)
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.Zero(t, fakeSched.preparesCount(), "empty active status cannot rebuild scheduler state from persisted source")
	assert.Zero(t, fakeSched.resumePersistedCount(), "empty active status has no refreshable source identity and must not validate persisted scheduler ownership")
	assert.Zero(t, fakeSched.pushCallsCount(), "static inline status must not trigger a refresher send")
}

func TestRefresher_StaticInlineRestoredPending_DoesNotOverwriteNewerAcceptedCast(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: true, restoredPending: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	displayAt := "2026-07-22T00:00:00Z"
	newerActive := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Newer accepted cast",
			Items: []dp1playlist.PlaylistItem{
				{
					ID:        "newer-active",
					Title:     "Newer Active",
					Source:    "https://example.com/newer.html",
					DisplayAt: &displayAt,
				},
			},
		},
	}
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, newerActive), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessDynamicPlaylist(gomock.Any(), gomock.Any(), gomock.Any()).
		Times(0)
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.Zero(t, fakeSched.preparesCount(), "filtered static status cannot rebuild scheduler state from persisted source")
	assert.Zero(t, fakeSched.resumePersistedCount(), "persisted source must not be force-cast over a newer accepted static cast")
	assert.Zero(t, fakeSched.pushCallsCount())
}

func TestRefresher_StaticInlineRestoredPending_IgnoresNonScheduledStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: true, restoredPending: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	plain := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			Title: "Plain",
			Items: []dp1playlist.PlaylistItem{
				{
					ID:     "plain",
					Title:  "Plain",
					Source: "https://example.com/plain.html",
				},
			},
		},
	}
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, plain), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessDynamicPlaylist(gomock.Any(), gomock.Any(), gomock.Any()).
		Times(0)
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).Times(0)

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.Zero(t, fakeSched.preparesCount(), "static player status cannot rebuild scheduler state from persisted source")
	assert.Zero(t, fakeSched.resumePersistedCount(), "non-scheduled status must not validate persisted schedule ownership")
	assert.Zero(t, fakeSched.pushCallsCount())
}

func TestRefresher_PrepareSendFailure_RestoresSchedulerCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{cacheOnPrepare: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	pl := createMockPlaylist()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(pl, nil).
		AnyTimes()
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(nil, assert.AnError).
		AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.GreaterOrEqual(t, fakeSched.restoresCount(), 1, "failed send must restore scheduler snapshot")
}

func TestRefresher_PlayerReject_RestoresSchedulerCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{cacheOnPrepare: true, restored: make(chan struct{}, 1)}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	// Reject must stay on the startup fast-retry loop (Sleep), not settle into
	// the 5m ticker as if the pass succeeded.
	mockClock.EXPECT().
		SleepContext(gomock.Any(), refresher.PLAYER_STATUS_POLLING_INTERVAL).
		MinTimes(1)
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	pl := createMockPlaylist()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(pl, nil).
		AnyTimes()
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]interface{}{"message": map[string]interface{}{"ok": false}}, nil).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	select {
	case <-fakeSched.restored:
	case <-time.After(2 * time.Second):
		t.Fatal("player rejection did not restore the scheduler cache")
	}
	r.Stop()

	assert.GreaterOrEqual(t, fakeSched.restoresCount(), 1, "player rejection must restore scheduler snapshot")
}

func TestRefresher_MalformedPlayerResponse_RestoresSchedulerCache(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{cacheOnPrepare: true}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockClock.EXPECT().
		SleepContext(gomock.Any(), refresher.PLAYER_STATUS_POLLING_INTERVAL).
		MinTimes(1)
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	pl := createMockPlaylist()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		ProcessPlaylistURL(ctx, playlistURL, false).
		Return(pl, nil).
		AnyTimes()
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return("success", nil).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	assert.GreaterOrEqual(t, fakeSched.restoresCount(), 1, "missing ok must restore scheduler snapshot")
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
