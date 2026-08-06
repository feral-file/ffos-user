package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// externalLinkProbe adapts the shared LinkChecker to the provisioning
// ActiveLink seam, excluding the device's own setup hotspot by NM profile name
// so a raised (or half-torn-down) AP never counts as an uplink. Defined here at
// file scope because run()'s daemon-lifetime variable named `context` shadows
// the context package inside that function.
func externalLinkProbe(lc *status.LinkChecker) func(context.Context) (bool, error) {
	if lc == nil {
		// Fail OPEN (guard disabled) rather than wiring a probe that errors
		// forever: a permanent error reads as linkUnknown, which defers the AP
		// on every tick, so first-run provisioning would silently never get
		// its setup AP. A nil ActiveLink keeps the connectivity-only baseline
		// (the AP still raises) instead.
		return nil
	}
	return func(ctx context.Context) (bool, error) {
		return lc.ExternalLink(ctx, softap.ProfileName)
	}
}

// provisioningRunner is the small lifecycle surface run() drives on the setup-AP
// state machine. *provisioning.Machine satisfies it; keeping it an interface lets
// main_test.go inject an ordering spy without constructing the real machine.
type provisioningRunner interface {
	Start(ctx context.Context)
	Stop()
}

// dbusConnectivity adapts feral-sys-monitord's D-Bus connectivity surface to the
// provisioning.Connectivity seam. Online reuses the exact GetConnectivityStatus
// call pattern the relayer gate uses; Subscribe registers an ADDITIVE bus-signal
// handler filtered to connectivity_change, so it coexists with the mediator's own
// handler (godbus fans a signal out to every registered handler) rather than
// replacing it.
//
// This lives in the main package, not in provisioning/, precisely so provisioning
// keeps importing neither dbus (which imports relayer) nor relayer/cdp — the
// isolation the import-lint enforces.
type dbusConnectivity struct {
	dbus   dbus.DBus
	logger *zap.Logger
}

// Online reports current reachability from a point-in-time monitord query.
func (c *dbusConnectivity) Online(ctx context.Context) (bool, error) {
	return getConnectivityStatus(ctx, c.dbus, c.logger)
}

// Subscribe registers an additive connectivity_change handler and returns an
// unsubscribe that removes exactly this handler. The handler is intentionally
// tolerant: a malformed body is dropped (returning nil) rather than surfaced,
// because this path must never destabilize the shared bus-signal fan-out.
func (c *dbusConnectivity) Subscribe(fn func(online bool)) (unsubscribe func()) {
	handler := func(_ context.Context, payload godbus.DBusPayload) ([]any, error) {
		if payload.Member != dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE {
			return nil, nil
		}
		if len(payload.Body) != 1 {
			return nil, nil
		}
		online, ok := payload.Body[0].(bool)
		if !ok {
			return nil, nil
		}
		fn(online)
		return nil, nil
	}
	c.dbus.OnBusSignal(handler)
	return func() { c.dbus.RemoveBusSignal(handler) }
}

// setupNarrationUI is the slice of setupui.Service the provisioning notifier
// drives. Consumer-owned so main_test.go can spy on the hide-guard behavior
// without a real CDP-backed service. *setupui.Service satisfies it.
type setupNarrationUI interface {
	ShowScanning()
	ShowSoftAPQR(ssid, psk string)
	ShowJoinFailed(reason string)
	ShowConnecting(message string)
	// ShowConnectingOrHide is the ap-recheck flavor: its manifest downgrade is
	// a HIDE, never join_failed — the blink recurs unboundedly on claimed
	// frames and a false failure title over exhibition artwork is the one
	// downgrade worse than a blank overlay (the delivery-skew posture).
	ShowConnectingOrHide(message string)
	ShowSetupError(reason string)
	ShowJoining()
	Hide()
}

// autoClaimFlow is the narrow slice of the executor the online claim trigger
// needs. Asserted at wiring time so test doubles without the method simply
// leave the trigger disabled.
type autoClaimFlow interface {
	MaybeShowClaimQROnOnline(ctx context.Context)
}

// startupOTAGateFlow is the narrow slice of the executor the online startup
// update trigger needs (the claimed-device counterpart of autoClaimFlow).
// Asserted at wiring time so test doubles without the method simply leave the
// trigger disabled.
type startupOTAGateFlow interface {
	MaybeRunStartupOTAGateOnOnline(ctx context.Context)
}

