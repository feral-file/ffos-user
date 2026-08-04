package provisioning

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
	"github.com/feral-file/ffos-user/components/feral-controld/wifictl"
)

// newEpisodeHarness builds a link-guard harness prepared for the §4.1
// episode: provisioned, UNCLAIMED, link present, WAN confirmed offline, and a
// confirmed-no-wire probe. Tests flip the individual fields to exercise the
// arming predicate.
func newEpisodeHarness(t *testing.T, fl *fakeLink) *harness {
	t.Helper()
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	h.m.claimed = false
	h.m.wiredLink = func(context.Context) (bool, error) { return false, nil }
	return h
}

func countReason(h *harness, state State, reason string) int {
	n := 0
	for _, c := range h.notifier.calls {
		if c.State == state && c.Detail.Reason == reason {
			n++
		}
	}
	return n
}

// --- §4.1 arming predicate ----------------------------------------------------

// TestEpisodeArmsOnlyOnFullPredicate pins the four-term arming predicate
// (constraint 11): unclaimed ∧ confirmed link-present ∧ confirmed offline ∧
// confirmed no-wire, with every inconclusive term deferring and every
// disproving term preventing the raise.
func TestEpisodeArmsOnlyOnFullPredicate(t *testing.T) {
	ctx := context.Background()

	t.Run("full predicate raises at the window", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.onConnectivity(ctx, false, false)
		require.Equal(t, StateOfflineRetrying, h.m.State())

		h.tickN(ctx, windowSamples-1)
		assert.Equal(t, 0, h.rec.count("ap.Up"), "a sub-window wait must not raise")
		h.tick(ctx)
		assert.Equal(t, StateAPActive, h.m.State())
		assert.Equal(t, 1, countReason(h, StateAPActive, ReasonSetupIncomplete))
	})

	t.Run("claimed never arms", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.claimed = true
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"), "#233: claimed devices keep the silent screen")
	})

	t.Run("claim-unknown reads as claimed and never arms", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		// Simulate the corrupt-state boot: New's fail-safe default, never
		// overridden by a known reading.
		h.m.claimed = true
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, 2*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"))
	})

	t.Run("failing WAN query never arms", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.onConnectivity(ctx, false, true) // assumed offline: query failed
		h.conn.setErr(errors.New("monitord down"))
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"), "an assumed verdict must never feed the raise")
	})

	t.Run("failing wire probe pauses, never arms by itself", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.wiredLink = func(context.Context) (bool, error) { return false, errors.New("nmcli flake") }
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"))
	})

	t.Run("confirmed wire cancels", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, 10)
		h.m.wiredLink = func(context.Context) (bool, error) { return true, nil }
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"), "a wired frame never runs the episode")
	})

	t.Run("kill-switch disables the fallback", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.tuning.SetupIncompleteDisabled = true
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"))
	})

	t.Run("nil wire probe never arms", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.wiredLink = nil
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"), "an unconfirmable wire verdict fails safe")
	})
}

// --- §4.1 deferral and cancel edges ------------------------------------------

// TestEpisodeHubContactDefersWithinBudgets pins the deferral: fresh hub
// contact pauses the count (charged per paused tick), and once the cycle
// budget is exhausted the count proceeds regardless of contact — contact can
// delay but never pin the fallback.
func TestEpisodeHubContactDefersWithinBudgets(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: true}
	h := newEpisodeHarness(t, fl)
	h.m.onConnectivity(ctx, false, false)

	h.tickN(ctx, 10) // half the window counted

	// Continuous app contact: each tick refreshes the signal. Freshness is
	// 3 min (12 ticks) and the cycle budget 5 min (20 ticks): the pause holds
	// while the budget lasts.
	for i := 0; i < 20; i++ {
		h.m.ObserveHubContact()
		h.tick(ctx)
	}
	assert.Equal(t, 0, h.rec.count("ap.Up"),
		"fresh contact within budget must pause the window")

	// Budget exhausted: contact no longer pauses; the remaining samples count.
	for i := 0; i < 12; i++ {
		h.m.ObserveHubContact()
		h.tick(ctx)
	}
	assert.Equal(t, StateAPActive, h.m.State(),
		"an exhausted deferral budget must let the raise proceed (contact cannot pin it)")
}

