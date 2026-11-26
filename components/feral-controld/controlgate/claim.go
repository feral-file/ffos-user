package controlgate

import (
	"context"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// invitationMinRemaining is the least life a painted invitation may have
// left; a shorter one is replaced before it goes on screen.
const invitationMinRemaining = time.Minute

// ClaimQR adapts the authority to devicectl.ControlClaim.
type ClaimQR struct {
	Auth *control.Authority
	// LANHost returns the device's LAN IPv4 literal, or "" when unknown
	// (DefaultRouteIPv4 in production). nil reads as unknown.
	LANHost func() string
}

// ClaimQRFields returns the §11 invitation fields while the device is legacy:
// inv, the pins and gen, then, on a device that dials relay v2, the key
// fields (control.Invitation.KeyFields). The key fields are tied to v2 on
// purpose: a controller reads their presence as "this device is reachable on
// the keyed relay channel", so a device that does not dial it must not emit
// them.
func (c ClaimQR) ClaimQRFields() ([]string, time.Time, bool) {
	inv, err := c.Auth.EnsureInvitation(invitationMinRemaining)
	if err != nil {
		return nil, time.Time{}, false
	}
	fields := []string{inv.Value, inv.Pins.Sig, inv.Pins.Enc, inv.Gen}
	if c.Auth.DialsRelayV2() {
		host := ""
		if c.LANHost != nil {
			// Re-read on every paint: the address is a transport shortcut
			// for the scanning phone and follows DHCP, not the invitation.
			host = c.LANHost()
		}
		fields = append(fields, inv.KeyFields(host)...)
	}
	return fields, time.Unix(inv.ExpiresAt, 0), true
}

// ClaimQRPins returns the §11 fields with an empty invitation whenever the
// device can vouch for its profile (every state but locked).
func (c ClaimQR) ClaimQRPins() ([]string, bool) {
	if c.Auth.State() == control.StateLocked {
		return nil, false
	}
	id := c.Auth.Store().Identity()
	if id == nil {
		return nil, false
	}
	p := id.Pins()
	return []string{"", p.Sig, p.Enc, id.Profile().Gen}, true
}

// Enrolled reports whether an active owner exists.
func (c ClaimQR) Enrolled() bool {
	return c.Auth.State() == control.StateEnrolled
}

// ClearInvitation voids the live invitation.
func (c ClaimQR) ClearInvitation() { c.Auth.ClearInvitation() }

// ConnectAuthority reports whether the protocol guards the claim (any state
// but legacy) and whether ctx carries an owner principal.
func (c ClaimQR) ConnectAuthority(ctx context.Context) (protected, owner bool) {
	p, ok := control.PrincipalFrom(ctx)
	return c.Auth.State() != control.StateLegacy, ok && p.Role == control.RoleOwner
}

// Retire deletes the control identity.
func (c ClaimQR) Retire() error { return c.Auth.Retire() }
