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
	"go.uber.org/zap/zaptest/observer"

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
func (c scriptedCmd) Pid() int                        { return 0 }

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

// TestTracker_ProvenPanelPowerOffReprobesQuickly pins the fast-lease promise:
// a display generation that has already answered DDC reads gets the SHORT
// reprobe lease when it later demotes. The dominant field case is a monitor
// manually powered off (connector still "connected", hard "no DDC/CI" from
// every probe): once it is powered back on, status must resume within
// ~ddcReprobeIntervalProven, not up to ten minutes later.
func TestTracker_ProvenPanelPowerOffReprobesQuickly(t *testing.T) {
	t.Parallel()
	fp := "fp-proven-panel"
	mode := "awake"
	reply := func(argv []string) ([]byte, error) {
		if isRecoveryPokeArgv(argv) {
			if mode == "off" {
				return []byte(noDdcOutput), errExit1
			}
			if mode == "asleep" {
				mode = "awake" // the poke wakes the panel's DDC
			}
			return []byte("VCP 60 SNC x0f\n"), nil
		}
		if isDetectArgv(argv) {
			if mode == "awake" {
				return []byte("Monitor: ACME : FieldPanel\n"), nil
			}
			return []byte(noDdcOutput), errExit1
		}
		if mode == "awake" {
			return []byte(healthyVcpOutput), nil
		}
		return []byte(noDdcOutput), errExit1
	}
	p, _, clock := newTrackedPanel(reply, &fp)

	// The panel proves itself with a healthy round.
	require.True(t, pollRound(t, p))

	// Monitor manually powered off: hard failures demote after the threshold.
	mode = "off"
	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p), "hard failures must close the gate")

	// Monitor powered back on (poke-needy, fingerprint unchanged): the PROVEN
	// lease must reopen the gate quickly and the rescue must rehabilitate.
	mode = "asleep"
	clock.advance(ddcReprobeIntervalProven + time.Second)
	require.True(t, pollRound(t, p), "proven panel must reprobe on the short lease")
	require.True(t, pollRound(t, p), "a rescued reprobe must reopen polling")
}

// TestTracker_UnprovenPanelKeepsSlowReprobe pins the flip side: a display
// generation that has NEVER answered DDC (a genuinely DDC-less TV) must NOT
// inherit the short lease — it stays on the slow interval so the verdict
// keeps it cheap.
func TestTracker_UnprovenPanelKeepsSlowReprobe(t *testing.T) {
	t.Parallel()
	fp := "fp-tv"
	p, _, clock := newTrackedPanel(replyNoDdc, &fp)

	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p))

	clock.advance(ddcReprobeIntervalProven + time.Second)
	require.False(t, pollRound(t, p), "unproven panel must not get the short lease")

	clock.advance(ddcReprobeInterval)
	require.True(t, pollRound(t, p), "the slow window must still reopen")
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

// --- Recovery-futility learning ---------------------------------------------

func isDetectArgv(argv []string) bool {
	return len(argv) >= 2 && argv[1] == "detect"
}

// isRecoveryPokeArgv matches the fixed `getvcp 60 --brief` rescue read.
func isRecoveryPokeArgv(argv []string) bool {
	for _, a := range argv {
		if a == "60" {
			return true
		}
	}
	return false
}

const partialVcpOutput = "VCP 10 C 50 100\n"

// replyPartialSupport mimics the field case: detect works, every getvcp
// batch exits non-zero yet still emits a usable VCP line for the codes that
// work, and the recovery poke succeeds without changing anything.
func replyPartialSupport(argv []string) ([]byte, error) {
	if isDetectArgv(argv) {
		return []byte("Monitor: ACME : PartialPanel\n"), nil
	}
	if isRecoveryPokeArgv(argv) {
		return []byte("VCP 60 SNC x0f\n"), nil
	}
	return []byte(partialVcpOutput), errExit1
}