// TestEpisodeCancelEdges pins each §4.1 cancel: WAN confirmation, a claim
// event, and confirmed link loss (which hands the device to the link-absent
// machinery).
func TestEpisodeCancelEdges(t *testing.T) {
	ctx := context.Background()

	t.Run("WAN confirmation cancels", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, windowSamples-1)
		h.m.onConnectivity(ctx, true, false) // online
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, windowSamples-1)
		assert.Equal(t, 0, h.rec.count("ap.Up"),
			"the accumulation must not survive an online confirmation")
	})

	t.Run("claim cancels", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, windowSamples-1)
		// LAN pairing landed: the SNAPSHOT write is synchronous (SetClaimed),
		// so the raise is vetoed even before the loop-side cancel nudge runs.
		h.m.SetClaimed(true)
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"))
	})

	t.Run("claim veto holds even when the cancel nudge is lost", func(t *testing.T) {
		// The reviewer-named hazard: the executor fires the claim observer
		// exactly once, and the loop can be blocked for minutes by a
		// synchronous blink — a queued-only snapshot would drop the claim
		// permanently and raise over a just-claimed frame. The snapshot write
		// is synchronous, so even with the nudge event never processed the
		// expiry re-read vetoes the raise.
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, windowSamples-1)
		h.m.SetClaimed(true)
		// Deliberately NOT draining the event queue (the loop is not running
		// in these unit tests): only the snapshot protects the raise.
		h.tickN(ctx, 3*windowSamples)
		assert.Equal(t, 0, h.rec.count("ap.Up"),
			"the synchronous snapshot must veto the raise without the nudge")
	})

	t.Run("confirmed link loss cancels and the link-absent path owns recovery", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newEpisodeHarness(t, fl)
		h.m.onConnectivity(ctx, false, false)
		h.tickN(ctx, windowSamples-1)

		fl.up = false // the association drops: the OTHER evidence shape
		h.tickN(ctx, windowSamples)
		assert.Equal(t, StateAPActive, h.m.State(),
			"the link-absent window must raise on its own cadence")
		assert.Equal(t, 1, countReason(h, StateAPActive, "sustained-offline"))
		assert.Equal(t, 0, countReason(h, StateAPActive, ReasonSetupIncomplete),
			"the canceled episode must not have raised")
	})
}

// --- §4.1 cadence: AP phases, station ladder, settle --------------------------

// TestEpisodeCadenceLadderAndSettle walks a full episode: raise at the 5-min
// window, 5-min AP phases (teardown lands on the narrated station-phase
// reason), the 5/10/20 station ladder, and the settle after 4 raises — with
// the episode surviving its own AP phases (constraint 12: no sampling runs
// while the machine sits in StateAPActive, so an AP phase can neither count
// nor cancel).
func TestEpisodeCadenceLadderAndSettle(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: true}
	h := newEpisodeHarness(t, fl)
	h.m.onConnectivity(ctx, false, false)

	// Raise 1 at the 20-sample window.
	h.tickN(ctx, windowSamples)
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 1, countReason(h, StateAPActive, ReasonSetupIncomplete))

	// AP phase 1: 5 minutes (20 ticks), then teardown to the narrated
	// station phase.
	h.tickN(ctx, windowSamples)
	require.Equal(t, StateOfflineRetrying, h.m.State(),
		"the episode AP phase must expire into a station phase")
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPSessionEnded))
	assert.Equal(t, 1, h.rec.count("ap.Down"))

	// Station phase 1 (ladder rung 0: 5 min) → raise 2.
	h.tickN(ctx, windowSamples)
	require.Equal(t, 2, countReason(h, StateAPActive, ReasonSetupIncomplete))

	// AP phase 2 → station phase 2 (rung 1: 10 min = 40 ticks) → raise 3.
	h.tickN(ctx, windowSamples)
	require.Equal(t, StateOfflineRetrying, h.m.State())
	h.tickN(ctx, 2*windowSamples-1)
	assert.Equal(t, 2, countReason(h, StateAPActive, ReasonSetupIncomplete),
		"the second station phase is a 10-minute rung — no early raise")
	h.tick(ctx)
	require.Equal(t, 3, countReason(h, StateAPActive, ReasonSetupIncomplete))

	// AP phase 3 → station phase 3 (rung 2: 20 min = 80 ticks) → raise 4.
	h.tickN(ctx, windowSamples)
	h.tickN(ctx, 4*windowSamples)
	require.Equal(t, 4, countReason(h, StateAPActive, ReasonSetupIncomplete))

	// AP phase 4 expiry: cycles exhausted — settle in STATION mode with the
	// terminal narration; no further raises, ever.
	h.tickN(ctx, windowSamples)
	require.Equal(t, StateOfflineRetrying, h.m.State())
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonSetupIncompleteSettled))
	h.tickN(ctx, 6*windowSamples)
	assert.Equal(t, 4, countReason(h, StateAPActive, ReasonSetupIncomplete),
		"a settled episode must not raise again")

	// D12 pin: settle → confirmed link loss → the sustained-offline path
	// raises normally (the suppression never outlives the link).
	fl.up = false
	h.tickN(ctx, windowSamples)
	assert.Equal(t, 1, countReason(h, StateAPActive, "sustained-offline"),
		"a settled episode must not suppress the link-absent raise")
}

