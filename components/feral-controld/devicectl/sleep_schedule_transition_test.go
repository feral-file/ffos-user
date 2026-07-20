package devicectl

import (
	"context"
	"encoding/json"
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
