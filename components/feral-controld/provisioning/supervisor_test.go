package provisioning

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeClock is a settable wrapper.Clock with a manually driven ticker.
type fakeClock struct {
	mu   sync.Mutex
	now  time.Time
	tick chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1_700_000_000, 0), tick: make(chan time.Time, 1)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) Sleep(time.Duration) {}

// SleepContext returns immediately (no real delay) unless ctx is already done,
// so supervisor backoff does not slow tests.
func (c *fakeClock) SleepContext(ctx context.Context, _ time.Duration) error {
	return ctx.Err()
}

func (c *fakeClock) NewTicker(time.Duration) wrapper.Ticker { return &fakeTicker{c: c.tick} }

type fakeTicker struct{ c chan time.Time }

func (t *fakeTicker) Reset(time.Duration) {}
func (t *fakeTicker) Stop()               {}
func (t *fakeTicker) C() <-chan time.Time { return t.c }

func TestSupervisorRestartsOnPanic(t *testing.T) {
	clk := newFakeClock()
	sup := newSupervisor(clk, zap.NewNop())

	var mu sync.Mutex
	calls := 0
	// Panic on the first three runs, then return normally on the fourth.
	fn := func(context.Context) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n <= 3 {
			panic("boom")
		}
	}

	sup.run(context.Background(), "test", fn)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, 4, calls, "should run until a clean return")
	assert.Equal(t, int64(3), sup.restartCount(), "one restart per panic")
}

func TestSupervisorStopsOnContextCancel(t *testing.T) {
	clk := newFakeClock()
	sup := newSupervisor(clk, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	var mu sync.Mutex
	calls := 0
	fn := func(context.Context) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			cancel() // cancel, then panic: backoff sleep must observe cancellation
		}
		panic("boom")
	}

	sup.run(ctx, "test", fn)

	mu.Lock()
	defer mu.Unlock()
	// One run, one recovered panic, then the backoff sleep sees ctx done and run
	// returns without invoking fn again.
	require.Equal(t, 1, calls)
	assert.Equal(t, int64(1), sup.restartCount())
}
