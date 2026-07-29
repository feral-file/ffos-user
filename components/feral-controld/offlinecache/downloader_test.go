package offlinecache_test

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
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

// TestDownloader_Start_UsesSoftwareWebGLAndMatchesKioskAutoplay pins the
// launch flags that keep WebGL context creation succeeding (so
// feature-detection-gated artwork resource fetches still happen) WITHOUT
// contending for the kiosk's real GPU hardware — see the rationale in
// downloader.go's start(). Reintroducing either --disable-gpu (breaks
// WebGL/canvas artwork capture) or the real-GPU-hardware flags
// (--ignore-gpu-blocklist/--enable-gpu-rasterization, which previously
// caused device-wide freezes by contending with the kiosk's own GPU use)
// would regress this.
func TestDownloader_Start_UsesSoftwareWebGLAndMatchesKioskAutoplay(t *testing.T) {
	ts := setupDownloader(t, time.Hour)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	ts.expectSuccessfulStart(t)

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	assert.NotContains(t, ts.lastStartArgs, "--disable-gpu")
	assert.NotContains(t, ts.lastStartArgs, "--ignore-gpu-blocklist")
	assert.NotContains(t, ts.lastStartArgs, "--enable-gpu-rasterization")
	assert.Contains(t, ts.lastStartArgs, "--use-gl=angle")
	assert.Contains(t, ts.lastStartArgs, "--use-angle=swiftshader-webgl")
	assert.Contains(t, ts.lastStartArgs, "--enable-unsafe-swiftshader")
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

// TestDownloader_Acquire_WaitsForPriorGenerationReapBeforeStartingReplacement
// pins the fix for a port/profile-lock race: once stopLocked (idle
// teardown here) has sent the kill signal and cleared cmd, a concurrent
// Acquire must not start a replacement Chromium on the same fixed
// --remote-debugging-port/--user-data-dir until the prior generation's
// cmd.Wait() has actually returned. gen1's Wait() is deliberately held
// open (independent of its context's cancellation) so the test can pin
// exactly that window.
func TestDownloader_Acquire_WaitsForPriorGenerationReapBeforeStartingReplacement(t *testing.T) {
	ts := setupDownloader(t, 20*time.Millisecond)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()

	releaseGen1 := make(chan struct{})
	gen1Cmd := mocks.NewMockExecCmd(ts.ctrl)
	gen1Cmd.EXPECT().Start().Return(nil).Times(1)
	gen1Cmd.EXPECT().Wait().DoAndReturn(func() error {
		<-releaseGen1
		return nil
	}).Times(1)

	var gen2Ctx context.Context
	gen2Cmd := mocks.NewMockExecCmd(ts.ctrl)
	gen2Cmd.EXPECT().Start().Return(nil).Times(1)
	gen2Cmd.EXPECT().Wait().DoAndReturn(func() error {
		<-gen2Ctx.Done()
		return context.Canceled
	}).AnyTimes()

	callCount := 0
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "/usr/bin/chromium", gomock.Any()).
		DoAndReturn(func(ctx context.Context, name string, args ...string) wrapper.ExecCmd {
			callCount++
			if callCount == 1 {
				return gen1Cmd
			}
			gen2Ctx = ctx
			return gen2Cmd
		}).Times(2)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil)
	require.NoError(t, err)
	ts.mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil).Return(req, nil).AnyTimes()
	ts.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil).Times(2)

	_, err = ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	ts.downloader.Release() // schedules idle teardown after 20ms

	// Give the idle teardown time to run stopLocked (kill signal sent,
	// cmd cleared) — gen1 is NOT yet reaped: its Wait() is still blocked
	// on releaseGen1.
	time.Sleep(80 * time.Millisecond)

	acquireDone := make(chan struct{})
	go func() {
		_, err := ts.downloader.Acquire(context.Background())
		assert.NoError(t, err)
		close(acquireDone)
	}()

	select {
	case <-acquireDone:
		t.Fatal("Acquire must wait for the prior generation to be reaped before starting a replacement")
	case <-time.After(150 * time.Millisecond):
	}

	close(releaseGen1) // let gen1's Wait() return, completing the reap
	select {
	case <-acquireDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire did not proceed once the prior generation was reaped")
	}
}

