package provisioning

// Autoconnect suppression around the rescan bounce (B1', incident
// FF1-8EVTK3RE). Between the rescan's teardown and its re-raise the radio sits
// in station mode with every saved profile's autoconnect live, and NM's policy
// engine races the re-raise for it — in the field the two were 0.52s apart.
// These tests lock the four load-bearing properties: the window's SHAPE (off
// before the teardown, on after the re-raise), its fail bias (open on suppress
// failure, latched on restore failure), its self-heal (every daemon start
// re-enables the flag), and its SCOPE (no other teardown path may ever suppress
// — the session landings depend on autoconnect to restore the previous
// network).

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lastIndexOf returns the last index of s, or -1.
func lastIndexOf(list []string, s string) int {
	for i := len(list) - 1; i >= 0; i-- {
		if list[i] == s {
			return i
		}
	}
	return -1
}

// settleStartupSelfHeal consumes the one `autoconnect on` every machine owes on
// its first tick (the crash self-heal), so the assertions that follow describe
// only the scenario under test.
func settleStartupSelfHeal(t *testing.T, h *harness, ctx context.Context) {
	t.Helper()
	h.m.onTick(ctx)
	require.False(t, h.m.autoconnectRestorePending, "the self-heal must have succeeded")
	h.wifi.resetAutoconnectLog()
}

// raiseUnprovisionedAP drives a fresh harness to a raised AP through the
// unprovisioned-immediate path, whose session policy is unbounded — no phase
// expiry can fire mid-test and confuse a tick-counting assertion.
func raiseUnprovisionedAP(t *testing.T, h *harness, ctx context.Context) {
	t.Helper()
	h.wifi.setProfile(false)
	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
}

// TestRescanSuppressesAutoconnectAroundBounce pins the window's shape: the flag
// goes off BEFORE the teardown (a suppression applied after it has already lost
// the race it exists to prevent) and back on AFTER the re-raise.
func TestRescanSuppressesAutoconnectAroundBounce(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raiseUnprovisionedAP(t, h, ctx)
	settleStartupSelfHeal(t, h, ctx)

	downsBefore := h.rec.count("ap.Down")
	h.m.applyRescan(ctx)

	assert.Equal(t, []bool{false, true}, h.wifi.autoconnectLog(),
		"exactly one suppress/restore pair per bounce")
	assert.False(t, h.m.autoconnectRestorePending, "a successful restore leaves no latch")

	list := h.rec.list()
	off := indexOf(list, "wifi.Autoconnect:off")
	require.GreaterOrEqual(t, off, 0)
	require.Greater(t, h.rec.count("ap.Down"), downsBefore, "the bounce must still tear the AP down")
	assert.Less(t, off, lastIndexOf(list, "ap.Down"),
		"suppression must precede the teardown that opens the race window: %v", list)
	assert.Less(t, lastIndexOf(list, "ap.Up"), lastIndexOf(list, "wifi.Autoconnect:on"),
		"restore must follow the re-raise that closes it: %v", list)
}

// TestRescanRestoresAutoconnectWhenReRaiseFails: the restore is deferred
// precisely so a failed re-raise cannot strand the suppression. The AP is then
// down with autoconnect ON, which is the correct state — NM grabbing a network
// while the tick retries the raise is recovery, not the race.
func TestRescanRestoresAutoconnectWhenReRaiseFails(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raiseUnprovisionedAP(t, h, ctx)
	settleStartupSelfHeal(t, h, ctx)

	h.ap.upErr = errors.New("nm refuses to create the hotspot")
	h.m.applyRescan(ctx)

	assert.Equal(t, []bool{false, true}, h.wifi.autoconnectLog(),
		"a failed re-raise must still restore autoconnect")
	assert.False(t, h.m.autoconnectRestorePending)
}

// TestRescanSuppressionFailureFailsOpen: the race is a quality problem, the
// rescan is the feature the user pressed, so a suppression that errors must not
// block the bounce.
//
// It must ALSO still restore. A suppress error does not prove the flag stayed
// on — no error return separates "never applied" from "applied, then the call
// failed" — and skipping the restore is the one path that can leave the flag
// off with no latch and no retry, which on a long-running daemon (no further
// start, so no self-heal) would silently strip the session landings'
// autoconnect until an NM restart.
func TestRescanSuppressionFailureFailsOpen(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raiseUnprovisionedAP(t, h, ctx)
	settleStartupSelfHeal(t, h, ctx)

	scansBefore := h.rec.count("wifi.RefreshScanCache")
	upsBefore := h.rec.count("ap.Up")
	h.wifi.autoconnectErrs = []error{errors.New("nmcli device set failed")}
	h.m.applyRescan(ctx)

	assert.Equal(t, []bool{false, true}, h.wifi.autoconnectLog(),
		"the restore must run even when the suppress reported failure")
	assert.False(t, h.m.autoconnectRestorePending, "the restore succeeded, so no latch")
	assert.Greater(t, h.rec.count("wifi.RefreshScanCache"), scansBefore, "the rescan must still run")
	assert.Greater(t, h.rec.count("ap.Up"), upsBefore, "the AP must still come back")
	assert.Equal(t, StateAPActive, h.m.State())
}

