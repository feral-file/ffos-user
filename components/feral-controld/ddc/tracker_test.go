package ddc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// scriptedExec fakes wrapper.Exec: every ddcutil invocation is recorded and
// answered by the reply function.
type scriptedExec struct {
	mu    sync.Mutex
	calls [][]string
	reply func(argv []string) ([]byte, error)
}

type scriptedCmd struct {
	out []byte
	err error
}

func (c scriptedCmd) String() string                 { return "scripted" }
func (c scriptedCmd) Run() error                     { return c.err }
func (c scriptedCmd) Start() error                   { return c.err }
func (c scriptedCmd) Wait() error                    { return c.err }
func (c scriptedCmd) Output() ([]byte, error)        { return c.out, c.err }
func (c scriptedCmd) CombinedOutput() ([]byte, error) { return c.out, c.err }

func (e *scriptedExec) CommandContext(_ context.Context, name string, arg ...string) wrapper.ExecCmd {
	argv := append([]string{name}, arg...)
	e.mu.Lock()
	e.calls = append(e.calls, argv)
	e.mu.Unlock()
	out, err := e.reply(argv)
	return scriptedCmd{out: out, err: err}
}

func (e *scriptedExec) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.calls)
}

// fakeTrackerClock satisfies wrapper.Clock with a settable now.
type fakeTrackerClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeTrackerClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeTrackerClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeTrackerClock) Sleep(time.Duration) {}
func (c *fakeTrackerClock) SleepContext(ctx context.Context, _ time.Duration) error {
	return ctx.Err()
}
func (c *fakeTrackerClock) NewTicker(time.Duration) wrapper.Ticker {
	panic("not used in tracker tests")
}

const noDdcOutput = "No displays implementing DDC/CI found"

// replyNoDdc answers every ddcutil invocation the way a DDC-less (but
// physically connected) display does.
func replyNoDdc(argv []string) ([]byte, error) {
	return []byte(noDdcOutput), errExit1
}

var errExit1 = &exitError{}

type exitError struct{}

func (e *exitError) Error() string { return "exit status 1" }

const healthyVcpOutput = "VCP 10 C 50 100\nVCP 12 C 30 100\nVCP 62 C 15 100\nVCP 8D SNC x01\nVCP D6 SNC x01\n"

// replyHealthy answers detect and getvcp like a DDC-capable panel.
func replyHealthy(argv []string) ([]byte, error) {
	if len(argv) >= 2 && argv[1] == "detect" {
		return []byte("Monitor: ASUS : ROG-Strix\n"), nil
	}
	return []byte(healthyVcpOutput), nil
}

func newTrackedPanel(reply func([]string) ([]byte, error), fingerprint *string) (*panelDdc, *scriptedExec, *fakeTrackerClock) {
	exec := &scriptedExec{reply: reply}
	clock := &fakeTrackerClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	p := &panelDdc{
		exec:          exec,
		clock:         clock,
		logger:        zap.NewNop(),
		fingerprintFn: func() string { return *fingerprint },
	}
	return p, exec, clock
}

// TestTracker_DemotesAfterConsecutiveNoDdcFailures pins the core promise: a
// connected display without DDC/CI stops costing ddcutil subprocesses after
// ddcProbeFailThreshold failing rounds, until the reprobe window.
func TestTracker_DemotesAfterConsecutiveNoDdcFailures(t *testing.T) {
	t.Parallel()
	fp := "fp-tv"
	p, exec, _ := newTrackedPanel(replyNoDdc, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold; i++ {
		st, err := p.CollectStatus(ctx)
		require.NoError(t, err, "round %d: failing rounds still report field errors, not a hard error", i)
		require.NotNil(t, st.Errors)
	}
	spentOnProbing := exec.callCount()
	require.Greater(t, spentOnProbing, 0)

	// Demoted: gated rounds must not spawn ANY subprocess.
	for i := 0; i < 5; i++ {
		st, err := p.CollectStatus(ctx)
		require.ErrorIs(t, err, ErrUnavailable)
		require.Nil(t, st)
	}
	assert.Equal(t, spentOnProbing, exec.callCount(), "gated CollectStatus must be subprocess-free")
}

// TestTracker_ReprobesAfterInterval proves "unsupported" is a lease, not a
// life sentence: DDC/CI toggled on in the monitor's OSD (no hotplug event!)
// is picked up by the next slow reprobe.
func TestTracker_ReprobesAfterInterval(t *testing.T) {
	t.Parallel()
	fp := "fp-tv"
	broken := true
	reply := func(argv []string) ([]byte, error) {
		if broken {
			return replyNoDdc(argv)
		}
		return replyHealthy(argv)
	}
	p, _, clock := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}
	_, err := p.CollectStatus(ctx)
	require.ErrorIs(t, err, ErrUnavailable)

	// User enables DDC/CI in the OSD; nothing observable via DRM.
	broken = false
	_, err = p.CollectStatus(ctx)
	require.ErrorIs(t, err, ErrUnavailable, "before the reprobe window the verdict holds")

	clock.advance(ddcReprobeInterval + time.Second)
	st, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.Brightness, "reprobe must run for real and succeed")

	// And a success fully rehabilitates: subsequent rounds poll normally.
	st, err = p.CollectStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.Brightness)
}