// bootPlayerRecoveryFlow is the narrow slice of the executor the boot-online
// player recovery needs. Asserted at wiring time like the other two flows.
// The completion half is called from the CDP on-connect callback: provisioning
// starts before the CDP supervisor, so the boot online transition can precede
// the first DevTools connection, and the recovery is parked until it arrives.
type bootPlayerRecoveryFlow interface {
	MaybeRecoverPlayerOnBootOnline(ctx context.Context)
	CompletePendingBootPlayerRecovery()
	SetBootLifecycleProbe(probe func() bool)
}

// bootLifecycleWindow bounds how long after kernel boot a controld start still
// counts as part of the boot. The boot-online player recovery is only wired
// inside this window: outside it, a controld (re)start coexists with a player
// page that already loaded with the network up, and re-mounting it would
// visibly restart playing artwork for no reason.
const bootLifecycleWindow = 2 * time.Minute

// startupOTAGateEntryWindow bounds how long after kernel boot a WAN arrival may
// still trigger the startup OTA gate. Deliberately much wider than
// bootLifecycleWindow, whose 2 minutes fit its own consumers (the wiring
// decision runs moments after process start, and the parked player recovery
// expiry guards a page that loaded with the network already up) but not this
// one: WAN routinely trails boot by several minutes on a site-wide power
// restore — the FF1 is up in under a minute while the building's router is
// still converging — and gating entry on the 2-minute window silently skipped
// the boot force-OTA in exactly the scenario reboots most often happen. Thirty
// minutes still excludes what the entry gate exists to exclude: a device that
// booted offline and gains WAN mid-exhibition, hours later, defers to the
// nightly updater timer instead of springing a required update's reboot.
const startupOTAGateEntryWindow = 30 * time.Minute

// wireBootLifecycleHooks attaches the executor's boot-scoped online hooks to
// the notifier — the startup OTA gate and the boot player recovery — and ONLY
// when this process started within the kernel boot window. Both restore
// boot-time behaviors, and the gate matters beyond taste: feral-controld.service
// is Restart=always, so a mid-exhibition daemon crash-restart re-delivers the
// initial online state, and an ungated OTA hook would let that restart spring a
// required update — and its reboot — on a healthy playing device whose
// mid-life updates belong to the nightly updater timer (the same disturbance
// class the player-recovery gate exists to prevent). The absent hooks are what
// encode "this is a boot, not a mid-life restart". Type assertions keep test
// doubles without the methods harmlessly unwired, as with the claim flow.
//
// stillWithinBootWindow is consulted two ways: once here for the wiring
// decision (moments after process start, so it doubles as "the process
// started within the window"), and later — via SetBootLifecycleProbe — as the
// continuing boot-window predicate for the player recovery's deferred
// CDP-connect completion (a display-plugged-in-later Chromium's page loaded
// with the network up). The OTA gate's ENTRY check (a device that booted
// offline and only gains WAN hours later must defer to the nightly updater
// timer, not reboot mid-exhibition) gets its own, wider probe —
// otaGateEntryOpen, cut to startupOTAGateEntryWindow — because WAN routinely
// trails boot past bootLifecycleWindow on a power restore. Both probes are
// injected independently of the hook assertions so neither hook can end up
// wired but unguarded.
func wireBootLifecycleHooks(n *setupNotifier, ex any, stillWithinBootWindow, otaGateEntryOpen func() bool) {
	if stillWithinBootWindow == nil || !stillWithinBootWindow() {
		return
	}
	if sink, ok := ex.(interface{ SetBootLifecycleProbe(func() bool) }); ok {
		sink.SetBootLifecycleProbe(stillWithinBootWindow)
	}
	if sink, ok := ex.(interface{ SetStartupOTAGateEntryProbe(func() bool) }); ok && otaGateEntryOpen != nil {
		sink.SetStartupOTAGateEntryProbe(otaGateEntryOpen)
	}
	if sg, ok := ex.(startupOTAGateFlow); ok {
		n.startupGate = sg.MaybeRunStartupOTAGateOnOnline
	}
	if pr, ok := ex.(bootPlayerRecoveryFlow); ok {
		n.playerRecovery = pr.MaybeRecoverPlayerOnBootOnline
	}
}

