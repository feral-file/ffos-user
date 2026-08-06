package provisioning

// §4.2 recheck backoff ladder (A1, incident FF1-8EVTK3RE): the recheck AP
// phase escalates 2m → 5m → 15m → 30m steady. These tests lock the ladder's
// three load-bearing decisions — where the index advances, where it re-bases,
// and what may defer a blink — so a future edit cannot quietly turn the
// cadence back into a fixed 30-minute blind window (or, worse, a forever-2m
// radio churn).

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wifictl"
)

func TestSessionPhaseBoundRecheckLadder(t *testing.T) {
	h := newHarness(t)
	h.m.latchSessionPolicy("sustained-offline")
	for _, tc := range []struct {
		cycle int
		want  time.Duration
	}{
		{0, 2 * time.Minute},
		{1, 5 * time.Minute},
		{2, 15 * time.Minute},
		{3, 30 * time.Minute}, // past the ladder: RecheckApPhase steady state
		{9, 30 * time.Minute},
	} {
		h.m.recheckCycle = tc.cycle
		assert.Equal(t, tc.want, h.m.sessionPhaseBound(), "cycle %d", tc.cycle)
	}

	// Regression guard: the bounded policies are indifferent to the recheck
	// counter — their phases were never ladder-shaped.
	h.m.recheckCycle = 1
	h.m.latchSessionPolicy(ReasonUserRequested)
	assert.Equal(t, defaultUserRequestedSession, h.m.sessionPhaseBound())
	h.m.latchSessionPolicy(ReasonSetupIncomplete)
	assert.Equal(t, defaultEpisodeApPhase, h.m.sessionPhaseBound())
}

func TestUsableRecheckLadder(t *testing.T) {
	logger := zap.NewNop()
	assert.Equal(t, defaultRecheckApPhaseLadder(), usableRecheckLadder(nil, logger),
		"empty means unset: the default ladder")
	custom := []time.Duration{time.Minute, 10 * time.Minute}
	assert.Equal(t, custom, usableRecheckLadder(custom, logger))
	assert.Equal(t, defaultRecheckApPhaseLadder(),
		usableRecheckLadder([]time.Duration{time.Minute, 0}, logger),
		"one bad rung discards the whole override (all-or-nothing)")
	assert.Equal(t, defaultRecheckApPhaseLadder(),
		usableRecheckLadder([]time.Duration{48 * time.Hour}, logger),
		"an over-ceiling rung is a typo, not intent")
	assert.Equal(t, defaultRecheckApPhaseLadder(),
		usableRecheckLadder([]time.Duration{30 * time.Second}, logger),
		"below minRecheckRung the blink dominates the cycle — rejected")
}

func TestWithDefaultsResolvesRecheckLadder(t *testing.T) {
	got := Tuning{}.withDefaults(15*time.Second, nil)
	assert.Equal(t, defaultRecheckApPhaseLadder(), got.RecheckApPhaseLadder)
}

// TestLatchSessionPolicyLeavesLadderIndexAlone: the blink's still-gone
// re-raise re-latches an ON-TABLE reason, so a latch-time reset would zero
// the counter on every cycle and disable the backoff entirely. Only
// clearOffline's episode boundary may reset it.
func TestLatchSessionPolicyLeavesLadderIndexAlone(t *testing.T) {
	h := newHarness(t)
	h.m.latchSessionPolicy("sustained-offline")
	h.m.recheckCycle = 2
	h.m.latchSessionPolicy("sustained-offline") // the blink re-raise re-latch
	assert.Equal(t, 2, h.m.recheckCycle)
	h.m.latchSessionPolicy("join-failed") // off-table: inherit path
	assert.Equal(t, 2, h.m.recheckCycle)
	assert.Equal(t, sessionRecheck, h.m.sessionPolicy)
}

// TestRecheckBackoffCadence pins the ladder end-to-end on the tick loop:
// blinks after 2m, then 5m, then 15m, then the 30-minute steady state.
func TestRecheckBackoffCadence(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)

	blinks := func() int { return countReason(h, StateOfflineRetrying, ReasonAPRecheck) }
	require.Equal(t, 0, blinks())
	h.tickN(ctx, 8) // 2m: rung 0 expires
	assert.Equal(t, 1, blinks(), "first blink after the 2-minute rung")
	h.tickN(ctx, 19) // 4m45s into rung 1 — one tick shy
	assert.Equal(t, 1, blinks())
	h.tickN(ctx, 1)
	assert.Equal(t, 2, blinks(), "second blink after the 5-minute rung")
	h.tickN(ctx, 60) // 15m: rung 2
	assert.Equal(t, 3, blinks(), "third blink after the 15-minute rung")
	h.tickN(ctx, 119) // one tick shy of the 30m steady state
	assert.Equal(t, 3, blinks(), "steady state is the 30-minute phase")
	h.tickN(ctx, 1)
	assert.Equal(t, 4, blinks())
}