// TestTracker_FailedReprobeArmsNextWindow proves a failing reprobe does not
// reopen continuous polling: one shot per window.
func TestTracker_FailedReprobeArmsNextWindow(t *testing.T) {
	t.Parallel()
	fp := "fp-tv"
	p, exec, clock := newTrackedPanel(replyNoDdc, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}

	clock.advance(ddcReprobeInterval + time.Second)
	_, err := p.CollectStatus(ctx)
	require.NoError(t, err, "the scheduled reprobe itself runs")
	after := exec.callCount()

	_, err = p.CollectStatus(ctx)
	require.ErrorIs(t, err, ErrUnavailable, "right after a failed reprobe the gate closes again")
	assert.Equal(t, after, exec.callCount())
}

// TestTracker_FingerprintChangeResetsImmediately proves plugging in a
// different display re-initializes at once — no waiting for the slow window.
func TestTracker_FingerprintChangeResetsImmediately(t *testing.T) {
	t.Parallel()
	fp := "fp-old-tv"
	healthy := false
	reply := func(argv []string) ([]byte, error) {
		if healthy {
			return replyHealthy(argv)
		}
		return replyNoDdc(argv)
	}
	p, _, _ := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}
	_, err := p.CollectStatus(ctx)
	require.ErrorIs(t, err, ErrUnavailable)

	// Swap in a DDC-capable monitor: connector status/EDID change the
	// fingerprint, and the very next round must probe again.
	fp = "fp-new-monitor"
	healthy = true
	st, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.Brightness)
	require.NotNil(t, st.Monitor)
}

// TestTracker_GenericFailuresNeverDemote pins the failure taxonomy: transient
// I2C trouble (no hard "no DDC/CI" signature) must not disable polling.
func TestTracker_GenericFailuresNeverDemote(t *testing.T) {
	t.Parallel()
	fp := "fp-flaky"
	reply := func(argv []string) ([]byte, error) {
		return []byte("i2c transaction failed"), errExit1
	}
	p, exec, _ := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold*3; i++ {
		st, err := p.CollectStatus(ctx)
		require.NoError(t, err)
		require.NotNil(t, st.Errors)
	}
	// Every round actually ran ddcutil — nothing was gated.
	assert.Greater(t, exec.callCount(), ddcProbeFailThreshold*3)
}

// TestTracker_SuccessResetsHardFailStreak: hard failures must be consecutive
// to demote; a success in between restarts the count.
func TestTracker_SuccessResetsHardFailStreak(t *testing.T) {
	t.Parallel()
	fp := "fp-wobbly"
	broken := true
	reply := func(argv []string) ([]byte, error) {
		if broken {
			return replyNoDdc(argv)
		}
		return replyHealthy(argv)
	}
	p, _, _ := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold-1; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}
	broken = false
	_, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	broken = true
	for i := 0; i < ddcProbeFailThreshold-1; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}

	// (threshold-1) fails + success + (threshold-1) fails: still not demoted.
	st, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, st)
}

// TestTracker_ApplyControlBypassesGateAndPromotes pins the two safety
// properties of the ungated control path: explicit intent always gets a real
// attempt even while unsupported, and its success rehabilitates the tracker
// (the standby-recovery escape hatch).
func TestTracker_ApplyControlBypassesGateAndPromotes(t *testing.T) {
	t.Parallel()
	fp := "fp-panel"
	broken := true
	reply := func(argv []string) ([]byte, error) {
		if broken {
			return replyNoDdc(argv)
		}
		if len(argv) >= 3 && argv[2] == "setvcp" {
			return []byte(""), nil
		}
		return replyHealthy(argv)
	}
	p, exec, _ := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}
	_, err := p.CollectStatus(ctx)
	require.ErrorIs(t, err, ErrUnavailable)
	gatedCalls := exec.callCount()

	// The panel wakes up (e.g. DDC was dead in standby). A user drags the
	// brightness slider: the attempt must happen despite the verdict.
	broken = false
	require.NoError(t, p.ApplyControl(ctx, DdcPanelActionBrightness, []byte("42")))
	require.Greater(t, exec.callCount(), gatedCalls, "ApplyControl must not be gated")

	// Its success reopens status polling immediately.
	st, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.Brightness)
}

// TestTracker_ApplyControlHardFailureCountsTowardDemotion: the sleep panel
// leg's setvcp attempts against a DDC-less display feed the same streak, so
// poller rounds and control attempts converge on one verdict.
func TestTracker_ApplyControlHardFailureCountsTowardDemotion(t *testing.T) {
	t.Parallel()
	fp := "fp-tv"
	p, exec, _ := newTrackedPanel(replyNoDdc, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold; i++ {
		err := p.ApplyControl(ctx, DdcPanelActionPower, []byte(`"standby"`))
		require.Error(t, err)
		require.True(t, strings.Contains(strings.ToLower(err.Error()), "no displays implementing ddc/ci"))
	}

	before := exec.callCount()
	_, err := p.CollectStatus(ctx)
	require.ErrorIs(t, err, ErrUnavailable)
	assert.Equal(t, before, exec.callCount())
}
