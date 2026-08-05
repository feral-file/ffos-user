package provisioning

// This file implements the escape policy of docs/network-recovery-ux.md:
// the §4.2 AP session policy (bounded sessions, the recheck cadence, the
// portal-activity deferral, the wired exit) and the §4.1 setup-incomplete
// episode (the link-present escape for unclaimed devices). Everything here
// runs on the machine's single loop goroutine unless a comment says
// otherwise; the two contact timestamps it reads (hub, portal) are written by
// request goroutines under m.mu.
//
// The two evidence shapes never share an episode (constraint 12): the episode
// below exists ONLY for the link-alive-but-useless shape and is canceled by
// confirmed link loss, which hands the device to the untouched link-absent
// machinery (sustained-offline/relocated — now with the recheck cadence).

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wifictl"
)

// sessionPolicy is the AP session's radio policy, LATCHED from the raise
// reason at raise time (§4.2's table) — never derived from hasProfile at
// expiry, which is falsified by wifictl.Join's pre-delete and biased wrong on
// nmcli errors.
type sessionPolicy int

const (
	// sessionUnbounded: the out-of-box unprovisioned raise. No timer, no
	// recheck — with no saved profile a station blink can learn nothing (the
	// portal's own rescan serves the "a network appeared" case).
	//
	// Amendment hazard: this is the ZERO value, and it now doubles as the
	// wired-exit / link-present exemption (wiredExitDue, onConnectivity's
	// linkPresent branch) — so an UNLATCHED policy fails OPEN into the
	// exemption. Harmless today because every first entry into StateAPActive
	// carries an on-table raise reason, and only later off-table reasons
	// (join failures, teardown-failure re-raises) inherit. A future raise
	// whose FIRST reason is off-table would silently get out-of-box
	// treatment: add it to latchSessionPolicy's table rather than relying on
	// the inherit path.
	sessionUnbounded sessionPolicy = iota
	// sessionRecheck: the link-confirmed-absent raises (sustained-offline,
	// relocated). AP-dominant cadence: AP up for a long phase, then a narrated
	// recheck blink, then re-raise if still no association. Unbounded cycles —
	// the QR is the correct steady state for a gone network, and every cycle
	// re-tests the world (D11's fix).
	sessionRecheck
	// sessionEpisode: the §4.1 setup-incomplete raise. Short AP phase, then
	// the escalating station ladder — AP must stay the minority of every
	// cycle (constraint 1's corollary), because the primary escape (LAN
	// pairing) lives in station mode.
	sessionEpisode
	// sessionUser: an app-triggered startWifiSetup raise. One bounded phase;
	// on expiry, teardown and resume normal state handling (the abandonment
	// net the sibling plan exists for).
	sessionUser
)

// Tuning carries the §4.1/§4.2 cadence knobs. Zero fields take the defaults
// below; withDefaults resolves them once in New. Wired from the on-device
// JSON config's permissive `provisioning` block (config.ProvisioningTuning) —
// retuning is a config edit plus daemon restart, no package rebuild.
type Tuning struct {
	// SetupIncompleteDisabled reverts unclaimed devices to
	// narration-plus-LAN-pairing only (the §4.1 kill-switch).
	SetupIncompleteDisabled bool

	EpisodeWindow         time.Duration
	EpisodeApPhase        time.Duration
	EpisodeStationLadder  []time.Duration
	EpisodeRaiseCycles    int
	HubContactFresh       time.Duration
	DeferralCycleBudget   time.Duration
	DeferralEpisodeBudget time.Duration

	RecheckApPhase time.Duration
	// RecheckApPhaseLadder is the escalating AP phase for the recheck
	// cadence's EARLY cycles (cycle 0 = first rung); once the ladder is
	// exhausted every later cycle uses RecheckApPhase. See
	// defaultRecheckApPhaseLadder for why the early rungs are short.
	RecheckApPhaseLadder  []time.Duration
	RecheckBlinkCeiling   time.Duration
	ActivationTimeout     time.Duration
	PortalActivityWindow  time.Duration
	PortalDeferralCeiling time.Duration
	UserRequestedSession  time.Duration
	SessionAbsoluteCap    time.Duration

	// episodeWindowSamples / episodeLadderSamples are the tick-sample
	// equivalents, derived by withDefaults — the episode window is
	// sample-counted like the offline window (one sample per tick).
	episodeWindowSamples int
	episodeLadderSamples []int
}

// Session-policy defaults (§4.1/§4.2). The numbers on-screen copy states are
// read from these same constants via the resolved Tuning, so prose and
// machine can not drift apart.
const (
	defaultEpisodeWindow         = 5 * time.Minute
	defaultEpisodeApPhase        = 5 * time.Minute
	defaultEpisodeRaiseCycles    = 4
	defaultHubContactFresh       = 3 * time.Minute
	defaultDeferralCycleBudget   = 5 * time.Minute
	defaultDeferralEpisodeBudget = 15 * time.Minute
	defaultRecheckApPhase        = 30 * time.Minute
	defaultRecheckBlinkCeiling   = 4 * time.Minute
	defaultActivationTimeout     = 90 * time.Second
	defaultPortalActivityWindow  = 2 * time.Minute
	defaultPortalDeferralCeiling = 15 * time.Minute
	defaultUserRequestedSession  = 30 * time.Minute
	defaultSessionAbsoluteCap    = 2 * time.Hour

	// episodeFreshSamplesAfterPause mirrors the offline window's post-pause
	// freshness debt: a single counted sample after a pause can never fire
	// the raise off stale evidence.
	episodeFreshSamplesAfterPause = 2

	// maxEpisodeRaiseCycles is the sanity ceiling on the configured cycle
	// count — the episode's only overall bound (see withDefaults). Generous by
	// design: it exists to reject a typo or an "effectively never settle"
	// value, not to second-guess an operator who genuinely wants a long
	// episode.
	maxEpisodeRaiseCycles = 100
)

