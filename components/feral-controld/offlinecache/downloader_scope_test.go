package offlinecache_test

import (
	"context"
	"errors"
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

// scopeTestLimits is the fixed HeadlessLimits every scope test uses, so
// property assertions are literal.
func scopeTestLimits() offlinecache.HeadlessLimits {
	return offlinecache.HeadlessLimits{
		Enabled:         true,
		CPUQuotaPercent: 300,
		AllowedCPUs:     "0-3",
		MemoryMaxBytes:  2 << 30,
	}
}

func setupScopedDownloader(t *testing.T) *downloaderTestSetup {
	return setupScopedDownloaderIdle(t, time.Minute)
}

func setupScopedDownloaderIdle(t *testing.T, idleTeardown time.Duration) *downloaderTestSetup {
	ctrl := gomock.NewController(t)
	mockExec := mocks.NewMockExec(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)

	mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	d := offlinecache.NewDownloader(
		"/usr/bin/chromium", "/tmp/offline-cache-headless", 9223,
		idleTeardown, scopeTestLimits(), mockExec, mockOS, wrapper.NewClock(), mockHTTP, zaptest.NewLogger(t),
	)
	return &downloaderTestSetup{ctrl: ctrl, mockExec: mockExec, mockOS: mockOS, mockHTTP: mockHTTP, downloader: d}
}

// expectScopeProbe wires the one-time capability check: the stale-scope
// glob sweep followed by the systemd-run /bin/true probe. probeErr != nil
// simulates an environment without transient-scope support.
func (ts *downloaderTestSetup) expectScopeProbe(probeErr error) {
	sweepCmd := mocks.NewMockExecCmd(ts.ctrl)
	sweepCmd.EXPECT().CombinedOutput().Return([]byte{}, nil).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-*.scope").
		Return(sweepCmd).Times(1)

	probeCmd := mocks.NewMockExecCmd(ts.ctrl)
	probeCmd.EXPECT().CombinedOutput().Return([]byte{}, probeErr).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemd-run", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, args ...string) wrapper.ExecCmd {
			// The probe must exercise the exact properties a real spawn
			// would use, so support for them is what gets validated.
			assert.Contains(ts.t, args, "/bin/true")
			assert.Contains(ts.t, args, "--property=CPUQuota=300%")
			return probeCmd
		}).Times(1)
}

// expectScopedStart wires one systemd-run --scope spawn of Chromium. The
// returned release func simulates the scoped process exiting (systemctl
// stop having killed the cgroup, or Chromium dying on its own): it
// unblocks the mocked Wait, letting the reaper run. Wait also returns if
// the process context is canceled, mirroring the real CommandContext
// contract, so backstop kills (procCancel) are observable too. procCtx
// points at the context the spawn received.
func (ts *downloaderTestSetup) expectScopedStart(t *testing.T) (spawnArgs *[]string, procCtx *context.Context, releaseWait func()) {
	t.Helper()
	exited := make(chan struct{})
	var exitOnce sync.Once
	var args []string
	var captured context.Context

	mockCmd := mocks.NewMockExecCmd(ts.ctrl)
	mockCmd.EXPECT().Start().Return(nil).Times(1)
	mockCmd.EXPECT().Wait().DoAndReturn(func() error {
		select {
		case <-exited:
			return nil
		case <-captured.Done():
			return context.Canceled
		}
	}).AnyTimes()

	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemd-run", gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ string, a ...string) wrapper.ExecCmd {
			captured = ctx
			args = a
			return mockCmd
		}).Times(1)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil)
	require.NoError(t, err)
	ts.mockHTTP.EXPECT().NewRequest(http.MethodGet, "http://127.0.0.1:9223/json/version", nil).Return(req, nil).AnyTimes()
	ts.mockHTTP.EXPECT().Do(gomock.Any()).Return(&http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader("")),
	}, nil).Times(1)

	return &args, &captured, func() { exitOnce.Do(func() { close(exited) }) }
}

