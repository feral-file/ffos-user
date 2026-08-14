package devicectl

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/otagate"
)

// These tests pin the fire-and-forget contract of updateToLatest. It exists
// because the synchronous version could not work: the OTA ladder ends in a
// reboot, so the reply was never delivered and the caller saw a dropped
// connection instead (hardware repro 2026-08-07, FF1-8EVTK3RE — the daemon
// logged "Executing system update command" with no matching hub response line,
// while the update itself completed and rebooted 59s later).

// blockingGateConfig satisfies otagate.ConfigProvider and holds the gate's very
// FIRST step open until release is closed. That is what lets these tests observe
// the command's return value at a moment the ladder is provably still running —
// the whole property under test. Once released it fails the read, which ends
// runLocal right there: a success would drag in the distributor fetch and the
// updater runner, neither of which these tests are about.
type blockingGateConfig struct {
	entered     chan struct{}
	release     chan struct{}
	enteredOnce sync.Once
	calls       atomic.Int32
}

func newBlockingGateConfig() *blockingGateConfig {
	return &blockingGateConfig{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (c *blockingGateConfig) LocalBuild(context.Context) (string, string, string, error) {
	c.calls.Add(1)
	c.enteredOnce.Do(func() { close(c.entered) })
	<-c.release
	return "", "", "", errors.New("simulated local-config read failure")
}

// awaitEntered blocks until the gate has actually started running, so a test
// never asserts "still in flight" against a goroutine that has not been
// scheduled yet.
func (c *blockingGateConfig) awaitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-c.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("the detached OTA gate never started")
	}
}

// blockedUpdateExecutor wires an executor onto a pre-set gate built from cfg.
// otaGateInstance keeps a pre-set gate as-is (its test seam), so the lazy
// production wiring — and its HTTP client, runner and narration callbacks —
// stays out of these tests.
func blockedUpdateExecutor(cfg *blockingGateConfig) (*executor, *observer.ObservedLogs) {
	core, observed := observer.New(zap.InfoLevel)
	return &executor{
		logger:  zap.New(core),
		otaGate: otagate.New(otagate.Deps{Config: cfg}),
	}, observed
}

func TestUpdateToLatest_ACKsWhileTheLadderIsStillRunning(t *testing.T) {
	cfg := newBlockingGateConfig()
	e, _ := blockedUpdateExecutor(cfg)

	res, err := e.updateToLatest(context.Background())
	require.NoError(t, err, "the ACK must not wait on — or report — the ladder")
	assert.Equal(t, CmdOK, res)

	// The command has already returned while the gate is still inside its first
	// step: that ordering is the contract. Synchronous, this assertion could not
	// hold at all.
	cfg.awaitEntered(t)
	assert.Equal(t, int32(1), cfg.calls.Load())

	close(cfg.release)
	require.Eventually(t, func() bool { return !e.otaUpdateInFlight.Load() },
		5*time.Second, 10*time.Millisecond, "the in-flight latch must release when the run ends")
}

func TestUpdateToLatest_LadderFailureIsLoggedNotReturned(t *testing.T) {
	cfg := newBlockingGateConfig()
	e, observed := blockedUpdateExecutor(cfg)

	res, err := e.updateToLatest(context.Background())
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)

	cfg.awaitEntered(t)
	close(cfg.release)

	// By the time a failure is known there is no caller left to return it to, so
	// the log line is the only record. Losing it would make a failed
	// user-triggered update silently indistinguishable from a successful one.
	require.Eventually(t, func() bool {
		return len(observed.FilterMessage("System update request failed").All()) == 1
	}, 5*time.Second, 10*time.Millisecond)
}

// TestUpdateToLatest_LatchDoesNotBlockAFlightOpenedByAnotherEntryPoint pins the
// scope of otaUpdateInFlight. A settled-device updateToLatest must still be free
// to run while a pre-claim or startup gate holds a flight — that is what makes
// the gate's live-claim narrator policy reachable, and both docs/api-design.md
// and otaGateInstance's comment call it load-bearing. It cannot break today (the
// other entry points never read the latch), but this latch is the first
// construct that would break it if a future edit pushed the guard down into the
// gate or into otaGateInstance.
//
// Scope note: whether the second run COALESCES onto the first is otagate's
// singleflight contract, covered by that package's tests. What is asserted here
// is only that the latch does not swallow the command.
func TestUpdateToLatest_LatchDoesNotBlockAFlightOpenedByAnotherEntryPoint(t *testing.T) {
	cfg := newBlockingGateConfig()
	e, observed := blockedUpdateExecutor(cfg)

	startupGateDone := make(chan struct{})
	go func() {
		defer close(startupGateDone)
		_, _ = e.otaGate.EnsureLatestAtStartup(context.Background())
	}()
	cfg.awaitEntered(t)

	res, err := e.updateToLatest(context.Background())
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	assert.Equal(t, 1, len(observed.FilterMessage("Executing system update command").All()),
		"the command must start its run, not be dropped as a duplicate")
	assert.Empty(t, observed.FilterMessage("System update already in flight; duplicate command ignored").All(),
		"the latch is scoped to this command; another entry point's flight must not trip it")

	close(cfg.release)
	<-startupGateDone
	require.Eventually(t, func() bool { return !e.otaUpdateInFlight.Load() },
		5*time.Second, 10*time.Millisecond)
}

func TestUpdateToLatest_DuplicateWhileInFlightIsDroppedNotStacked(t *testing.T) {
	cfg := newBlockingGateConfig()
	e, observed := blockedUpdateExecutor(cfg)

	res, err := e.updateToLatest(context.Background())
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	cfg.awaitEntered(t)

	// Repeated taps while the first run is in flight are ACKed, not rejected —
	// the user gets no error for asking twice — but they must not each spawn a
	// waiter on the gate's single flight.
	for i := 0; i < 3; i++ {
		res, err := e.updateToLatest(context.Background())
		require.NoError(t, err)
		assert.Equal(t, CmdOK, res)
	}
	assert.Equal(t, 3, len(observed.FilterMessage("System update already in flight; duplicate command ignored").All()))
	assert.Equal(t, 1, len(observed.FilterMessage("Executing system update command").All()),
		"only the run that won the latch may start the gate")
	assert.Equal(t, int32(1), cfg.calls.Load())

	close(cfg.release)
	require.Eventually(t, func() bool { return !e.otaUpdateInFlight.Load() },
		5*time.Second, 10*time.Millisecond)

	// The latch is a per-run guard, not a one-shot: a later update must still
	// reach the gate. release is already closed, so this run passes straight
	// through LocalBuild instead of blocking.
	res, err = e.updateToLatest(context.Background())
	require.NoError(t, err)
	assert.Equal(t, CmdOK, res)
	require.Eventually(t, func() bool { return cfg.calls.Load() == 2 },
		5*time.Second, 10*time.Millisecond, "a fresh update after the latch released must reach the gate")
}