// defaultRecheckApPhaseLadder is the escalating AP phase for the recheck
// cadence's early cycles: 2m → 5m → 15m, then RecheckApPhase (30m) steady.
// A fixed 30-minute first phase left a structural blind window — the single
// radio cannot scan under its own AP, so the ONLY way the machine notices the
// user's network returning is the blink at phase end. Field incident
// FF1-8EVTK3RE (2026-08-05): the user restored their hotspot ~3 minutes after
// the AP rose and the frame sat blind for what would have been the full 30
// minutes. Short early rungs catch that dominant real recovery (a human
// fixing the network within minutes); the steady state keeps the documented
// 30-minute cadence so a truly gone network costs no extra radio churn.
//
// Accepted worst-case duty cycle on rung 0: the blink may hold the AP down
// for up to RecheckBlinkCeiling (4m, e.g. a hidden-profile activation eating
// its full 90s timeout), so a 2-minute rung can spend more time blinking
// than broadcasting. Tolerated for at most the first two rungs because
// during a recheck session the escape a present human actually uses is
// joining THEIR OWN returned network — which is what the blink attempts —
// unlike the §4.1 episode, whose LAN escape lives in station mode and pins
// "AP ≤ 33% of every cycle". Anyone lowering the knob below the built-in
// rungs trades exactly this duty cycle; minRecheckRung is the hard floor.
func defaultRecheckApPhaseLadder() []time.Duration {
	return []time.Duration{2 * time.Minute, 5 * time.Minute, 15 * time.Minute}
}

// minRecheckRung is the floor a configured recheck rung must clear: below
// one minute the AP-down blink time dominates the cycle outright and the QR
// flaps faster than a human can act on it — a typo, not a cadence.
const minRecheckRung = time.Minute

// usableRecheckLadder validates a configured recheck AP-phase ladder,
// ALL-OR-NOTHING like usableLadder above and for the same reason: the rungs
// are one escalation shape, and splicing a default into a custom ladder
// produces a cadence nobody designed. A rung below minRecheckRung or over
// the tuning ceiling discards the whole override. An empty ladder means
// "unset" and takes the default — an operator who wants the old fixed
// cadence sets a single rung equal to recheckApPhase.
func usableRecheckLadder(ladder []time.Duration, logger *zap.Logger) []time.Duration {
	if len(ladder) == 0 {
		return defaultRecheckApPhaseLadder()
	}
	for _, d := range ladder {
		if d < minRecheckRung || d > maxTuningDuration {
			logger.Warn("provisioning: recheck AP-phase ladder has an out-of-range rung; using the built-in ladder",
				zap.Duration("rung", d), zap.Durations("configured", ladder))
			return defaultRecheckApPhaseLadder()
		}
	}
	return ladder
}

// defaultEpisodeStationLadder is the escalating station phase between episode
// AP phases: early cycles favor the user who is probably still nearby
// (missing an AP phase costs 5 minutes, not 20), later cycles favor observing
// WAN recovery; AP stays ≤ 33% of every cycle.
func defaultEpisodeStationLadder() []time.Duration {
	return []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute}
}

// usableLadder validates a configured station ladder, ALL-OR-NOTHING: one
// out-of-range rung discards the whole override. Per-rung substitution is the
// tempting alternative and is wrong — the rungs are not independent numbers
// but one escalation shape (early cycles favor the user who is probably still
// nearby, later ones favor observing WAN recovery), and splicing a default
// into a custom ladder produces a cadence nobody designed.
//
// A non-positive rung is the failure that actually bites: withDefaults rounds
// every phase up to at least ONE sample, so a zero or negative rung yields a
// station phase one tick long. The episode then spends nearly all of every
// cycle in AP mode — the exact inversion of §4.1's "AP ≤ 33% of every cycle",
// whose whole point is that the primary escape (LAN pairing) only exists in
// station mode.
func usableLadder(ladder []time.Duration, logger *zap.Logger) []time.Duration {
	if len(ladder) == 0 {
		return defaultEpisodeStationLadder()
	}
	for _, d := range ladder {
		if d <= 0 || d > maxTuningDuration {
			logger.Warn("provisioning: episode station ladder has an out-of-range rung; using the built-in ladder",
				zap.Duration("rung", d), zap.Durations("configured", ladder))
			return defaultEpisodeStationLadder()
		}
	}
	return ladder
}

// maxTuningDuration is the sanity ceiling every configured duration knob must
// clear: an honestly-typed but absurd value (a year-long AP phase, a week-long
// station rung) would disable the escape policy this whole file exists to
// guarantee, with nothing on screen or in the logs saying so. Past the ceiling
// the value is treated as a typo, not as intent.
//
// What it does NOT catch, so nobody trusts it for this: an integer-seconds
// config value large enough to overflow the seconds→Duration multiplication.
// That wraps to an arbitrary small value BEFORE it ever reaches this type
// (18446744074 seconds becomes ~290ms), and no ceiling on the far side can
// recover the original magnitude. Overflow is rejected at the conversion site
// instead — see maxTuningSeconds and the secs helper in
// provisioning_wiring.go, whose 24h bound mirrors this one by hand.
// This ceiling remains the guard for Tuning values built directly in Go
// (tests, future callers) that never pass through that conversion.
const maxTuningDuration = 24 * time.Hour

