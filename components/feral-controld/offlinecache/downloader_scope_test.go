package offlinecache_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

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

// scopeTestWrapper is the argv prefix scopeTestLimits implies: the session
// bus is stripped (so Chromium cannot re-parent out of the scope) and the
// CPU pin is carried by taskset (the systemd AllowedCPUs property is inert
// without a delegated cpuset controller).
func scopeTestWrapper() []string {
	return []string{"env", "-u", "DBUS_SESSION_BUS_ADDRESS", "taskset", "-c", "0-3"}
}

func setupScopedDownloader(t *testing.T) *downloaderTestSetup {
	return setupScopedDownloaderIdle(t, time.Minute)
}

func setupScopedDownloaderIdle(t *testing.T, idleTeardown time.Duration) *downloaderTestSetup {
	ts, _ := setupScopedDownloaderLogged(t, idleTeardown, zaptest.NewLogger(t))
	return ts
}

// setupScopedDownloaderLogged builds a scoped downloader around a caller
// supplied logger, so tests can assert on what was logged (the escape
// check's whole product is a log line).
func setupScopedDownloaderLogged(t *testing.T, idleTeardown time.Duration, logger *zap.Logger) (*downloaderTestSetup, *gomock.Controller) {
	ctrl := gomock.NewController(t)
	mockExec := mocks.NewMockExec(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)

	mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	d := offlinecache.NewDownloader(
		"/usr/bin/chromium", "/tmp/offline-cache-headless", 9223,
		idleTeardown, scopeTestLimits(), mockExec, mockOS, wrapper.NewClock(), mockHTTP, logger,
	)
	return &downloaderTestSetup{ctrl: ctrl, mockExec: mockExec, mockOS: mockOS, mockHTTP: mockHTTP, downloader: d}, ctrl
}

// expectScopeProbe wires the full one-time capability check for the happy
// path: the stale-scope glob sweep, the wrapper probe (which production
// code resolves FIRST, independently), then the systemd-run scope probe
// carrying the resolved wrapper. probeErr != nil simulates an environment
// without transient-scope support.
func (ts *downloaderTestSetup) expectScopeProbe(probeErr error) {
	ts.expectStaleScopeSweep()
	ts.expectWrapperProbe(nil)
	ts.expectScopeProbeOnly(probeErr, scopeTestWrapper())
}

// expectStaleScopeSweep wires the glob stop of leftover capture scopes
// that opens every capability probe.
func (ts *downloaderTestSetup) expectStaleScopeSweep() {
	sweepCmd := mocks.NewMockExecCmd(ts.ctrl)
	sweepCmd.EXPECT().CombinedOutput().Return([]byte{}, nil).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-*.scope").
		Return(sweepCmd).Times(1)
}

// expectScopeProbeOnly wires the systemd-run scope probe. wantWrapper is
// the argv wrapper the probe must carry (nil = none): the probe has to
// exercise the exact SHAPE a real spawn would use, properties AND wrapper,
// since probing a bare /bin/true is what let an inert cap report itself as
// active.
func (ts *downloaderTestSetup) expectScopeProbeOnly(probeErr error, wantWrapper []string) {
	probeCmd := mocks.NewMockExecCmd(ts.ctrl)
	probeCmd.EXPECT().CombinedOutput().Return([]byte{}, probeErr).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemd-run", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, args ...string) wrapper.ExecCmd {
			assert.Contains(ts.t, args, "/bin/true")
			assert.Contains(ts.t, args, "--property=CPUQuota=300%")
			sep := slices.Index(args, "--")
			require.NotEqual(ts.t, -1, sep)
			assert.Equal(ts.t, append(slices.Clone(wantWrapper), "/bin/true"), args[sep+1:])
			return probeCmd
		}).Times(1)
}

// expectWrapperProbe wires the PINNED wrapper probe, which production code
// resolves first and independently of the scope: a broken wrapper must not
// cost the scope too, nor get misreported as a systemd problem.
func (ts *downloaderTestSetup) expectWrapperProbe(probeErr error) {
	ts.expectWrapperProbeArgs(append(scopeTestWrapper()[1:], "/bin/true"), probeErr)
}

