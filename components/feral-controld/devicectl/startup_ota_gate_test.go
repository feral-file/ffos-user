package devicectl

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/otagate"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

// fakeStartupGate scripts successive gate outcomes and records how often the
// gate ran, standing in for otagate via the startupOTAGate test seam. When
// enter/release are set, each call announces itself and then blocks, letting
// tests hold a run in flight.
type fakeStartupGate struct {
	mu      sync.Mutex
	results []otagate.Result
	errs    []error
	n       int

	enter   chan struct{}
	release chan struct{}
}

func (f *fakeStartupGate) call(context.Context) (otagate.Result, error) {
	f.mu.Lock()
	i := f.n
	f.n++
	if i >= len(f.results) {
		i = len(f.results) - 1
	}
	var err error
	if i < len(f.errs) {
		err = f.errs[i]
	}
	res := f.results[i]
	f.mu.Unlock()
	if f.enter != nil {
		f.enter <- struct{}{}
		<-f.release
	}
	return res, err
}

func (f *fakeStartupGate) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.n
}

// startupGateExecutor builds an executor whose claim state comes from the
// injected state manager and whose gate is the fake.
func startupGateExecutor(t *testing.T, claimed bool, gate *fakeStartupGate) *executor {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	st := &state.State{}
	if claimed {
		st.ConnectedDevice = &state.Device{ID: "phone-1"}
	}
	sm.EXPECT().GetState().Return(st).AnyTimes()

	return &executor{
		logger:         zap.NewNop(),
		clock:          &autoClaimClock{},
		startupOTAGate: gate.call,
	}
}

// TestStartupOTAGate_UnclaimedIsNoOp: an unclaimed device must not run the
// startup gate — the auto-claim flow owns the gate until the device is claimed
// (running both would be redundant even though the single-flight coalesces).
func TestStartupOTAGate_UnclaimedIsNoOp(t *testing.T) {
	gate := &fakeStartupGate{results: []otagate.Result{otagate.ResultNoUpdateNeeded}}
	e := startupGateExecutor(t, false, gate)

	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, 0, gate.calls())
}

// TestStartupOTAGate_SettledOutcomesLatchOncePerProcess: each settled outcome
// (no update, update started, too old, even a latched update failure) marks the
// boot check done — a later connectivity flap must not re-run it.
func TestStartupOTAGate_SettledOutcomesLatchOncePerProcess(t *testing.T) {
	cases := []struct {
		name   string
		result otagate.Result
		err    error
	}{
		{name: "no update needed", result: otagate.ResultNoUpdateNeeded},
		{name: "update started", result: otagate.ResultUpdateStarted},
		{name: "too old to upgrade", result: otagate.ResultTooOldToUpgrade},
		{name: "update failed (latched)", result: otagate.ResultUpdateStarted, err: errors.New("ladder failed")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gate := &fakeStartupGate{results: []otagate.Result{tc.result}, errs: []error{tc.err}}
			e := startupGateExecutor(t, true, gate)

			e.MaybeRunStartupOTAGateOnOnline(context.Background())
			assert.Equal(t, 1, gate.calls())

			// Online flap: the latch must hold.
			e.MaybeRunStartupOTAGateOnOnline(context.Background())
			assert.Equal(t, 1, gate.calls())
		})
	}
}

// TestStartupOTAGate_VersionCheckFailureRetriesWithBackoff: a failed version
// check (boot-time DNS convergence race) retries in-process with the
// auto-claim backoff until the check settles; the settled outcome then
// latches. The doubling is observable through the fake clock: 30s after the
// first failure, 60s after the second.
func TestStartupOTAGate_VersionCheckFailureRetriesWithBackoff(t *testing.T) {
	gate := &fakeStartupGate{
		results: []otagate.Result{
			otagate.ResultVersionCheckFailed,
			otagate.ResultVersionCheckFailed,
			otagate.ResultNoUpdateNeeded,
		},
		errs: []error{errors.New("dns"), errors.New("dns"), nil},
	}
	e := startupGateExecutor(t, true, gate)
	clk := e.clock.(*autoClaimClock)
	start := clk.Now()

	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, 3, gate.calls(), "two transient failures then the settled check")
	assert.Equal(t, autoClaimRetryMin+2*autoClaimRetryMin, clk.Now().Sub(start),
		"backoff must double per retry (30s then 60s)")

	// Settled now: a later flap is a no-op.
	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, 3, gate.calls())
}

// TestStartupOTAGate_VersionCheckExhaustionLatches: ResultVersionCheckFailed
// also covers deterministic failures (unreadable/unparseable local build
// config), so the retry loop is bounded — after the attempt budget the boot
// check gives up, latches, and leaves recovery to the nightly updater timer
// instead of spinning for the daemon's lifetime.
func TestStartupOTAGate_VersionCheckExhaustionLatches(t *testing.T) {
	gate := &fakeStartupGate{
		results: []otagate.Result{otagate.ResultVersionCheckFailed},
		errs:    []error{errors.New("corrupt ff1-config.json")},
	}
	e := startupGateExecutor(t, true, gate)

	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, startupOTAGateMaxCheckAttempts, gate.calls(),
		"the loop must stop at the attempt budget")

	// Exhaustion is a settled outcome: a later flap must not restart the loop.
	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, startupOTAGateMaxCheckAttempts, gate.calls())
}