// uptimeWithin reports whether the kernel is currently within `window` of
// boot, read from /proc/uptime. Called with bootLifecycleWindow at wiring time
// (moments after process start, where it means "this process started within
// the window") and again as the injected boot-lifecycle probe at the parked
// recovery's deferred CDP-connect completion — and with the wider
// startupOTAGateEntryWindow as the OTA gate's entry probe — where it means
// "that window has not closed yet". readFile is injected for tests. Fails
// CLOSED on any read/parse problem: a spurious mid-exhibition reload is worse
// than a missed boot recovery (which a kiosk restart also fixes), and on FF1
// /proc/uptime is always readable, so the closed path is dev-host-only.
func uptimeWithin(window time.Duration, readFile func(string) ([]byte, error), logger *zap.Logger) bool {
	b, err := readFile("/proc/uptime")
	if err != nil {
		logger.Debug("boot window: /proc/uptime unreadable; treating start as mid-life", zap.Error(err))
		return false
	}
	fields := strings.Fields(string(b))
	if len(fields) == 0 {
		return false
	}
	up, err := strconv.ParseFloat(fields[0], 64)
	if err != nil {
		logger.Debug("boot window: unparseable /proc/uptime; treating start as mid-life", zap.Error(err))
		return false
	}
	return time.Duration(up*float64(time.Second)) < window
}

// setupNotifier maps provisioning state changes to on-screen setup narration.
// Every Show* call is fire-and-forget (setupui enqueues and returns), so calling
// them inline on the machine's event-loop goroutine satisfies the Notifier
// contract's "must not block" requirement.
type setupNotifier struct {
	ui     setupNarrationUI
	logger *zap.Logger

	// deviceName, when non-blank, is the mDNS-advertised identity
	// ("FF1-XXXX") appended to trouble-state prose per the §4.6 copy formula —
	// a user reporting a stuck frame over the phone needs to say WHICH frame.
	deviceName string

	// narrating tracks whether THIS surface currently owns the overlay (it has
	// painted softap/joining/join-failed narration that is still up). The
	// Online/Unprovisioned branches hide only when it is set: the shared
	// setupui.Service is also driven by the executor's claim flow, and an
	// unconditional Hide on every →Online transition let a mere connectivity
	// flap (offline→online while the claim QR is showing) erase the claim QR —
	// and poison Resync, whose "last" became hidden. Guarding Hide keeps the
	// "the two surfaces never legitimately drive the overlay at the same time"
	// invariant SetSetupUI documents actually true.
	//
	// No lock: the Notifier contract runs OnStateChange inline on the machine's
	// single event-loop goroutine, so this field is single-writer/single-reader.
	narrating bool

	// claim, when set, is the executor's online claim flow
	// (MaybeShowClaimQROnOnline): the launcher-ui-replacement trigger that
	// paints the claim QR for an unclaimed device once it is reachable. Run on
	// its own goroutine — it waits on network state (relayer topic, OTA gate)
	// and the Notifier must not block. claimCtx scopes those waits to the
	// daemon lifetime.
	claim    func(context.Context)
	claimCtx context.Context

	// startupGate, when set, is the executor's boot-time mandatory update check
	// for claimed devices (MaybeRunStartupOTAGateOnOnline): the setupd-era
	// "Required-mode check on every boot with internet" restored for the Ready
	// phase. Run on its own goroutine for the same reason as claim — it waits
	// on network state (version check, possibly an update ladder) and the
	// Notifier must not block. Shares claimCtx (both are daemon-lifetime).
	startupGate func(context.Context)

	// playerRecovery, when set, is the executor's one-shot boot-online player
	// recovery (MaybeRecoverPlayerOnBootOnline). Only wired when the daemon
	// started inside the boot window (uptimeWithin) — the trigger
	// existing at all is what encodes "this is a boot, not a mid-life
	// restart". Same goroutine/ctx contract as the other two hooks.
	playerRecovery func(context.Context)
}

// withIdentity appends the device identity to trouble-state prose (the §4.6
// copy formula: a user reporting a stuck frame needs to say WHICH frame).
func (n *setupNotifier) withIdentity(msg string) string {
	if n.deviceName == "" || msg == "" {
		return msg
	}
	return msg + " (" + n.deviceName + ")"
}