// withDefaults resolves unset/out-of-range fields to the package defaults and
// derives the tick-sample equivalents. Rounding errs upward (more evidence
// before a one-way raise), minimum one sample. The logger carries the
// substitutions: config.ProvisioningTuning decodes permissively by design (a
// bad block must never crash-loop the daemon), so this is the only place an
// operator learns a knob was rejected.
func (t Tuning) withDefaults(tick time.Duration, logger *zap.Logger) Tuning {
	if logger == nil {
		logger = zap.NewNop()
	}
	def := func(field string, v *time.Duration, d time.Duration) {
		switch {
		case *v == 0:
			// The documented "unset" path — not a rejection, so no warn.
			*v = d
		case *v < 0 || *v > maxTuningDuration:
			logger.Warn("provisioning: tuning value out of range; using the built-in default",
				zap.String("field", field), zap.Duration("configured", *v), zap.Duration("default", d))
			*v = d
		}
	}
	def("episodeWindow", &t.EpisodeWindow, defaultEpisodeWindow)
	def("episodeApPhase", &t.EpisodeApPhase, defaultEpisodeApPhase)
	def("hubContactFresh", &t.HubContactFresh, defaultHubContactFresh)
	def("deferralCycleBudget", &t.DeferralCycleBudget, defaultDeferralCycleBudget)
	def("deferralEpisodeBudget", &t.DeferralEpisodeBudget, defaultDeferralEpisodeBudget)
	def("recheckApPhase", &t.RecheckApPhase, defaultRecheckApPhase)
	t.RecheckApPhaseLadder = usableRecheckLadder(t.RecheckApPhaseLadder, logger)
	def("recheckBlinkCeiling", &t.RecheckBlinkCeiling, defaultRecheckBlinkCeiling)
	def("activationTimeout", &t.ActivationTimeout, defaultActivationTimeout)
	def("portalActivityWindow", &t.PortalActivityWindow, defaultPortalActivityWindow)
	def("portalDeferralCeiling", &t.PortalDeferralCeiling, defaultPortalDeferralCeiling)
	def("userRequestedSession", &t.UserRequestedSession, defaultUserRequestedSession)
	def("sessionAbsoluteCap", &t.SessionAbsoluteCap, defaultSessionAbsoluteCap)
	// The cycle counter is the ONLY thing bounding an episode's overall length,
	// which is why an absurd value here has to be rejected rather than merely
	// looking silly. Every episode re-raise carries reason "setup-incomplete",
	// an on-table reason, so latchSessionPolicy re-stamps sessionFirstRaise and
	// the §4.2 two-hour absolute cap re-bases with it: that cap bounds each AP
	// PHASE, never the episode. (The per-session reset is deliberate — a fresh
	// raise really is a fresh session — so the fix belongs here, not in
	// sessionFirstRaise.) Left unbounded, episodeRaiseCycles = 100000 would
	// postpone the documented four-cycle settlement effectively forever, and
	// the settled state is where the LAN escape lives.
	if t.EpisodeRaiseCycles <= 0 || t.EpisodeRaiseCycles > maxEpisodeRaiseCycles {
		if t.EpisodeRaiseCycles != 0 {
			logger.Warn("provisioning: episodeRaiseCycles out of range; using the built-in default",
				zap.Int("configured", t.EpisodeRaiseCycles),
				zap.Int("max", maxEpisodeRaiseCycles),
				zap.Int("default", defaultEpisodeRaiseCycles))
		}
		t.EpisodeRaiseCycles = defaultEpisodeRaiseCycles
	}
	t.EpisodeStationLadder = usableLadder(t.EpisodeStationLadder, logger)
	samples := func(d time.Duration) int {
		n := int((d + tick - 1) / tick)
		if n < 1 {
			n = 1
		}
		return n
	}
	t.episodeWindowSamples = samples(t.EpisodeWindow)
	t.episodeLadderSamples = make([]int, len(t.EpisodeStationLadder))
	for i, d := range t.EpisodeStationLadder {
		t.episodeLadderSamples[i] = samples(d)
	}
	return t
}

// -----------------------------------------------------------------------------
// Claim snapshot + contact seams
// -----------------------------------------------------------------------------

// SetClaimed feeds the executor's claim observer into the claim snapshot
// (constraint 8). The SNAPSHOT write is synchronous under m.mu — never queued
// — because the executor fires exactly once per claim flip and a dropped
// queue event would be permanent: an episode already counting would then
// raise the AP over a just-claimed frame, the precise #233 harm the snapshot
// exists to prevent (and the synchronous recheck blink can hold the loop for
// minutes, making a full queue a real timing window, not a theoretical one).
// The event send below is only a best-effort NUDGE so a pending episode
// cancels promptly; if it drops, every raise decision re-reads the snapshot
// (isClaimed) before firing, so the worst case is stale bookkeeping, never a
// raise.
func (m *Machine) SetClaimed(claimed bool) {
	m.mu.Lock()
	changed := m.claimed != claimed
	m.claimed = claimed
	m.mu.Unlock()
	if !changed {
		return
	}
	m.logger.Info("provisioning: claim snapshot updated", zap.Bool("claimed", claimed))
	select {
	case m.events <- event{kind: evClaim, claimed: claimed}:
	default:
		m.logger.Warn("provisioning: claim nudge dropped (queue full); raise checks re-read the snapshot",
			zap.Bool("claimed", claimed))
	}
}

// isClaimed reads the claim snapshot. Under m.mu because SetClaimed writes it
// from executor goroutines.
func (m *Machine) isClaimed() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.claimed
}

// applyClaim runs the loop-side claim NUDGE: the snapshot itself was already
// written by SetClaimed; this only performs the prompt episode cancel (§4.1's
// cancel edge — the app user who just paired must not have the link yanked by
// a pending raise).
func (m *Machine) applyClaim(ctx context.Context, claimed bool) {
	if claimed {
		m.cancelEpisode(ctx, "claimed")
	}
}

// ObserveHubContact records one control-plane hub contact (wired to the hub's
// contact observer; already filtered to the counted routes and non-loopback
// sources there). Request-goroutine-safe.
func (m *Machine) ObserveHubContact() {
	m.mu.Lock()
	m.lastHubContact = m.clock.Now()
	m.mu.Unlock()
}

// observePortalActivity records one human-caused portal request (wired into
// portal.Config.ActivityObserved by ensureAPUp). Request-goroutine-safe.
func (m *Machine) observePortalActivity() {
	m.mu.Lock()
	m.lastPortalActivity = m.clock.Now()
	m.mu.Unlock()
}

// observePortalTraffic records one portal request of ANY kind — captive
// probes and root fetches included (wired into portal.Config.TrafficObserved
// by ensureAPUp). Weaker evidence than observePortalActivity: it proves a
// phone is attached to the AP, not that a human acted. Consumed only by the
// recheck blink's attached-phone deferral in sessionExpiryDue.
// Request-goroutine-safe.
func (m *Machine) observePortalTraffic() {
	m.mu.Lock()
	m.lastPortalTraffic = m.clock.Now()
	m.mu.Unlock()
}

// hubContactFresh reports whether a counted hub contact landed within the
// freshness window — the "a human's app is talking to this device" signal the
// episode defers on.
func (m *Machine) hubContactFresh() bool {
	m.mu.Lock()
	last := m.lastHubContact
	m.mu.Unlock()
	return !last.IsZero() && m.clock.Now().Sub(last) < m.tuning.HubContactFresh
}

// -----------------------------------------------------------------------------
// §4.7 health snapshot
// -----------------------------------------------------------------------------

// NetworkSnapshot is the machine's contribution to the additive `network`
// status object (network-recovery-ux §4.7): the app renders the same
// diagnosis the screen shows. Everything here is CACHED evidence from the
// machine's own probes — a status poll performs no nmcli work.
type NetworkSnapshot struct {
	// State is the machine state; Reason its last transition reason.
	State  string
	Reason string
	// SSID is the current/last join target where one is known (the portal
	// status's SSID); empty otherwise.
	SSID string
	// Link is "wifi" | "ethernet" | "none" | "unknown" — the last REAL probe's
	// outcome plus, when a wire verdict accompanied it, the type. During AP
	// phases the frame is off the LAN and this surface is dark anyway.
	Link string
	// Deferred reports the setup-incomplete episode is currently pausing its
	// raise on this control channel's own contact.
	Deferred bool
}

