package devicectl

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
)

// panelDDCTrackerStub supplies default implementations of the PanelDDC
// tracker probes (ShouldPoll/Generation) for test fakes that don't exercise
// them: always pollable, single display generation.
type panelDDCTrackerStub struct{}

func (panelDDCTrackerStub) ShouldPoll() bool   { return true }
func (panelDDCTrackerStub) Generation() uint64 { return 0 }

// slowBlockingPanelDDC blocks in ApplyControl until unblock is closed, so tests
// can prove sleep transitions do not wait on DDC.
type slowBlockingPanelDDC struct {
	panelDDCTrackerStub
	applyStarted chan struct{}
	unblock      chan struct{}
}

func (s *slowBlockingPanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	return &ddc.DdcPanelStatus{}, nil
}

func (s *slowBlockingPanelDDC) ApplyControl(ctx context.Context, action ddc.DdcPanelAction, value json.RawMessage) error {
	select {
	case s.applyStarted <- struct{}{}:
	default:
	}
	select {
	case <-s.unblock:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestApplySleepTransition_DoesNotBlockOnFfpPowerDDC(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil)

	applyStarted := make(chan struct{}, 1)
	unblock := make(chan struct{})
	panel := &slowBlockingPanelDDC{applyStarted: applyStarted, unblock: unblock}

	e := &executor{
		cdp:      mockCDP,
		panelDDC: panel,
		logger:   zaptest.NewLogger(t),
	}

	ctx := context.Background()
	done := make(chan error, 1)
	go func() {
		done <- e.applySleepTransition(ctx, sleepschedule.StateSleeping, "test")
	}()

	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("applySleepTransition blocked; FFP DDC should run asynchronously")
	}

	select {
	case <-applyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("expected ApplyControl to have started in background")
	}

	close(unblock)
}

func TestInvalidatePlayerSleepState_ForcesRedriveAfterCDPReconnect(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	// The IfChanged path only re-drives the player while CDP is connected.
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	// Exactly two player round-trips: the initial drive and the post-invalidation
	// re-drive. The aligned tick in between must not send.
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil).Times(2)

	e := &executor{
		cdp:    mockCDP,
		logger: zaptest.NewLogger(t),
	}
	ctx := context.Background()

	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "test"))
	// Aligned state: the de-dup skips the player.
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "test"))

	// Chromium restarted: the player web app reloaded awake while the tracker still
	// says sleeping. Invalidation must make the next tick re-drive the player
	// instead of skipping until the next schedule boundary.
	e.invalidatePlayerSleepState()
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "test"))
}

func TestInvalidatePlayerSleepState_UnsupportedExecutorIsNoOp(t *testing.T) {
	ctrl := gomock.NewController(t)
	// The exported helper takes the narrow Executor interface; an implementation
	// without the private hook (e.g. a mock) must be a safe no-op, not a panic.
	InvalidatePlayerSleepState(mocks.NewMockExecutor(ctrl), zaptest.NewLogger(t))
}

// TestSetSessionGeneration_WiresAndUnsupportedIsNoOp mirrors the other
// devicectl seam setters' contract: the concrete executor picks it up, and a
// caller passing something that lacks the private hook (a mock) degrades to a
// no-op instead of a panic.
func TestSetSessionGeneration_WiresAndUnsupportedIsNoOp(t *testing.T) {
	e := &executor{logger: zaptest.NewLogger(t)}
	SetSessionGeneration(e, func() uint64 { return 5 }, e.logger)
	assert.Equal(t, uint64(5), e.currentGeneration())

	ctrl := gomock.NewController(t)
	SetSessionGeneration(mocks.NewMockExecutor(ctrl), func() uint64 { return 1 }, e.logger)
}

func TestCurrentGeneration_NilSeamReadsZero(t *testing.T) {
	e := &executor{logger: zaptest.NewLogger(t)}
	assert.Equal(t, uint64(0), e.currentGeneration())
}