// TestEpisodePortalJoinResetsCycles pins the strongest cancel: a completed
// portal join resets the WHOLE episode (cycle counter included), so a user
// who picked a wrong-but-live SSID first gets a full-length episode on the
// network they actually meant.
func TestEpisodePortalJoinResetsCycles(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: true}
	h := newEpisodeHarness(t, fl)
	h.m.onConnectivity(ctx, false, false)

	h.tickN(ctx, windowSamples) // raise 1
	require.Equal(t, StateAPActive, h.m.State())

	// The user joins a (still WAN-less) network through the portal.
	fl.up = false // the AP bounce drops the association during the join
	h.m.applyJoin(ctx, "MeantNet", "pw", false)
	fl.up = true // the join associated; still no WAN
	require.Equal(t, StateOfflineRetrying, h.m.State())

	assert.Equal(t, 0, h.m.episodeCycle, "a completed join must reset the cycle counter")
	h.tickN(ctx, windowSamples-1)
	assert.Equal(t, 1, countReason(h, StateAPActive, ReasonSetupIncomplete),
		"the fresh episode needs its full window before raising again")
	h.tick(ctx)
	assert.Equal(t, 2, countReason(h, StateAPActive, ReasonSetupIncomplete))
}

// --- §4.2 session policy ------------------------------------------------------

// TestUserRequestedSessionBounds pins the user-requested row — including from
// StateUnprovisioned, the abandonment net the sibling plan exists for: 30
// minutes, then teardown and resume normal state handling with a constraint-4
// landing.
func TestUserRequestedSessionBounds(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.wifi.setProfile(false) // unprovisioned target frame
	// claimed stays true (the startWifiSetup target is a claimed frame).

	h.m.transition(ctx, StateAPActive, Detail{Reason: ReasonUserRequested, Message: "user requested"})
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 1, h.rec.count("ap.Up"))

	h.tickN(ctx, 119) // 30 min = 120 ticks
	assert.Equal(t, 0, h.rec.count("ap.Down"), "the session holds for its full bound")
	h.tick(ctx)
	assert.Equal(t, 1, h.rec.count("ap.Down"), "expiry tears the abandoned session down")
	assert.Equal(t, StateUnprovisioned, h.m.State())
	// Claimed frame: the landing is the silent hide (constraint 4).
	assert.Equal(t, 1, countReason(h, StateUnprovisioned, ReasonAPSessionEndedSilent))
}

// TestJoinFailureKeepsBoundedSession pins the one-typo rule: a join failure
// inside a user-requested session re-raises under the SAME bounded policy —
// never the unbounded/recheck cadence — and preserves the join status for the
// phone's /status poll.
func TestJoinFailureKeepsBoundedSession(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.wifi.setProfile(false)

	h.m.transition(ctx, StateAPActive, Detail{Reason: ReasonUserRequested})
	require.Equal(t, StateAPActive, h.m.State())

	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth}
	h.m.applyJoin(ctx, "HomeNet", "typo", false)
	require.Equal(t, StateAPActive, h.m.State(), "the failure re-raises")
	assert.Equal(t, "auth-failure", h.m.Status().Reason,
		"the re-raise must preserve the join outcome the phone polls for")

	// The retained policy's fresh clock: the session still expires on the
	// user-requested bound (bounded, not unbounded).
	h.tickN(ctx, 121)
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"a typo must not escalate a bounded session into an unbounded one")
}

