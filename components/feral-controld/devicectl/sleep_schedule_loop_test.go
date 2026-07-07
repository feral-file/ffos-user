package devicectl

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
)

// flakyPanelDDC fails its first failN ApplyControl calls, then succeeds,
// recording every successful power value. Used to prove the loop retries a
// transient panel (DDC) failure without re-driving the player. An optional
// delay widens the worker's in-flight window so concurrent enqueues coalesce.
type flakyPanelDDC struct {
	failN int
	delay time.Duration

	mu       sync.Mutex
	attempts int
	powers   []string
}

func (f *flakyPanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	return &ddc.DdcPanelStatus{}, nil
}

func (f *flakyPanelDDC) ApplyControl(ctx context.Context, action ddc.DdcPanelAction, value json.RawMessage) error {
	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.attempts++
	if f.attempts <= f.failN {
		return errors.New("ddcutil transient failure")
	}
	f.powers = append(f.powers, string(value))
	return nil
}

func (f *flakyPanelDDC) appliedPowers() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.powers))
	copy(out, f.powers)
	return out
}

// TestApplySleepTransitionIfChanged_SkipsUnchangedHealthyState proves the loop's
// de-dup wrapper does not re-drive the player once a state has been applied
// successfully, so the capped poll ticks stay cheap.
func TestApplySleepTransitionIfChanged_SkipsUnchangedHealthyState(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	// Exactly one player round-trip despite three IfChanged calls for the same state.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]any{"result": map[string]any{}}, nil).
		Times(1)

	e := &executor{
		cdp:      mockCDP,
		panelDDC: &tinyDelayPanelDDC{},
		logger:   zaptest.NewLogger(t),
	}

	ctx := context.Background()
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick-1"))
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick-2"))
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick-3"))
}

// TestApplySleepTransitionIfChanged_RetriesAfterFailure proves a failed transition
// (e.g. the player page not yet ready at boot) is retried on the next tick rather
// than being treated as applied and dropped until the next schedule boundary.
func TestApplySleepTransitionIfChanged_RetriesAfterFailure(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	gomock.InOrder(
		// First tick: player not ready -> failure.
		mockCDP.EXPECT().
			Send(cdp.METHOD_EVALUATE, gomock.Any()).
			Return(nil, errors.New("handleCDPRequest is not defined")),
		// Second tick: retried and succeeds.
		mockCDP.EXPECT().
			Send(cdp.METHOD_EVALUATE, gomock.Any()).
			Return(map[string]any{"result": map[string]any{}}, nil),
	)

	e := &executor{
		cdp:      mockCDP,
		panelDDC: &tinyDelayPanelDDC{},
		logger:   zaptest.NewLogger(t),
	}

	ctx := context.Background()

	// Tick 1: apply fails and is recorded as not-applied.
	require.Error(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick-1"))
	// Tick 2: same desired state, but the prior failure forces a retry that succeeds.
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick-2"))
	// Tick 3: now healthy and unchanged -> no further player round-trip (asserted by Times above).
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick-3"))
}

// TestApplySleepTransitionIfChanged_RetriesPanelWithoutRedrivingPlayer proves a
// transient panel (DDC) failure is retried by later ticks while the player leg,
// already in the desired state, is not re-driven.
func TestApplySleepTransitionIfChanged_RetriesPanelWithoutRedrivingPlayer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	// The player is driven exactly once: the initial full transition. Panel retries
	// must not trigger any further player round-trip.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]any{"result": map[string]any{}}, nil).
		Times(1)

	panel := &flakyPanelDDC{failN: 1}
	e := &executor{
		cdp:      mockCDP,
		panelDDC: panel,
		logger:   zaptest.NewLogger(t),
	}

	ctx := context.Background()
	// Drive the loop's entry point repeatedly, as the capped poll would. The first
	// call is a full transition (player + panel); the panel's first attempt fails,
	// so subsequent ticks re-enqueue the panel alignment until it succeeds.
	require.Eventually(t, func() bool {
		require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick"))
		return len(panel.appliedPowers()) == 1
	}, 3*time.Second, 20*time.Millisecond, "expected panel to reach standby after retry")

	require.Equal(t, []string{`"standby"`}, panel.appliedPowers())
}

