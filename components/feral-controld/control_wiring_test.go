package main

import (
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"slices"
	"testing"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

const wiringDir = "/home/feralfile/.state/control"

// ownedState provisions .state/control holding one active owner grant, as an
// enrolled device's directory looks after an OTA replaced controld.json.
func ownedState(t *testing.T) *control.MemFS {
	t.Helper()
	devSig, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	devEnc, _ := ecdh.P256().GenerateKey(rand.Reader)
	id, err := control.NewIdentityFromKeys("FF1-WIRE", "AAAAAAAAAAAAAAAAAAAAAA", devSig, devEnc, 1)
	if err != nil {
		t.Fatal(err)
	}
	ctlSig, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	ctlEnc, _ := ecdh.P256().GenerateKey(rand.Reader)
	sig, _ := control.PublicJWKFromECDSA(&ctlSig.PublicKey)
	enc, _ := control.PublicJWKFromECDH(ctlEnc.PublicKey())
	owner := control.Grant{Ctl: sig.Thumbprint(), Sig: sig, Enc: enc, Role: control.RoleOwner, Label: "phone",
		Platform: "ios", CreatedAt: 1, ApprovedBy: "invitation"}
	fsys := control.NewMemFS()
	if err := control.InitDir(fsys, wiringDir, id, []control.Grant{owner}); err != nil {
		t.Fatal(err)
	}
	return fsys
}

func wiringFor(t *testing.T, cfg config.ControlConfig, fsys control.FS) *controlWiring {
	t.Helper()
	t.Cleanup(func() { status.SetControlStatusSource(nil) })
	return newControlWiring(cfg, "FF1-WIRE", wiringDir, fsys, zap.NewNop())
}

func TestControlWiring_NoStateDisabledIsOff(t *testing.T) {
	// No control state + disabled (or malformed): the protocol stays off and
	// every transport is wired exactly as before.
	for _, cfg := range []config.ControlConfig{{}, {Malformed: true}, {Enforced: true}} {
		fsys := control.NewMemFS()
		if w := wiringFor(t, cfg, fsys); w != nil {
			t.Fatalf("%+v: protocol started without state or enabled", cfg)
		}
		if control.StateExists(fsys, wiringDir) {
			t.Fatalf("%+v: a disabled start created control state", cfg)
		}
	}
}

func TestControlWiring_StateOnDiskForcesProtocolOn(t *testing.T) {
	for name, cfg := range map[string]config.ControlConfig{
		"disabled":  {},
		"malformed": {Malformed: true},
	} {
		fsys := ownedState(t)
		w := wiringFor(t, cfg, fsys)
		if w == nil {
			t.Fatalf("%s: control state on disk but the protocol is off", name)
		}
		// The policy runs on the persisted owner, not on the config.
		if st := w.auth.State(); st != control.StateEnrolled {
			t.Fatalf("%s: state %s", name, st)
		}
		if d := w.auth.Policy(control.RouteLAN, "displayPlaylist", nil); d.Allow || d.Code != control.CodeEnrollmentRequired {
			t.Fatalf("%s: LAN plaintext %+v", name, d)
		}
		if d := w.auth.Policy(control.RouteRelayer, "sshAccess", nil); d.Allow {
			t.Fatalf("%s: relayer owner-tier plaintext allowed", name)
		}
		if _, err := w.auth.IssueInvitation(); err == nil {
			t.Fatalf("%s: invitation offered on an owned device", name)
		}
	}
	// enforced only tightens: with an owner and enforced it is enforced;
	// a malformed enforced (read false) is still enrolled.
	if w := wiringFor(t, config.ControlConfig{Enforced: true}, ownedState(t)); w.auth.State() != control.StateEnforced {
		t.Fatal("enforced not applied")
	}
	if w := wiringFor(t, config.ControlConfig{Enabled: true, Malformed: true}, ownedState(t)); w.auth.State() != control.StateEnrolled {
		t.Fatal("malformed enforced fell below enrolled")
	}
}

func TestControlWiring_TombstoneForcesLocked(t *testing.T) {
	fsys := control.NewMemFS()
	fsys.SetFile(control.RetiredMarker(wiringDir), []byte(`{"v":1}`))
	w := wiringFor(t, config.ControlConfig{}, fsys)
	if w == nil {
		t.Fatal("a rolled-back reset came back with the protocol off")
	}
	if w.auth.State() != control.StateLocked {
		t.Fatalf("state %s", w.auth.State())
	}
	if w.auth.Policy(control.RouteLAN, "displayPlaylist", nil).Allow {
		t.Fatal("rolled-back reset reopened LAN plaintext")
	}
	if !w.auth.Policy(control.RouteLAN, "factoryReset", nil).Allow {
		t.Fatal("rolled-back reset cannot be retried")
	}
}

func TestControlWiring_UnreadableHostnameWithStateFailsClosed(t *testing.T) {
	fsys := ownedState(t)
	t.Cleanup(func() { status.SetControlStatusSource(nil) })
	w := newControlWiring(config.ControlConfig{}, "", wiringDir, fsys, zap.NewNop())
	if w == nil {
		t.Fatal("state on disk but no protocol because the hostname is unreadable")
	}
}

func TestV2EligibilityUsesActualAuthorityAndEndpoint(t *testing.T) {
	w := wiringFor(t, config.ControlConfig{}, ownedState(t))
	const ep = "wss://relay.test"
	if !relayV2Eligible(ep, w, "FF1-SKYZ2E3A") {
		t.Fatal("persisted authority with config off must dial v2")
	}
	for _, tc := range []struct {
		endpoint  string
		authority *controlWiring
		id        string
	}{
		{"", w, "FF1-SKYZ2E3A"},
		{ep, nil, "FF1-SKYZ2E3A"},
		{ep, w, "FF1"},
		{ep, w, unknownDeviceID},
	} {
		if relayV2Eligible(tc.endpoint, tc.authority, tc.id) {
			t.Fatalf("ineligible v2 dial: %+v", tc)
		}
	}
}

// The v2 endpoint is always derived from relayer.endpoint, so every
// fielded controld.json now resolves one. A device with no control block and
// no control state must still stay legacy-only: authority, not the endpoint,
// gates the dial.
func TestV2DerivedEndpointDoesNotDialWithoutAuthority(t *testing.T) {
	var cfg config.Config
	if err := json.Unmarshal([]byte(`{"relayer":{"endpoint":"wss://relay.test","apiKey":"k"}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	ep := cfg.RelayV2Endpoint(zap.NewNop())
	if ep != "wss://relay.test" {
		t.Fatalf("derived endpoint %q", ep)
	}
	fsys := control.NewMemFS()
	w := wiringFor(t, cfg.ControlTuning(zap.NewNop()), fsys)
	if w != nil {
		t.Fatal("control protocol started without a control block or state")
	}
	if relayV2Eligible(ep, w, "FF1-SKYZ2E3A") {
		t.Fatal("derived v2 endpoint dialed without control authority")
	}
	// The same derived endpoint dials once authority exists on disk.
	if !relayV2Eligible(ep, wiringFor(t, cfg.ControlTuning(zap.NewNop()), ownedState(t)), "FF1-SKYZ2E3A") {
		t.Fatal("derived v2 endpoint not dialed with persisted authority")
	}
}

// §11: control.transports lists the configured transports.
func TestControlWiring_Transports(t *testing.T) {
	var off *controlWiring
	off.setTransports(true) // protocol off: nothing to report, no panic
	w := wiringFor(t, config.ControlConfig{}, ownedState(t))
	w.setTransports(true)
	if got := w.auth.Status().Transports; !slices.Equal(got, []string{"lan", "relayer"}) {
		t.Fatalf("legacy-only transports: %v", got)
	}
	w.v2 = &mocks.MockRelayer{}
	w.setTransports(true)
	if got := w.auth.Status().Transports; !slices.Equal(got, []string{"lan", "relayer", "relayV2"}) {
		t.Fatalf("dual-stack transports: %v", got)
	}
}
