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

// fakeSender records every Send call. alwaysFail returns err on every call
// (the existing "transport is down" shape). failNext, when >0, fails that
// many calls (decrementing each time) before succeeding — for pinning the
// retry-after-transient-failure behavior (ffos-user#356 review F3).
type fakeSender struct {
	mu         sync.Mutex
	calls      []godbus.DBusPayload
	err        error
	alwaysFail bool
	failNext   int
}

func (f *fakeSender) Send(payload godbus.DBusPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, payload)
	if f.alwaysFail {
		return f.err
	}
	if f.failNext > 0 {
		f.failNext--
		return f.err
	}
	return nil
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

	// Exactly one confirmation on the very first tick (ffos-user#356,
	// feralfile-bot's second-pass fix): a connection healthy from the
	// start must still send one explicit "not stuck" so feral-watchdog has
	// a positive signal to wait for rather than inferring health from
	// silence plus a local timeout. No repeats after that.
	assert.Equal(t, []bool{false}, bus.stuckValues(), "a healthy CDP connection must send exactly one first-ever confirmation, then stay silent")
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

	// Still disconnected on later ticks, but well under another full
	// threshold: must not resend "stuck" every interval
	// (components/feral-watchdog/chromium.go's restartChromium doc explains
	// exactly why a repeated signal every tick would be dangerous — it
	// would re-trigger a restart every tick instead of respecting the
	// watchdog's own startup-grace/restart-budget ladder). See
	// TestMonitor_ReaffirmsStuckPeriodicallyWhileUnresolved for the
	// once-per-threshold re-affirmation this does NOT suppress.
	for i := 0; i < 5; i++ {
		clock.advance(pollInterval)
		m.tick()
	}
	assert.Equal(t, []bool{true}, bus.stuckValues(), "must not re-report while still stuck")
}

// TestMonitor_ReaffirmsStuckPeriodicallyWhileUnresolved pins ffos-user#356
// review round 2, F1: feral-watchdog's cdpStuck is in-memory only and ships
// on a Restart=always unit restarted independently of controld (package
// updates, crashes) — if that restart lands mid-episode, the fresh
// ChromiumMonitor has no way to learn the episode is still live unless
// controld keeps re-affirming it, since Initialized() never toggles true in
// between to naturally trigger a fresh report.
func TestMonitor_ReaffirmsStuckPeriodicallyWhileUnresolved(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)

	clock.advance(StuckThreshold)
	m.tick()
	require.Equal(t, []bool{true}, bus.stuckValues())

	// One more full threshold with no reconnect: must re-affirm exactly once.
	clock.advance(StuckThreshold)
	m.tick()
	assert.Equal(t, []bool{true, true}, bus.stuckValues(), "expected one re-affirmation after a full threshold still unresolved")

	// A further short gap must not re-affirm again early.
	clock.advance(pollInterval)
	m.tick()
	assert.Equal(t, []bool{true, true}, bus.stuckValues())

	clock.advance(StuckThreshold)
	m.tick()
	assert.Equal(t, []bool{true, true, true}, bus.stuckValues(), "expected a second re-affirmation after another full threshold")
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
	bus.alwaysFail = true
	goUnhealthy(m, cdp)

	clock.advance(StuckThreshold)
	assert.NotPanics(t, m.tick, "a DBus send failure must not crash the health poller")
}

// TestMonitor_FailedSendRetriesNextTick pins ffos-user#356 review F3: a
// transient Send failure right at the threshold-crossing tick must not
// permanently drop the report for that episode — reported/unhealthySince
// must stay as if nothing was sent, so the very next tick retries and
// eventually gets the signal out.
func TestMonitor_FailedSendRetriesNextTick(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	bus.err = assertError{}
	bus.failNext = 1
	goUnhealthy(m, cdp)

	clock.advance(StuckThreshold)
	m.tick()
	require.Len(t, bus.calls, 1, "one send attempt, which failed")
	assert.False(t, m.reported, "a failed send must not latch reported, or the episode's signal is dropped forever")

	// A later tick, still past threshold, must retry and succeed this time.
	clock.advance(pollInterval)
	m.tick()
	require.Equal(t, []bool{true, true}, bus.stuckValues(), "expected one failed attempt then one successful retry, both for stuck=true")
	assert.True(t, m.reported)

	// Must not keep re-sending once delivery succeeded.
	clock.advance(pollInterval)
	m.tick()
	assert.Len(t, bus.calls, 2, "no further sends once delivery succeeded")
}