// OnStateChange renders the least-surprising narration for each provisioning
// state. The StateAPActive branch is disambiguated by the Detail the machine
// sends (see provisioning.Detail.PSK):
//   - Reason "scanning" -> the pre-raise Wi-Fi scan is running -> narrate the
//     scan (the AP comes up only after it completes).
//   - PSK present  -> the "AP is up" announcement -> render the soft-AP QR.
//   - SSID present, no PSK -> a join just failed for that target SSID and the AP
//     is being re-raised -> narrate the failure (the follow-up AP-up announcement
//     re-renders the QR).
//   - neither -> the AP-not-yet-up entry; nothing to render until it comes up.
func (n *setupNotifier) OnStateChange(s provisioning.State, d provisioning.Detail) {
	// The escalation-latch narration (§4.6) is REASON-keyed, not state-keyed:
	// the machine emits it from whatever state the failing retry loop happens
	// to run in (a raise wedge lives in ap_active; a teardown wedge can sit in
	// any resting state), so it is dispatched before the state switch. Both
	// legs return: these notifications are edges of the latch, not state
	// transitions, and must not trigger the transition hooks below.
	switch d.Reason {
	case provisioning.ReasonSetupError:
		n.narrating = true
		n.ui.ShowSetupError(n.withIdentity(d.Message))
		return
	case provisioning.ReasonSetupErrorCleared:
		// The wedge resolved in a state whose own narration will not repaint;
		// dismiss the panel — but only if this surface still owns the overlay
		// (the same narrating guard every hide here takes).
		if n.narrating {
			n.narrating = false
			n.ui.Hide()
		}
		return
	}
	switch s {
	case provisioning.StateAPActive:
		switch {
		case d.Reason == "scanning":
			n.narrating = true
			n.ui.ShowScanning()
		case d.PSK != "":
			n.narrating = true
			n.ui.ShowSoftAPQR(d.SSID, d.PSK)
		case d.SSID != "":
			n.narrating = true
			n.ui.ShowJoinFailed(d.Reason)
		default:
			// AP not raised yet; wait for the credentials-bearing announcement.
		}
	case provisioning.StateJoining:
		n.narrating = true
		n.ui.ShowJoining()
	case provisioning.StateOnline, provisioning.StateUnprovisioned:
		// Wi-Fi setup is done or unnecessary (connected, or reachable by wire):
		// clear the setup overlay so the player shows content — but ONLY if this
		// surface painted it. A connectivity flap arriving while the executor's
		// claim QR is on screen must not hide someone else's narration (see the
		// narrating field).
		if n.narrating {
			n.narrating = false
			n.ui.Hide()
		}
		// An unclaimed device that just became reachable needs the claim QR —
		// nothing else can start the claim flow (the app only connects AFTER
		// scanning it). The flow itself no-ops for claimed devices and when the
		// relayer topic never arrives (e.g. wired link without internet).
		if n.claim != nil {
			go n.claim(n.claimCtx)
		}
		// StateUnprovisioned carries OFFLINE legs too (link-present,
		// link-unknown, link-lost: the machine parks there when the WAN probe
		// failed but a local link exists or is being probed). The two hooks
		// below exist to do network work the moment WAN is confirmed —
		// running them on an offline leg would burn the player recovery's
		// one-shot latch (and the OTA gate's bounded version-check budget) on
		// fetches that provably cannot succeed, leaving nothing for the real
		// online transition. Hence a POSITIVE match on the two WAN-confirmed
		// shapes only — StateOnline, or StateUnprovisioned's online leg
		// (ReasonUnprovisioned) — so any future offline leg fails closed by
		// default. The claim hook above is deliberately NOT filtered: it
		// self-heals (topic wait no-ops offline, and later transitions
		// re-trigger it), which is its pre-existing contract.
		wanConfirmed := s == provisioning.StateOnline || d.Reason == provisioning.ReasonUnprovisioned
		// A claimed device that just became reachable runs the boot-time
		// mandatory update check (force-released builds must not wait for the
		// daily updater timer). No-ops for unclaimed devices, when already
		// settled this boot, and while a run is in flight.
		if n.startupGate != nil && wanConfirmed {
			go n.startupGate(n.claimCtx)
		}
		// A player page that loaded before the network was up gets one
		// recovery pass (in-app artwork refresh, page reload as fallback) now
		// that WAN reachability is confirmed (monitord's probe drives this
		// transition). One-shot; nil outside the boot lifecycle. Settled
		// devices only, and when DevTools is not attached yet the recovery
		// parks until the CDP supervisor's first connection — see
		// MaybeRecoverPlayerOnBootOnline.
		if n.playerRecovery != nil && wanConfirmed {
			go n.playerRecovery(n.claimCtx)
		}
	case provisioning.StateOfflineRetrying:
		switch d.Reason {
		case provisioning.ReasonAPSessionEndedSilent:
			// A teardown (or a narrated→silent shape change, constraint 4(b))
			// on a frame whose screen must return to artwork: hide, guarded
			// like every hide on this surface.
			if n.narrating {
				n.narrating = false
				n.ui.Hide()
			}
		case provisioning.ReasonAPRecheck:
			// The recheck blink narrates on BOTH claim states, with the hide
			// downgrade (see setupNarrationUI.ShowConnectingOrHide).
			n.narrating = true
			n.ui.ShowConnectingOrHide(n.withIdentity(d.Message))
		case provisioning.ReasonBootOffline,
			provisioning.ReasonBootNoInternet,
			provisioning.ReasonBootLinkUnknown,
			provisioning.ReasonJoinedNoInternet,
			provisioning.ReasonJoinedConnUnknown,
			provisioning.ReasonAPSessionEnded,
			provisioning.ReasonSetupIncompleteSettled:
			// Entry edges that would otherwise be a silent black screen for
			// minutes or forever, in front of a user who is actively watching
			// (a moved frame booting offline; a join that associated to a
			// network with no internet — F-01 / ux-must-fix M-0/M-1). The
			// machine's Message says exactly what is happening and what will
			// happen next, carried by the player's "connecting" state, whose
			// title is deliberately neutral: on a NORMAL reboot this narration
			// is painted in the ~1s between CDP connect and the first online
			// confirmation, and the previously borrowed join_failed screen
			// flashed a false "Couldn't connect" title on every boot.
			// ShowConnecting downgrades itself to join_failed on player
			// bundles that predate the state (see setupui.ShowConnecting).
			n.narrating = true
			n.ui.ShowConnecting(n.withIdentity(d.Message))
		default:
			// Transient provisioned-device outage (the exhibition
			// online→offline edge, redundant re-emissions): leave the screen
			// as-is rather than flashing a setup overlay on a blip. This
			// default is load-bearing — narrating here would put a "join
			// failed" screen over playing artwork on every WAN blip.
		}
	}
}

