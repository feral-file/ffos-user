package controlgate

import (
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

// Enrolled reports whether an active owner exists.
func (c ClaimQR) Enrolled() bool {
	st := c.Auth.State()
	return st == control.StateEnrolled || st == control.StateEnforced
}

// Retire deletes the control identity.
func (c ClaimQR) Retire() error { return c.Auth.Retire() }