// TestMonitor_FailedClearRetriesNextTick is the symmetric case: a failed
// clearing emit(false) on reconnect must not be silently treated as
// delivered, or feral-watchdog's cdpStuck latch would stay true forever.
func TestMonitor_FailedClearRetriesNextTick(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)
	clock.advance(StuckThreshold)
	m.tick()
	require.Equal(t, []bool{true}, bus.stuckValues())

	bus.err = assertError{}
	bus.failNext = 1
	cdp.initialized = true
	m.tick()
	require.Len(t, bus.calls, 2, "one failed clear attempt recorded")
	assert.True(t, m.reported, "a failed clear must not drop the reported latch, or feral-watchdog's cdpStuck never clears")

	m.tick()
	assert.Equal(t, []bool{true, false, false}, bus.stuckValues(), "one failed clear attempt, then one successful retry")
	assert.False(t, m.reported)
}

type assertError struct{}

func (assertError) Error() string { return "dbus: send failed" }

// TestMonitor_UsesControldOwnIdentity guards the exact wire identity
// feral-watchdog is wired to match in its own duplicated constant
// (components/feral-watchdog/mediator.go's DBUS_CONTROLD_EVENT_CDP_STUCK,
// received over the common /com/feralfile match namespace set up in
// feral-watchdog/main.go) — a drift here silently breaks the cross-module
// contract with no compiler to catch it.
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

// TestMonitor_SendsFirstConfirmationExactlyOnce pins the exact mechanism
// behind the feralfile-bot fix (ffos-user#356, second review pass):
// feral-watchdog's canForgetRebootBudget gate must never infer health from
// silence plus elapsed local time, because cdphealth's own StuckThreshold
// timer is anchored to when THIS process first observes a disconnect — a
// clock with no shared epoch or ordering guarantee against watchdog's own.
// The fix is this one explicit, one-time confirmation on first-ever health:
// watchdog waits for it instead of waiting out its own clock.
func TestMonitor_SendsFirstConfirmationExactlyOnce(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)

	m.tick()
	require.Equal(t, []bool{false}, bus.stuckValues(), "the first-ever healthy tick must send one explicit confirmation")

	for i := 0; i < 5; i++ {
		clock.advance(pollInterval)
		m.tick()
	}
	assert.Equal(t, []bool{false}, bus.stuckValues(), "must not repeat the confirmation on later healthy ticks")

	// A later disconnect-then-reconnect episode still gets its own
	// true/false pair — everConfirmed only ever suppresses the FIRST
	// confirmation's duplicate, not a genuine later transition.
	goUnhealthy(m, cdp)
	clock.advance(StuckThreshold)
	m.tick()
	cdp.initialized = true
	m.tick()
	assert.Equal(t, []bool{false, true, false}, bus.stuckValues())
}

// TestMonitor_FailedFirstConfirmationRetries pins the retry half: a failed
// Send on the very first healthy tick (before everConfirmed is ever true)
// must not be silently treated as delivered, or feral-watchdog never
// receives the one positive signal it is now waiting for.
func TestMonitor_FailedFirstConfirmationRetries(t *testing.T) {
	m, _, bus, _ := newTestMonitor(t)
	bus.err = assertError{}
	bus.failNext = 1

	m.tick()
	require.Len(t, bus.calls, 1, "one failed attempt")
	assert.False(t, m.everConfirmed, "a failed send must not latch everConfirmed, or the one positive signal watchdog needs is lost")

	m.tick()
	assert.Equal(t, []bool{false, false}, bus.stuckValues(), "expected one failed attempt then one successful retry")
	assert.True(t, m.everConfirmed)
}