func TestDownloader_ScopedStart_WrapsChromiumWithResourceLimits(t *testing.T) {
	ts := setupScopedDownloader(t)
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	spawnArgs, _, releaseWait := ts.expectScopedStart(t)
	defer releaseWait()

	endpoint, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9223", endpoint)

	args := *spawnArgs
	// Scope shape and every resource property.
	assert.Contains(t, args, "--user")
	assert.Contains(t, args, "--scope")
	assert.Contains(t, args, "--collect")
	assert.Contains(t, args, "--unit=feral-offline-capture-1.scope")
	assert.Contains(t, args, "--property=TimeoutStopSec=5")
	assert.Contains(t, args, "--property=CPUQuota=300%")
	assert.Contains(t, args, "--property=AllowedCPUs=0-3")
	assert.Contains(t, args, "--property=MemoryMax=2147483648")
	// The wrapped command is the untouched Chromium invocation.
	assert.Contains(t, args, "--")
	assert.Contains(t, args, "/usr/bin/chromium")
	assert.Contains(t, args, "--headless=new")
	assert.Contains(t, args, "--remote-debugging-port=9223")

	// Teardown of a scoped generation goes through systemctl stop of THIS
	// unit (never a raw kill of systemd-run — that would orphan Chromium
	// inside the scope).
	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		releaseWait() // systemctl stop kills the cgroup -> systemd-run exits
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
		Return(stopCmd).Times(1)

	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
}

func TestDownloader_ScopedStart_FallsBackToPlainSpawnWhenScopeUnavailable(t *testing.T) {
	ts := setupScopedDownloader(t)
	ts.t = t
	defer ts.ctrl.Finish()

	// Probe fails (no session bus / systemd-run missing): the downloader
	// must degrade to the plain uncapped spawn rather than refuse to work.
	ts.expectScopeProbe(errors.New("Failed to connect to bus"))
	procCtx := ts.expectSuccessfulStart(t)

	endpoint, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9223", endpoint)
	// The plain path spawned Chromium directly with its normal flags.
	assert.Contains(t, ts.lastStartArgs, "--headless=new")

	// Plain-spawn teardown is the original context-cancel kill.
	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
	select {
	case <-(*procCtx).Done():
	default:
		t.Fatal("plain-spawn teardown must cancel the process context")
	}
}

// TestDownloader_ScopedTeardown_BackstopKillsAndResweepsOnStopFailure pins
// the one branch that deliberately breaks the done-channel guarantee:
// when `systemctl stop` fails, the backstop kills systemd-run directly (a
// Chromium may survive orphaned in the scope) and raises the re-sweep
// flag so the NEXT scoped spawn glob-stops the orphan before probing the
// debug endpoint it still holds.
func TestDownloader_ScopedTeardown_BackstopKillsAndResweepsOnStopFailure(t *testing.T) {
	// Short idle teardown: Release drives the scoped teardown (and its
	// failing systemctl stop) without an explicit Close, leaving the
	// downloader usable for the re-sweep assertion afterwards.
	ts := setupScopedDownloaderIdle(t, 30*time.Millisecond)
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	_, procCtx1, releaseWait1 := ts.expectScopedStart(t)
	defer releaseWait1()

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	// systemctl stop fails -> backstop must cancel the process context
	// (killing systemd-run), which unblocks the mocked Wait and reaps.
	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().Return([]byte("boom"), errors.New("systemctl wedged")).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
		Return(stopCmd).Times(1)

	ts.downloader.Release()
	select {
	case <-(*procCtx1).Done():
	case <-time.After(2 * time.Second):
		t.Fatal("backstop must cancel the process context when systemctl stop fails")
	}

	// The next scoped spawn must re-run the glob sweep BEFORE spawning
	// (the orphan may still hold the debug port), then start generation 2.
	resweepCmd := mocks.NewMockExecCmd(ts.ctrl)
	resweepCmd.EXPECT().CombinedOutput().Return([]byte{}, nil).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-*.scope").
		Return(resweepCmd).Times(1)
	spawnArgs2, _, releaseWait2 := ts.expectScopedStart(t)
	defer releaseWait2()

	_, err = ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Contains(t, *spawnArgs2, "--unit=feral-offline-capture-2.scope")

	stop2 := mocks.NewMockExecCmd(ts.ctrl)
	stop2.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		releaseWait2()
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-2.scope").
		Return(stop2).Times(1)

	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
}