// wireInternetProbe attaches the executor's cached internet-reachability seam
// (SetInternetProbe) to sys-monitord's cached connectivity read. Defined here
// at file scope — like externalLinkProbe — because initializeApp's
// daemon-lifetime variable named `context` shadows the context package inside
// that function, and the seam's signature names context.Context. Type-asserted
// so test doubles without the method stay harmlessly unwired (nil probe keeps
// the pre-§4.6 silent hide).
//
// Deliberately NOT internetProbeFrom: that helper degrades errors to false
// for the hub status field, but this consumer narrates "no internet access"
// off a false — a monitord restart or D-Bus timeout must surface as an error
// (the executor then keeps the silent hide), never as a confirmed-offline
// verdict.
func wireInternetProbe(ex any, dc dbus.DBus, logger *zap.Logger) {
	if sink, ok := ex.(interface {
		SetInternetProbe(func(context.Context) (bool, error))
	}); ok {
		sink.SetInternetProbe(func(ctx context.Context) (bool, error) {
			deadlineCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
			defer cancel()
			resp, err := dc.Call(
				deadlineCtx,
				dbus.MONITORD_NAME,
				dbus.MONITORD_PATH,
				dbus.MONITORD_INTERFACE,
				dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS,
				false,
			)
			if err != nil {
				return false, err
			}
			if len(resp) != 1 {
				return false, fmt.Errorf("connectivity status: unexpected reply shape (%d values)", len(resp))
			}
			connected, ok := resp[0].(bool)
			if !ok {
				return false, fmt.Errorf("connectivity status: non-bool reply %T", resp[0])
			}
			return connected, nil
		})
	}
}

// externalLinkDetailProbe is externalLinkProbe's detail flavor: the same
// hotspot-excluding probe, plus the ethernet-only verdict from the same nmcli
// read, so the machine's §4.7 health snapshot learns the link TYPE at zero
// extra probe cost. Same nil and failure semantics.
func externalLinkDetailProbe(lc *status.LinkChecker) func(context.Context) (bool, bool, error) {
	if lc == nil {
		return nil
	}
	return func(ctx context.Context) (bool, bool, error) {
		return lc.ExternalLinkDetail(ctx, softap.ProfileName)
	}
}

