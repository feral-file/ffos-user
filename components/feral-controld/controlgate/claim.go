package controlgate

import (
	"context"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// invitationMinRemaining is the least life a painted invitation may have
// left; a shorter one is replaced before it goes on screen.
const invitationMinRemaining = 2 * time.Minute

// ClaimQR adapts the authority to devicectl.ControlClaim.
type ClaimQR struct{ Auth *control.Authority }

// ClaimQRFields returns the §11 invitation fields while the device is legacy.
func (c ClaimQR) ClaimQRFields() ([]string, time.Time, bool) {
	inv, err := c.Auth.EnsureInvitation(invitationMinRemaining)
	if err != nil {
		return nil, time.Time{}, false
	}
	return []string{inv.Value, inv.Pins.Sig, inv.Pins.Enc, inv.Gen}, time.Unix(inv.ExpiresAt, 0), true
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
	st := c.Auth.State()
	return st == control.StateEnrolled || st == control.StateEnforced
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