// TestPortalActivityDefersTeardown pins the mid-portal rug-pull guard: a
// human-caused portal request within the 2-minute window defers the expiry,
// and the +15-minute ceiling bounds the deferral.
func TestPortalActivityDefersTeardown(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.wifi.setProfile(false)

	h.m.transition(ctx, StateAPActive, Detail{Reason: ReasonUserRequested})
	require.Equal(t, StateAPActive, h.m.State())

	h.tickN(ctx, 119)
	h.m.observePortalActivity() // the user is mid-form
	h.tickN(ctx, 4)             // past the bound, within the activity window
	assert.Equal(t, 0, h.rec.count("ap.Down"),
		"a human mid-portal must defer the teardown")

	// Keep the user active past the ceiling: the teardown eventually wins.
	for i := 0; i < 62; i++ { // 15.5 min of continuous activity
		h.m.observePortalActivity()
		h.tick(ctx)
	}
	assert.Equal(t, 1, h.rec.count("ap.Down"),
		"the deferral ceiling must bound even continuous activity")
}

// TestWiredExitLowersRaisedAP pins the §4.2 wired exit: a confirmed ethernet
// sighting lowers a RAISED AP (user-requested included) with a constraint-4
// landing, while Wi-Fi link readings still cannot (the own-hotspot ambiguity
// pin — probeLink short-circuits while apUp).
func TestWiredExitLowersRaisedAP(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.m.transition(ctx, StateAPActive, Detail{Reason: ReasonUserRequested})
	require.Equal(t, StateAPActive, h.m.State())

	// A Wi-Fi "link" reading cannot exit a raised AP (nil guard tick is
	// linkAbsent anyway; the invariant is pinned by the wired probe below
	// being the ONLY exit that fires).
	h.tickN(ctx, 3)
	require.Equal(t, StateAPActive, h.m.State())

	h.m.wiredLink = func(context.Context) (bool, error) { return true, nil }
	h.tick(ctx)
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"a wired sighting must end the session — the AP cannot fix a wired network")
	assert.Equal(t, 1, h.rec.count("ap.Down"))
	assert.Equal(t, 1, countReason(h, StateUnprovisioned, ReasonAPSessionEndedSilent))
}

// --- §4.2 recheck cadence -----------------------------------------------------

// driveSustainedRaise walks a link-absent provisioned harness into the
// sustained-offline raise (the recheck-cadence session).
func driveSustainedRaise(t *testing.T, h *harness, ctx context.Context) {
	t.Helper()
	h.m.onConnectivity(ctx, false, false)
	h.tickN(ctx, windowSamples)
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 1, countReason(h, StateAPActive, "sustained-offline"))
}

// TestRecheckBlinkReRaisesWhenNetworkStillGone pins the blink's core loop:
// after the 30-minute AP phase, teardown → narrated ap-recheck → forced scan →
// in-range MRU activation (hidden last; out-of-range never attempted) →
// re-raise with the ORIGINAL reason, no offlineSince churn, no
// resetJoinStatus, and the blink's fresh scan standing in for the re-raise's
// pre-AP pass.
func TestRecheckBlinkReRaisesWhenNetworkStillGone(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	// Profiles: A out of range, B in range (older), C in range (newer),
	// H hidden. All activations fail — the network is gone.
	h.wifi.activationProfiles = []wifictl.ActivationProfile{
		{UUID: "a", SSID: "GoneNet", LastUsed: 50},
		{UUID: "b", SSID: "Net", LastUsed: 10},
		{UUID: "c", SSID: "Net", LastUsed: 99},
		{UUID: "h", SSID: "Ghost", Hidden: true, LastUsed: 5},
	}
	h.wifi.activateErrs = map[string]error{
		"a": errors.New("unreachable"), "b": errors.New("no"),
		"c": errors.New("no"), "h": errors.New("no"),
	}
	driveSustainedRaise(t, h, ctx)

	scansBefore := h.rec.count("wifi.RefreshScanCache")
	h.tickN(ctx, 120) // the 30-minute AP phase
	// Blink ran: narrated, scanned, activated in MRU order with hidden last
	// and the out-of-range profile never attempted, then re-raised.
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))
	assert.Equal(t, []string{"c", "b", "h"}, h.wifi.activations,
		"in-range MRU first, hidden last, out-of-range never")
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 2, countReason(h, StateAPActive, "sustained-offline"),
		"the re-raise carries the ORIGINAL reason")
	// The blink's scan stood in for the re-raise's pre-AP pass: exactly one
	// scan for the whole blink+re-raise.
	assert.Equal(t, scansBefore+1, h.rec.count("wifi.RefreshScanCache"),
		"one radio pass, not two")
	assert.Equal(t, portal.JoinIdle, h.m.Status().State,
		"the re-raise must not wipe a join status a phone may poll (no resetJoinStatus)")
}