// TestMonitor_ReaffirmsHealthyConfirmationPeriodically pins ffos-user#356
// feralfile-bot review F3: a watchdog that subscribes (or restarts) AFTER
// controld's one-time first-ever confirmation already fired — the startup
// script starts feral-watchdog last, so this is the ordinary boot case, not
// an edge case — would otherwise wait forever for a signal controld will
// never resend while CDP just stays healthy. Mirrors
// TestMonitor_ReaffirmsStuckPeriodicallyWhileUnresolved's shape exactly, for
// the opposite (healthy) state.
func TestMonitor_ReaffirmsHealthyConfirmationPeriodically(t *testing.T) {
	m, _, bus, clock := newTestMonitor(t)

	m.tick()
	require.Equal(t, []bool{false}, bus.stuckValues(), "first-ever healthy tick must confirm")

	// Under a full threshold since the last confirmation: no resend yet.
	for i := 0; i < 5; i++ {
		clock.advance(pollInterval)
		m.tick()
	}
	assert.Equal(t, []bool{false}, bus.stuckValues(), "must not resend before a full threshold has elapsed")

	// A full threshold with no disconnect in between: must re-confirm, so a
	// late-subscribing or freshly-restarted watchdog eventually sees it.
	clock.advance(StuckThreshold)
	m.tick()
	assert.Equal(t, []bool{false, false}, bus.stuckValues(), "expected one re-confirmation after a full threshold of staying healthy")

	// A further short gap must not re-confirm again early.
	clock.advance(pollInterval)
	m.tick()
	assert.Equal(t, []bool{false, false}, bus.stuckValues())

	clock.advance(StuckThreshold)
	m.tick()
	assert.Equal(t, []bool{false, false, false}, bus.stuckValues(), "expected a second re-confirmation after another full threshold")
}

// TestMonitor_HealthyObservationResetsDisconnectTimerEvenOnFailedSend pins
// ffos-user#356 feralfile-bot review F4: the bot's own repro was a
// disconnect that ran 85s (just under StuckThreshold), a healthy observation
// whose confirmation Send failed, and a fresh disconnect only 5s later —
// before the fix, unhealthySince was left over from the FIRST episode (only
// reset inside the conditional, retryable emit path), so the brand-new
// disconnect inherited 85s of stale elapsed time and crossed StuckThreshold
// on this tick alone, reporting stuck on a connection that had only just
// dropped.
func TestMonitor_HealthyObservationResetsDisconnectTimerEvenOnFailedSend(t *testing.T) {
	m, cdp, bus, clock := newTestMonitor(t)
	goUnhealthy(m, cdp)

	clock.advance(StuckThreshold - 5*time.Second)
	m.tick()
	require.Empty(t, bus.stuckValues(), "85s in: must not have reported yet")

	bus.err = assertError{}
	bus.failNext = 1
	cdp.initialized = true
	m.tick()
	require.Equal(t, []bool{false}, bus.stuckValues(), "one failed confirmation attempt recorded")

	// A fresh disconnect only 5s after the reconnect must NOT inherit the
	// previous episode's elapsed time just because its confirmation failed
	// to send: no new send is expected yet.
	clock.advance(5 * time.Second)
	cdp.initialized = false
	m.tick()
	assert.Equal(t, []bool{false}, bus.stuckValues(), "a disconnect only 5s old must not be reported as stuck, failed confirmation notwithstanding")

	// The mechanism still works for a genuinely sustained NEW episode: a
	// full fresh threshold from this second disconnect must still report.
	clock.advance(StuckThreshold)
	m.tick()
	assert.Equal(t, []bool{false, true}, bus.stuckValues(), "a genuinely sustained fresh episode must still be reported")
}