// TestDownloader_Close_BoundsWaitWhenScopedTeardownAlreadyInFlight pins
// the Close-vs-idle-teardown race: once a scoped teardown has started,
// stopLocked has already cleared cmd/scopeUnit, so a racing Close cannot
// tell what kind of teardown is in flight — it must take the BOUNDED
// wait (scopeCloseWait, inside main.go's 2s forced-exit budget) rather
// than blocking unbounded on a systemctl stop that can legitimately run
// for seconds.
func TestDownloader_Close_BoundsWaitWhenScopedTeardownAlreadyInFlight(t *testing.T) {
	ts := setupScopedDownloaderIdle(t, 30*time.Millisecond)
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	_, _, releaseWait := ts.expectScopedStart(t)
	defer releaseWait()

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	// The idle teardown's systemctl stop starts, then WEDGES until the
	// test ends — simulating a stop job that outlives the shutdown budget.
	stopStarted := make(chan struct{})
	stopRelease := make(chan struct{})
	defer close(stopRelease)
	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		close(stopStarted)
		<-stopRelease
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
		Return(stopCmd).Times(1)

	ts.downloader.Release()
	<-stopStarted // teardown is now unambiguously in flight

	closed := make(chan struct{})
	go func() {
		_ = ts.downloader.Close()
		close(closed)
	}()
	select {
	case <-closed:
	case <-time.After(1800 * time.Millisecond):
		// scopeCloseWait is 1s; anything close to main.go's 2s forced-exit
		// budget means Close fell into an unbounded wait.
		t.Fatal("Close must bound its wait when a teardown of unknowable kind is already in flight")
	}
}

// TestDownloader_ScopedGeneration_SelfExitReapsAndNextAcquireStartsFresh
// covers a scoped Chromium dying on its own (crash, or the cgroup
// MemoryMax OOM kill this feature introduces): the reaper must clear the
// generation INCLUDING its scope unit (no systemctl stop is ever issued
// for it), and the next Acquire starts a fresh scope with a new unit name.
func TestDownloader_ScopedGeneration_SelfExitReapsAndNextAcquireStartsFresh(t *testing.T) {
	ts := setupScopedDownloader(t)
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	_, _, releaseWait1 := ts.expectScopedStart(t)

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	// Chromium dies on its own mid-hold (e.g. cgroup OOM kill).
	releaseWait1()
	ts.downloader.Release()

	// A subsequent Acquire must eventually observe the reaped generation
	// and start a FRESH scope (seq 2) — never issue a systemctl stop for
	// the dead one (no such expectation is registered: gomock fails on
	// any stop call). The reap is asynchronous (the reaper goroutine runs
	// whenever Wait returns), so an Acquire landing before it simply
	// reuses the dying generation — poll until the fresh spawn happens.
	spawnArgs2, _, releaseWait2 := ts.expectScopedStart(t)
	defer releaseWait2()

	require.Eventually(t, func() bool {
		endpoint, err := ts.downloader.Acquire(context.Background())
		require.NoError(t, err)
		require.Equal(t, "http://127.0.0.1:9223", endpoint)
		if len(*spawnArgs2) > 0 {
			return true // gen 2 spawned; keep the slot for the teardown below
		}
		ts.downloader.Release() // pre-reap reuse of the dying gen: retry
		return false
	}, 2*time.Second, 10*time.Millisecond, "second Acquire never started a fresh scoped generation")
	assert.Contains(t, *spawnArgs2, "--unit=feral-offline-capture-2.scope")

	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		releaseWait2()
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-2.scope").
		Return(stopCmd).Times(1)

	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
}