// Snapshot serves the §4.7 health object from the mu-guarded caches. Safe
// from any goroutine; never probes.
func (m *Machine) Snapshot() NetworkSnapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	link := "unknown"
	if m.linkOutcomeKnown {
		switch m.linkOutcome {
		case linkAbsent:
			link = "none"
		case linkPresent:
			switch {
			case m.wiredKnown && m.wiredLast:
				link = "ethernet"
			case m.wiredKnown:
				link = "wifi"
			default:
				// A confirmed link whose type was never measured (no detail
				// probe wired and no WiredLink run yet): the honest answer is
				// unknown, not a guess.
				link = "unknown"
			}
		}
	}
	return NetworkSnapshot{
		State:    string(m.state),
		Reason:   m.lastReason,
		SSID:     m.status.SSID,
		Link:     link,
		Deferred: m.episodeDeferredShared,
	}
}

// -----------------------------------------------------------------------------
// App-triggered setup (startWifiSetup — docs/app-triggered-wifi-setup.md)
// -----------------------------------------------------------------------------

// Admission rejections for StartWifiSetup, matched by the command handler to
// its wire codes ({ok:false, code}).
var (
	// ErrSetupBusy: the machine is mid-join or still starting — a raise now
	// would tear the radio out from under an in-flight activation.
	ErrSetupBusy = errors.New("provisioning is busy")
	// ErrWiredLinkActive: a live ethernet link, an errored probe, or an
	// unavailable probe — all fail CLOSED. Raising the AP on a wired frame
	// would be torn down by the next online reading (the flow silently
	// fails), and suppressing that exit would expose the unauthenticated
	// portal on a routable wired address; see the sibling plan's §3 risk row.
	ErrWiredLinkActive = errors.New("wired link active")
)

// StartWifiSetup validates admission for the app-triggered raise and QUEUES
// it, returning before any radio work: the caller's reply must precede the
// raise (constraint 1 of the sibling plan — the AP severs the station link
// that carries the response). Accepts from online / offline_retrying /
// unprovisioned / ap_active (idempotent refresh); rejects busy from
// joining/starting; rejects wired (fail closed on probe errors). Safe from
// any goroutine.
func (m *Machine) StartWifiSetup(ctx context.Context) error {
	switch m.State() {
	case StateJoining, StateStarting:
		return ErrSetupBusy
	}
	if m.wiredLink == nil {
		return fmt.Errorf("%w: wired-link probe unavailable", ErrWiredLinkActive)
	}
	wired, err := m.probeWired(ctx)
	if err != nil {
		return fmt.Errorf("%w: probe failed: %w", ErrWiredLinkActive, err)
	}
	if wired {
		return ErrWiredLinkActive
	}
	select {
	case m.events <- event{kind: evUserSetup}:
		return nil
	default:
		return ErrSetupBusy
	}
}

// applyUserSetup performs the user-requested raise on the loop goroutine,
// mirroring every existing raise site (constraint 3's triple; resetJoinStatus
// runs before the transition because it is edge-gated on state !=
// StateAPActive). The §4.2 session machinery then bounds the session at the
// user-requested row's 30 minutes — the abandonment net.
func (m *Machine) applyUserSetup(ctx context.Context) {
	// Re-check on the loop: a portal join may have started since admission.
	switch m.State() {
	case StateJoining, StateStarting:
		m.logger.Warn("provisioning: user-requested setup ignored; machine became busy after admission")
		return
	}
	m.logger.Info("provisioning: user-requested setup; raising the AP")
	m.clearOffline()
	m.resetJoinStatus()
	m.transition(ctx, StateAPActive, Detail{
		Reason:  ReasonUserRequested,
		Message: "Wi-Fi setup requested from the app",
	})
	// Arm AFTER the transition (the latch inside it is what sets the
	// user-requested bound). On a fresh raise this double-arms harmlessly
	// with ensureAPUp's own arm (same tick, same clock reading); on the
	// ACCEPTED-FROM-ap_active flavor it is load-bearing — the AP is already
	// up, so ensureAPUp early-returns and never arms, and without this the
	// re-latched user session would inherit the PREVIOUS session's phase
	// clock (stale by up to a full recheck phase) or, from the unbounded
	// out-of-box session, no clock at all — violating §4.2's "30 min —
	// always, including from StateUnprovisioned".
	m.armSessionTimer()
}

// -----------------------------------------------------------------------------
// §4.2 session policy
// -----------------------------------------------------------------------------

// latchSessionPolicy maps a raise reason onto the session policy table
// (§4.2). Called by transition for every entry into StateAPActive; reasons
// outside the table (join-failure re-raises, teardown-failure re-raises, the
// AP-up/scanning announcements) RETAIN the current policy — an attempted join
// is positive evidence of a present human, and one typo must not escalate a
// bounded session into an unbounded cadence (nor the reverse).
func (m *Machine) latchSessionPolicy(reason string) {
	var policy sessionPolicy
	switch reason {
	case "unprovisioned":
		policy = sessionUnbounded
	case "sustained-offline", ReasonRelocated:
		policy = sessionRecheck
	case ReasonSetupIncomplete:
		policy = sessionEpisode
	case ReasonUserRequested:
		policy = sessionUser
	default:
		return // inherit: keep policy, reason, and the session-cap anchor
	}
	m.sessionPolicy = policy
	m.sessionReason = reason
	m.sessionFirstRaise = m.clock.Now()
}

// sessionPhaseBound returns the current AP phase's duration, or 0 for an
// unbounded session.
func (m *Machine) sessionPhaseBound() time.Duration {
	switch m.sessionPolicy {
	case sessionRecheck:
		// Escalating backoff: early cycles read the ladder (short phases so
		// a network the user fixes within minutes is noticed within
		// minutes), later cycles settle on RecheckApPhase. recheckCycle is
		// advanced by the blink's still-gone re-raise and reset only at
		// clearOffline's episode boundary — see both sites.
		if m.recheckCycle < len(m.tuning.RecheckApPhaseLadder) {
			return m.tuning.RecheckApPhaseLadder[m.recheckCycle]
		}
		return m.tuning.RecheckApPhase
	case sessionEpisode:
		return m.tuning.EpisodeApPhase
	case sessionUser:
		return m.tuning.UserRequestedSession
	default:
		return 0
	}
}

// armSessionTimer starts the current AP phase's clock. Called by ensureAPUp
// on every successful raise (so a join-failure re-raise resets its clock, per
// the §4.2 table) and by applyRescan's re-arm.
func (m *Machine) armSessionTimer() {
	if m.sessionPhaseBound() > 0 {
		m.sessionPhaseStart = m.clock.Now()
	}
}