// wiredLinkProbe adapts the shared LinkChecker's ethernet-only verdict to the
// provisioning WiredLink seam (constraint 6 — never ExternalLink, which
// counts stations). File scope for the same `context`-shadowing reason as
// externalLinkProbe. nil checker → nil probe: the machine then never confirms
// a wire, its documented fail-safe.
func wiredLinkProbe(lc *status.LinkChecker) func(context.Context) (bool, error) {
	if lc == nil {
		return nil
	}
	return lc.WiredLink
}

// maxTuningSeconds bounds every integer-seconds knob in the config block, and
// is the seconds-side twin of provisioning's own maxTuningDuration ceiling
// (24h). The two are mirrored BY HAND across the package boundary; changing
// one means changing the other.
//
// The mirror is not redundancy — this side is the one that can actually catch
// an overflow, and it must run BEFORE the multiplication below. See the secs
// helper for why a post-conversion check cannot.
const maxTuningSeconds = 24 * 60 * 60

// provisioningTuningFromConfig maps the config file's permissive provisioning
// block onto the machine's typed knobs (integer-with-unit fields → durations).
// Out-of-range entries are rejected to ZERO, which is the machine's documented
// "unset" value, so withDefaults substitutes the built-in default for them.
func provisioningTuningFromConfig(t config.ProvisioningTuning, logger *zap.Logger) provisioning.Tuning {
	if logger == nil {
		logger = zap.NewNop()
	}
	// secs validates the INT and only then converts. The ordering is the whole
	// point: time.Duration(n) * time.Second overflows int64 above roughly
	// 9.2e9 seconds and WRAPS, so 18446744074 lands as ~290ms — a small,
	// plausible-looking positive that then passes every downstream sanity
	// check, provisioning's >24h ceiling included. Once the multiplication has
	// happened the original magnitude is unrecoverable, so validating on the
	// far side cannot work no matter how careful it is.
	secs := func(field string, n int) time.Duration {
		if n < 0 || n > maxTuningSeconds {
			logger.Warn("provisioning config: seconds value out of range; using the built-in default",
				zap.String("field", field), zap.Int("configured", n),
				zap.Int("maxSeconds", maxTuningSeconds))
			return 0
		}
		return time.Duration(n) * time.Second
	}
	out := provisioning.Tuning{
		SetupIncompleteDisabled: t.SetupIncompleteDisabled,
		EpisodeWindow:           secs("episodeWindowSeconds", t.EpisodeWindowSeconds),
		EpisodeApPhase:          secs("episodeApPhaseSeconds", t.EpisodeApPhaseSeconds),
		EpisodeRaiseCycles:      t.EpisodeRaiseCycles,
		HubContactFresh:         secs("hubContactFreshSeconds", t.HubContactFreshSeconds),
		DeferralCycleBudget:     secs("deferralCycleBudgetSeconds", t.DeferralCycleBudgetSeconds),
		DeferralEpisodeBudget:   secs("deferralEpisodeBudgetSeconds", t.DeferralEpisodeBudgetSeconds),
		RecheckApPhase:          secs("recheckApPhaseSeconds", t.RecheckApPhaseSeconds),
		RecheckBlinkCeiling:     secs("recheckBlinkCeilingSeconds", t.RecheckBlinkCeilingSeconds),
		ActivationTimeout:       secs("activationTimeoutSeconds", t.ActivationTimeoutSeconds),
		PortalActivityWindow:    secs("portalActivityWindowSeconds", t.PortalActivityWindowSeconds),
		PortalDeferralCeiling:   secs("portalDeferralCeilingSeconds", t.PortalDeferralCeilingSeconds),
		UserRequestedSession:    secs("userRequestedSessionSeconds", t.UserRequestedSessionSeconds),
		SessionAbsoluteCap:      secs("sessionAbsoluteCapSeconds", t.SessionAbsoluteCapSeconds),
	}
	// A rejected rung becomes 0, which usableLadder reads as out-of-range and
	// answers by discarding the WHOLE override for the built-in ladder — the
	// all-or-nothing rule, reached through the zero rather than duplicated here.
	for i, s := range t.EpisodeStationLadderSeconds {
		out.EpisodeStationLadder = append(out.EpisodeStationLadder,
			secs("episodeStationLadderSeconds["+strconv.Itoa(i)+"]", s))
	}
	// Same zero-then-all-or-nothing route for the recheck AP-phase ladder
	// (usableRecheckLadder on the provisioning side).
	for i, s := range t.RecheckApPhaseLadderSeconds {
		out.RecheckApPhaseLadder = append(out.RecheckApPhaseLadder,
			secs("recheckApPhaseLadderSeconds["+strconv.Itoa(i)+"]", s))
	}
	return out
}

