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
// idleTeardown is set to 1ms (as tight as a duration can practically be)
// specifically to make the buggy window's failure mode manifest almost
// immediately rather than requiring luck on timing; many goroutines run
// tight Acquire/Release loops with no delay between iterations, which is
// exactly the shape needed to land in that window if it still exists.
// This is a probabilistic stress test, not a hand-forced single
// interleaving — Go's scheduler decides how much the race is actually
// exercised — but it reliably reproduces the bug on the pre-fix code
// (verified by hand before committing this test) and cannot produce a
// false failure on the fixed code, since the fix's ordering guarantee
// (via Go's channel memory-model semantics — see Release's doc) holds
// unconditionally, not just usually.
//
// The regression is caught deterministically, not just observed as a
// symptom: expectSuccessfulStart wires CommandContext to exactly
// Times(1) for the whole test. Any wrongful teardown-and-restart cycle
// calls CommandContext a second time, which gomock fails immediately —
// so if this test passes at all, no teardown ever raced an active job.
//
// The wait below is deliberately bounded (rather than a plain
// wg.Wait()): on the pre-fix code this test hangs rather than merely
// failing an assertion — a teardown that wins the race tears down a
// process a queued Acquire is still waiting to reuse, and the resulting
// state corruption can leave that Acquire (and everything queued behind
// it) blocked forever on the semaphore. A bounded wait turns "the whole
// test binary hangs until CI kills the job" into a fast, clear failure.
func TestDownloader_ConcurrentReleaseAndAcquire_NeverTearsDownReacquiredProcess(t *testing.T) {
	ts := setupDownloader(t, time.Millisecond)
	defer ts.ctrl.Finish()
	defer func() { _ = ts.downloader.Close() }()
	ts.expectSuccessfulStart(t) // Times(1): must never restart across the whole stress loop below

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	ts.downloader.Release()

	const goroutines = 8
	const iterationsPerGoroutine = 300
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

	// A final Acquire confirms the SAME generation (endpoint identity is
	// stable across restarts too, so this alone would not catch a
	// restart — the Times(1) CommandContext expectation above is what
	// actually pins that) is still reachable after the stress loop.
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
