package cdphealth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/godbus"

	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeCDP is a settable cdpStatus.
type fakeCDP struct {
	initialized bool
}

func (f *fakeCDP) Initialized() bool { return f.initialized }

// fakeSender records every Send call.
type fakeSender struct {
	mu    sync.Mutex
	calls []godbus.DBusPayload
	err   error
}

func (f *fakeSender) Send(payload godbus.DBusPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, payload)
	return f.err
}

func (f *fakeSender) stuckValues() []bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]bool, len(f.calls))
	for i, c := range f.calls {
		out[i] = c.Body[0].(bool)
	}
	return out
}

// fakeClock is a settable wrapper.Clock; only Now is exercised by tick().
type fakeClock struct {
	now time.Time
}

func (c *fakeClock) Now() time.Time                                          { return c.now }
func (c *fakeClock) advance(d time.Duration)                                 { c.now = c.now.Add(d) }
func (c *fakeClock) Sleep(time.Duration)                                     {}
func (c *fakeClock) SleepContext(ctx context.Context, d time.Duration) error { return ctx.Err() }
func (c *fakeClock) NewTicker(time.Duration) wrapper.Ticker                  { panic("unused") }

func newTestMonitor(t *testing.T) (*Monitor, *fakeCDP, *fakeSender, *fakeClock) {
	t.Helper()
	cdp := &fakeCDP{initialized: true}
	bus := &fakeSender{}
	clock := &fakeClock{now: time.Date(2026, 9, 18, 15, 52, 24, 0, time.UTC)}
	m := New(cdp, bus, clock, zaptest.NewLogger(t))
	return m, cdp, bus, clock
}

func TestMonitor_NoSignalWhileConnected(t *testing.T) {
	m, _, bus, clock := newTestMonitor(t)

	for i := 0; i < 5; i++ {
		clock.advance(pollInterval)
		m.tick()
	}

	assert.Empty(t, bus.stuckValues(), "a healthy CDP connection must never emit cdp_stuck")
}

// goUnhealthy flips cdp to disconnected and runs the tick that marks
// unhealthySince — the reference point every threshold check below measures
// from — without itself being able to cross the threshold (pollInterval <<
// StuckThreshold).
func goUnhealthy(m *Monitor, cdp *fakeCDP) {
	cdp.initialized = false
	m.tick()
}

func TestMonitor_NoSignalBeforeThreshold(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)

	// Advance to just under StuckThreshold; the ffos-user#356 fix must not
	// fire faster than a legitimate kiosk restart or cold boot would take.
	clock.advance(StuckThreshold - time.Second)
	m.tick()

	assert.Empty(t, bus.stuckValues(), "must wait out the full threshold before reporting stuck")
}

func TestMonitor_ReportsStuckOnceAtThreshold(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)

	clock.advance(StuckThreshold)
	m.tick()
	require.Equal(t, []bool{true}, bus.stuckValues())

	// Still disconnected on later ticks: must not resend "stuck" every
	// interval (components/feral-watchdog/chromium.go's restartChromium
	// doc explains exactly why a repeated signal would be dangerous — it
	// would re-trigger a restart every tick instead of respecting the
	// watchdog's own startup-grace/restart-budget ladder).
	for i := 0; i < 5; i++ {
		clock.advance(pollInterval)
		m.tick()
	}
	assert.Equal(t, []bool{true}, bus.stuckValues(), "must not re-report while still stuck")
}

func TestMonitor_ClearsOnReconnect(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)

	clock.advance(StuckThreshold)
	m.tick()
	require.Equal(t, []bool{true}, bus.stuckValues())

	cdp.initialized = true
	m.tick()
	assert.Equal(t, []bool{true, false}, bus.stuckValues(), "reconnect must clear the signal exactly once")

	// A further healthy tick must not re-send the clear.
	m.tick()
	assert.Equal(t, []bool{true, false}, bus.stuckValues())
}

func TestMonitor_RelapseAfterRecoveryReportsAgain(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)
	clock.advance(StuckThreshold)
	m.tick()
	require.Equal(t, []bool{true}, bus.stuckValues())

	cdp.initialized = true
	m.tick()
	require.Equal(t, []bool{true, false}, bus.stuckValues())

	// A fresh, independent disconnect episode must report again — this is
	// what lets feral-watchdog's ChromiumMonitor react to a SECOND
	// unrelated blind-spot episode rather than only the first ever one.
	goUnhealthy(m, cdp)
	clock.advance(StuckThreshold)
	m.tick()
	assert.Equal(t, []bool{true, false, true}, bus.stuckValues())
}

func TestMonitor_SendErrorIsLoggedNotFatal(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	bus.err = assertError{}
	goUnhealthy(m, cdp)

	clock.advance(StuckThreshold)
	assert.NotPanics(t, m.tick, "a DBus send failure must not crash the health poller")
}

type assertError struct{}

func (assertError) Error() string { return "dbus: send failed" }

// TestMonitor_UsesControldOwnIdentity guards the exact wire identity
// feral-watchdog is wired to match in its own duplicated constants
// (components/feral-watchdog/main.go's second WithMatchPathNamespace and
// mediator.go's DBUS_CONTROLD_EVENT_CDP_STUCK) — a drift here silently
// breaks the cross-module contract with no compiler to catch it.
func TestMonitor_UsesControldOwnIdentity(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)
	clock.advance(StuckThreshold)
	m.tick()

	require.Len(t, bus.calls, 1)
	assert.Equal(t, dbus.INTERFACE, bus.calls[0].Interface)
	assert.Equal(t, dbus.PATH, bus.calls[0].Path)
	assert.Equal(t, dbus.EVENT_CDP_STUCK, bus.calls[0].Member)
}