// expectEnvOnlyWrapperProbe wires the second wrapper probe: the pin
// dropped, escape prevention kept. Production only reaches it when the
// pinned probe failed.
func (ts *downloaderTestSetup) expectEnvOnlyWrapperProbe(probeErr error) {
	ts.expectWrapperProbeArgs([]string{"-u", "DBUS_SESSION_BUS_ADDRESS", "/bin/true"}, probeErr)
}

func (ts *downloaderTestSetup) expectWrapperProbeArgs(wantArgs []string, probeErr error) {
	probeCmd := mocks.NewMockExecCmd(ts.ctrl)
	probeCmd.EXPECT().CombinedOutput().Return([]byte{}, probeErr).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "env", gomock.Any()).
		DoAndReturn(func(_ context.Context, _ string, args ...string) wrapper.ExecCmd {
			assert.Equal(ts.t, wantArgs, args)
			return probeCmd
		}).Times(1)
}

// scopeTestPID is the PID expectScopedStart reports for the spawned
// process. `systemd-run --scope` execs in place, so in production this is
// the capture Chromium's own browser process (see wrapper.ExecCmd.Pid).
const scopeTestPID = 4242

// userSliceCgroup renders a realistic cgroup-v2 /proc/<pid>/cgroup line
// for a unit sitting under the user manager's app.slice.
func userSliceCgroup(unit string) string {
	return "0::/user.slice/user-1000.slice/user@1000.service/app.slice/" + unit
}

// expectCgroupReadInScope wires the escape check for a HEALTHY spawn: the
// process reports the very scope unit we spawned it into.
func (ts *downloaderTestSetup) expectCgroupReadInScope(unit string) {
	ts.expectCgroupReadRaw(userSliceCgroup(unit), nil)
}