// TestRecheckBlinkListingErrorAborts pins the fail-bias: a profile-listing
// error aborts the blink — never a blind activation — and the re-raise
// proceeds.
func TestRecheckBlinkListingErrorAborts(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	h.wifi.activationErr = errors.New("nmcli unreadable")
	driveSustainedRaise(t, h, ctx)

	h.tickN(ctx, 120)
	assert.Empty(t, h.wifi.activations, "no blind activation off an unreadable list")
	assert.Equal(t, StateAPActive, h.m.State(), "the re-raise still happens")
}

// TestRecheckBlinkAssociationExitsToSilentShape pins the recovery routing: an
// activation that associates ends the cadence — associated-but-no-WAN lands
// in the link-present shape, and the constraint-4(b) rewrite hides the
// ap-recheck panel instead of stranding "…returns in a moment" over the
// artwork.
func TestRecheckBlinkAssociationExitsToSilentShape(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	h.wifi.activationProfiles = []wifictl.ActivationProfile{{UUID: "b", SSID: "Net", LastUsed: 1}}
	driveSustainedRaise(t, h, ctx)

	// The router came back mid-AP-phase; the blink's activation succeeds.
	fl.up = true
	h.tickN(ctx, 120)
	assert.Equal(t, []string{"b"}, h.wifi.activations)
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"associated-but-no-WAN parks in the link-present shape")
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPSessionEndedSilent),
		"the 4(b) rewrite must hide the ap-recheck panel")
	assert.Equal(t, 1, h.rec.count("ap.Up"),
		"no re-raise after a successful reactivation")
}

// TestRecheckBlinkOnlineRecoveryEndsEverything: the blink's reactivation plus
// a WAN confirmation lands the machine in StateOnline — artwork back, worst
// blindness one AP phase (D11's fix).
func TestRecheckBlinkOnlineRecoveryEndsEverything(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	h.wifi.activationProfiles = []wifictl.ActivationProfile{{UUID: "b", SSID: "Net", LastUsed: 1}}
	driveSustainedRaise(t, h, ctx)

	fl.up = true
	h.conn.push(true) // WAN is back too (the push also queues an event; the
	// blink's own query reads the same value)
	h.tickN(ctx, 120)
	assert.Equal(t, StateOnline, h.m.State())
}

// TestRecheckTeardownFailureAbortsBlink pins the false-evidence guard: a
// failed ensureAPDown aborts the blink (the hotspot may still hold the radio,
// so "still gone" would be fabricated) and retries next cycle.
func TestRecheckTeardownFailureAbortsBlink(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)

	h.ap.downErr = errors.New("nm refuses")
	h.tickN(ctx, 121)
	assert.Equal(t, 0, countReason(h, StateOfflineRetrying, ReasonAPRecheck),
		"no recheck narration while the teardown cannot complete")
	assert.Empty(t, h.wifi.activations, "no activation with the hotspot possibly up")

	h.ap.downErr = nil
	h.tickN(ctx, 121) // the re-armed phase expires again; the blink now runs
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))
}

// --- §4.4 verdict repaint and 4(b) --------------------------------------------