// TestApplySleepTransition_ConcurrentManualAndLoopIsRaceFree hammers the manual
// (applySleepTransition) and loop (applySleepTransitionIfChanged) entry points
// concurrently to confirm the shared apply/tracker path is serialized and free of
// data races and deadlocks (run under -race), and that the serialization contract
// holds: panel enqueues are ordered with state decisions, so once the align
// worker drains, the last DDC power applied matches the last player-commanded
// state — no stale panel-only retry may supersede a newer transition.
func TestApplySleepTransition_ConcurrentManualAndLoopIsRaceFree(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		AnyTimes().
		Return(map[string]any{"result": map[string]any{}}, nil)

	panel := &flakyPanelDDC{delay: 200 * time.Microsecond}
	e := &executor{
		cdp:      mockCDP,
		panelDDC: panel,
		logger:   zaptest.NewLogger(t),
	}

	ctx := context.Background()
	const goroutines = 16
	const iters = 50
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
				if g%2 == 0 {
					_ = e.applySleepTransition(ctx, st, "manual")
				} else {
					_ = e.applySleepTransitionIfChanged(ctx, st, "loop")
				}
			}
		}(g)
	}

	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("concurrent sleep transitions deadlocked")
	}

	// No more state decisions happen past this point, so the last enqueued panel
	// job must carry the final player-commanded state. Wait for the worker to
	// drain, then check the last power actually applied.
	e.sleepApplyMu.Lock()
	require.NotNil(t, e.sleepAppliedState)
	finalState := *e.sleepAppliedState
	e.sleepApplyMu.Unlock()

	require.Eventually(t, func() bool {
		e.sleepApplyMu.Lock()
		defer e.sleepApplyMu.Unlock()
		return e.sleepPanelOK && e.sleepPanelState != nil && *e.sleepPanelState == finalState
	}, 5*time.Second, 10*time.Millisecond, "panel should settle on the last commanded state")

	wantPower := `"on"`
	if finalState == sleepschedule.StateSleeping {
		wantPower = `"standby"`
	}
	powers := panel.appliedPowers()
	require.NotEmpty(t, powers)
	require.Equal(t, wantPower, powers[len(powers)-1],
		"last DDC power command must match the last commanded sleep state")
}

// scriptedPanelDDC blocks each ApplyControl until the test feeds it an outcome
// through release, so a test can hold the align worker mid-flight and interleave
// transitions at exact points. Successful applies record their power value.
type scriptedPanelDDC struct {
	entered chan struct{}
	release chan error

	mu     sync.Mutex
	powers []string
}

func (s *scriptedPanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	return &ddc.DdcPanelStatus{}, nil
}

func (s *scriptedPanelDDC) ApplyControl(ctx context.Context, action ddc.DdcPanelAction, value json.RawMessage) error {
	s.entered <- struct{}{}
	if err := <-s.release; err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.powers = append(s.powers, string(value))
	return nil
}

func (s *scriptedPanelDDC) appliedPowers() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, len(s.powers))
	copy(out, s.powers)
	return out
}

// TestApplySleepTransitionIfChanged_ManualSupersedesStalePanelRetry drives the
// exact interleaving where the loop's panel-only retry races a newer manual
// transition: the player is asleep with the panel leg behind, a schedule tick
// re-enqueues the stale standby and the worker picks it up, and while that
// attempt is still in flight a manual wake applies awake and enqueues panel
// "on". The manual command must be what the panel ultimately receives — the
// stale retry must not supersede it.
func TestApplySleepTransitionIfChanged_ManualSupersedesStalePanelRetry(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	// Exactly two player round-trips: the initial sleep and the manual wake. The
	// panel-only retry must not re-drive the player.
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]any{"result": map[string]any{}}, nil).
		Times(2)

	panel := &scriptedPanelDDC{entered: make(chan struct{}), release: make(chan error)}
	e := &executor{
		cdp:      mockCDP,
		panelDDC: panel,
		logger:   zaptest.NewLogger(t),
	}
	ctx := context.Background()

	waitEntered := func(msg string) {
		t.Helper()
		select {
		case <-panel.entered:
		case <-time.After(2 * time.Second):
			t.Fatal(msg)
		}
	}

	// Initial transition: player goes to sleep; the panel standby attempt fails,
	// leaving the panel leg behind so the next tick takes the panel-only branch.
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateSleeping, "manual-sleep"))
	waitEntered("expected the initial panel standby attempt")
	panel.release <- errors.New("ddcutil transient failure")
	require.Eventually(t, func() bool {
		e.sleepApplyMu.Lock()
		defer e.sleepApplyMu.Unlock()
		return e.sleepPanelState != nil && *e.sleepPanelState == sleepschedule.StateSleeping && !e.sleepPanelOK
	}, 2*time.Second, 5*time.Millisecond, "panel failure should be recorded")

	// Schedule tick: player already aligned, panel behind -> panel-only retry.
	// The worker picks the stale standby up and blocks mid-flight.
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "schedule-loop"))
	waitEntered("expected the stale panel standby retry")

	// Manual wake lands while the stale standby retry is still in flight: the
	// player is driven awake and panel "on" is enqueued behind the retry.
	require.NoError(t, e.applySleepTransition(ctx, sleepschedule.StateAwake, "manual-wake"))

	// Finish the stale retry (failing); the worker must then apply the newer
	// manual "on" rather than any re-coalesced standby.
	panel.release <- errors.New("ddcutil transient failure")
	waitEntered("expected the manual panel on attempt after the stale retry")
	panel.release <- nil

	require.Eventually(t, func() bool {
		p := panel.appliedPowers()
		return len(p) == 1 && p[0] == `"on"`
	}, 2*time.Second, 5*time.Millisecond, "the manual wake's panel command must win")

	// Both legs are now aligned to awake: a further tick must not enqueue another
	// panel attempt or player round-trip.
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateAwake, "tick"))
	select {
	case <-panel.entered:
		t.Fatal("no further panel attempt expected once both legs are aligned")
	case <-time.After(200 * time.Millisecond):
	}
}