// TestTracker_PartialSupportKeepsValuesFlowingAndDropsRecovery pins the two
// promises for partially-DDC-capable displays: the readable VCPs keep
// updating on EVERY poll round (no demotion, ShouldPoll stays true), and
// after ddcRecoveryFutileThreshold futile recoveries each round costs a
// single getvcp instead of getvcp+poke+retry.
func TestTracker_PartialSupportKeepsValuesFlowingAndDropsRecovery(t *testing.T) {
	t.Parallel()
	fp := "fp-partial"
	p, exec, _ := newTrackedPanel(replyPartialSupport, &fp)
	ctx := context.Background()

	// Futility-learning rounds: detect + batch + poke + retry = 4 subprocesses.
	for i := 0; i < ddcRecoveryFutileThreshold; i++ {
		before := exec.callCount()
		st, err := p.CollectStatus(ctx)
		require.NoError(t, err)
		require.NotNil(t, st.Brightness, "round %d: readable VCPs must parse", i)
		require.Equal(t, 4, exec.callCount()-before,
			"round %d: recovery still attempted while learning", i)
	}

	// Futile: single-shot rounds (detect + batch = 2 subprocesses), values
	// still flowing, polling never suspended.
	for i := 0; i < 5; i++ {
		require.True(t, p.ShouldPoll(), "partial support must never demote")
		before := exec.callCount()
		st, err := p.CollectStatus(ctx)
		require.NoError(t, err)
		require.NotNil(t, st.Brightness)
		require.Equal(t, 50, *st.Brightness)
		require.Equal(t, 2, exec.callCount()-before,
			"futile round %d: no recovery poke, no retry", i)
	}
}

// TestTracker_RecoveryRescueKeepsRecoveryEnabled: when the recovery poke
// genuinely fixes the read (a real transient), futility must not engage.
func TestTracker_RecoveryRescueKeepsRecoveryEnabled(t *testing.T) {
	t.Parallel()
	fp := "fp-transient"
	failNext := true
	reply := func(argv []string) ([]byte, error) {
		if isDetectArgv(argv) {
			return []byte("Monitor: ACME : FlakyPanel\n"), nil
		}
		if isRecoveryPokeArgv(argv) {
			failNext = false // the poke "wakes" the panel
			return []byte("VCP 60 SNC x0f\n"), nil
		}
		if failNext {
			return []byte("i2c transaction failed"), errExit1
		}
		failNext = true // next round's initial read fails again
		return []byte(healthyVcpOutput), nil
	}
	p, exec, _ := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	for i := 0; i < ddcRecoveryFutileThreshold*2; i++ {
		before := exec.callCount()
		st, err := p.CollectStatus(ctx)
		require.NoError(t, err)
		require.NotNil(t, st.Brightness, "rescued round %d must parse", i)
		require.Equal(t, 4, exec.callCount()-before,
			"round %d: rescuing recovery must stay enabled", i)
	}
}

// TestTracker_CleanReadReenablesRecovery: a clean initial read after futility
// proves the wedge cleared, so a later failure gets the rescue again.
func TestTracker_CleanReadReenablesRecovery(t *testing.T) {
	t.Parallel()
	fp := "fp-healing"
	mode := "partial"
	reply := func(argv []string) ([]byte, error) {
		if isDetectArgv(argv) {
			return []byte("Monitor: ACME : HealingPanel\n"), nil
		}
		if isRecoveryPokeArgv(argv) {
			return []byte("VCP 60 SNC x0f\n"), nil
		}
		if mode == "clean" {
			return []byte(healthyVcpOutput), nil
		}
		return []byte(partialVcpOutput), errExit1
	}
	p, exec, _ := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	for i := 0; i < ddcRecoveryFutileThreshold; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}
	before := exec.callCount()
	_, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, 2, exec.callCount()-before, "futile mode engaged")

	// The panel heals: one clean round re-arms the recovery path.
	mode = "clean"
	_, err = p.CollectStatus(ctx)
	require.NoError(t, err)

	mode = "partial"
	before = exec.callCount()
	_, err = p.CollectStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, exec.callCount()-before,
		"recovery must be re-enabled after a clean read")
}

