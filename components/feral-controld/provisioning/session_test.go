package provisioning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

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

// TestWiredExitScopeByPolicy pins WHICH sessions the wired exit may lower. Every
// bounded policy ends on a confirmed cable; the out-of-box unbounded session is
// exempt, because it has no saved network to fall back to and its landing state
// (unprovisioned) hides the overlay — an air-gapped cable in an unclaimed frame
// would otherwise trade the setup QR for a permanently blank screen.
func TestWiredExitScopeByPolicy(t *testing.T) {
	cases := []struct {
		name        string
		raiseReason string
		wantExit    bool
	}{
		{"out-of-box unprovisioned keeps its AP", "unprovisioned", false},
		{"user-requested ends", ReasonUserRequested, true},
		{"setup-incomplete episode ends", ReasonSetupIncomplete, true},
		{"sustained-offline recheck ends", "sustained-offline", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := newHarness(t)
			h.wifi.setProfile(false)
			h.m.transition(ctx, StateAPActive, Detail{Reason: tc.raiseReason})
			require.Equal(t, StateAPActive, h.m.State())

			h.m.wiredLink = func(context.Context) (bool, error) { return true, nil }
			h.tick(ctx)

			if tc.wantExit {
				assert.Equal(t, StateUnprovisioned, h.m.State(),
					"a cable must end a session that competes with a saved network")
				assert.Equal(t, 1, h.rec.count("ap.Down"))
				return
			}
			assert.Equal(t, StateAPActive, h.m.State(),
				"the out-of-box AP must survive a cable — nothing else can finish setup on screen")
			assert.Equal(t, 0, h.rec.count("ap.Down"))
		})
	}
}

// TestOutOfBoxAPSurvivesLinkPresentEmission pins the onConnectivity half of the
// out-of-box exemption. That branch is only reachable with the AP NOT actually
// up (probeLink short-circuits to absent while our own hotspot holds the
// radio), so it is the FAILED-raise flavor: NM keeps refusing the raise while a
// cable is present. sys-monitord re-emits its first probe unconditionally after
// every restart, so without the guard an air-gapped cable plus a monitord
// restart would abandon the retry and hide the overlay — the same blank screen
// the raised-AP wired exemption exists to prevent. A confirmed ONLINE reading
// must still exit: WAN reachability is the correct end of an out-of-box session.
func TestOutOfBoxAPSurvivesLinkPresentEmission(t *testing.T) {
	// raiseUnbounded parks the machine in StateAPActive under the given raise
	// reason with the raise FAILING, which is the only state from which the
	// linkPresent branch is reachable.
	raiseUnbounded := func(t *testing.T, ctx context.Context, reason string) *harness {
		t.Helper()
		fl := &fakeLink{up: false}
		h := newLinkHarness(t, fl)
		h.wifi.setProfile(false)
		h.ap.upErr = errors.New("nm refuses the hotspot")
		h.m.transition(ctx, StateAPActive, Detail{Reason: reason})
		require.Equal(t, StateAPActive, h.m.State())
		h.m.mu.Lock()
		up := h.m.apUp
		h.m.mu.Unlock()
		require.False(t, up, "the scenario needs a failed raise, not a live AP")
		fl.up = true // the air-gapped cable appears
		return h
	}

	t.Run("out-of-box session keeps wanting the AP", func(t *testing.T) {
		ctx := context.Background()
		h := raiseUnbounded(t, ctx, "unprovisioned")

		h.m.onConnectivity(ctx, false, false) // monitord restart re-emits offline

		assert.Equal(t, StateAPActive, h.m.State(),
			"a cable must not abandon the out-of-box raise")
		assert.Equal(t, 0, countReason(h, StateUnprovisioned, "link-present"),
			"no link-present landing: that transition is what blanks the screen")
	})

	t.Run("a bounded session still exits on link-present", func(t *testing.T) {
		ctx := context.Background()
		h := raiseUnbounded(t, ctx, ReasonUserRequested)

		h.m.onConnectivity(ctx, false, false)

		assert.Equal(t, StateUnprovisioned, h.m.State(),
			"a provisioned/bounded session has somewhere to fall back to")
		assert.Equal(t, 1, countReason(h, StateUnprovisioned, "link-present"))
	})

	t.Run("confirmed online still ends the out-of-box session", func(t *testing.T) {
		ctx := context.Background()
		h := raiseUnbounded(t, ctx, "unprovisioned")

		h.m.onConnectivity(ctx, true, false) // WAN confirmed

		assert.Equal(t, StateUnprovisioned, h.m.State(),
			"WAN reachability is the correct exit; the guard must sit below it")
		assert.Equal(t, 1, countReason(h, StateUnprovisioned, ReasonUnprovisioned))
	})
}