// wireNetworkHealth attaches the executor's getDeviceStatus network-object
// composer: the machine's cached snapshot plus the cached monitord internet
// verdict — the relayer/cast reply then carries the same §4.7 diagnosis the
// hub status routes serve. File scope for the `context`-shadowing reason;
// type-asserted like the sibling seams.
func wireNetworkHealth(ex any, m *provisioning.Machine, internet func(context.Context) bool) {
	if sink, ok := ex.(interface {
		SetNetworkHealth(func(context.Context) *status.NetworkHealth)
	}); ok {
		sink.SetNetworkHealth(func(ctx context.Context) *status.NetworkHealth {
			snap := m.Snapshot()
			return &status.NetworkHealth{
				State:    snap.State,
				Reason:   snap.Reason,
				SSID:     snap.SSID,
				Link:     snap.Link,
				Internet: internet(ctx),
				Deferred: snap.Deferred,
			}
		})
	}
}

// wireWifiSetupStarter attaches the executor's startWifiSetup seam to the
// provisioning machine's admission+queue entry point. File scope for the same
// `context`-shadowing reason as externalLinkProbe; type-asserted so test
// doubles without the method leave the command rejecting as unavailable.
func wireWifiSetupStarter(ex any, m *provisioning.Machine) {
	if sink, ok := ex.(interface {
		SetWifiSetupStarter(func(context.Context) error)
	}); ok {
		sink.SetWifiSetupStarter(m.StartWifiSetup)
	}
}

// setupStateSource is the read-only slice of the provisioning machine the hub
// status provider needs. *provisioning.Machine satisfies it.
type setupStateSource interface {
	State() provisioning.State
}

// internetProbeFrom returns the live internet-reachability probe the hub
// status provider serves as the claim-QR-parity "internet" field. It reads
// sys-monitord's CACHED state (refresh=false): LAN clients poll /api/status
// while claiming, and each poll must cost one local D-Bus round-trip, never a
// network probe. Errors degrade to false — the same value an offline device
// reports — and log at Debug to keep polling out of production logs.
func internetProbeFrom(dc dbus.DBus, logger *zap.Logger) func(context.Context) bool {
	return func(ctx context.Context) bool {
		deadlineCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		defer cancel()
		resp, err := dc.Call(
			deadlineCtx,
			dbus.MONITORD_NAME,
			dbus.MONITORD_PATH,
			dbus.MONITORD_INTERFACE,
			dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS,
			false,
		)
		if err != nil || len(resp) != 1 {
			logger.Debug("internet probe: connectivity status unavailable", zap.Error(err))
			return false
		}
		connected, ok := resp[0].(bool)
		return ok && connected
	}
}

// provisioningStatusProvider wraps the default hub status provider and overrides
// setup_state with the live provisioning machine's state. When no machine is
// wired (a nil machine, e.g. the default/test path) the base provider's
// placeholder claim-derived setup_state is kept instead. internet, when wired,
// supplies live internet reachability (the sys-monitord signal) for claim-QR
// parity — the base provider only knows LAN-link state.
type provisioningStatusProvider struct {
	base     hub.StatusProvider
	machine  setupStateSource
	internet func(ctx context.Context) bool
	// snapshot, when wired, serves the machine's cached §4.7 health object
	// (provisioning.Machine.Snapshot — probe-free by contract).
	snapshot func() provisioning.NetworkSnapshot
}

func (p *provisioningStatusProvider) Status(ctx context.Context) hub.StatusInfo {
	info := p.base.Status(ctx)
	if p.machine != nil {
		info.SetupState = string(p.machine.State())
	}
	if p.internet != nil {
		info.Internet = p.internet(ctx)
	}
	// The §4.7 health object: the machine's cached snapshot plus the same
	// cached internet verdict the top-level field carries. No probe runs on
	// this path — LAN clients poll while claiming.
	if p.snapshot != nil {
		snap := p.snapshot()
		info.Network = &status.NetworkHealth{
			State:    snap.State,
			Reason:   snap.Reason,
			SSID:     snap.SSID,
			Link:     snap.Link,
			Internet: info.Internet,
			Deferred: snap.Deferred,
		}
	}
	return info
}