// cancelSessionTimer stops the phase clock WITHOUT touching the policy latch:
// applyJoin cancels the timer, and the failure re-raise re-arms a fresh one
// under the retained policy (the one-typo rule).
func (m *Machine) cancelSessionTimer() {
	m.sessionPhaseStart = time.Time{}
}

// sessionExpiryDue reports whether the raised AP session's current phase is
// over, honoring the portal-activity deferral and the absolute cap. Loop
// goroutine, with the AP actually up.
func (m *Machine) sessionExpiryDue() bool {
	bound := m.sessionPhaseBound()
	if bound <= 0 || m.sessionPhaseStart.IsZero() {
		return false
	}
	now := m.clock.Now()
	// The absolute cap backstops the BOUNDED rows against applyRescan re-arms
	// (an actively rescanning user is present, so re-arming is intended — but
	// not forever). The recheck cadence is unbounded by design, so there is
	// no cap to consume there.
	if m.sessionPolicy != sessionRecheck && !m.sessionFirstRaise.IsZero() &&
		now.Sub(m.sessionFirstRaise) >= m.tuning.SessionAbsoluteCap {
		return true
	}
	if now.Sub(m.sessionPhaseStart) < bound {
		return false
	}
	// Portal-activity deferral: a human-caused portal request (the /connect
	// and /rescan handlers only — OS captive probes never count, see
	// portal.Config.ActivityObserved) within the window defers the teardown,
	// up to a ceiling past the phase bound. Applies to session bounds and
	// recheck phases alike (§4.2: "a teardown — session bound and recheck
	// alike — defers").
	m.mu.Lock()
	lastActivity := m.lastPortalActivity
	m.mu.Unlock()
	if !lastActivity.IsZero() && now.Sub(lastActivity) < m.tuning.PortalActivityWindow &&
		now.Sub(m.sessionPhaseStart) < bound+m.tuning.PortalDeferralCeiling {
		return false
	}
	// Attached-phone deferral, RECHECK ONLY: any portal traffic — root
	// fetches and OS captive probes included — within the activity window
	// defers a recheck blink, up to the same ceiling. This deliberately
	// reverses portal.Config.ActivityObserved's "probes never count" rule
	// for this one policy: with ladder-short early phases (2 minutes, not
	// 30), a phone that has joined the AP but not yet submitted anything —
	// the user reading the network list — would otherwise be kicked by the
	// first blink, and an associated phone's automatic probes are exactly
	// the "a human is attached" evidence that gap needs. The ceiling keeps
	// an idle phone left on the AP from pinning it forever, and the bounded
	// policies keep the probes-never-count rule because their phases were
	// never shortened.
	//
	// Best-effort, not a guarantee: the portal page itself never polls, so
	// after the initial page load this signal is only as fresh as the OS's
	// own captive re-probe cadence — minutes-scale and backing off on some
	// platforms. A phone whose OS goes quiet for a full activity window is
	// treated as absent and the blink proceeds; the mitigation narrows R1,
	// it does not close it.
	if m.sessionPolicy == sessionRecheck {
		m.mu.Lock()
		lastTraffic := m.lastPortalTraffic
		m.mu.Unlock()
		if !lastTraffic.IsZero() && now.Sub(lastTraffic) < m.tuning.PortalActivityWindow &&
			now.Sub(m.sessionPhaseStart) < bound+m.tuning.PortalDeferralCeiling {
			return false
		}
	}
	return true
}

// expireSession runs the policy's phase-expiry action. Loop goroutine.
func (m *Machine) expireSession(ctx context.Context) {
	switch m.sessionPolicy {
	case sessionRecheck:
		m.runRecheckBlink(ctx)
	case sessionEpisode:
		m.endEpisodeAPPhase(ctx)
	case sessionUser:
		m.endUserSession(ctx)
	}
}

// releaseAPForLanding performs the teardown half of a constraint-4 session
// landing: drop the AP and stop the phase clock. It deliberately DISCARDS
// ensureAPDown's verdict, and that discard is load-bearing — every session
// landing below depends on it.
//
// Why the landing must proceed even when softap.Down fails: ensureAPDown has
// already stopped the portal and cleared apUp by the time it reports false
// (only the NM profile deletion is outstanding, latched in apDownPending and
// retried by every subsequent reconcile). Returning early therefore preserves
// nothing except the on-screen softap_qr — a QR advertising an AP that is gone
// with no portal behind it, which is exactly what constraint 4(a) forbids.
// Worse, it is not self-correcting: the next tick takes the failed-raise
// link-present exit, whose "link-present" reason the notifier deliberately
// leaves un-repainted, and if the apDownPending retry then succeeds no repaint
// is ever scheduled at all, so the stale QR survives indefinitely.
//
// The recheck blink is the ONE teardown that still aborts on failure
// (runRecheckBlink) — it is not a landing but the prelude to a measurement,
// and a still-held radio would fabricate its verdict.
func (m *Machine) releaseAPForLanding(ctx context.Context) {
	m.ensureAPDown(ctx)
	m.cancelSessionTimer()
}

// endUserSession tears an expired user-requested session down and resumes
// normal state handling: the saved profile is never touched, so NM
// autoconnect restores the previous network on its own (bench-verified, §3 of
// the plan: 6s reassociation after teardown). This exists only so a user who
// taps startWifiSetup and changes their mind cannot strand the frame in setup
// mode.
func (m *Machine) endUserSession(ctx context.Context) {
	// Lands regardless of the teardown's verdict — see releaseAPForLanding.
	m.releaseAPForLanding(ctx)
	m.clearOffline()
	m.transition(ctx, m.restingStateForProfile(ctx), m.teardownLandingDetail())
}

// restingStateForProfile picks the teardown landing state by saved-profile
// presence — the same routing every other AP exit uses.
func (m *Machine) restingStateForProfile(ctx context.Context) State {
	if m.hasProfile(ctx) {
		return StateOfflineRetrying
	}
	return StateUnprovisioned
}

// teardownLandingDetail picks the constraint-4 teardown landing narration:
// the machine holds the claim snapshot, so IT picks the narrated or the
// silent-hide variant and the notifier stays a pure reason switch. A claimed
// frame's teardown hides (artwork returns); an unclaimed frame's narrates the
// LAN escape with floor-safe copy (no live timer is running at this instant —
// constraint 13).
func (m *Machine) teardownLandingDetail() Detail {
	if m.isClaimed() {
		return Detail{Reason: ReasonAPSessionEndedSilent}
	}
	return Detail{
		Reason: ReasonAPSessionEnded,
		Message: "Reconnecting to your Wi-Fi network… To finish setup, connect your phone " +
			"to the same network as the Art Computer and open the Feral File app.",
	}
}