// TestJoinedConnUnknownRepaintsOnConfirmedOffline pins the D4 fix: the
// "Checking internet access…" hedge is no longer terminal — a confirmed
// offline re-query repaints to the joined-no-internet wording, and a
// confirmed online exits to StateOnline.
func TestJoinedConnUnknownRepaintsOnConfirmedOffline(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.m.onConnectivity(ctx, false, false) // AP up
	require.Equal(t, StateAPActive, h.m.State())

	// The join associates but the post-join query fails: the hedge paints.
	h.conn.setErr(errors.New("monitord starting"))
	h.wifi.setProfile(true)
	h.m.applyJoin(ctx, "HomeNet", "pw", false)
	require.Equal(t, StateOfflineRetrying, h.m.State())
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonJoinedConnUnknown))

	// The re-query succeeds and confirms OFFLINE: the screen must follow the
	// verdict.
	h.conn.setErr(nil)
	h.conn.online = false
	h.tick(ctx)
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonJoinedNoInternet),
		"a confirmed offline verdict must repaint the hedge (D4)")
}

// TestNarratedToSilentEmitsExplicitHide pins constraint 4(b) generically: any
// transition from a narrated OfflineRetrying reason into a silent leg emits
// the explicit silent-hide reason rather than stranding the panel.
func TestNarratedToSilentEmitsExplicitHide(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.wifi.setProfile(true)

	// Enter a narrated leg directly (white-box: the recheck blink's
	// narration).
	h.m.transition(ctx, StateOfflineRetrying, Detail{Reason: ReasonAPRecheck, Message: "checking"})
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPRecheck))

	// A redundant offline reading re-targets the state with the silent
	// generic reason.
	h.m.onConnectivity(ctx, false, false)
	assert.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPSessionEndedSilent),
		"the narrated→silent edge must emit the explicit hide")
}

// TestExhibitionSilentPathStaysSilent is the regression pin for constraint
// 10's scope: transitions into the silent legs keep today's dedupe — the
// mid-exhibition outage narrates nothing, 4(b) hide included (nothing was
// narrated).
func TestExhibitionSilentPathStaysSilent(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	h.wifi.setProfile(true)
	h.m.claimed = true

	h.m.onConnectivity(ctx, true, false) // exhibition steady state
	h.m.onConnectivity(ctx, false, false)
	h.m.onConnectivity(ctx, false, false) // redundant re-emission
	assert.Equal(t, 0, countReason(h, StateOfflineRetrying, ReasonAPSessionEndedSilent))
	for _, c := range h.notifier.calls {
		if c.State == StateOfflineRetrying {
			assert.False(t, narratedOfflineReason(c.Detail.Reason),
				"the exhibition path must stay unnarrated")
		}
	}
}

// TestTeardownInvariantGeneric pins constraint 4(a) over the notification
// log: across a full episode plus a user-session expiry, after every teardown
// with no raise in progress the LAST narration is never softap_qr/scanning,
// and no silent reason follows a narrated panel without the explicit hide.
func TestTeardownInvariantGeneric(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: true}
	h := newEpisodeHarness(t, fl)
	h.m.onConnectivity(ctx, false, false)
	h.tickN(ctx, windowSamples) // raise
	h.tickN(ctx, windowSamples) // AP phase → station landing
	h.tickN(ctx, windowSamples) // raise 2
	h.tickN(ctx, windowSamples) // AP phase → station landing 2
	require.Equal(t, StateOfflineRetrying, h.m.State())

	// Every ap.Down in the recorder is eventually followed by a narration
	// that is not scanning/softap (the landing reasons repaint) — assert the
	// FINAL screen state after each teardown-landing pair by walking the
	// combined log.
	list := h.rec.list()
	for i, e := range list {
		if e != "ap.Down" {
			continue
		}
		// Find the next narration after the teardown.
		for j := i + 1; j < len(list); j++ {
			if len(list[j]) > 7 && list[j][:7] == "notify:" {
				assert.NotContains(t, list[j], ":scanning",
					"a teardown landing must never leave scanning as the next paint")
				break
			}
		}
	}
}