// TestApplySleepTransitionLocked_GenerationStable_RecordsApplied is the
// baseline: an unchanged generation across the setSleepMode send records
// success exactly as before the re-check existed.
func TestApplySleepTransitionLocked_GenerationStable_RecordsApplied(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil)

	e := &executor{
		cdp:               mockCDP,
		logger:            zaptest.NewLogger(t),
		sessionGeneration: func() uint64 { return 7 },
	}

	err := e.applySleepTransitionLocked(context.Background(), sleepschedule.StateAwake, "test")
	require.NoError(t, err)
	assert.Equal(t, playerAwake, e.sleepPlayerState)
	assert.Equal(t, sleepschedule.StateAwake, e.sleepLastGood)
}

// TestApplySleepTransitionLocked_GenerationMovedDuringSend_NotRecordedApplied
// pins design doc §2.4: the send itself succeeds, but a generation that moved
// WHILE it was in flight (a recovery navigation, reconnect, or watchdog
// reload landing mid-apply) means the ACK may not describe the current
// document — this must NOT be recorded as applied, so the schedule loop
// re-drives against whatever document is current now, and it must NOT be
// surfaced as an error (the send did not fail).
func TestApplySleepTransitionLocked_GenerationMovedDuringSend_NotRecordedApplied(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	gen := uint64(1)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(string, map[string]interface{}) (interface{}, error) {
			gen = 2 // the page changed generation while the send was in flight
			return map[string]any{"result": map[string]any{}}, nil
		})

	e := &executor{
		cdp:               mockCDP,
		logger:            zaptest.NewLogger(t),
		sessionGeneration: func() uint64 { return gen },
	}

	err := e.applySleepTransitionLocked(context.Background(), sleepschedule.StateSleeping, "test")
	// [minor #8] applySleepTransitionLocked itself returns the distinct
	// errSleepTransitionGenerationRace sentinel (not nil) so its own callers'
	// onAwake-edge check is correctly suppressed and the schedule-loop path
	// re-drives; it is applySleepTransition (the RPC-facing wrapper, tested
	// separately) that swallows this specific error before it reaches a
	// manual-override caller — a generation race is not itself a send
	// failure FROM THE CALLER'S perspective, but it IS unconfirmed from this
	// function's own perspective.
	require.ErrorIs(t, err, errSleepTransitionGenerationRace)
	assert.Equal(t, playerUnknownFailed, e.sleepPlayerState, "must not record success across a generation change")
	assert.Equal(t, sleepschedule.StateSleeping, e.sleepAttempted)
}

// TestApplySleepTransition_GenerationRace_SwallowsErrorAndSuppressesOnAwake
// pins minor #8 end to end: applySleepTransition (the RPC-facing wrapper)
// must swallow the generation-race sentinel — the send itself did not fail,
// so a manual wakeNow still reports success to its caller — while onAwake
// must NOT fire for the unconfirmed wake (it would otherwise fire again on
// the schedule loop's next re-drive too, double-recomputing displayAt).
func TestApplySleepTransition_GenerationRace_SwallowsErrorAndSuppressesOnAwake(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()

	gen := uint64(1)
	firstSend := mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]any{"result": map[string]any{}}, nil).Times(1)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(string, map[string]interface{}) (interface{}, error) {
			gen = 2 // the page changed generation while THIS (wake) send was in flight
			return map[string]any{"result": map[string]any{}}, nil
		}).Times(1).After(firstSend)

	e := &executor{
		cdp:               mockCDP,
		logger:            zaptest.NewLogger(t),
		sessionGeneration: func() uint64 { return gen },
	}
	ctx := context.Background()

	called := make(chan struct{}, 1)
	SetOnAwake(e, func(context.Context) { called <- struct{}{} }, zaptest.NewLogger(t))

	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "test-sleep"))

	err := e.applySleepTransition(ctx, sleepschedule.StateAwake, "test-wake")
	require.NoError(t, err, "a generation race must not surface to the RPC-facing caller")

	select {
	case <-called:
		t.Fatal("onAwake must not fire for an unconfirmed (generation-raced) wake")
	case <-time.After(100 * time.Millisecond):
	}

	e.sleepApplyMu.Lock()
	state := e.sleepPlayerState
	e.sleepApplyMu.Unlock()
	assert.Equal(t, playerUnknownFailed, state, "the raced wake must not be recorded as applied")
}