// wiredExitDue probes for a live ethernet link under a RAISED AP (§4.2's
// wired exit): the AP cannot fix a wired network, and the probeLink
// short-circuit that protects Wi-Fi's own-hotspot ambiguity does not apply —
// an ethernet row in the device survey has no such ambiguity (constraint 6).
// This also lowers a user-requested session when a cable is plugged in
// mid-setup — intended. Costs one nmcli read per tick for the AP session's
// duration; accepted. Errors defer (constraint 11).
//
// The out-of-box unbounded session is EXEMPT, and that exemption is the whole
// reason this policy check lives here rather than at the call site. The wired
// exit exists to stop an AP from competing with a network the frame could fall
// back to; an unprovisioned frame has no saved network to fall back to, so the
// exit would tear the setup AP down and land StateUnprovisioned, whose
// notifier branch HIDES the overlay — a permanently blank screen on an
// air-gapped wired frame that has never been claimed. Pre-PR the AP simply
// stayed up, and a wired out-of-box device completes its claim over LAN /
// relayer with the AP still broadcasting, so leaving it up costs nothing.
//
// Scope, precisely: this covers a RAISED out-of-box AP, and onConnectivity's
// linkPresent branch covers the failed-raise flavor of the same session. It
// does NOT cover a frame that boots with the cable already plugged in — that
// path never enters StateAPActive at all (StateStarting sees linkPresent and
// parks in StateUnprovisioned), so an air-gapped wired frame booted from cold
// still shows nothing. That remains an open dead end, out of scope here.
func (m *Machine) wiredExitDue(ctx context.Context) bool {
	if m.sessionPolicy == sessionUnbounded {
		return false
	}
	if m.wiredLink == nil {
		return false
	}
	wired, err := m.probeWired(ctx)
	if err != nil {
		m.logger.Debug("provisioning: wired-link probe failed during AP session; deferring", zap.Error(err))
		return false
	}
	return wired
}

// exitRaisedAPForWire tears a raised AP down after a confirmed wired
// sighting and lands per constraint 4.
func (m *Machine) exitRaisedAPForWire(ctx context.Context) {
	m.logger.Info("provisioning: wired link sighted under a raised AP; ending the session")
	// Lands regardless of the teardown's verdict — see releaseAPForLanding.
	m.releaseAPForLanding(ctx)
	m.cancelEpisode(ctx, "wired link sighted")
	m.clearOffline()
	m.transition(ctx, m.restingStateForProfile(ctx), m.teardownLandingDetail())
}

// -----------------------------------------------------------------------------
// §4.2 recheck blink
// -----------------------------------------------------------------------------

// runRecheckBlink executes one narrated recheck of the world for a
// link-absent AP session: teardown → forced scan → explicit activation of
// in-range saved profiles (MRU order, hidden profiles last) → re-raise if
// nothing associates. It runs SYNCHRONOUSLY on the loop goroutine — the same
// precedent as applyJoin's bounded block — which makes the §4.2 "every
// tick-driven AP raise is suppressed while a recheck is active" rule
// STRUCTURAL: no tick can interleave with the blink, so no competing
// sustained-offline raise can fire mid-blink, and the suppression cannot
// outlive the blink because it is the call frame itself. The whole blink runs
// under a hard ceiling deliberately below the offline window so it cannot
// race it.
func (m *Machine) runRecheckBlink(ctx context.Context) {
	if !m.ensureAPDown(ctx) {
		// The one teardown that still aborts on failure, and deliberately so:
		// unlike the session LANDINGS (see releaseAPForLanding), the blink is
		// the prelude to a MEASUREMENT, and the hotspot may still hold the
		// radio — a "still gone" verdict read through our own broadcasting AP
		// would be fabricated evidence, which is worse than a delay. The state
		// stays StateAPActive with a re-armed phase clock, so the next tick's
		// reconcile retries the deletion and re-raises, repainting softap_qr;
		// the landings have no such re-raise coming, which is why they cannot
		// afford the same early return. Persistent teardown failure escalates
		// via the §4.6 release latch.
		m.armSessionTimer()
		return
	}
	// Narrate the blink BEFORE the radio work: both claim states — the QR
	// just vanished, and a watching user needs to know it returns shortly.
	// The push downgrades to a HIDE (never join_failed) on player manifests
	// lacking `connecting` — this is the recurring push the delivery-skew
	// posture's hide-fallback exists for.
	m.transition(ctx, StateOfflineRetrying, Detail{
		Reason:  ReasonAPRecheck,
		Message: "Checking for your Wi-Fi network… Setup mode will return in a moment.",
	})

	blinkCtx, cancel := context.WithTimeout(ctx, m.tuning.RecheckBlinkCeiling)
	defer cancel()

	// Forced scan through the shared cache (waitForScanReady is embedded in
	// RefreshScanCache): the blink's result doubles as a fresher portal
	// picker for the re-raise.
	ssids, scanErr := m.wifi.RefreshScanCache(blinkCtx)
	scanUsable := scanErr == nil && len(ssids) > 0
	if scanErr != nil {
		m.logger.Warn("provisioning: recheck scan failed; skipping in-range activation", zap.Error(scanErr))
	}

	associated := false
	measured := false
	if scanErr == nil {
		associated, measured = m.activateInRangeProfiles(blinkCtx, ssids)
	}

	if associated {
		// The shape transition is the session's exit: normal machinery takes
		// over. An online reading ends everything; associated-but-no-WAN
		// lands in the link-present shape, and the constraint-4(b) rewrite in
		// transition hides the ap-recheck panel rather than stranding
		// "…returns in a moment" as a permanent lie.
		m.cancelSessionTimer()
		online, oerr := m.conn.Online(ctx)
		if oerr != nil {
			m.logger.Warn("provisioning: post-recheck connectivity query failed; assuming offline until a query succeeds", zap.Error(oerr))
			online = false
		}
		m.onConnectivity(ctx, online, oerr != nil)
		return
	}

	// Still gone: re-raise with the original reason. Deliberately a BARE
	// transition — no clearOffline (the raise decision was already made) and
	// no resetJoinStatus (a phone may be polling /status for a join outcome).
	// The blink's own successful scan stands in for ensureAPUp's pre-raise
	// pass; an errored or EMPTY scan does not (the empty post-bounce scan is
	// the documented common failure the retry loop exists for), so the
	// re-raise then runs its normal scan retries.
	m.skipNextPreAPScan = scanUsable
	// Escalate the backoff ONLY after a COMPLETED still-gone measurement: a
	// usable scan (non-empty, error-free) AND a finished activation pass over
	// a readable profile list. Every abort shape re-arms the SAME rung — the
	// teardown failure (early return above), a scan error or the documented
	// common empty post-bounce scan (scanUsable false), a profile-listing
	// error, and an exhausted blink budget (measured false) — because a blink
	// that never looked at the world says nothing about whether the network
	// is still gone, and escalating on it would walk the cadence back into
	// the 30-minute blind window this ladder exists to remove. The accepted
	// trade-off: a CHRONICALLY unmeasurable world (every scan empty or
	// erroring — an empty RF environment, a wedged nmcli) holds the short
	// rung indefinitely, and on an unattended frame that churn is UNBOUNDED
	// — deliberately preferred over silent blindness, because the churn is
	// visible in logs and on screen while the blind window is invisible by
	// definition, and its usual causes co-occur with louder failures the
	// §4.6 escalation latches already surface. The re-raise below
	// re-latches an on-table reason
	// without touching the counter, and only clearOffline's episode boundary
	// resets it — a blink re-raise deliberately never calls clearOffline,
	// which is what lets the cadence survive its own cycles.
	if scanUsable && measured {
		m.recheckCycle++
	}
	m.transition(ctx, StateAPActive, Detail{
		Reason:  m.sessionReason,
		Message: "Wi-Fi still unavailable; reopening setup",
	})
}