// blockingFailPanelDDC always fails, and blocks each ApplyControl until the test
// releases it, so the test can step the async align worker one attempt at a time.
type blockingFailPanelDDC struct {
	entered chan struct{}
	release chan struct{}

	mu       sync.Mutex
	attempts int
}

func (b *blockingFailPanelDDC) CollectStatus(ctx context.Context) (*ddc.DdcPanelStatus, error) {
	return &ddc.DdcPanelStatus{}, nil
}

func (b *blockingFailPanelDDC) ApplyControl(ctx context.Context, action ddc.DdcPanelAction, value json.RawMessage) error {
	b.mu.Lock()
	b.attempts++
	b.mu.Unlock()
	b.entered <- struct{}{}
	<-b.release
	return errors.New("ddcutil: display does not support power control")
}

func (b *blockingFailPanelDDC) attemptCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.attempts
}

// TestApplySleepTransitionIfChanged_PanelRetryIsBounded proves a panel that can
// never do DDC power control is retried only panelRetryMax times, then left alone
// until the next real transition — so an upgraded old device is not hammered with
// ddcutil every tick. The player is driven exactly once.
func TestApplySleepTransitionIfChanged_PanelRetryIsBounded(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		Return(map[string]any{"result": map[string]any{}}, nil).
		Times(1)

	panel := &blockingFailPanelDDC{entered: make(chan struct{}), release: make(chan struct{})}
	e := &executor{
		cdp:      mockCDP,
		panelDDC: panel,
		logger:   zaptest.NewLogger(t),
	}
	ctx := context.Background()

	// Step the worker through one failing attempt and wait for the streak to be
	// recorded, so the next tick observes the updated streak deterministically.
	drive := func(wantStreak int) {
		select {
		case <-panel.entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("expected a panel ApplyControl attempt for streak %d", wantStreak)
		}
		panel.release <- struct{}{}
		require.Eventually(t, func() bool {
			e.sleepApplyMu.Lock()
			defer e.sleepApplyMu.Unlock()
			return e.sleepPanelFailStreak == wantStreak
		}, 2*time.Second, 5*time.Millisecond, "fail streak should reach %d", wantStreak)
	}

	// Each tick enqueues a panel alignment (the first is a full transition), up to
	// the retry cap.
	for s := 1; s <= panelRetryMax; s++ {
		require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick"))
		drive(s)
	}

	// Cap reached: a further tick must not enqueue another panel attempt.
	require.NoError(t, e.applySleepTransitionIfChanged(ctx, sleepschedule.StateSleeping, "tick-capped"))
	select {
	case <-panel.entered:
		t.Fatal("panel retry should be capped; no further ddcutil attempt expected")
	case <-time.After(200 * time.Millisecond):
	}

	require.Equal(t, panelRetryMax, panel.attemptCount())
}

func TestSleepScheduleTickWait(t *testing.T) {
	// nil next transition -> capped tick, never the 0 "wait forever" sentinel.
	require.Equal(t, sleepScheduleMaxTick, sleepScheduleTickWait(nil))

	// A transition far in the future is capped at the max tick.
	far := time.Now().Add(3 * time.Hour)
	require.Equal(t, sleepScheduleMaxTick, sleepScheduleTickWait(&far))

	// A past-due transition floors at 1s (re-check promptly, never block on 0).
	past := time.Now().Add(-2 * time.Hour)
	require.Equal(t, time.Second, sleepScheduleTickWait(&past))

	// A near-future transition waits roughly until it, below the cap.
	near := time.Now().Add(10 * time.Second)
	got := sleepScheduleTickWait(&near)
	require.Greater(t, got, time.Second)
	require.LessOrEqual(t, got, 10*time.Second)
}
