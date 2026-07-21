package main

import (
	"context"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
)

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
	ShowJoining()
	Hide()
}

// autoClaimFlow is the narrow slice of the executor the online claim trigger
// needs. Asserted at wiring time so test doubles without the method simply
// leave the trigger disabled.
type autoClaimFlow interface {
	MaybeShowClaimQROnOnline(ctx context.Context)
}

// setupNotifier maps provisioning state changes to on-screen setup narration.
// Every Show* call is fire-and-forget (setupui enqueues and returns), so calling
// them inline on the machine's event-loop goroutine satisfies the Notifier
// contract's "must not block" requirement.
type setupNotifier struct {
	ui     setupNarrationUI
	logger *zap.Logger

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
	case provisioning.StateOfflineRetrying:
		// Transient provisioned-device outage: leave the screen as-is rather than
		// flashing a setup overlay on a blip.
	}
}

// setupStateSource is the read-only slice of the provisioning machine the hub
// status provider needs. *provisioning.Machine satisfies it.
type setupStateSource interface {
	State() provisioning.State
}

// provisioningStatusProvider wraps the default hub status provider and overrides
// setup_state with the live provisioning machine's state. When no machine is
// wired (a nil machine, e.g. the default/test path) the base provider's
// placeholder claim-derived setup_state is kept instead.
type provisioningStatusProvider struct {
	base    hub.StatusProvider
	machine setupStateSource
}

func (p *provisioningStatusProvider) Status(ctx context.Context) hub.StatusInfo {
	info := p.base.Status(ctx)
	if p.machine != nil {
		info.SetupState = string(p.machine.State())
	}
	return info
}