// activateInRangeProfiles force-activates saved profiles whose SSID the scan
// shows in range, most-recently-used first, with hidden-SSID profiles
// (invisible to scans by definition) getting one attempt last if budget
// remains. Returns (associated, measured): `associated` is whether any
// activation succeeded; `measured` is whether the pass COMPLETED — the
// profile list was readable and every candidate got its attempt (or there
// legitimately were none in range). The backoff ladder escalates only on a
// completed measurement, so a listing error or an exhausted blink budget
// returns measured=false and the same rung re-arms. Relocation is
// definitionally multi-profile, and a blind `connection up` on an
// out-of-range profile blocks up to a full activation deadline — half the
// blink budget — so the wrong pick would starve the right one; hence the
// in-range filter. Fail-bias per §4.2: a LISTING error aborts (returns
// false → the blink re-raises) — never a blind activation off an unreadable
// list; a per-ACTIVATION error logs and continues (fail-open).
func (m *Machine) activateInRangeProfiles(blinkCtx context.Context, ssids []string) (associated, measured bool) {
	profiles, err := m.wifi.ActivationProfiles(blinkCtx)
	if err != nil {
		m.logger.Warn("provisioning: recheck profile listing failed; aborting the blink", zap.Error(err))
		return false, false
	}
	inRange := make(map[string]bool, len(ssids))
	for _, s := range ssids {
		inRange[s] = true
	}
	var candidates, hidden []wifictl.ActivationProfile
	for _, p := range profiles {
		switch {
		case p.Hidden:
			hidden = append(hidden, p)
		case inRange[p.SSID]:
			candidates = append(candidates, p)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		return candidates[i].LastUsed > candidates[j].LastUsed
	})
	candidates = append(candidates, hidden...)

	for _, p := range candidates {
		if blinkCtx.Err() != nil {
			// Exhausted budget = INCOMPLETE measurement: candidates remain
			// unattempted, so this pass must not escalate the ladder.
			m.logger.Info("provisioning: recheck blink budget exhausted; re-raising")
			return false, false
		}
		actCtx, cancel := context.WithTimeout(blinkCtx, m.tuning.ActivationTimeout)
		err := m.wifi.ActivateProfile(actCtx, p.UUID)
		cancel()
		if err == nil {
			m.logger.Info("provisioning: recheck reactivated a saved profile",
				zap.String("ssid", p.SSID))
			return true, true
		}
		m.logger.Info("provisioning: recheck activation failed; trying the next candidate",
			zap.String("ssid", p.SSID), zap.Error(err))
	}
	return false, true
}

// -----------------------------------------------------------------------------
// §4.1 setup-incomplete episode
// -----------------------------------------------------------------------------

// episodeSample takes one link-PRESENT tick sample for the setup-incomplete
// episode: arming, counting, pausing, and raising per §4.1's window rules.
// Called from the OfflineRetrying tick's linkPresent branch — the only place
// the episode's evidence exists — which is what makes constraint 12
// structural: while the episode's own AP is up the machine is in
// StateAPActive and no sample runs, so an AP phase can neither count toward
// nor cancel the episode.
//
// Sample predicate (all four confirmed, constraint 11): unclaimed ∧ link
// present (given by the branch) ∧ WAN confirmed offline ∧ confirmed no wire.
// Hub contact is a PAUSE MODIFIER drawing from a budget, not a predicate
// term — once the budget is spent, samples count regardless of contact.
func (m *Machine) episodeSample(ctx context.Context) {
	if m.tuning.SetupIncompleteDisabled || m.isClaimed() || m.online {
		return
	}
	if m.episodeSettled {
		// Settled is terminal for RAISING, but every §4.1 cancel edge stays
		// live — link loss and claim run in their own paths, and the wired
		// sighting is only observable here, so it is probed even while
		// settled (one nmcli read per tick on a settled frame; the raised-AP
		// wired exit already accepted the same cost).
		if m.wiredLink != nil {
			if wired, err := m.probeWired(ctx); err == nil && wired {
				m.cancelEpisode(ctx, "wired link confirmed while settled")
			}
		}
		return
	}
	// WAN verdict: a failing query (connUnknown) is inconclusive — pause.
	if m.connUnknown {
		m.episodePauseSample()
		return
	}
	// Wire verdict: an error is inconclusive; a wire cancels outright.
	if m.wiredLink == nil {
		// Never confirmable: the episode cannot arm (fail-safe for wirings
		// that predate the seam).
		return
	}
	wired, err := m.probeWired(ctx)
	if err != nil {
		m.episodePauseSample()
		return
	}
	if wired {
		m.cancelEpisode(ctx, "wired link confirmed")
		return
	}
	// Hub-contact deferral: fresh contact pauses while budget lasts, charged
	// per paused tick (a 30-second app check charges its freshness tail, not
	// a whole allowance). Exhausted budgets stop pausing so contact can delay
	// but never pin the fallback.
	if m.hubContactFresh() &&
		m.episodeDeferredCycle < m.tuning.DeferralCycleBudget &&
		m.episodeDeferredTotal < m.tuning.DeferralEpisodeBudget {
		m.episodeDeferredCycle += m.checkInterval
		m.episodeDeferredTotal += m.checkInterval
		m.setEpisodeDeferred(true)
		m.episodePauseSample()
		return
	}
	m.setEpisodeDeferred(false)

	if m.episodeTarget == 0 {
		m.episodeTarget = m.tuning.episodeWindowSamples
	}
	m.episodePause = 0
	m.episodeSamples++
	if m.episodeFreshNeeded > 0 {
		m.episodeFreshNeeded--
	}
	if m.episodeSamples < m.episodeTarget || m.episodeFreshNeeded > 0 {
		return
	}

	// Window expired: raise through the standard entry sequence
	// (constraint 3), with the claim snapshot re-read at the last moment —
	// this re-read is what makes a dropped claim NUDGE harmless (see
	// SetClaimed): a LAN pairing that landed between samples always vetoes
	// the raise here.
	if m.isClaimed() {
		return
	}
	m.logger.Info("provisioning: setup-incomplete window expired; reopening setup",
		zap.Int("cycle", m.episodeCycle+1))
	m.episodeSamples = 0
	m.episodeDeferredCycle = 0
	m.episodeCycle++
	m.clearOffline()
	m.resetJoinStatus()
	m.transition(ctx, StateAPActive, Detail{
		Reason:  ReasonSetupIncomplete,
		Message: "This network has no internet access; reopening setup",
	})
}

