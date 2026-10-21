package controlgate_test

import (
	"context"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
)

func TestConnectAuthority(t *testing.T) {
	a, _ := newAuthority(t, false)
	c := controlgate.ClaimQR{Auth: a}
	owner := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "o", Role: control.RoleOwner})
	ctl := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "c", Role: control.RoleController})

	if p, o := c.ConnectAuthority(context.Background()); p || o {
		t.Fatalf("legacy plaintext: protected=%v owner=%v", p, o)
	}
	if _, o := c.ConnectAuthority(owner); !o {
		t.Fatal("owner principal not recognised")
	}
	if _, o := c.ConnectAuthority(ctl); o {
		t.Fatal("controller principal treated as owner")
	}
}

func TestClaimQRPins(t *testing.T) {
	a, _ := newAuthority(t, false)
	fields, ok := controlgate.ClaimQR{Auth: a}.ClaimQRPins()
	if !ok || len(fields) != 4 || fields[0] != "" {
		t.Fatalf("fields %v ok %v", fields, ok)
	}
	p := a.Status().Profile
	if fields[3] != p.Gen {
		t.Fatalf("gen %q want %q", fields[3], p.Gen)
	}
}