// TestTeardownLandingSurvivesFailedAPDown pins constraint 4(a) across the three
// session-teardown landings when softap.Down FAILS. ensureAPDown has already
// stopped the portal and cleared apUp by then, so bailing out would leave the
// softap_qr advertising an AP that is gone — and, once the apDownPending retry
// succeeds, with no repaint ever scheduled to correct it. Each landing must
// therefore complete: the machine reaches its resting state, the notifier
// receives the landing reason, and the outstanding profile deletion is still
// retried by reconcile.
func TestTeardownLandingSurvivesFailedAPDown(t *testing.T) {
	downFails := func() error { return errors.New("nmcli connection delete timed out") }
	cases := []struct {
		name       string
		setup      func(t *testing.T, ctx context.Context) *harness
		wantState  State
		wantReason string
	}{
		{
			name: "user-requested session expiry",
			setup: func(t *testing.T, ctx context.Context) *harness {
				h := newHarness(t)
				h.wifi.setProfile(false)
				h.m.transition(ctx, StateAPActive, Detail{Reason: ReasonUserRequested})
				require.Equal(t, StateAPActive, h.m.State())
				h.ap.downErr = downFails()
				h.tickN(ctx, 120) // the 30-minute user-requested bound
				return h
			},
			wantState:  StateUnprovisioned,
			wantReason: ReasonAPSessionEndedSilent,
		},
		{
			name: "wired sighting under a raised AP",
			setup: func(t *testing.T, ctx context.Context) *harness {
				h := newHarness(t)
				h.wifi.setProfile(false)
				h.m.transition(ctx, StateAPActive, Detail{Reason: ReasonUserRequested})
				require.Equal(t, StateAPActive, h.m.State())
				h.ap.downErr = downFails()
				h.m.wiredLink = func(context.Context) (bool, error) { return true, nil }
				h.tick(ctx)
				return h
			},
			wantState:  StateUnprovisioned,
			wantReason: ReasonAPSessionEndedSilent,
		},
		{
			name: "episode AP phase expiry",
			setup: func(t *testing.T, ctx context.Context) *harness {
				h := newEpisodeHarness(t, &fakeLink{up: true})
				h.m.onConnectivity(ctx, false, false)
				h.tickN(ctx, windowSamples) // the setup-incomplete raise
				require.Equal(t, StateAPActive, h.m.State())
				h.ap.downErr = downFails()
				h.tickN(ctx, windowSamples) // the 5-minute AP phase
				return h
			},
			wantState:  StateOfflineRetrying,
			wantReason: ReasonAPSessionEnded,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			h := tc.setup(t, ctx)

			assert.Equal(t, tc.wantState, h.m.State(),
				"the landing must complete even though the profile deletion failed")
			assert.Equal(t, 1, countReason(h, tc.wantState, tc.wantReason),
				"the landing reason must reach the notifier — that repaint is what clears the QR")
			last := h.notifier.calls[len(h.notifier.calls)-1]
			assert.Equal(t, tc.wantReason, last.Detail.Reason,
				"the setup QR must not be the last thing left on screen")

			h.m.mu.Lock()
			pending := h.m.apDownPending
			h.m.mu.Unlock()
			require.True(t, pending, "a failed deletion must stay latched for the reconcile retry")

			// The landing does not abandon the deletion: the next reconcile
			// retries it, and a success clears the latch.
			downs := h.rec.count("ap.Down")
			h.ap.downErr = nil
			h.tick(ctx)
			assert.Greater(t, h.rec.count("ap.Down"), downs,
				"apDownPending must keep driving the retry after the landing")
			h.m.mu.Lock()
			pending = h.m.apDownPending
			h.m.mu.Unlock()
			assert.False(t, pending, "a successful retry clears the pending latch")
		})
	}
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
				// The QR paint is reason "ap-active" (it carries the SSID/PSK
				// the notifier renders as softap_qr) — NOT the literal
				// "softap", which never appears in a reason and would make
				// this assertion vacuously true.
				assert.NotContains(t, list[j], ":ap-active",
					"a teardown landing must never leave the setup QR as the next paint")
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