// episodePauseSample absorbs one inconclusive (or deferred) sample: nothing
// counted, nothing discarded, and expiry afterwards requires fresh
// consecutive counted samples — no stale-evidence raise off a single sample.
func (m *Machine) episodePauseSample() {
	if m.episodeSamples == 0 && m.episodeCycle == 0 {
		return // nothing armed and no cycles pending: nothing to protect
	}
	m.episodePause++
	m.episodeFreshNeeded = episodeFreshSamplesAfterPause
	// Constraint 13: copy states a number only while the governing timer is
	// running normally. The station-phase panel promises "about N minutes",
	// and a pause stretches the phase past it — repaint the floor-safe
	// variant once, at the pause's start (the panel stays floor-safe for the
	// rest of the phase: it is truthful whether or not counting resumes, and
	// re-flipping per sample would flap the copy at tick cadence).
	if m.episodePause == 1 && m.episodeNarration == ReasonAPSessionEnded {
		m.notify(StateOfflineRetrying, Detail{
			Reason: ReasonAPSessionEnded,
			Message: "Retrying your Wi-Fi network — setup mode will reopen if this network " +
				"still has no internet. Or connect your phone to the same network as the " +
				"Art Computer and open the Feral File app.",
		})
	}
}

// endEpisodeAPPhase tears an expired episode AP phase down and starts the
// next station phase — or settles after the cycle budget. The station phase
// IS the re-arm interval for cycles 2+ (one ladder, no second window): its
// target is the ladder rung for the completed cycle count.
func (m *Machine) endEpisodeAPPhase(ctx context.Context) {
	// Lands regardless of the teardown's verdict — see releaseAPForLanding.
	// §4.6's release escalation still narrates a persistently failing deletion.
	m.releaseAPForLanding(ctx)
	if m.episodeCycle >= m.tuning.EpisodeRaiseCycles {
		// Cycles exhausted: settle in STATION mode — the LAN escape (§1.3)
		// lives there — with the terminal diagnosis. The recheck-style
		// unbounded cadence is deliberately NOT used here (rejected
		// alternative: settling on a GONE network would be a dark screen, but
		// this network is alive — the QR is not the asset, the LAN is).
		m.episodeSettled = true
		m.logger.Info("provisioning: setup-incomplete episode settled after its raise cycles")
		m.transition(ctx, StateOfflineRetrying, Detail{
			Reason: ReasonSetupIncompleteSettled,
			Message: "This network has no internet access. Connect your phone to the same " +
				"network as the Art Computer and open the Feral File app to finish setup, " +
				"or restart the Art Computer to try again.",
		})
		return
	}
	// Station phase: ladder rung by completed cycles (cycle 1 → rung 0).
	rung := m.episodeCycle - 1
	if rung >= len(m.tuning.episodeLadderSamples) {
		rung = len(m.tuning.episodeLadderSamples) - 1
	}
	if rung < 0 {
		rung = 0
	}
	m.episodeSamples = 0
	m.episodePause = 0
	m.episodeFreshNeeded = 0
	m.episodeTarget = m.tuning.episodeLadderSamples[rung]
	minutes := int(m.tuning.EpisodeStationLadder[rung] / time.Minute)
	m.transition(ctx, StateOfflineRetrying, Detail{
		Reason: ReasonAPSessionEnded,
		Message: "Retrying your Wi-Fi network — setup mode will reopen in about " +
			strconv.Itoa(minutes) + " minutes if this network still has no internet. Or connect " +
			"your phone to the same network as the Art Computer and open the Feral File app.",
	})
}

// cancelEpisode resets the whole episode (window, cycles, budgets, settle).
// Cancel edges per §4.1: WAN confirmation, a claim, a wired-link sighting,
// confirmed link loss (the link-absent tick branch calls this before handing
// the device to the untouched link-absent machinery), and a completed portal
// join (applyJoin success — the strongest world-changed signal there is: a
// user who picked a wrong-but-live SSID first must get a full-length episode
// on the network they actually meant).
func (m *Machine) cancelEpisode(ctx context.Context, why string) {
	if m.episodeSamples == 0 && m.episodeCycle == 0 && !m.episodeSettled &&
		m.episodePause == 0 && m.episodeDeferredTotal == 0 {
		return
	}
	_ = ctx
	m.logger.Info("provisioning: setup-incomplete episode canceled", zap.String("reason", why))
	m.episodeSamples = 0
	m.episodePause = 0
	m.episodeFreshNeeded = 0
	m.episodeTarget = 0
	m.episodeCycle = 0
	m.episodeSettled = false
	m.episodeDeferredCycle = 0
	m.episodeDeferredTotal = 0
	m.setEpisodeDeferred(false)
}

// setEpisodeDeferred mirrors the contact-deferral state into the mu-guarded
// block for the §4.7 snapshot's `deferred` sub-state — the app learns its own
// presence is what is holding the AP down, and can surface the pairing /
// startWifiSetup action instead of silently resetting the clock.
// Known slack: during an INCONCLUSIVE run (failed WAN query / wire probe) the
// sample flow early-returns before either edge below, so a true flag can
// outlive contact freshness until the first conclusive tick or any cancel
// edge corrects it — benign (the raise genuinely stays paused meanwhile).
func (m *Machine) setEpisodeDeferred(v bool) {
	m.mu.Lock()
	m.episodeDeferredShared = v
	m.mu.Unlock()
}