// TestRecheckLadderRebasesAfterLanding: a landing (online) is the
// clearOffline boundary — the NEXT outage is a new session and starts back
// on the short rung.
func TestRecheckLadderRebasesAfterLanding(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	h.tickN(ctx, 8) // blink 1 → ladder index 1 (5m rung)
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))
	require.Equal(t, 1, h.m.recheckCycle)

	// The network returns; the 5m rung's blink associates and lands online.
	h.wifi.activationProfiles = []wifictl.ActivationProfile{{UUID: "b", SSID: "Net", LastUsed: 1}}
	fl.up = true
	h.conn.push(true)
	h.tickN(ctx, 20)
	require.Equal(t, StateOnline, h.m.State())
	assert.Equal(t, 0, h.m.recheckCycle, "the landing re-bases the ladder")

	// Second outage: the first blink comes after 2 minutes again.
	h.wifi.activationProfiles = nil
	h.wifi.activations = nil
	fl.up = false
	h.m.onConnectivity(ctx, false, false)
	h.tickN(ctx, windowSamples)
	require.Equal(t, StateAPActive, h.m.State())
	before := countReason(h, StateOfflineRetrying, ReasonAPRecheck)
	h.tickN(ctx, 8)
	assert.Equal(t, before+1, countReason(h, StateOfflineRetrying, ReasonAPRecheck),
		"a fresh outage restarts at the short rung")
}

// TestRescanReArmKeepsLadderIndex locks the R5 decision: an applyRescan
// re-arm restarts the CURRENT rung's clock, never the ladder index —
// resetting the index would let repeated rescans hold the cadence at
// 2-minute blinks forever.
func TestRescanReArmKeepsLadderIndex(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	h.tickN(ctx, 8) // blink 1 → ladder index 1 (5m rung)
	require.Equal(t, 1, h.m.recheckCycle)

	h.m.applyRescan(ctx)
	assert.Equal(t, 1, h.m.recheckCycle, "the bounce keeps the ladder index")
	blinksBefore := countReason(h, StateOfflineRetrying, ReasonAPRecheck)
	h.tickN(ctx, 19) // one tick shy of the re-armed 5m rung
	assert.Equal(t, blinksBefore, countReason(h, StateOfflineRetrying, ReasonAPRecheck),
		"the re-armed phase is the current rung (5m), not the first (2m)")
	h.tickN(ctx, 1)
	assert.Equal(t, blinksBefore+1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))
}

// TestTeardownFailureKeepsLadderRung: an aborted blink (ensureAPDown failed —
// no measurement happened) must re-arm the SAME rung, not escalate.
func TestTeardownFailureKeepsLadderRung(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)

	h.ap.downErr = errors.New("nm refuses")
	h.tickN(ctx, 9) // the rung-0 expiry attempts a blink; the teardown fails
	assert.Equal(t, 0, h.m.recheckCycle, "no measurement, no escalation")

	// One extra tick beyond the rung: the first tick after the failure
	// resolves the pending deletion and re-raises (re-arming the clock),
	// THEN the re-armed rung 0 must elapse in full before the blink runs.
	h.ap.downErr = nil
	h.tickN(ctx, 9)
	assert.Equal(t, 1, h.m.recheckCycle)
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))
}

// TestRecheckAttachedPhoneDeferral: ANY portal traffic — probes included —
// defers a recheck blink within the activity window, but the deferral
// ceiling still bounds an idle phone left attached to the AP.
func TestRecheckAttachedPhoneDeferral(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	require.NotEmpty(t, h.portals)
	traffic := h.portals[len(h.portals)-1].cfg.TrafficObserved
	require.NotNil(t, traffic, "ensureAPUp must wire TrafficObserved")
	blinks := func() int { return countReason(h, StateOfflineRetrying, ReasonAPRecheck) }

	// Probes keep arriving: 4 minutes pass — twice the rung — with no blink.
	for i := 0; i < 16; i++ {
		traffic()
		h.tick(ctx)
	}
	assert.Equal(t, 0, blinks(), "attached-phone traffic defers the blink")

	// Probes never stop: the ceiling (rung + 15m) fires the blink anyway.
	for i := 0; i < 53; i++ {
		traffic()
		h.tick(ctx)
	}
	assert.Equal(t, 1, blinks(), "the deferral ceiling bounds an idle attached phone")
}

