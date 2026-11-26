package controlgate_test

import (
	"context"
	"crypto/elliptic"
	"encoding/base64"
	"strconv"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
)

func TestConnectAuthority(t *testing.T) {
	a, _ := newAuthority(t)
	c := controlgate.ClaimQR{Auth: a}
	owner := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "o", Role: control.RoleOwner})
	ctl := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "c", Role: control.RoleController})

	if p, o := c.ConnectAuthority(context.Background()); p || o {
		t.Fatalf("legacy plaintext: protected=%v owner=%v", p, o)
	}
	if _, o := c.ConnectAuthority(owner); !o {
		t.Fatal("owner principal not recognized")
	}
	if _, o := c.ConnectAuthority(ctl); o {
		t.Fatal("controller principal treated as owner")
	}
}

func TestClaimQRPins(t *testing.T) {
	a, _ := newAuthority(t)
	fields, ok := controlgate.ClaimQR{Auth: a}.ClaimQRPins()
	if !ok || len(fields) != 4 || fields[0] != "" {
		t.Fatalf("fields %v ok %v", fields, ok)
	}
	p := a.Status().Profile
	if fields[3] != p.Gen {
		t.Fatalf("gen %q want %q", fields[3], p.Gen)
	}
}

// §11: the key fields ride only on a QR whose device dials relay v2, because
// a controller reads their presence as "reachable on the keyed channel".
func TestClaimQRFieldsKeyFields(t *testing.T) {
	tests := []struct {
		name       string
		transports []string
		lanHost    func() string
		wantLen    int
		wantHost   string
	}{
		{name: "no v2: pins only", transports: []string{control.TransportLAN, control.TransportRelayer}, lanHost: func() string { return "192.168.1.5" }, wantLen: 4},
		{name: "v2 with a LAN address", transports: []string{control.TransportLAN, control.TransportRelayV2}, lanHost: func() string { return "192.168.1.5" }, wantLen: 8, wantHost: "192.168.1.5"},
		{name: "v2, address unknown", transports: []string{control.TransportRelayV2}, lanHost: func() string { return "" }, wantLen: 8},
		{name: "v2, seam unwired", transports: []string{control.TransportRelayV2}, wantLen: 8},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			a, _ := newAuthority(t)
			a.SetTransports(tc.transports)
			fields, expiresAt, ok := controlgate.ClaimQR{Auth: a, LANHost: tc.lanHost}.ClaimQRFields()
			if !ok || len(fields) != tc.wantLen {
				t.Fatalf("fields %v ok %v", fields, ok)
			}
			if tc.wantLen == 4 {
				return
			}
			p := a.Status().Profile
			if fields[4] != strconv.FormatInt(expiresAt.Unix(), 10) || fields[7] != tc.wantHost {
				t.Fatalf("exp %q host %q", fields[4], fields[7])
			}
			// A controller rebuilds the profile from the compressed points
			// and checks it against the pins in fields 1 and 2.
			for i, want := range []control.PublicJWK{p.Sig, p.Enc} {
				raw, err := base64.RawURLEncoding.DecodeString(fields[5+i])
				if err != nil || len(raw) != 33 {
					t.Fatalf("key field %d: %v len %d", i, err, len(raw))
				}
				x, y := elliptic.UnmarshalCompressed(elliptic.P256(), raw)
				if x == nil {
					t.Fatalf("key field %d is not a compressed P-256 point", i)
				}
				var got control.PublicJWK
				x.FillBytes(got.X[:])
				y.FillBytes(got.Y[:])
				if !got.Equal(want) || got.Thumbprint() != fields[1+i] {
					t.Fatalf("key field %d does not decode to the pinned key", i)
				}
			}
		})
	}
}