// expectRestartableStarts wires an unbounded number of
// CommandContext+Start+Wait cycles, each capturing its own process
// context, and reports the most recently started one.
//
// Unlike expectSuccessfulStart's Times(1), this tolerates the downloader
// legitimately tearing itself down when idle and starting a fresh
// process on the next Acquire — which is the idle teardown working as
// designed, and therefore cannot on its own be treated as a failure.
func (ts *downloaderTestSetup) expectRestartableStarts(t *testing.T) func() context.Context {
	t.Helper()
	var mu sync.Mutex
	var current context.Context

	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "/usr/bin/chromium", gomock.Any()).
		DoAndReturn(func(ctx context.Context, name string, args ...string) wrapper.ExecCmd {
			mockCmd := mocks.NewMockExecCmd(ts.ctrl)
			mockCmd.EXPECT().Start().Return(nil).AnyTimes()
			mockCmd.EXPECT().Wait().DoAndReturn(func() error {
				<-ctx.Done()
				return context.Canceled
			}).AnyTimes()
			mu.Lock()
			current = ctx
			mu.Unlock()
			return mockCmd
		}).AnyTimes()

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil)
	require.NoError(t, err)
	ts.mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil).Return(req, nil).AnyTimes()
	ts.mockHTTP.EXPECT().Do(gomock.Any()).DoAndReturn(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(""))}, nil
	}).AnyTimes()

	return func() context.Context {
		mu.Lock()
		defer mu.Unlock()
		return current
	}
}

// TestDownloader_ConcurrentReleaseAndAcquire_NeverTearsDownReacquiredProcess
// is the regression test for a Release/Acquire interleaving bug: Release
// used to free the semaphore slot BEFORE recording its idle-teardown
// decision under d.mu, leaving a window where a concurrent Acquire could
// take the now-free slot, see d.teardownCancel still nil (nothing to
// cancel), and reuse the running process — while Release, resuming a
// moment later, would go on to schedule a teardown anyway (its own
// d.cmd != nil check still holds) that nothing had told it was now moot.
// That teardown's timer would eventually fire and kill Chromium out from
// under the new job. See downloader.go's Release doc for the fix (the
// slot is now freed strictly after the teardown decision is recorded).
//
// WHAT IS ASSERTED, and why it is not a restart count: an earlier version
// pinned CommandContext to Times(1) across the whole test, reasoning that
// any teardown-and-restart cycle had to be the bug. It does not. With a
// 1ms idle timer the downloader legitimately tears itself down after any
// 1ms gap with no job holding the slot, and such gaps really do occur
// mid-loop — reliably so under GOMAXPROCS=1, where a worker can run well
// ahead of its peers. That is the feature working, and pinning the count
// made this test fail spuriously on CI and under -count/-cpu 1.
//
// The invariant that actually distinguishes the bug is narrower: a
// teardown must never kill the process a job is CURRENTLY holding. Each
// iteration therefore holds the slot across a short sleep — comfortably
// longer than idleTeardown, so a teardown wrongly scheduled by a
// concurrent Release fires DURING someone's hold rather than in a gap —
// and checks that the process it was handed is still alive when it lets
// go. Holding also keeps the downloader busy, which is what makes a
// legitimate idle teardown rare rather than routine.
//
// The bounded wait is the other half: on the pre-fix code a teardown that
// wins the race can leave a queued Acquire (and everything behind it)
// blocked forever, so a plain wg.Wait() would hang the whole binary until
// CI killed the job.
func TestDownloader_ConcurrentReleaseAndAcquire_NeverTearsDownReacquiredProcess(t *testing.T) {
	const idleTeardown = time.Millisecond
	ts := setupDownloader(t, idleTeardown)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	liveProcess := ts.expectRestartableStarts(t)

	var tornDownUnderAJob atomic.Bool
	const goroutines = 8
	const iterationsPerGoroutine = 40
	var wg sync.WaitGroup
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < iterationsPerGoroutine; j++ {
				_, err := ts.downloader.Acquire(context.Background())
				if !assert.NoError(t, err) {
					return
				}
				held := liveProcess()
				// Long enough that a teardown scheduled by another
				// goroutine's Release lands inside this hold.
				time.Sleep(3 * idleTeardown)
				if held != nil && held.Err() != nil {
					tornDownUnderAJob.Store(true)
				}
				ts.downloader.Release()
			}
		}()
	}

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("stress loop did not finish: a teardown likely raced an active job and wedged the semaphore (see this test's doc)")
	}

	assert.False(t, tornDownUnderAJob.Load(),
		"a teardown killed the headless Chromium while a job was still holding it — the exact interleaving this test exists to catch")

	// Still usable afterwards, whatever legitimate teardown/restart
	// cycles the loop went through.
	endpoint, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9223", endpoint)
	ts.downloader.Release()
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
