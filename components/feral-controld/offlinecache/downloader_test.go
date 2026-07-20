package offlinecache_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// downloaderTestSetup exercises Downloader against real time (wrapper.NewClock)
// with a short idleTeardown so idle-teardown scheduling can be observed
// within a fast test, and against mocked Exec/HTTPClient since spawning a
// real Chromium binary is neither available nor desirable in unit tests.
type downloaderTestSetup struct {
	ctrl       *gomock.Controller
	mockExec   *mocks.MockExec
	mockOS     *mocks.MockOS
	mockHTTP   *mocks.MockHTTPClient
	downloader offlinecache.Downloader
	// lastStartArgs records the Chromium CLI args passed to the most
	// recent CommandContext call, so tests can assert on flags (e.g.
	// TestDownloader_Start_EnablesGPUAndMatchesKioskAutoplay) without
	// every setupDownloader caller needing its own capture plumbing.
	lastStartArgs []string
}

func setupDownloader(t *testing.T, idleTeardown time.Duration) *downloaderTestSetup {
	ctrl := gomock.NewController(t)
	mockExec := mocks.NewMockExec(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	logger := zaptest.NewLogger(t)

	mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	d := offlinecache.NewDownloader(
		"/usr/bin/chromium", "/tmp/offline-cache-headless", 9223,
		idleTeardown, mockExec, mockOS, wrapper.NewClock(), mockHTTP, logger,
	)

	return &downloaderTestSetup{ctrl: ctrl, mockExec: mockExec, mockOS: mockOS, mockHTTP: mockHTTP, downloader: d}
}

// expectSuccessfulStart wires one CommandContext+Start+Wait cycle and a
// /json/version probe that answers 200 immediately, returning the process
// context passed to CommandContext so tests can assert it is canceled on
// teardown.
func (ts *downloaderTestSetup) expectSuccessfulStart(t *testing.T) (procCtx *context.Context) {
	t.Helper()
	var captured context.Context
	mockCmd := mocks.NewMockExecCmd(ts.ctrl)
	mockCmd.EXPECT().Start().Return(nil).Times(1)
	// AnyTimes (rather than exactly once): production code always calls
	// Wait() on the reaper goroutine it spawns, but that goroutine's
	// completion is not synchronized with test teardown, so asserting an
	// exact call count would race gomock's ctrl.Finish() against it.
	mockCmd.EXPECT().Wait().DoAndReturn(func() error {
		<-captured.Done()
		return context.Canceled
	}).AnyTimes()

	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "/usr/bin/chromium", gomock.Any()).
		DoAndReturn(func(ctx context.Context, name string, args ...string) wrapper.ExecCmd {
			captured = ctx
			ts.lastStartArgs = args
			return mockCmd
		}).Times(1)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil)
	require.NoError(t, err)
	ts.mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil).Return(req, nil).AnyTimes()
	ts.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil).Times(1)

	return &captured
}

func TestDownloader_Acquire_StartsChromiumAndReturnsEndpoint(t *testing.T) {
	ts := setupDownloader(t, time.Hour)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	ts.expectSuccessfulStart(t)

	endpoint, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9223", endpoint)
}

// TestDownloader_Start_EnablesGPUAndMatchesKioskAutoplay pins the launch
// flags that keep headless capture's rendering behavior aligned with the
// kiosk Chromium (users/feralfile/scripts/start-kiosk.sh). Reintroducing
// --disable-gpu here would silently break WebGL/canvas artwork capture:
// see the rationale in downloader.go's start().
func TestDownloader_Start_EnablesGPUAndMatchesKioskAutoplay(t *testing.T) {
	ts := setupDownloader(t, time.Hour)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	ts.expectSuccessfulStart(t)

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	assert.NotContains(t, ts.lastStartArgs, "--disable-gpu")
	assert.Contains(t, ts.lastStartArgs, "--ignore-gpu-blocklist")
	assert.Contains(t, ts.lastStartArgs, "--enable-gpu-rasterization")
	assert.Contains(t, ts.lastStartArgs, "--autoplay-policy=no-user-gesture-required")
}

func TestDownloader_Acquire_ReusesRunningProcess(t *testing.T) {
	ts := setupDownloader(t, time.Hour)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	ts.expectSuccessfulStart(t) // exactly once: the second Acquire must not restart Chromium

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	ts.downloader.Release()

	endpoint, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9223", endpoint)
}

func TestDownloader_Acquire_SerializesJobs(t *testing.T) {
	ts := setupDownloader(t, time.Hour)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	ts.expectSuccessfulStart(t)

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	var wg sync.WaitGroup
	acquired := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, err := ts.downloader.Acquire(context.Background())
		assert.NoError(t, err)
		close(acquired)
	}()

	select {
	case <-acquired:
		t.Fatal("second Acquire must not succeed while the first job holds the slot")
	case <-time.After(100 * time.Millisecond):
	}

	ts.downloader.Release()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire did not proceed after Release")
	}
	wg.Wait()
}

func TestDownloader_Release_TearsDownAfterIdleTimeout(t *testing.T) {
	ts := setupDownloader(t, 50*time.Millisecond)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	procCtx := ts.expectSuccessfulStart(t)

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	ts.downloader.Release()

	assert.Eventually(t, func() bool {
		return (*procCtx).Err() != nil
	}, 2*time.Second, 10*time.Millisecond, "idle teardown should cancel the process context")
}

func TestDownloader_Acquire_CancelsIdleTeardownOnReuse(t *testing.T) {
	ts := setupDownloader(t, 200*time.Millisecond)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	procCtx := ts.expectSuccessfulStart(t) // must start exactly once across both Acquire calls

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	ts.downloader.Release()

	// Re-acquire well before the idle timeout elapses; this must cancel the
	// pending teardown rather than let it kill the still-wanted process.
	time.Sleep(20 * time.Millisecond)
	_, err = ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	time.Sleep(300 * time.Millisecond)
	assert.Nil(t, (*procCtx).Err(), "process must survive: the idle teardown was canceled by re-acquire")

	ts.downloader.Release()
}

func TestDownloader_Close_TearsDownAndRejectsFurtherAcquire(t *testing.T) {
	ts := setupDownloader(t, time.Hour)
	defer ts.ctrl.Finish()
	procCtx := ts.expectSuccessfulStart(t)

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	ts.downloader.Release()

	require.NoError(t, ts.downloader.Close())
	assert.Eventually(t, func() bool {
		return (*procCtx).Err() != nil
	}, 2*time.Second, 10*time.Millisecond)

	_, err = ts.downloader.Acquire(context.Background())
	assert.ErrorIs(t, err, offlinecache.ErrDownloaderClosed)
}

func TestDownloader_Acquire_ContextCanceledWhileWaitingForSlot(t *testing.T) {
	ts := setupDownloader(t, time.Hour)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	ts.expectSuccessfulStart(t)

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	defer ts.downloader.Release()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = ts.downloader.Acquire(ctx)
	assert.ErrorIs(t, err, context.Canceled)
}
