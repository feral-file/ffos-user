package commandrouter_test

import (
	"context"
	"errors"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// TestProcessEnforcesPrincipalTier pins the second tier check (feral-file#3551
// §7.1): an envelope principal on ctx that lacks the command's tier is refused
// before any handler or the CDP fallthrough runs. The mocks carry no
// expectations, so any call past the guard fails the test. Plaintext and
// internal callers (no principal) are not affected by this guard.
func TestProcessEnforcesPrincipalTier(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	ctl := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "c", Role: control.RoleController})
	owner := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "o", Role: control.RoleOwner})
	display := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "d", Role: control.RoleDisplay})
	for _, tc := range []struct {
		ctx context.Context
		cmd commands.Type
		raw string
	}{
		{ctl, commands.CMD_FACTORY_RESET, ""},
		{ctl, commands.CMD_SSH_ACCESS, ""},
		{ctl, "someUnknownCommand", ""},   // unknown: owner-only, never forwarded
		{owner, "mintPairingDisplay", ""}, // internal: nobody from outside
		{owner, commands.Type(control.CmdEnvelope), ""},
		// A display grant (§5.7) runs the Play-shape cast and nothing else.
		{display, commands.CMD_DEVICE_STATUS, `{}`},
		{display, commands.CMD_DISPLAY_PLAYLIST, `{"dp1_call":{}}`},
	} {
		_, err := ts.handler.Process(tc.ctx, commands.Command{Type: tc.cmd, RawArguments: []byte(tc.raw)})
		if !errors.Is(err, commandrouter.ErrPermissionDenied) {
			t.Errorf("%s: err = %v, want ErrPermissionDenied", tc.cmd, err)
		}
	}
}