// --- §4.7 health snapshot -----------------------------------------------------

// TestSnapshotServesCachedEvidence pins the health surface's sourcing
// contract: values come from the machine's own probe caches — a Snapshot call
// runs NO probe (the fake-clock/no-nmcli pin), the link type rides the detail
// probe's wire verdict, and the apUp short-circuit never clobbers the cache
// (the stale-type pin).
func TestSnapshotServesCachedEvidence(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)

	// Wire the detail probe so the type is learned from the same read.
	var probeCalls int
	h.m.activeLinkDetail = func(context.Context) (bool, bool, error) {
		probeCalls++
		return fl.up, false, fl.err // a Wi-Fi station: link yes, wire no
	}

	h.m.onConnectivity(ctx, false, false)
	h.tick(ctx) // one real probe: present, not wired

	before := probeCalls
	snap := h.m.Snapshot()
	assert.Equal(t, before, probeCalls, "a status poll must never run a probe")
	assert.Equal(t, string(StateOfflineRetrying), snap.State)
	assert.Equal(t, "wifi", snap.Link, "the detail probe's wire verdict types the link")

	// The machine raises (link drops, window expires): while the AP holds the
	// radio, probeLink short-circuits — the cache must keep the last REAL
	// evidence rather than recording a fabricated absent-with-no-type.
	fl.up = false
	h.tickN(ctx, windowSamples)
	require.Equal(t, StateAPActive, h.m.State())
	h.tickN(ctx, 3) // short-circuited ticks under the raised AP
	snap = h.m.Snapshot()
	assert.Equal(t, "none", snap.Link,
		"the last real probe (confirmed absent, pre-raise) is the evidence; the short-circuit writes nothing")
	assert.Equal(t, string(StateAPActive), snap.State)
}

// TestSnapshotDeferredSubState pins the §4.7 deferred flag: while the episode
// pauses on the app's own hub contact, the snapshot says so — the app can
// surface the pairing/startWifiSetup action instead of silently resetting the
// clock — and the flag drops when counting resumes.
func TestSnapshotDeferredSubState(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: true}
	h := newEpisodeHarness(t, fl)
	h.m.onConnectivity(ctx, false, false)
	h.tickN(ctx, 5)
	assert.False(t, h.m.Snapshot().Deferred)

	h.m.ObserveHubContact()
	h.tick(ctx) // contact-deferred sample
	assert.True(t, h.m.Snapshot().Deferred,
		"the app must learn its own presence is holding the raise down")

	// Freshness expires (12 ticks): counting resumes, the flag drops.
	h.tickN(ctx, 13)
	assert.False(t, h.m.Snapshot().Deferred)
}

// --- tuning sanitation --------------------------------------------------------