// TestBoundedSessionsIgnorePortalTraffic locks the deferral's scope: the
// bounded policies keep the probes-never-count rule — only the recheck
// cadence reads the traffic signal.
func TestBoundedSessionsIgnorePortalTraffic(t *testing.T) {
	h := newHarness(t)
	h.m.latchSessionPolicy(ReasonUserRequested)
	h.m.armSessionTimer()
	h.clk.advance(defaultUserRequestedSession + time.Second)
	h.m.mu.Lock()
	h.m.lastPortalTraffic = h.clk.Now()
	h.m.mu.Unlock()
	assert.True(t, h.m.sessionExpiryDue(),
		"a bounded session must expire through fresh probe traffic")

	// The same freshness on a recheck session defers.
	h.m.latchSessionPolicy("sustained-offline")
	h.m.recheckCycle = 3 // steady 30m rung
	h.m.armSessionTimer()
	h.clk.advance(30*time.Minute + time.Second)
	h.m.mu.Lock()
	h.m.lastPortalTraffic = h.clk.Now()
	h.m.mu.Unlock()
	assert.False(t, h.m.sessionExpiryDue(),
		"the recheck cadence defers on the same traffic")
}

// The three non-teardown abort shapes: a blink that never completed a usable
// measurement must NOT escalate the ladder — otherwise a chronically failing
// scan walks the cadence straight back into the 30-minute blind window.

func TestScanErrorKeepsLadderRung(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)

	h.wifi.scanErrs = []error{errors.New("radio busy")} // the blink's scan
	h.tickN(ctx, 8)
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck),
		"the blink still narrates and re-raises")
	assert.Equal(t, 0, h.m.recheckCycle, "an errored scan is not a measurement")

	h.tickN(ctx, 8) // the re-armed rung 0 expires; this blink's scan succeeds
	assert.Equal(t, 1, h.m.recheckCycle, "a completed measurement escalates")
}

func TestEmptyScanKeepsLadderRung(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)

	h.wifi.emptyScans = 1 // the documented common post-bounce failure
	h.tickN(ctx, 8)
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))
	assert.Equal(t, 0, h.m.recheckCycle,
		"an empty scan saw nothing and must not escalate")
}

func TestListingErrorKeepsLadderRung(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	h.wifi.activationErr = errors.New("nmcli unreadable")
	driveSustainedRaise(t, h, ctx)

	h.tickN(ctx, 8)
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))
	assert.Equal(t, 0, h.m.recheckCycle,
		"an aborted activation pass is not a measurement")
}

// Sparse-probe boundary for the attached-phone deferral: the design depends
// on the OS's own captive re-probe cadence, so pin both sides of the
// activity window.

func TestRecheckDeferralHoldsAtProbeCadenceInsideWindow(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	traffic := h.portals[len(h.portals)-1].cfg.TrafficObserved
	require.NotNil(t, traffic)

	// A 90-second probe cadence (every 6 ticks) stays inside the 2-minute
	// window: 5 minutes pass — well past the rung — with no blink.
	for i := 0; i < 20; i++ {
		if i%6 == 0 {
			traffic()
		}
		h.tick(ctx)
	}
	assert.Equal(t, 0, countReason(h, StateOfflineRetrying, ReasonAPRecheck),
		"probes inside the window keep deferring")
}

func TestRecheckDeferralLapsesWhenProbesGoQuiet(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	traffic := h.portals[len(h.portals)-1].cfg.TrafficObserved
	require.NotNil(t, traffic)

	// One probe at phase start, then silence: by the rung's expiry the
	// window has lapsed and the blink proceeds — best-effort, not a pin.
	traffic()
	h.tickN(ctx, 8)
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck),
		"a quiet phone is treated as absent")
}

// TestChronicScanFailureHoldsShortRungIndefinitely locks the deliberate
// trade-off direction: a world that never yields a usable measurement holds
// rung 0 forever (visible churn) rather than escalating blind.
func TestChronicScanFailureHoldsShortRungIndefinitely(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)

	// Every scan errors, cycle after cycle — each blink consumes one script
	// entry and each re-raise's pre-AP retry loop consumes up to three more,
	// so the script must outlast the whole run.
	h.wifi.scanErrs = repeatErrs(40, "busy")
	h.tickN(ctx, 40) // ≥4 rung-0 cycles' worth of ticks
	assert.Equal(t, 0, h.m.recheckCycle,
		"a chronically unmeasurable world never escalates")
	assert.GreaterOrEqual(t, countReason(h, StateOfflineRetrying, ReasonAPRecheck), 3,
		"...and keeps blinking at the short rung (the visible-churn trade-off)")
}