// stagedPanelDDC waits on release before each ApplyControl so tests can interleave
// sleep transitions and observe serialized DDC order.
type stagedPanelDDC struct {
	panelDDCTrackerStub
	release chan struct{}

	mu      sync.Mutex
	powers  []string
	entered chan struct{}
}

func (s *stagedPanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	return &ddc.DdcPanelStatus{}, nil
}

func (s *stagedPanelDDC) ApplyControl(ctx context.Context, action ddc.DdcPanelAction, value json.RawMessage) error {
	select {
	case s.entered <- struct{}{}:
	default:
	}
	<-s.release
	s.mu.Lock()
	s.powers = append(s.powers, string(value))
	s.mu.Unlock()
	return nil
}

func (s *stagedPanelDDC) appliedPowers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.powers))
	copy(out, s.powers)
	return out
}

func TestApplySleepTransition_SerializesRapidFfpPowerChanges(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil).Times(2)

	release := make(chan struct{})
	entered := make(chan struct{}, 2)
	panel := &stagedPanelDDC{release: release, entered: entered}

	e := &executor{
		cdp:      mockCDP,
		panelDDC: panel,
		logger:   zaptest.NewLogger(t),
	}

	ctx := context.Background()
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "t1"))

	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("expected first DDC apply to start")
	}

	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "t2"))

	release <- struct{}{}
	release <- struct{}{}

	require.Eventually(t, func() bool {
		p := panel.appliedPowers()
		return len(p) >= 2
	}, 3*time.Second, 20*time.Millisecond, "expected two DDC power applies")

	p := panel.appliedPowers()
	require.Equal(t, []string{`"standby"`, `"on"`}, p)
}

// tinyDelayPanelDDC adds a short delay so concurrent enqueue races the worker.
type tinyDelayPanelDDC struct {
	panelDDCTrackerStub
	delay time.Duration
}

func (t *tinyDelayPanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	return &ddc.DdcPanelStatus{}, nil
}

func (t *tinyDelayPanelDDC) ApplyControl(ctx context.Context, action ddc.DdcPanelAction, value json.RawMessage) error {
	if t.delay > 0 {
		select {
		case <-time.After(t.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

func TestApplyFfpPowerStateAsyncConcurrentEnqueue(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).AnyTimes().Return(map[string]any{"result": map[string]any{}}, nil)

	e := &executor{
		cdp:      mockCDP,
		panelDDC: &tinyDelayPanelDDC{delay: time.Millisecond},
		logger:   zaptest.NewLogger(t),
	}

	const goroutines = 32
	const iters = 64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for g := 0; g < goroutines; g++ {
		go func(g int) {
			defer wg.Done()
			for i := 0; i < iters; i++ {
				st := sleepschedule.StateAwake
				if (g+i)%2 == 0 {
					st = sleepschedule.StateSleeping
				}
				e.applyFfpPowerStateAsync(st, "stress")
			}
		}(g)
	}
	wg.Wait()
}

func TestSetOnAwake_FiresAfterSuccessfulWakeOutsideSleepLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil).AnyTimes()

	e := &executor{
		cdp:    mockCDP,
		logger: zaptest.NewLogger(t),
	}
	ctx := context.Background()

	called := make(chan struct{}, 1)
	SetOnAwake(e, func(callCtx context.Context) {
		assert.Equal(t, ctx, callCtx)
		called <- struct{}{}
	}, zaptest.NewLogger(t))

	// The hook fires on the sleep→awake EDGE only, so the wake must end a
	// recorded sleep.
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "test-sleep"))
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "test-wake"))

	select {
	case <-called:
	case <-time.After(2 * time.Second):
		t.Fatal("onAwake was not called after successful wake")
	}

	// Taking sleepApplyMu after the callback returned proves the wake path
	// released it before invoking onAwake (otherwise this would deadlock with
	// a callback that also needed the lock).
	done := make(chan struct{})
	go func() {
		e.sleepApplyMu.Lock()
		_ = e.sleepPlayerState
		e.sleepApplyMu.Unlock()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("sleepApplyMu still held after wake+onAwake")
	}
}