// TestEpisodePauseRepaintsFloorSafeCopy pins constraint 13 for the numbered
// station-phase panel: a deferral/pause stretches the phase past its promised
// "about N minutes", so the pause's first tick repaints the floor-safe
// variant (no number), exactly once per pause.
func TestEpisodePauseRepaintsFloorSafeCopy(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: true}
	h := newEpisodeHarness(t, fl)
	h.m.onConnectivity(ctx, false, false)

	h.tickN(ctx, windowSamples) // raise 1
	h.tickN(ctx, windowSamples) // AP phase → numbered station-phase panel
	require.Equal(t, StateOfflineRetrying, h.m.State())
	require.Equal(t, 1, countReason(h, StateOfflineRetrying, ReasonAPSessionEnded))

	// Fresh hub contact pauses the station count: the panel must drop its
	// minutes promise.
	h.m.ObserveHubContact()
	h.tickN(ctx, 3)
	require.Equal(t, 2, countReason(h, StateOfflineRetrying, ReasonAPSessionEnded),
		"the pause must repaint exactly once, not per paused tick")
	last := h.notifier.calls[len(h.notifier.calls)-1]
	assert.NotContains(t, last.Detail.Message, "minutes",
		"the paused variant must not promise a number the timer cannot keep")
	assert.Contains(t, last.Detail.Message, "will reopen if",
		"the paused variant keeps the floor-safe promise")
}

// --- startWifiSetup admission (docs/app-triggered-wifi-setup.md §4.1) ---------

// TestStartWifiSetupAdmission pins the admission table: rejects busy from
// joining/starting, rejects wired (fail closed on probe error AND on an
// unavailable probe), accepts from online / offline_retrying / unprovisioned —
// and acceptance only QUEUES: the reply precedes any radio work (constraint 1
// of the sibling plan).
func TestStartWifiSetupAdmission(t *testing.T) {
	ctx := context.Background()
	notWired := func(context.Context) (bool, error) { return false, nil }

	t.Run("accepts from resting states and the raise runs the entry triple", func(t *testing.T) {
		for _, drive := range []struct {
			name  string
			setup func(h *harness)
			state State
		}{
			{"online", func(h *harness) {
				h.wifi.setProfile(true)
				h.m.onConnectivity(ctx, true, false)
			}, StateOnline},
			{"offline_retrying", func(h *harness) {
				h.wifi.setProfile(true)
				h.m.onConnectivity(ctx, false, false)
			}, StateOfflineRetrying},
			{"unprovisioned", func(h *harness) {
				h.wifi.setProfile(false)
				h.m.onConnectivity(ctx, true, false)
			}, StateUnprovisioned},
		} {
			t.Run(drive.name, func(t *testing.T) {
				fl := &fakeLink{up: true}
				h := newLinkHarness(t, fl)
				h.m.wiredLink = notWired
				drive.setup(h)
				require.Equal(t, drive.state, h.m.State())
				// Seed a stale join outcome so the resetJoinStatus pin is real:
				// a fresh machine is already JoinIdle, and constraint 2's
				// failure mode is precisely a WEEKS-old success/failure banner
				// greeting the new session.
				h.m.mu.Lock()
				h.m.status = portal.Status{State: portal.JoinFailed, SSID: "OldNet", Reason: "auth-failure"}
				h.m.mu.Unlock()

				require.NoError(t, h.m.StartWifiSetup(ctx))
				// Constraint 1: acceptance queued, no radio work yet.
				assert.Equal(t, 0, h.rec.count("ap.Up"),
					"the reply must precede any radio work")

				// Drain the queued raise the way the loop would.
				ev := <-h.m.events
				require.Equal(t, evUserSetup, ev.kind)
				h.m.applyUserSetup(ctx)
				assert.Equal(t, StateAPActive, h.m.State())
				assert.Equal(t, 1, h.rec.count("ap.Up"))
				assert.Equal(t, 1, countReason(h, StateAPActive, ReasonUserRequested))
				assert.Equal(t, portal.JoinIdle, h.m.Status().State,
					"resetJoinStatus must clear a stale outcome before the fresh session")
			})
		}
	})

	t.Run("rejects busy while joining", func(t *testing.T) {
		fl := &fakeLink{up: false}
		h := newLinkHarness(t, fl)
		h.m.wiredLink = notWired
		h.wifi.setProfile(false)
		h.m.onConnectivity(ctx, false, false) // AP up
		require.Equal(t, StateAPActive, h.m.State())
		h.m.mu.Lock()
		h.m.state = StateJoining // a join is in flight
		h.m.mu.Unlock()
		assert.ErrorIs(t, h.m.StartWifiSetup(ctx), ErrSetupBusy)
	})

	t.Run("rejects wired, probe error, and missing probe as wired_link_active", func(t *testing.T) {
		fl := &fakeLink{up: true}
		h := newLinkHarness(t, fl)
		h.wifi.setProfile(true)
		h.m.onConnectivity(ctx, true, false)

		h.m.wiredLink = func(context.Context) (bool, error) { return true, nil }
		assert.ErrorIs(t, h.m.StartWifiSetup(ctx), ErrWiredLinkActive)

		h.m.wiredLink = func(context.Context) (bool, error) { return false, errors.New("nmcli flake") }
		assert.ErrorIs(t, h.m.StartWifiSetup(ctx), ErrWiredLinkActive,
			"a probe error fails CLOSED — never a raise the next online reading tears down")

		h.m.wiredLink = nil
		assert.ErrorIs(t, h.m.StartWifiSetup(ctx), ErrWiredLinkActive)
	})
}