// expectCgroupReadRaw wires one escape-check read of /proc/<pid>/cgroup
// with arbitrary content (or a failure).
func (ts *downloaderTestSetup) expectCgroupReadRaw(content string, err error) {
	ts.mockOS.EXPECT().
		ReadFile(fmt.Sprintf("/proc/%d/cgroup", scopeTestPID)).
		Return([]byte(content), err).Times(1)
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
	mockCmd.EXPECT().Pid().Return(scopeTestPID).AnyTimes()
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

// expectWrappedPlainStart is expectSuccessfulStart for the degraded path:
// no scope, so the spawned COMMAND is the wrapper's head (`env`) rather
// than Chromium itself, with the binary carried in the args.
func (ts *downloaderTestSetup) expectWrappedPlainStart(t *testing.T) (procCtx *context.Context) {
	t.Helper()
	var captured context.Context
	mockCmd := mocks.NewMockExecCmd(ts.ctrl)
	mockCmd.EXPECT().Start().Return(nil).Times(1)
	mockCmd.EXPECT().Wait().DoAndReturn(func() error {
		<-captured.Done()
		return context.Canceled
	}).AnyTimes()

	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "env", gomock.Any()).
		DoAndReturn(func(ctx context.Context, _ string, args ...string) wrapper.ExecCmd {
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

func TestDownloader_ScopedStart_WrapsChromiumWithResourceLimits(t *testing.T) {
	ts := setupScopedDownloader(t)
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	ts.expectCgroupReadInScope("feral-offline-capture-1.scope")
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

	// The argv wrapper must sit BETWEEN the scope separator and the binary,
	// in order: without `env -u` Chromium re-parents itself out of the scope
	// (voiding the quota and the ceiling) and without taskset the CPU pin —
	// the limit that bounds package power draw — applies nowhere at all,
	// because AllowedCPUs= needs a delegated cpuset controller the FF1 does
	// not have.
	sep := slices.Index(args, "--")
	require.NotEqual(t, -1, sep, "scope args must terminate with --")
	wantPrefix := append(scopeTestWrapper(), "/usr/bin/chromium")
	require.GreaterOrEqual(t, len(args), sep+1+len(wantPrefix))
	assert.Equal(t, wantPrefix, args[sep+1:sep+1+len(wantPrefix)])

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

// TestDownloader_ScopedStart_FallsBackToPinnedPlainSpawnWhenScopeUnavailable
// pins the FIRST degradation step. Losing the scope costs the cgroup quota
// and memory ceiling, but it must NOT cost the CPU pin: taskset needs no
// systemd at all, and the pin is what bounds package power draw. An
// unpinned SwiftShader capture was in flight when an FF1 hard-reset in the
// field, so "no scope" must never mean "no limits".
func TestDownloader_ScopedStart_FallsBackToPinnedPlainSpawnWhenScopeUnavailable(t *testing.T) {
	ts := setupScopedDownloader(t)
	ts.t = t
	defer ts.ctrl.Finish()

	// Scope probe fails (no session bus / systemd-run missing), wrapper
	// probe succeeds: degrade to a plain spawn that is still pinned.
	ts.expectScopeProbe(errors.New("Failed to connect to bus"))
	procCtx := ts.expectWrappedPlainStart(t)

	endpoint, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:9223", endpoint)
	// Chromium keeps its normal flags, behind the wrapper.
	assert.Contains(t, ts.lastStartArgs, "--headless=new")
	wantPrefix := append(scopeTestWrapper()[1:], "/usr/bin/chromium")
	require.GreaterOrEqual(t, len(ts.lastStartArgs), len(wantPrefix))
	assert.Equal(t, wantPrefix, ts.lastStartArgs[:len(wantPrefix)])

	// Plain-spawn teardown is the original context-cancel kill. It still
	// reaches Chromium: env and taskset exec in place, so the PID the
	// context owns IS Chromium's.
	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
	select {
	case <-(*procCtx).Done():
	default:
		t.Fatal("plain-spawn teardown must cancel the process context")
	}
}

// TestDownloader_ScopedStart_FallsBackToBareSpawnWhenWrapperAlsoUnavailable
// pins the SECOND degradation step: with neither a scope nor a usable
// wrapper (no taskset on the image, say), capture still runs — uncapped,
// exactly the pre-limits behavior. Refusing to capture would be the worse
// failure, but the daemon must say so rather than imply limits are on.
func TestDownloader_ScopedStart_FallsBackToBareSpawnWhenWrapperAlsoUnavailable(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ts, _ := setupScopedDownloaderLogged(t, time.Minute, zap.New(core))
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectStaleScopeSweep()
	ts.expectWrapperProbe(errors.New("taskset: command not found"))
	ts.expectEnvOnlyWrapperProbe(errors.New("env: command not found"))
	ts.expectScopeProbeOnly(errors.New("Failed to connect to bus"), nil)
	procCtx := ts.expectSuccessfulStart(t) // bare /usr/bin/chromium

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Contains(t, ts.lastStartArgs, "--headless=new")
	assert.NotContains(t, ts.lastStartArgs, "taskset")

	require.Len(t, observed.FilterMessageSnippet("WITHOUT resource limits").All(), 1,
		"a fully degraded capture must warn that nothing is capping it")

	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
	select {
	case <-(*procCtx).Done():
	default:
		t.Fatal("plain-spawn teardown must cancel the process context")
	}
}

// TestDownloader_ScopedStart_WarnsWhenChromiumEscapesItsScope pins the
// regression detector for the defect this wrapper exists to prevent: a
// Chromium build that re-parents itself into its own app scope anyway
// leaves the quota and ceiling enforcing nothing. That must be loud — the
// field crash went undiagnosed precisely because the daemon reported the
// limits as active while they applied to an empty cgroup.
func TestDownloader_ScopedStart_WarnsWhenChromiumEscapesItsScope(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ts, _ := setupScopedDownloaderLogged(t, time.Minute, zap.New(core))
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	// Chromium re-parented itself: its cgroup names its OWN app scope, not
	// the capture scope we spawned it into.
	ts.expectCgroupReadRaw(userSliceCgroup("app-org.chromium.Chromium-7759.scope"), nil)
	_, _, releaseWait := ts.expectScopedStart(t)
	defer releaseWait()

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	escapeWarnings := observed.FilterMessageSnippet("moved itself out of its resource scope").All()
	require.Len(t, escapeWarnings, 1)
	ctxMap := escapeWarnings[0].ContextMap()
	assert.Equal(t, "feral-offline-capture-1.scope", ctxMap["expected_unit"])
	assert.Contains(t, ctxMap["actual_cgroup"], "app-org.chromium.Chromium-7759.scope",
		"the warning must name where the process actually went")

	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		releaseWait()
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
		Return(stopCmd).Times(1)

	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
}

// TestDownloader_ScopedStart_NoEscapeWarningWhenCgroupUnreadable pins the
// "cannot tell" path: absence of evidence must not be reported as an
// escape. A false alarm here points debugging at the wrong subsystem
// entirely, which is the opposite of what this check is for.
func TestDownloader_ScopedStart_NoEscapeWarningWhenCgroupUnreadable(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ts, _ := setupScopedDownloaderLogged(t, time.Minute, zap.New(core))
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	// The process's cgroup is unreadable — it may simply have exited
	// already. That is the readiness probe's business to report, not
	// evidence about containment either way.
	ts.expectCgroupReadRaw("", errors.New("open /proc/4242/cgroup: no such file or directory"))

	_, _, releaseWait := ts.expectScopedStart(t)
	defer releaseWait()

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Empty(t, observed.FilterMessageSnippet("moved itself out of its resource scope").All(),
		"an unreadable cgroup must not be reported as an escape")

	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		releaseWait()
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
		Return(stopCmd).Times(1)

	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
}

// TestDownloader_ScopedStart_NoEscapeWarningWhenChromiumStaysInScope is the
// negative half: on the healthy path the process reports the capture scope
// as its own cgroup, and the check must stay silent.
func TestDownloader_ScopedStart_NoEscapeWarningWhenChromiumStaysInScope(t *testing.T) {
	core, observed := observer.New(zap.WarnLevel)
	ts, _ := setupScopedDownloaderLogged(t, time.Minute, zap.New(core))
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectScopeProbe(nil)
	ts.expectCgroupReadInScope("feral-offline-capture-1.scope")
	_, _, releaseWait := ts.expectScopedStart(t)
	defer releaseWait()

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)
	assert.Empty(t, observed.FilterMessageSnippet("moved itself out of its resource scope").All())

	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		releaseWait()
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
		Return(stopCmd).Times(1)

	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
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
	ts.expectCgroupReadInScope("feral-offline-capture-1.scope")
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
	ts.expectCgroupReadInScope("feral-offline-capture-2.scope")
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
	ts.expectCgroupReadInScope("feral-offline-capture-1.scope")
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
	ts.expectCgroupReadInScope("feral-offline-capture-1.scope")
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
	ts.expectCgroupReadInScope("feral-offline-capture-2.scope")
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

// setupScopedDownloaderLimits builds a scoped downloader with caller
// supplied limits, for cases that vary the CPU spec.
func setupScopedDownloaderLimits(t *testing.T, limits offlinecache.HeadlessLimits) *downloaderTestSetup {
	ctrl := gomock.NewController(t)
	mockExec := mocks.NewMockExec(ctrl)
	mockOS := mocks.NewMockOS(ctrl)
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockOS.EXPECT().MkdirAll(gomock.Any(), gomock.Any()).Return(nil).AnyTimes()

	d := offlinecache.NewDownloader(
		"/usr/bin/chromium", "/tmp/offline-cache-headless", 9223,
		time.Minute, limits, mockExec, mockOS, wrapper.NewClock(), mockHTTP, zaptest.NewLogger(t),
	)
	return &downloaderTestSetup{ctrl: ctrl, mockExec: mockExec, mockOS: mockOS, mockHTTP: mockHTTP, downloader: d, t: t}
}

// TestDownloader_CaptureWrapper_HonoursCPUSpecSyntax pins which CPU specs
// earn a taskset pin. systemd's AllowedCPUs= accepts whitespace-separated
// lists that `taskset -c` rejects outright, so a spelling that is perfectly
// legal upstream must NOT be handed to taskset: doing so makes every spawn
// fail to exec, and the resulting fallback is a bare, fully uncapped
// Chromium — the exact state that preceded the field hard-reset. The gate
// is countAllowedCPUs, the same parser alignHeadlessLimits uses, so the two
// always agree about what a CPU list is.
func TestDownloader_CaptureWrapper_HonoursCPUSpecSyntax(t *testing.T) {
	envOnly := []string{"env", "-u", "DBUS_SESSION_BUS_ADDRESS"}
	for _, tc := range []struct {
		name        string
		allowedCPUs string
		wantWrapper []string
	}{
		{"range", "0-3", append(slices.Clone(envOnly), "taskset", "-c", "0-3")},
		{"comma list", "0,2", append(slices.Clone(envOnly), "taskset", "-c", "0,2")},
		{"mixed", "0-1,4", append(slices.Clone(envOnly), "taskset", "-c", "0-1,4")},
		// systemd accepts this; taskset cannot parse it. No pin, but the
		// escape prevention (and so the quota and ceiling) must survive.
		{"whitespace list systemd allows but taskset rejects", "0 1 2 3", envOnly},
		{"unset", "", envOnly},
	} {
		t.Run(tc.name, func(t *testing.T) {
			limits := scopeTestLimits()
			limits.AllowedCPUs = tc.allowedCPUs
			ts := setupScopedDownloaderLimits(t, limits)
			defer ts.ctrl.Finish()

			ts.expectStaleScopeSweep()
			ts.expectWrapperProbeArgs(append(slices.Clone(tc.wantWrapper[1:]), "/bin/true"), nil)
			// Scope probe: assert only the generic shape here; the wrapper
			// content is what this table is about.
			probeCmd := mocks.NewMockExecCmd(ts.ctrl)
			probeCmd.EXPECT().CombinedOutput().Return([]byte{}, nil).Times(1)
			ts.mockExec.EXPECT().
				CommandContext(gomock.Any(), "systemd-run", gomock.Any()).
				Return(probeCmd).Times(1)

			ts.expectCgroupReadInScope("feral-offline-capture-1.scope")
			spawnArgs, _, releaseWait := ts.expectScopedStart(t)
			defer releaseWait()

			_, err := ts.downloader.Acquire(context.Background())
			require.NoError(t, err)

			args := *spawnArgs
			sep := slices.Index(args, "--")
			require.NotEqual(t, -1, sep)
			want := append(slices.Clone(tc.wantWrapper), "/usr/bin/chromium")
			require.GreaterOrEqual(t, len(args), sep+1+len(want))
			assert.Equal(t, want, args[sep+1:sep+1+len(want)])

			stopCmd := mocks.NewMockExecCmd(ts.ctrl)
			stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
				releaseWait()
				return []byte{}, nil
			}).Times(1)
			ts.mockExec.EXPECT().
				CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
				Return(stopCmd).Times(1)
			ts.downloader.Release()
			require.NoError(t, ts.downloader.Close())
		})
	}
}

// TestDownloader_CaptureWrapper_KeepsEscapePreventionWhenTasksetUnusable
// pins the middle rung of the degradation matrix: taskset missing costs
// the pin, but must NOT cost the scope. Collapsing to a bare spawn there
// would throw away the quota and memory ceiling over an unrelated failure.
func TestDownloader_CaptureWrapper_KeepsEscapePreventionWhenTasksetUnusable(t *testing.T) {
	ts := setupScopedDownloader(t)
	ts.t = t
	defer ts.ctrl.Finish()

	ts.expectStaleScopeSweep()
	ts.expectWrapperProbe(errors.New("taskset: command not found"))
	ts.expectEnvOnlyWrapperProbe(nil)
	ts.expectScopeProbeOnly(nil, []string{"env", "-u", "DBUS_SESSION_BUS_ADDRESS"}) // scope still probed, with env-only wrapper
	ts.expectCgroupReadInScope("feral-offline-capture-1.scope")
	spawnArgs, _, releaseWait := ts.expectScopedStart(t)
	defer releaseWait()

	_, err := ts.downloader.Acquire(context.Background())
	require.NoError(t, err)

	args := *spawnArgs
	assert.Contains(t, args, "--property=MemoryMax=2147483648", "the ceiling must survive a missing taskset")
	assert.Contains(t, args, "--property=CPUQuota=300%", "the quota must survive a missing taskset")
	sep := slices.Index(args, "--")
	require.NotEqual(t, -1, sep)
	assert.Equal(t, []string{"env", "-u", "DBUS_SESSION_BUS_ADDRESS", "/usr/bin/chromium"}, args[sep+1:sep+5])

	stopCmd := mocks.NewMockExecCmd(ts.ctrl)
	stopCmd.EXPECT().CombinedOutput().DoAndReturn(func() ([]byte, error) {
		releaseWait()
		return []byte{}, nil
	}).Times(1)
	ts.mockExec.EXPECT().
		CommandContext(gomock.Any(), "systemctl", "--user", "stop", "feral-offline-capture-1.scope").
		Return(stopCmd).Times(1)
	ts.downloader.Release()
	require.NoError(t, ts.downloader.Close())
}