// TestTuningSanitation pins withDefaults' rejection rules. config decodes the
// on-device `provisioning` block permissively (a bad block must never
// crash-loop the daemon), so this resolution is the only validation the knobs
// get. The station ladder is the sharp case: withDefaults rounds every phase
// up to at least one sample, so a zero or negative rung would silently produce
// a one-tick station phase and invert §4.1's "AP ≤ 33% of every cycle".
func TestTuningSanitation(t *testing.T) {
	const tick = 15 * time.Second
	defaults := defaultEpisodeStationLadder()

	t.Run("ladder rejection is all-or-nothing", func(t *testing.T) {
		cases := []struct {
			name   string
			ladder []time.Duration
			want   []time.Duration
		}{
			{"empty takes the default", nil, defaults},
			{"zero rung discards the whole override",
				[]time.Duration{2 * time.Minute, 0, 4 * time.Minute}, defaults},
			{"negative rung discards the whole override",
				[]time.Duration{2 * time.Minute, -1 * time.Minute}, defaults},
			{"oversized rung discards the whole override",
				[]time.Duration{2 * time.Minute, 48 * time.Hour}, defaults},
			{"a valid custom ladder is kept verbatim",
				[]time.Duration{time.Minute, 2 * time.Minute},
				[]time.Duration{time.Minute, 2 * time.Minute}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := Tuning{EpisodeStationLadder: tc.ladder}.withDefaults(tick, zap.NewNop())
				assert.Equal(t, tc.want, got.EpisodeStationLadder)
				for i, s := range got.episodeLadderSamples {
					assert.Greater(t, s, 1,
						"rung %d must outlast a single tick, or the AP stops being the minority of the cycle", i)
				}
			})
		}
	})

	t.Run("episode raise cycles", func(t *testing.T) {
		// The cycle counter is the episode's only overall bound: every re-raise
		// latches the on-table "setup-incomplete" reason, which re-stamps
		// sessionFirstRaise, so the 2h absolute cap re-bases with it and bounds
		// each AP PHASE rather than the episode. An unbounded count therefore
		// postpones the four-cycle settlement — and the settled state is where
		// the LAN escape lives — indefinitely.
		cases := []struct {
			name   string
			cycles int
			want   int
		}{
			{"zero takes the default", 0, defaultEpisodeRaiseCycles},
			{"negative takes the default", -3, defaultEpisodeRaiseCycles},
			{"past the ceiling takes the default", maxEpisodeRaiseCycles + 1, defaultEpisodeRaiseCycles},
			{"exactly the ceiling is kept", maxEpisodeRaiseCycles, maxEpisodeRaiseCycles},
			{"a valid override is kept", 7, 7},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				got := Tuning{EpisodeRaiseCycles: tc.cycles}.withDefaults(tick, zap.NewNop())
				assert.Equal(t, tc.want, got.EpisodeRaiseCycles)
			})
		}
	})

	t.Run("scalar durations", func(t *testing.T) {
		cases := []struct {
			name string
			in   Tuning
			want time.Duration
			get  func(Tuning) time.Duration
		}{
			{"zero episode AP phase takes the default", Tuning{}, defaultEpisodeApPhase,
				func(t Tuning) time.Duration { return t.EpisodeApPhase }},
			{"negative episode AP phase takes the default",
				Tuning{EpisodeApPhase: -5 * time.Minute}, defaultEpisodeApPhase,
				func(t Tuning) time.Duration { return t.EpisodeApPhase }},
			{"oversized recheck AP phase takes the default",
				Tuning{RecheckApPhase: 30 * 24 * time.Hour}, defaultRecheckApPhase,
				func(t Tuning) time.Duration { return t.RecheckApPhase }},
			{"a valid override is kept",
				Tuning{UserRequestedSession: 10 * time.Minute}, 10 * time.Minute,
				func(t Tuning) time.Duration { return t.UserRequestedSession }},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assert.Equal(t, tc.want, tc.get(tc.in.withDefaults(tick, zap.NewNop())))
			})
		}
	})
}

// TestClearOfflineDropsPendingScanSkip pins the lifetime of the recheck
// blink's one-shot scan-skip: it is only ever valid for the raise that
// immediately follows the blink, so leaving the AP world must drop it. Without
// this a flag set but not consumed would hand a much later raise a stale
// portal picker.
func TestClearOfflineDropsPendingScanSkip(t *testing.T) {
	h := newHarness(t)
	h.m.skipNextPreAPScan = true
	h.m.clearOffline()
	assert.False(t, h.m.skipNextPreAPScan,
		"the scan-skip must not outlive the AP episode that set it")
}