// TestRescanBrokenSeamLatchesAndConverges: when BOTH calls fail — the seam is
// broken, so the flag's true state is unknown — the machine must not assume the
// optimistic reading. It latches and keeps retrying, then converges the moment
// the seam recovers.
func TestRescanBrokenSeamLatchesAndConverges(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raiseUnprovisionedAP(t, h, ctx)
	settleStartupSelfHeal(t, h, ctx)

	h.wifi.autoconnectErrSticky = errors.New("nmcli unavailable")
	h.m.applyRescan(ctx)
	require.Equal(t, StateAPActive, h.m.State(), "a broken seam must not block the rescan")
	assert.True(t, h.m.autoconnectRestorePending,
		"an unknown flag state must latch, not be assumed on")

	h.m.onTick(ctx)
	assert.True(t, h.m.autoconnectRestorePending, "still broken: the latch holds")

	h.wifi.autoconnectErrSticky = nil
	h.m.onTick(ctx)
	assert.False(t, h.m.autoconnectRestorePending, "a recovered seam converges")
	assert.Equal(t, []bool{false, true, true, true}, h.wifi.autoconnectLog())
}

// TestAutoconnectRestoreLatchRetriesUntilSuccess: a failed restore is the one
// path that could leave the device unable to autoconnect, so it latches and
// every tick retries until it succeeds — and stops calling once it has.
func TestAutoconnectRestoreLatchRetriesUntilSuccess(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	raiseUnprovisionedAP(t, h, ctx)
	settleStartupSelfHeal(t, h, ctx)

	// off succeeds, then two restore attempts fail before the third works.
	h.wifi.autoconnectErrs = []error{nil, errors.New("boom"), errors.New("boom")}
	h.m.applyRescan(ctx)
	require.True(t, h.m.autoconnectRestorePending, "a failed restore must latch")

	h.m.onTick(ctx)
	assert.True(t, h.m.autoconnectRestorePending, "still failing: the latch holds")
	h.m.onTick(ctx)
	assert.False(t, h.m.autoconnectRestorePending, "a successful retry clears the latch")

	callsAfterClear := len(h.wifi.autoconnectLog())
	h.m.onTick(ctx)
	assert.Len(t, h.wifi.autoconnectLog(), callsAfterClear,
		"a cleared latch must stop issuing per-tick flips")
	assert.Equal(t, []bool{false, true, true, true}, h.wifi.autoconnectLog())
}

// TestStartupTickSelfHealsAutoconnect: the latch starts TRUE so a suppression a
// crash (or a SIGKILL mid-bounce) left behind is re-enabled by the first tick,
// with no crash-time bookkeeping to get wrong. Idempotent by construction.
func TestStartupTickSelfHealsAutoconnect(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	require.True(t, h.m.autoconnectRestorePending, "a fresh machine owes one restore")

	h.m.onTick(ctx)

	assert.Equal(t, []bool{true}, h.wifi.autoconnectLog())
	assert.False(t, h.m.autoconnectRestorePending)
}

// TestAutoconnectSuppressionScopedToRescan is the §2.1 scope regression: the
// other three teardown paths either drive activation themselves (blink, join)
// or DEPEND on autoconnect to restore the previous network (the session
// landings). None of them may ever suppress it.
func TestAutoconnectSuppressionScopedToRescan(t *testing.T) {
	ctx := context.Background()

	t.Run("recheck blink", func(t *testing.T) {
		fl := &fakeLink{up: false}
		h := newLinkHarness(t, fl)
		h.wifi.setProfile(true)
		driveSustainedRaise(t, h, ctx)
		blinksBefore := countReason(h, StateOfflineRetrying, ReasonAPRecheck)
		h.tickN(ctx, 8) // rung 0 (2m) expires: the blink tears down and re-raises
		require.Greater(t, countReason(h, StateOfflineRetrying, ReasonAPRecheck), blinksBefore,
			"the blink must actually have run for this to be evidence")
		assert.Equal(t, 0, h.rec.count("wifi.Autoconnect:off"))
	})

	t.Run("portal join", func(t *testing.T) {
		h := newHarness(t)
		raiseUnprovisionedAP(t, h, ctx)
		h.conn.online = true
		h.wifi.setProfile(true)
		h.m.applyJoin(ctx, "HomeNet", "pw", false)
		require.Equal(t, 1, h.rec.count("wifi.Join:HomeNet"))
		assert.Equal(t, 0, h.rec.count("wifi.Autoconnect:off"))
	})

	t.Run("user session landing", func(t *testing.T) {
		h := newHarness(t)
		h.wifi.setProfile(false)
		h.m.onConnectivity(ctx, true, false) // online, unprovisioned: no AP yet
		require.Equal(t, StateUnprovisioned, h.m.State())

		h.m.applyUserSetup(ctx)
		require.Equal(t, StateAPActive, h.m.State())
		// The user-requested session's 30-minute abandonment net expires; the
		// landing hands the radio back to NM and RELIES on autoconnect to
		// restore the previous network.
		h.tickN(ctx, 121)
		require.Equal(t, StateUnprovisioned, h.m.State(), "the session must have landed")
		assert.Equal(t, 0, h.rec.count("wifi.Autoconnect:off"))
	})
}