// TestTracker_PowerCycleRecoversDespiteFutileLatch pins the field regression:
// a monitor manually powered off (DRM connector still "connected", so the
// fingerprint never changes) makes every recovery futile and latches the
// verdict. When the monitor comes back needing the getvcp-60 wake poke, the
// latch must not suppress the rescue — a totally-failed read (no VCP lines)
// is not the partial-support shape futility was learned from, and without the
// poke the "clean initial read" escape hatch is unreachable, deadlocking
// ddc_status forever.
func TestTracker_PowerCycleRecoversDespiteFutileLatch(t *testing.T) {
	t.Parallel()
	fp := "fp-stable-across-power-cycle"
	// off: monitor powered off, all DDC traffic fails (generic, so the poll
	//      gate never closes). asleep: monitor back on, DDC answers only
	//      after the wake poke. awake: fully up.
	mode := "off"
	reply := func(argv []string) ([]byte, error) {
		if isRecoveryPokeArgv(argv) {
			if mode == "off" {
				return []byte("Display not found\n"), errExit1
			}
			if mode == "asleep" {
				mode = "awake" // the poke wakes the panel's DDC
			}
			return []byte("VCP 60 SNC x0f\n"), nil
		}
		if isDetectArgv(argv) {
			if mode == "awake" {
				return []byte("Monitor: ACME : FieldPanel\n"), nil
			}
			return []byte("Display not found\n"), errExit1
		}
		if mode == "awake" {
			return []byte(healthyVcpOutput), nil
		}
		return []byte("Display not found\n"), errExit1
	}
	p, exec, _ := newTrackedPanel(reply, &fp)
	ctx := context.Background()

	// Off period: enough rounds to latch futility.
	for i := 0; i < ddcRecoveryFutileThreshold+2; i++ {
		require.True(t, pollRound(t, p), "generic failures must keep the gate open")
	}
	p.mu.Lock()
	futile := p.recoveryFutile
	p.mu.Unlock()
	require.True(t, futile, "futility must latch while the monitor is off")

	// Monitor powered back on; same fingerprint; DDC needs the wake poke.
	mode = "asleep"
	before := exec.callCount()
	st, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.NotNil(t, st.Brightness, "power-cycled panel must recover via the rescue poke")
	require.Equal(t, 50, *st.Brightness)
	require.Equal(t, 4, exec.callCount()-before,
		"totally-failed read must bypass futility suppression: detect + batch + poke + retry")

	p.mu.Lock()
	futile = p.recoveryFutile
	p.mu.Unlock()
	require.False(t, futile, "a rescuing recovery must lift the futility latch")
}

// TestTracker_PowerCycleReprobeRecoversDespiteFutileLatch is the hard-signature
// variant: the off period demotes the tracker (unsupported) AND latches
// futility. The scheduled reprobe after the monitor returns must still get the
// rescue poke, or every reprobe fails single-shot and the verdict never lifts.
func TestTracker_PowerCycleReprobeRecoversDespiteFutileLatch(t *testing.T) {
	t.Parallel()
	fp := "fp-stable-across-power-cycle"
	mode := "off"
	reply := func(argv []string) ([]byte, error) {
		if isRecoveryPokeArgv(argv) {
			if mode == "off" {
				return []byte(noDdcOutput), errExit1
			}
			if mode == "asleep" {
				mode = "awake"
			}
			return []byte("VCP 60 SNC x0f\n"), nil
		}
		if isDetectArgv(argv) {
			if mode == "awake" {
				return []byte("Monitor: ACME : FieldPanel\n"), nil
			}
			return []byte(noDdcOutput), errExit1
		}
		if mode == "awake" {
			return []byte(healthyVcpOutput), nil
		}
		return []byte(noDdcOutput), errExit1
	}
	p, _, clock := newTrackedPanel(reply, &fp)

	// Off period: hard signature demotes after the threshold (and the futile
	// recoveries along the way latch suppression).
	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p), "hard failures must close the gate")

	// Monitor back on (same fingerprint, poke-needy); the next reprobe window
	// must rescue and fully rehabilitate.
	mode = "asleep"
	clock.advance(ddcReprobeInterval + time.Second)
	require.True(t, pollRound(t, p), "the scheduled reprobe must run")
	require.True(t, pollRound(t, p), "a rescued reprobe must reopen polling")
}