// TestStartupOTAGate_ConcurrentTriggersDoNotStack: online flaps arrive as
// independent goroutines; while one run holds the gate (it may sit in a
// retry-backoff loop for minutes), a second trigger must return without
// spawning another gate run — otherwise flaps could stack update ladders.
func TestStartupOTAGate_ConcurrentTriggersDoNotStack(t *testing.T) {
	gate := &fakeStartupGate{
		results: []otagate.Result{otagate.ResultNoUpdateNeeded},
		enter:   make(chan struct{}),
		release: make(chan struct{}),
	}
	e := startupGateExecutor(t, true, gate)

	done := make(chan struct{})
	go func() {
		e.MaybeRunStartupOTAGateOnOnline(context.Background())
		close(done)
	}()
	<-gate.enter // first runner is now inside the gate

	// Second trigger while in flight: must be a no-op, not a second gate call.
	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, 1, gate.calls())

	close(gate.release)
	<-done
	assert.Equal(t, 1, gate.calls())
}

// TestStartupOTAGate_CanceledRetryLeavesLatchClear: a retry loop aborted by ctx
// cancellation (daemon shutdown, or the sleep interrupted) must leave the done
// latch clear so the next online transition gets another chance.
func TestStartupOTAGate_CanceledRetryLeavesLatchClear(t *testing.T) {
	gate := &fakeStartupGate{
		results: []otagate.Result{
			otagate.ResultVersionCheckFailed,
			otagate.ResultNoUpdateNeeded,
		},
		errs: []error{errors.New("dns"), nil},
	}
	e := startupGateExecutor(t, true, gate)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel during the first backoff sleep so the loop exits before retrying.
	e.clock.(*autoClaimClock).onSleep = cancel
	e.MaybeRunStartupOTAGateOnOnline(ctx)
	assert.Equal(t, 1, gate.calls(), "loop must stop on ctx cancellation")

	// The next online transition retries and settles.
	e.clock.(*autoClaimClock).onSleep = nil
	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, 2, gate.calls())
}

// TestStartupOTAGate_EntryGatedOnBootWindow: the hook stays wired for the
// whole process, so a claimed device that BOOTED offline and only gains WAN
// hours later would otherwise launch a Required-mode update — and its reboot
// — mid-exhibition. Entry must defer to the nightly updater timer once the
// boot window closes, without latching the done flag (deferral is not a
// settled boot check). A nil probe (tests, doubles) means no gating.
func TestStartupOTAGate_EntryGatedOnBootWindow(t *testing.T) {
	gate := &fakeStartupGate{results: []otagate.Result{otagate.ResultNoUpdateNeeded}}
	e := startupGateExecutor(t, true, gate)

	e.bootLifecycleProbe = func() bool { return false } // WAN arrived after the window
	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, 0, gate.calls(), "a post-window online transition must not run the gate")
	assert.False(t, e.startupOTAGateDone.Load(), "deferral must not latch the boot check as done")

	// Within the window the same entry runs normally.
	e.bootLifecycleProbe = func() bool { return true }
	e.MaybeRunStartupOTAGateOnOnline(context.Background())
	assert.Equal(t, 1, gate.calls())
	assert.True(t, e.startupOTAGateDone.Load())
}

// TestStartupOTAGate_ResetMidRetryStopsWithoutLatching: a factory reset can
// clear the claim state while the VersionCheckFailed retry loop sleeps (the
// reset's staged reboot can be delayed or fail). The next attempt must NOT
// run — a Required-mode update, and its reboot, over the freshly unclaimed
// setup flow is on the wrong side of the claimSettled partition — and the
// done latch must stay clear so a re-claimed device's later online
// transition may legitimately re-enter.
func TestStartupOTAGate_ResetMidRetryStopsWithoutLatching(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	// Mutable shared state: claimed at entry, reset mid-sleep.
	st := &state.State{ConnectedDevice: &state.Device{ID: "phone-1"}}
	sm := mocks.NewMockStateManager(ctrl)
	state.InjectStateManagerForTesting(sm)
	t.Cleanup(state.ResetForTesting) // don't leave a finished mock as the global manager
	sm.EXPECT().GetState().Return(st).AnyTimes()

	gate := &fakeStartupGate{results: []otagate.Result{otagate.ResultVersionCheckFailed}}
	clk := &autoClaimClock{}
	clk.onSleep = func() {
		st.ConnectedDevice = nil // factory reset lands during the backoff sleep
	}
	e := &executor{logger: zap.NewNop(), clock: clk, startupOTAGate: gate.call}

	e.MaybeRunStartupOTAGateOnOnline(context.Background())

	assert.Equal(t, 1, gate.calls(), "no further attempt may run after the reset")
	assert.False(t, e.startupOTAGateDone.Load(), "a stopped run must not latch the boot check")
}