// TestUserSessionAbandonmentNoImmediateReRaise pins the sibling plan's §5
// regression: an abandoned user-requested session must not produce an
// immediate sustained-offline re-raise — the expiry teardown resets the
// offline window (the clearOffline pairing), so a fresh full window of
// confirmed absence is required before any automatic raise.
func TestUserSessionAbandonmentNoImmediateReRaise(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	h.m.onConnectivity(ctx, true, false)
	require.Equal(t, StateOnline, h.m.State())

	require.NoError(t, func() error {
		h.m.wiredLink = func(context.Context) (bool, error) { return false, nil }
		return h.m.StartWifiSetup(ctx)
	}())
	<-h.m.events
	h.m.applyUserSetup(ctx)
	require.Equal(t, StateAPActive, h.m.State())

	h.tickN(ctx, 120) // the 30-minute session expires; teardown lands
	require.Equal(t, StateOfflineRetrying, h.m.State())
	require.Equal(t, 1, h.rec.count("ap.Down"))

	h.tickN(ctx, windowSamples-1)
	assert.Equal(t, 1, h.rec.count("ap.Up"),
		"no re-raise inside the fresh window — the abandoned session must not chain")
	h.tick(ctx)
	assert.Equal(t, 2, h.rec.count("ap.Up"),
		"a genuine full window of confirmed absence still raises normally")
}

// TestStartWifiSetupFromActiveAPRefreshesSession pins the ap_active accept's
// amended semantics (the sibling plan's admission amendment): the re-latched
// user-requested session gets a FRESH 30-minute clock even though ensureAPUp
// early-returns (the AP is already up and never re-arms the timer) — from the
// out-of-box UNBOUNDED session this is what makes §4.2's "30 min — always,
// including from StateUnprovisioned" true.
func TestStartWifiSetupFromActiveAPRefreshesSession(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.m.wiredLink = func(context.Context) (bool, error) { return false, nil }
	h.wifi.setProfile(false)
	h.m.onConnectivity(ctx, false, false) // out-of-box raise: UNBOUNDED session
	require.Equal(t, StateAPActive, h.m.State())
	h.tickN(ctx, 130) // well past 30 minutes: the unbounded session holds
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 0, h.rec.count("ap.Down"))

	// The app asks for setup while the AP is already up.
	require.NoError(t, h.m.StartWifiSetup(ctx))
	ev := <-h.m.events
	require.Equal(t, evUserSetup, ev.kind)
	h.m.applyUserSetup(ctx)
	require.Equal(t, StateAPActive, h.m.State())

	// The refreshed session is BOUNDED with a fresh clock.
	h.tickN(ctx, 119)
	assert.Equal(t, 0, h.rec.count("ap.Down"), "the fresh 30-minute clock holds")
	h.tick(ctx)
	assert.Equal(t, 1, h.rec.count("ap.Down"),
		"the re-latched user session must expire on ITS OWN fresh bound")
}
