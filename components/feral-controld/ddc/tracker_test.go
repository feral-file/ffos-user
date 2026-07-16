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

func (c scriptedCmd) String() string                  { return "scripted" }
func (c scriptedCmd) Run() error                      { return c.err }
func (c scriptedCmd) Start() error                    { return c.err }
func (c scriptedCmd) Wait() error                     { return c.err }
func (c scriptedCmd) Output() ([]byte, error)         { return c.out, c.err }
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

// pollRound mimics the status poller's flow: only collect when ShouldPoll
// allows it. Returns whether the round actually collected.
func pollRound(t *testing.T, p *panelDdc) bool {
	t.Helper()
	if !p.ShouldPoll() {
		return false
	}
	_, err := p.CollectStatus(context.Background())
	require.NoError(t, err, "CollectStatus must never hard-error; failures ride the Errors map")
	return true
}

// TestTracker_DemotesAfterConsecutiveNoDdcFailures pins the core promise: a
// connected display without DDC/CI stops costing ddcutil subprocesses after
// ddcProbeFailThreshold failing poll rounds, until the reprobe window.
func TestTracker_DemotesAfterConsecutiveNoDdcFailures(t *testing.T) {
	t.Parallel()
	fp := "fp-tv"
	p, exec, _ := newTrackedPanel(replyNoDdc, &fp)

	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p), "round %d must still poll", i)
	}
	spentOnProbing := exec.callCount()
	require.Greater(t, spentOnProbing, 0)

	// Demoted: subsequent poll rounds are gated and subprocess-free.
	for i := 0; i < 5; i++ {
		require.False(t, pollRound(t, p), "gated round %d must not poll", i)
	}
	assert.Equal(t, spentOnProbing, exec.callCount(), "gated rounds must be subprocess-free")
}

// TestTracker_ExplicitCollectBypassesGate pins the command-path contract: a
// cloud ddcPanelStatus request is explicit intent, so CollectStatus itself is
// never gated and always returns a status object — only ShouldPoll suppresses
// the background poll.
func TestTracker_ExplicitCollectBypassesGate(t *testing.T) {
	t.Parallel()
	fp := "fp-tv"
	p, exec, _ := newTrackedPanel(replyNoDdc, &fp)
	ctx := context.Background()

	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, p.ShouldPoll(), "background polling must be suspended")
	gated := exec.callCount()

	st, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, st, "explicit request must get the pre-tracker wire shape")
	require.NotEmpty(t, st.Errors)
	require.Greater(t, exec.callCount(), gated, "explicit request must really probe")
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

	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p))

	// User enables DDC/CI in the OSD; nothing observable via DRM.
	broken = false
	require.False(t, pollRound(t, p), "before the reprobe window the verdict holds")

	clock.advance(ddcReprobeInterval + time.Second)
	require.True(t, pollRound(t, p), "reprobe window must reopen polling")

	// The successful reprobe fully rehabilitates: polling continues normally.
	require.True(t, pollRound(t, p))
}

// TestTracker_FailedReprobeArmsNextWindow proves a failing reprobe does not
// reopen continuous polling — one shot per window — REGARDLESS of whether the
// reprobe failure carries the hard signature or a generic error. The generic
// case is the regression guard: an unarmed nextProbeAt in the past would
// silently resurrect the every-5s ddcutil flood.
func TestTracker_FailedReprobeArmsNextWindow(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name  string
		reply func([]string) ([]byte, error)
	}{
		{name: "hard signature reprobe failure", reply: replyNoDdc},
		{name: "generic reprobe failure", reply: func(argv []string) ([]byte, error) {
			return []byte("i2c transaction failed"), errExit1
		}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fp := "fp-tv"
			demoteReply := replyNoDdc
			useDemote := true
			reply := func(argv []string) ([]byte, error) {
				if useDemote {
					return demoteReply(argv)
				}
				return tc.reply(argv)
			}
			p, exec, clock := newTrackedPanel(reply, &fp)

			for i := 0; i < ddcProbeFailThreshold; i++ {
				require.True(t, pollRound(t, p))
			}
			require.False(t, pollRound(t, p), "demoted: gate closed")

			useDemote = false
			clock.advance(ddcReprobeInterval + time.Second)
			require.True(t, pollRound(t, p), "the scheduled reprobe itself runs")
			after := exec.callCount()

			require.False(t, pollRound(t, p), "a failed reprobe must close the gate again")
			require.False(t, pollRound(t, p))
			assert.Equal(t, after, exec.callCount(), "post-reprobe rounds must be subprocess-free")

			clock.advance(ddcReprobeInterval + time.Second)
			require.True(t, pollRound(t, p), "the NEXT window must open again")
		})
	}
}

// TestTracker_FingerprintChangeResetsImmediately proves plugging in a
// different display re-initializes at once — no waiting for the slow window —
// and bumps the generation consumers key their own give-up state on.
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

	genBefore := p.Generation()
	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p))
	require.Equal(t, genBefore, p.Generation(), "no display change: generation stable")

	// Swap in a DDC-capable monitor: connector status/EDID change the
	// fingerprint; the very next round must poll again.
	fp = "fp-new-monitor"
	healthy = true
	require.Greater(t, p.Generation(), genBefore, "display change must bump the generation")
	require.True(t, pollRound(t, p), "display change must reopen polling immediately")

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

	for i := 0; i < ddcProbeFailThreshold*3; i++ {
		require.True(t, pollRound(t, p), "generic failures must never close the gate")
	}
	assert.Greater(t, exec.callCount(), ddcProbeFailThreshold*3)
}

// TestTracker_DisplayNotFoundIsNotADemotionSignature pins the taxonomy split
// with the recovery layer: execDdcutilWithDisplayRecovery treats "display not
// found" as transient and retries it, so the tracker must not demote on it —
// otherwise a brief HPD blip or DP link retrain would suspend polling on a
// genuinely DDC-capable monitor for a whole reprobe interval.
func TestTracker_DisplayNotFoundIsNotADemotionSignature(t *testing.T) {
	t.Parallel()
	fp := "fp-blip"
	reply := func(argv []string) ([]byte, error) {
		return []byte("Display not found\n"), errExit1
	}
	p, _, _ := newTrackedPanel(reply, &fp)

	for i := 0; i < ddcProbeFailThreshold*3; i++ {
		require.True(t, pollRound(t, p), "'display not found' must stay retryable, not demote")
	}
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

	for i := 0; i < ddcProbeFailThreshold-1; i++ {
		require.True(t, pollRound(t, p))
	}
	broken = false
	require.True(t, pollRound(t, p))
	broken = true
	for i := 0; i < ddcProbeFailThreshold-1; i++ {
		require.True(t, pollRound(t, p))
	}

	// (threshold-1) fails + success + (threshold-1) fails: still not demoted.
	require.True(t, pollRound(t, p))
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
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p))
	gatedCalls := exec.callCount()

	// The panel wakes up (e.g. DDC was dead in standby). A user drags the
	// brightness slider: the attempt must happen despite the verdict.
	broken = false
	require.NoError(t, p.ApplyControl(ctx, DdcPanelActionBrightness, []byte("42")))
	require.Greater(t, exec.callCount(), gatedCalls, "ApplyControl must not be gated")

	// Its success reopens status polling immediately.
	require.True(t, pollRound(t, p))
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
	require.False(t, p.ShouldPoll(), "control-path hard failures must demote background polling")
	assert.Equal(t, before, exec.callCount())
}
