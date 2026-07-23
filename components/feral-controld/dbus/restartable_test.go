package dbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/feral-file/godbus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeRaw stands in for a godbus client. It mirrors the contract Restartable
// relies on: handler registration is an in-memory append valid before Start,
// Start either fully succeeds or leaves partial state that Stop tears down,
// and Call only works on a started client.
type fakeRaw struct {
	mu       sync.Mutex
	startErr error
	started  bool
	stopped  bool
	handlers []godbus.BusSignalHandler
}

func (f *fakeRaw) Start() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.startErr != nil {
		return f.startErr
	}
	f.started = true
	return nil
}

func (f *fakeRaw) Stop() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stopped = true
	return nil
}

func (f *fakeRaw) Export(_ interface{}, _ godbus.Path, _ godbus.Interface) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started {
		return errors.New("not started")
	}
	return nil
}

func (f *fakeRaw) Call(_ context.Context, _ string, _ godbus.Path, _ godbus.Interface, _ godbus.Member, _ ...any) ([]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.started || f.stopped {
		return nil, errors.New("not started")
	}
	return []any{true}, nil
}

func (f *fakeRaw) OnBusSignal(h godbus.BusSignalHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers = append(f.handlers, h)
}

func (f *fakeRaw) RemoveBusSignal(h godbus.BusSignalHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := range f.handlers {
		if fmt.Sprintf("%p", f.handlers[i]) == fmt.Sprintf("%p", h) {
			f.handlers = append(f.handlers[:i], f.handlers[i+1:]...)
			return
		}
	}
}

// deliver invokes every registered handler once, as the bus would for one
// signal. The returned count is the delivery multiplicity a real signal would
// see — the accumulation regression asserts it stays 1.
func (f *fakeRaw) deliver() int {
	f.mu.Lock()
	handlers := append([]godbus.BusSignalHandler(nil), f.handlers...)
	f.mu.Unlock()
	for _, h := range handlers {
		_, _ = h(context.Background(), godbus.DBusPayload{})
	}
	return len(handlers)
}

func (f *fakeRaw) snapshot() (started, stopped bool, handlerCount int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.started, f.stopped, len(f.handlers)
}

// TestRestartableLifecycle is the regression test for the unsafe D-Bus
// recovery path: a failed initial Start, repeated name-conflict style
// failures, live concurrent consumers throughout, a successful retry, and
// shutdown — with the invariants that failed attempts are torn down (no
// registration accumulation on the shared bus), a signal is delivered to a
// handler exactly once, and the whole sequence is race-detector clean.
func TestRestartableLifecycle(t *testing.T) {
	const failedAttempts = 3 // session-bus race, then repeated name conflicts

	var mu sync.Mutex
	var clients []*fakeRaw
	factory := func() DBus {
		f := &fakeRaw{}
		mu.Lock()
		defer mu.Unlock()
		if len(clients) == 0 {
			f.startErr = errors.New("session bus unavailable")
		} else if len(clients) < failedAttempts {
			f.startErr = errors.New("failed to request name: 3")
		}
		clients = append(clients, f)
		return f
	}

	r := NewRestartable(zap.NewNop(), factory)

	// A handler registered before the bus is up (mediator/provisioning do this
	// at startup) must land exactly once on whichever client finally starts.
	var deliveries int
	handler := func(_ context.Context, _ godbus.DBusPayload) ([]any, error) {
		mu.Lock()
		deliveries++
		mu.Unlock()
		return nil, nil
	}
	r.OnBusSignal(handler)

	// Concurrent consumers hammer Call across every phase — this is the
	// Start-vs-Call race the adapter exists to close; the race detector is the
	// assertion.
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				_, _ = r.Call(ctx, "svc", "/path", "iface", "method")
			}
		}()
	}

	// Failed attempts: Call must keep erroring (degraded mode), and each
	// abandoned client must have been torn down so its registrations on the
	// shared bus die with its connection.
	for i := range failedAttempts {
		require.Error(t, r.Start(), "attempt %d should fail", i)
		_, err := r.Call(ctx, "svc", "/path", "iface", "method")
		require.Error(t, err, "Call must degrade while the bus is down")
	}

	// Successful retry.
	require.NoError(t, r.Start())
	require.NoError(t, r.Start(), "Start on a started client is a no-op")

	_, err := r.Call(ctx, "svc", "/path", "iface", "method")
	require.NoError(t, err, "Call must work once a retry lands")

	cancel()
	wg.Wait()

	mu.Lock()
	attempts := len(clients)
	mu.Unlock()
	require.Equal(t, failedAttempts+1, attempts, "one fresh client per attempt")

	// Every failed attempt was torn down; only the last client is live.
	for i, c := range clients[:failedAttempts] {
		started, stopped, _ := c.snapshot()
		assert.False(t, started, "failed client %d must not be live", i)
		assert.True(t, stopped, "failed client %d must be torn down", i)
	}
	live := clients[failedAttempts]
	started, stopped, handlerCount := live.snapshot()
	require.True(t, started)
	require.False(t, stopped)

	// The accumulation regression: after N failed attempts the live client
	// holds the handler exactly once, so one signal delivers exactly once.
	require.Equal(t, 1, handlerCount, "handler must be registered exactly once, not once per retry")
	live.deliver()
	mu.Lock()
	require.Equal(t, 1, deliveries, "one signal must reach the handler exactly once")
	mu.Unlock()

	// Shutdown: the live client stops, consumers degrade again, and a late
	// retry (e.g. a background loop losing the shutdown race) cannot
	// resurrect the bus after Stop.
	require.NoError(t, r.Stop())
	_, stoppedNow, _ := live.snapshot()
	require.True(t, stoppedNow)
	_, err = r.Call(context.Background(), "svc", "/path", "iface", "method")
	require.Error(t, err)
	require.Error(t, r.Start(), "Start after Stop must not restart")
	mu.Lock()
	require.Equal(t, failedAttempts+1, len(clients), "no new client may be built after Stop")
	mu.Unlock()
}

// Handlers registered while the client is live must be delegated immediately,
// and removal must work in both the recorded set and the live client so a
// later restart does not resurrect removed handlers.
func TestRestartableHandlerDelegation(t *testing.T) {
	var current *fakeRaw
	r := NewRestartable(zap.NewNop(), func() DBus {
		current = &fakeRaw{}
		return current
	})
	require.NoError(t, r.Start())

	h := func(_ context.Context, _ godbus.DBusPayload) ([]any, error) { return nil, nil }
	r.OnBusSignal(h)
	_, _, count := current.snapshot()
	require.Equal(t, 1, count, "live registration must delegate")

	r.RemoveBusSignal(h)
	_, _, count = current.snapshot()
	require.Equal(t, 0, count, "removal must delegate")
	r.mu.Lock()
	require.Empty(t, r.handlers, "removal must also drop the recorded copy")
	r.mu.Unlock()
}
