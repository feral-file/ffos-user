package dbus

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"
)

// Restartable makes a failed godbus Start safely retryable. A godbus client
// must NOT be re-Started in place:
//   - Start() writes the connection field without the client mutex while
//     Call/Send/Export/Stop read it under the mutex — a data race once a
//     retry runs concurrently with live calls.
//   - Start() re-registers the signal channel and match rules on the shared
//     session-bus connection with no removal path, so every retry after a
//     partial failure (e.g. a name conflict) adds a permanent duplicate
//     delivery.
//   - Stop() closes the client's done channel permanently, so a Stop→Start
//     cycle leaves the new signal-delivery goroutine stillborn.
//
// Restartable sidesteps all three by building a FRESH underlying client per
// Start attempt and only publishing it — under this wrapper's mutex — once it
// is fully started, so consumers never observe a half-started client. A failed
// attempt is torn down with Stop(), which closes the shared session-bus
// connection and takes the attempt's channel/match registrations down with it;
// dbus v5's SessionBus() redials a closed cached connection, so the next
// attempt starts clean. Signal handlers registered while the bus is down are
// recorded here and replayed onto whichever client finally starts.
//
// The mutex is held for the full duration of a Start attempt, so a concurrent
// Call blocks until the attempt resolves. Attempts are rare (startup retries
// only) and a session-bus dial fails fast when the socket is absent; the
// underlying client already serializes whole Calls under its own mutex, so
// this adds no new order of magnitude of contention.
type Restartable struct {
	mu      sync.Mutex
	factory func() DBus
	logger  *zap.Logger

	inner    DBus // nil until a Start attempt succeeds, nil again after Stop
	handlers []godbus.BusSignalHandler
	stopped  bool
}

func NewRestartable(logger *zap.Logger, factory func() DBus) *Restartable {
	return &Restartable{factory: factory, logger: logger}
}

func (r *Restartable) Start() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.stopped {
		return fmt.Errorf("dbus: client stopped")
	}
	if r.inner != nil {
		return nil
	}

	raw := r.factory()
	// Replay before Start so no signal can slip between the bus connecting and
	// the handlers being attached (registration is an in-memory append that is
	// valid on an unstarted client).
	for _, h := range r.handlers {
		raw.OnBusSignal(h)
	}
	if err := raw.Start(); err != nil {
		// Tear down whatever the attempt registered on the shared session-bus
		// connection before abandoning the client (see the type comment).
		if serr := raw.Stop(); serr != nil {
			r.logger.Warn("Failed to tear down partially-started DBus client", zap.Error(serr))
		}
		return err
	}
	r.inner = raw
	return nil
}

func (r *Restartable) Stop() error {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.stopped = true
	if r.inner == nil {
		return nil
	}
	err := r.inner.Stop()
	r.inner = nil
	return err
}

// live returns the published client, or nil while the bus is down.
func (r *Restartable) live() DBus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.inner
}

func (r *Restartable) Export(obj interface{}, path godbus.Path, iface godbus.Interface) error {
	inner := r.live()
	if inner == nil {
		return fmt.Errorf("dbus: client not started")
	}
	return inner.Export(obj, path, iface)
}

func (r *Restartable) Call(ctx context.Context, name string, path godbus.Path, iface godbus.Interface, method godbus.Member, args ...any) ([]any, error) {
	inner := r.live()
	if inner == nil {
		return nil, fmt.Errorf("dbus: client not started")
	}
	return inner.Call(ctx, name, path, iface, method, args...)
}

func (r *Restartable) OnBusSignal(handler godbus.BusSignalHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.handlers = append(r.handlers, handler)
	if r.inner != nil {
		r.inner.OnBusSignal(handler)
	}
}

func (r *Restartable) RemoveBusSignal(handler godbus.BusSignalHandler) {
	r.mu.Lock()
	defer r.mu.Unlock()

	// Pointer identity, matching the underlying client's registration
	// semantics. Method values of the same method compare equal, which is what
	// the mediator's register/remove pairing relies on.
	p := reflect.ValueOf(handler).Pointer()
	for i, existing := range r.handlers {
		if reflect.ValueOf(existing).Pointer() == p {
			r.handlers = append(r.handlers[:i], r.handlers[i+1:]...)
			break
		}
	}
	if r.inner != nil {
		r.inner.RemoveBusSignal(handler)
	}
}