func TestSetOnAwake_NotCalledOnSleep(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil).AnyTimes()

	e := &executor{
		cdp:    mockCDP,
		logger: zaptest.NewLogger(t),
	}
	called := 0
	SetOnAwake(e, func(context.Context) { called++ }, zaptest.NewLogger(t))

	require.NoError(t, e.applySleepTransition(context.Background(), sleepschedule.StateSleeping, "test-sleep"))
	assert.Equal(t, 0, called)
}

// The hook must fire only on the sleep→awake edge: awake applies with no
// recorded sleep (boot, or a CDP reconnect that invalidated the tracker) and
// awake→awake re-drives (wakeNow on an already-awake device) are routine and
// would otherwise force-push — and visibly remount — an unchanged displayAt
// set on every reconnect, doubling the on-connect RecomputeNow.
func TestSetOnAwake_NotCalledWithoutSleepEdge(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil).AnyTimes()

	e := &executor{
		cdp:    mockCDP,
		logger: zaptest.NewLogger(t),
	}
	ctx := context.Background()
	called := 0
	SetOnAwake(e, func(context.Context) { called++ }, zaptest.NewLogger(t))

	// Boot / post-invalidation shape: no recorded prior state.
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "boot-awake"))
	assert.Equal(t, 0, called, "awake with no recorded sleep must not fire the hook")

	// Awake→awake re-drive (wakeNow on an already-awake device).
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "redundant-wake"))
	assert.Equal(t, 0, called, "awake→awake re-drive must not fire the hook")

	// CDP reconnect invalidates the tracker; the loop's awake re-drive of the
	// reloaded (awake) page must not fire either — the on-connect RecomputeNow
	// already serves that page.
	e.invalidatePlayerSleepState()
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateAwake, "reconnect-redrive"))
	assert.Equal(t, 0, called, "post-reconnect awake re-drive must not fire the hook")

	// A real edge still fires.
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "sleep"))
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "wake"))
	assert.Equal(t, 1, called, "sleep→awake edge must fire the hook")
}

// A FAILED sleep apply still records StateSleeping (not-applied), and the
// player state behind it is unknown — a displayAt timer may have fired into
// it. The subsequent awake must treat that as the edge and fire the hook:
// one redundant push is the safe direction, a stale playlist is not.
func TestSetOnAwake_FiresAfterFailedSleepApply(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	failNext := true
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(string, map[string]interface{}) (interface{}, error) {
			if failNext {
				failNext = false
				return nil, assert.AnError
			}
			return map[string]any{"result": map[string]any{}}, nil
		}).AnyTimes()

	e := &executor{
		cdp:    mockCDP,
		logger: zaptest.NewLogger(t),
	}
	ctx := context.Background()
	called := 0
	SetOnAwake(e, func(context.Context) { called++ }, zaptest.NewLogger(t))

	require.Error(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "failing-sleep"))
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "wake-after-failed-sleep"))
	assert.Equal(t, 1, called)
}