// TestTracker_RecoveryFromUnsupportedBumpsGeneration pins the display-comeback
// contract for the fingerprint-blind power cycle: a monitor toggled off and on
// while its HPD/EDID stay up never changes the DRM fingerprint, so the comeback
// is only visible as a successful attempt against an "unsupported" verdict.
// That success must bump Generation() — it is what re-arms Generation()-keyed
// give-up state (the sleep panel leg's retry cap) so the scheduled power state
// is re-driven onto the returned panel instead of waiting for the next
// schedule boundary.
func TestTracker_RecoveryFromUnsupportedBumpsGeneration(t *testing.T) {
	t.Parallel()
	fp := "fp-stable-across-power-cycle"
	mode := "awake"
	reply := func(argv []string) ([]byte, error) {
		if mode == "awake" {
			return replyHealthy(argv)
		}
		return replyNoDdc(argv)
	}
	p, _, clock := newTrackedPanel(reply, &fp)

	require.True(t, pollRound(t, p))
	genProven := p.Generation()

	// Power off (fingerprint unchanged): demotion alone must NOT bump the
	// generation — the give-up state it keys is per display, and the display
	// has not come back yet.
	mode = "off"
	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p), "hard failures must close the gate")
	require.Equal(t, genProven, p.Generation(), "power-off alone must not bump the generation")

	// Power back on: the successful reprobe is the only comeback signal there
	// is, and it must start a new generation.
	mode = "awake"
	clock.advance(ddcReprobeIntervalProven + time.Second)
	require.True(t, pollRound(t, p), "proven lease must reopen the reprobe")
	genRecovered := p.Generation()
	require.Greater(t, genRecovered, genProven, "recovery from unsupported must bump the generation")

	// everSucceeded must survive the bump: the next off period still gets the
	// short proven-panel lease, not the slow unproven one.
	mode = "off"
	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p))
	clock.advance(ddcReprobeIntervalProven + time.Second)
	require.True(t, pollRound(t, p), "panel must keep the proven short lease after a recovery bump")
}

// TestTracker_UnavailableReprobeIsQuiet pins the log taxonomy for the
// powered-off-monitor steady state: once the tracker holds the "unsupported"
// verdict, a failing scheduled reprobe (initial read, recovery poke, retry)
// must emit nothing at Info level or above. Pre-fix it logged an Info + Warn
// pair every reprobe lease, forever, while a monitor was merely powered off.
func TestTracker_UnavailableReprobeIsQuiet(t *testing.T) {
	t.Parallel()
	fp := "fp-quiet-reprobe"
	mode := "awake"
	reply := func(argv []string) ([]byte, error) {
		if mode == "awake" {
			return replyHealthy(argv)
		}
		return replyNoDdc(argv)
	}
	core, observed := observer.New(zap.InfoLevel)
	exec := &scriptedExec{reply: reply}
	clock := &fakeTrackerClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	p := &panelDdc{
		exec:          exec,
		clock:         clock,
		logger:        zap.New(core),
		fingerprintFn: func() string { return fp },
	}

	require.True(t, pollRound(t, p))
	mode = "off"
	for i := 0; i < ddcProbeFailThreshold; i++ {
		require.True(t, pollRound(t, p))
	}
	require.False(t, pollRound(t, p), "hard failures must close the gate")

	clock.advance(ddcReprobeIntervalProven + time.Second)
	observed.TakeAll() // demotion-phase logs are expected; only the reprobe must be quiet
	require.True(t, pollRound(t, p), "reprobe window must reopen the gate")
	if entries := observed.TakeAll(); len(entries) != 0 {
		t.Fatalf("a failing reprobe of a known-unavailable panel must not log at Info or above, got %d: %v",
			len(entries), entries)
	}
}

// TestTracker_DisplayChangeReenablesRecovery: futility is a per-generation
// verdict; a swapped display starts with recovery available again.
func TestTracker_DisplayChangeReenablesRecovery(t *testing.T) {
	t.Parallel()
	fp := "fp-old"
	p, exec, _ := newTrackedPanel(replyPartialSupport, &fp)
	ctx := context.Background()

	for i := 0; i < ddcRecoveryFutileThreshold+1; i++ {
		_, err := p.CollectStatus(ctx)
		require.NoError(t, err)
	}

	fp = "fp-new"
	before := exec.callCount()
	_, err := p.CollectStatus(ctx)
	require.NoError(t, err)
	require.Equal(t, 4, exec.callCount()-before,
		"new display generation must get the recovery path back")
}
