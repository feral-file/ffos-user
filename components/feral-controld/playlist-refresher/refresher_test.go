package refresher_test

import (
	"context"
	"errors"
	"sync"
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

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
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
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx, cancel := context.WithCancel(context.Background())

	// Dependencies
	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockClock := mocks.NewMockClock(ctrl)

	refresher := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, mockClock, logger)

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
		Ok:          true,
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
		ProcessPlaylistURLConditional(ts.ctx, playlistURL, false, gomock.Any()).
		Return(&dp1.PlaylistURLResult{NotModified: false, ETag: `"t1"`, Playlist: mockPlaylist}, nil).
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

func TestRefresher_ProcessPlayingPlaylist_PlaylistURL_NotModified_NoCDP(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	setupBackgroundMocks(ts)

	playlistURL := "http://example.com/playlist.json"

	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()

	ts.mockDP1.EXPECT().
		ProcessPlaylistURLConditional(ts.ctx, playlistURL, false, gomock.Any()).
		Return(&dp1.PlaylistURLResult{NotModified: true}, nil).
		AnyTimes()

	ts.refresher.Start()
	time.Sleep(100 * time.Millisecond)
	ts.refresher.Stop()
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

func TestRefresher_ProcessPlayingPlaylist_WrongCommand(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Expect status poller to return player status with wrong command
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_SHUTDOWN), nil, nil), nil).
		AnyTimes()

	// No playlist URL or embedded playlist: processPlayingPlaylist errors and retries (refresher
	// does not branch on castCommand).
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
		AnyTimes()

	// Should not call DP1 or CDP

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

	// Expect status poller to return player status with no playlist data
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, nil), nil).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
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
		ProcessPlaylistURLConditional(ts.ctx, playlistURL, false, gomock.Any()).
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
		ProcessPlaylistURLConditional(ts.ctx, playlistURL, false, gomock.Any()).
		Return(&dp1.PlaylistURLResult{NotModified: false, ETag: `"t1"`, Playlist: mockPlaylist}, nil).
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

	// Test with invalid player status (no playlist URL or playlist)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), nil, nil), nil).
		AnyTimes()

	// Expect Sleep to be called during retry logic
	ts.mockClock.EXPECT().
		Sleep(gomock.Any()).
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
		ProcessPlaylistURLConditional(ts.ctx, playlistURL, false, gomock.Any()).
		Return(&dp1.PlaylistURLResult{NotModified: false, ETag: `"t1"`, Playlist: mockPlaylist}, nil).
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
		ProcessPlaylistURLConditional(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&dp1.PlaylistURLResult{NotModified: false, ETag: `"t1"`, Playlist: createMockPlaylist()}, nil).
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
		ProcessPlaylistURLConditional(gomock.Any(), gomock.Any(), gomock.Any(), gomock.Any()).
		Return(&dp1.PlaylistURLResult{NotModified: false, ETag: `"t1"`, Playlist: createMockPlaylist()}, nil).
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