// TestSetOnAwake_FailedWakeThenRetryFiresOnce is M1: a successful sleep
// followed by a FAILED wake attempt (transport error) must not itself fire
// onAwake (nothing succeeded), but the RETRY that follows must — the failed
// wake left the tracker at playerUnknownFailed with lastGood==sleeping, and
// the onAwake predicate's M1 disjunct (state==playerUnknownFailed &&
// lastGood==sleeping) must catch it.
func TestSetOnAwake_FailedWakeThenRetryFiresOnce(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	failNext := false
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(string, map[string]interface{}) (interface{}, error) {
			if failNext {
				failNext = false
				return nil, assert.AnError
			}
			return map[string]any{"result": map[string]any{}}, nil
		}).AnyTimes()

	e := &executor{cdp: mockCDP, logger: zaptest.NewLogger(t)}
	ctx := context.Background()
	called := 0
	SetOnAwake(e, func(context.Context) { called++ }, zaptest.NewLogger(t))

	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "sleep"))
	assert.Equal(t, 0, called)

	failNext = true
	require.Error(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "wake-fails"))
	assert.Equal(t, 0, called, "a failed wake attempt itself must not fire the hook")

	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "wake-retry"))
	assert.Equal(t, 1, called, "the retry after a failed wake must fire exactly once")
}

// TestSetOnAwake_InvalidateDuringAwakeHoursFiresZero: a CDP reconnect during
// awake hours (the schedule's cached desired state is awake) must invalidate
// the tracker to playerFreshDocument with desiredAtInvalidate==awake, and the
// subsequent awake re-drive must NOT fire onAwake — the reconnect
// double-push this predicate exists to suppress.
func TestSetOnAwake_InvalidateDuringAwakeHoursFiresZero(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil).AnyTimes()

	e := &executor{cdp: mockCDP, logger: zaptest.NewLogger(t)}
	ctx := context.Background()
	called := 0
	SetOnAwake(e, func(context.Context) { called++ }, zaptest.NewLogger(t))

	// The schedule loop's per-tick cache, as it would be during awake hours.
	e.sleepApplyMu.Lock()
	e.sleepScheduleDesiredCache = sleepschedule.StateAwake
	e.sleepApplyMu.Unlock()

	e.invalidatePlayerSleepState()
	e.sleepApplyMu.Lock()
	assert.Equal(t, playerFreshDocument, e.sleepPlayerState)
	assert.Equal(t, sleepschedule.StateAwake, e.sleepDesiredAtInvalidate)
	e.sleepApplyMu.Unlock()

	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateAwake, "reconnect-redrive"))
	assert.Equal(t, 0, called, "invalidate during awake hours must not fire the hook on the awake re-drive")
}

// TestSetOnAwake_RestartWhileSleepingWakeBoundaryFiresOnce is [NV8]: a
// restart while the schedule says sleeping — with the sleeping re-drive
// skipped or deferred, so the tracker is STILL playerFreshDocument when the
// wake boundary arrives — must still fire onAwake at that boundary. Without
// this disjunct the displayAt recompute would stay silent from boot to wake.
func TestSetOnAwake_RestartWhileSleepingWakeBoundaryFiresOnce(t *testing.T) {
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(map[string]any{"result": map[string]any{}}, nil).AnyTimes()

	e := &executor{cdp: mockCDP, logger: zaptest.NewLogger(t)}
	ctx := context.Background()
	called := 0
	SetOnAwake(e, func(context.Context) { called++ }, zaptest.NewLogger(t))

	// Restart at 03:00: the schedule's cached desired state is sleeping.
	e.sleepApplyMu.Lock()
	e.sleepScheduleDesiredCache = sleepschedule.StateSleeping
	e.sleepApplyMu.Unlock()
	e.invalidatePlayerSleepState()

	// The sleeping re-drive never lands before the wake boundary (deferred,
	// skipped, or simply overtaken by the schedule): the tracker is still
	// playerFreshDocument with desiredAtInvalidate==sleeping when 07:00
	// arrives and the loop applies awake.
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateAwake, "wake-boundary"))
	assert.Equal(t, 1, called, "restart-while-sleeping must still fire at the wake boundary")
}
