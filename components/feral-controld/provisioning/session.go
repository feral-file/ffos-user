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

	RecheckApPhase        time.Duration
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
)

// defaultEpisodeStationLadder is the escalating station phase between episode
// AP phases: early cycles favor the user who is probably still nearby
// (missing an AP phase costs 5 minutes, not 20), later cycles favor observing
// WAN recovery; AP stays ≤ 33% of every cycle.
func defaultEpisodeStationLadder() []time.Duration {
	return []time.Duration{5 * time.Minute, 10 * time.Minute, 20 * time.Minute}
}

// withDefaults resolves zero fields to the package defaults and derives the
// tick-sample equivalents. Rounding errs upward (more evidence before a
// one-way raise), minimum one sample.
func (t Tuning) withDefaults(tick time.Duration) Tuning {
	def := func(v *time.Duration, d time.Duration) {
		if *v <= 0 {
			*v = d
		}
	}
	def(&t.EpisodeWindow, defaultEpisodeWindow)
	def(&t.EpisodeApPhase, defaultEpisodeApPhase)
	def(&t.HubContactFresh, defaultHubContactFresh)
	def(&t.DeferralCycleBudget, defaultDeferralCycleBudget)
	def(&t.DeferralEpisodeBudget, defaultDeferralEpisodeBudget)
	def(&t.RecheckApPhase, defaultRecheckApPhase)
	def(&t.RecheckBlinkCeiling, defaultRecheckBlinkCeiling)
	def(&t.ActivationTimeout, defaultActivationTimeout)
	def(&t.PortalActivityWindow, defaultPortalActivityWindow)
	def(&t.PortalDeferralCeiling, defaultPortalDeferralCeiling)
	def(&t.UserRequestedSession, defaultUserRequestedSession)
	def(&t.SessionAbsoluteCap, defaultSessionAbsoluteCap)
	if t.EpisodeRaiseCycles <= 0 {
		t.EpisodeRaiseCycles = defaultEpisodeRaiseCycles
	}
	if len(t.EpisodeStationLadder) == 0 {
		t.EpisodeStationLadder = defaultEpisodeStationLadder()
	}
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

// endUserSession tears an expired user-requested session down and resumes
// normal state handling: the saved profile is never touched, so NM
// autoconnect restores the previous network on its own (bench-verified, §3 of
// the plan: 6s reassociation after teardown). This exists only so a user who
// taps startWifiSetup and changes their mind cannot strand the frame in setup
// mode.
func (m *Machine) endUserSession(ctx context.Context) {
	if !m.ensureAPDown(ctx) {
		// The hotspot may still hold the radio; retry at the next tick (the
		// §4.6 release escalation narrates persistent failure).
		return
	}
	m.cancelSessionTimer()
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
			"to the same network as the frame and open the Feral File app.",
	}
}

// wiredExitDue probes for a live ethernet link under a RAISED AP (§4.2's
// wired exit): the AP cannot fix a wired network, and the probeLink
// short-circuit that protects Wi-Fi's own-hotspot ambiguity does not apply —
// an ethernet row in the device survey has no such ambiguity (constraint 6).
// This also lowers a user-requested session when a cable is plugged in
// mid-setup — intended. Costs one nmcli read per tick for the AP session's
// duration; accepted. Errors defer (constraint 11).
func (m *Machine) wiredExitDue(ctx context.Context) bool {
	if m.wiredLink == nil {
		return false
	}
	wired, err := m.wiredLink(ctx)
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
	if !m.ensureAPDown(ctx) {
		return // retry next tick
	}
	m.cancelSessionTimer()
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
		// The hotspot may still hold the radio — a "still gone" verdict read
		// through our own broadcasting AP would be false evidence. Abort the
		// blink and retry next cycle; persistent teardown failure escalates
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
	if scanErr == nil {
		associated = m.activateInRangeProfiles(blinkCtx, ssids)
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
	m.transition(ctx, StateAPActive, Detail{
		Reason:  m.sessionReason,
		Message: "Wi-Fi still unavailable; reopening setup",
	})
}

// activateInRangeProfiles force-activates saved profiles whose SSID the scan
// shows in range, most-recently-used first, with hidden-SSID profiles
// (invisible to scans by definition) getting one attempt last if budget
// remains. Returns whether any activation succeeded. Relocation is
// definitionally multi-profile, and a blind `connection up` on an
// out-of-range profile blocks up to a full activation deadline — half the
// blink budget — so the wrong pick would starve the right one; hence the
// in-range filter. Fail-bias per §4.2: a LISTING error aborts (returns false
// → the blink re-raises) — never a blind activation off an unreadable list;
// a per-ACTIVATION error logs and continues (fail-open).
func (m *Machine) activateInRangeProfiles(blinkCtx context.Context, ssids []string) bool {
	profiles, err := m.wifi.ActivationProfiles(blinkCtx)
	if err != nil {
		m.logger.Warn("provisioning: recheck profile listing failed; aborting the blink", zap.Error(err))
		return false
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
			m.logger.Info("provisioning: recheck blink budget exhausted; re-raising")
			return false
		}
		actCtx, cancel := context.WithTimeout(blinkCtx, m.tuning.ActivationTimeout)
		err := m.wifi.ActivateProfile(actCtx, p.UUID)
		cancel()
		if err == nil {
			m.logger.Info("provisioning: recheck reactivated a saved profile",
				zap.String("ssid", p.SSID))
			return true
		}
		m.logger.Info("provisioning: recheck activation failed; trying the next candidate",
			zap.String("ssid", p.SSID), zap.Error(err))
	}
	return false
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
			if wired, err := m.wiredLink(ctx); err == nil && wired {
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
	wired, err := m.wiredLink(ctx)
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
		m.episodePauseSample()
		return
	}

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
				"frame and open the Feral File app.",
		})
	}
}

// endEpisodeAPPhase tears an expired episode AP phase down and starts the
// next station phase — or settles after the cycle budget. The station phase
// IS the re-arm interval for cycles 2+ (one ladder, no second window): its
// target is the ladder rung for the completed cycle count.
func (m *Machine) endEpisodeAPPhase(ctx context.Context) {
	if !m.ensureAPDown(ctx) {
		return // retry next tick; §4.6 escalation covers persistence
	}
	m.cancelSessionTimer()
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
				"network as the frame and open the Feral File app to finish setup, or " +
				"restart the frame to try again.",
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
			"your phone to the same network as the frame and open the Feral File app.",
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
}
