package main

import (
	"context"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
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

// setupNotifier maps provisioning state changes to on-screen setup narration.
// Every Show* call is fire-and-forget (setupui enqueues and returns), so calling
// them inline on the machine's event-loop goroutine satisfies the Notifier
// contract's "must not block" requirement.
type setupNotifier struct {
	ui     *setupui.Service
	logger *zap.Logger
}

// OnStateChange renders the least-surprising narration for each provisioning
// state. The StateAPActive branch is disambiguated by the Detail the machine
// sends (see provisioning.Detail.PSK):
//   - PSK present  -> the "AP is up" announcement -> render the soft-AP QR.
//   - SSID present, no PSK -> a join just failed for that target SSID and the AP
//     is being re-raised -> narrate the failure (the follow-up AP-up announcement
//     re-renders the QR).
//   - neither -> the AP-not-yet-up entry; nothing to render until it comes up.
func (n *setupNotifier) OnStateChange(s provisioning.State, d provisioning.Detail) {
	switch s {
	case provisioning.StateAPActive:
		switch {
		case d.PSK != "":
			n.ui.ShowSoftAPQR(d.SSID, d.PSK)
		case d.SSID != "":
			n.ui.ShowJoinFailed(d.Reason)
		default:
			// AP not raised yet; wait for the credentials-bearing announcement.
		}
	case provisioning.StateJoining:
		n.ui.ShowJoining()
	case provisioning.StateOnline, provisioning.StateUnprovisioned:
		// Wi-Fi setup is done or unnecessary (connected, or reachable by wire):
		// clear any setup overlay so the player shows content / the claim flow's
		// own narration surface takes over.
		n.ui.Hide()
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
