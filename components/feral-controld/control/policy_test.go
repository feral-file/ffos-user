package control

import (
	"encoding/json"
	"testing"
)

// TestPolicy pins §7.2 as a table over (state, switch, route, command). The
// LAN rule (feral-file#3551): the controller tier is open on the LAN in
// legacy and in enrolled with the owner's switch open; owner tier is never
// plaintext on the LAN except the legacy first-owner QR.
func TestPolicy(t *testing.T) {
	const (
		open, closed = true, false
	)
	cases := []struct {
		state State
		lan   bool
		route Route
		cmd   string
		code  string // "" = allow
	}{
		// legacy, LAN: the undoable set plus the first-owner QR.
		{StateLegacy, open, RouteLAN, "displayPlaylist", ""},
		{StateLegacy, open, RouteLAN, "connect", ""},
		{StateLegacy, open, RouteLAN, "getDeviceStatus", ""},
		{StateLegacy, open, RouteLAN, "reboot", ""},
		{StateLegacy, open, RouteLAN, "uploadLogs", ""},
		{StateLegacy, open, RouteLAN, "showPairingQRCode", ""},
		{StateLegacy, closed, RouteLAN, "displayPlaylist", ""}, // the switch is an owner's; none yet
		{StateLegacy, open, RouteLAN, "sshAccess", CodeEnrollmentRequired},
		{StateLegacy, open, RouteLAN, "factoryReset", CodeEnrollmentRequired},
		{StateLegacy, open, RouteLAN, "startWifiSetup", CodeEnrollmentRequired},
		{StateLegacy, open, RouteLAN, "shutdown", CodeEnrollmentRequired},
		{StateLegacy, open, RouteLAN, "joinMintPairingChannel", CodeEnrollmentRequired},
		{StateLegacy, open, RouteLAN, "unknownThing", CodeEnrollmentRequired},
		{StateLegacy, open, RouteLAN, "ping", CodeEnrollmentRequired},
		// legacy, v1 relayer: unchanged (carries no traffic on new images).
		{StateLegacy, open, RouteRelayer, "factoryReset", ""},
		// enrolled, LAN, switch open: the controller tier only.
		{StateEnrolled, open, RouteLAN, "displayPlaylist", ""},
		{StateEnrolled, open, RouteLAN, "connect", ""},
		{StateEnrolled, open, RouteLAN, "reboot", ""},
		{StateEnrolled, open, RouteLAN, "uploadLogs", ""},
		{StateEnrolled, open, RouteLAN, "shutdown", CodeEnrollmentRequired},
		{StateEnrolled, open, RouteLAN, "showPairingQRCode", CodeEnrollmentRequired},
		{StateEnrolled, open, RouteLAN, "sshAccess", CodeEnrollmentRequired},
		{StateEnrolled, open, RouteLAN, "setLanControl", CodeEnrollmentRequired},
		{StateEnrolled, open, RouteLAN, "ping", CodeEnrollmentRequired},
		// enrolled, LAN, switch closed: nothing.
		{StateEnrolled, closed, RouteLAN, "displayPlaylist", CodeEnrollmentRequired},
		{StateEnrolled, closed, RouteLAN, "getDeviceStatus", CodeEnrollmentRequired},
		// enrolled, v1 relayer: unchanged, the switch does not apply.
		{StateEnrolled, closed, RouteRelayer, "displayPlaylist", ""},
		{StateEnrolled, open, RouteRelayer, "connect", ""},
		{StateEnrolled, open, RouteRelayer, "sshAccess", CodeEnvelopeRequired},
		{StateEnrolled, open, RouteRelayer, "ping", CodeEnvelopeRequired},
		// locked: unchanged.
		{StateLocked, open, RouteLAN, "displayPlaylist", CodeLocked},
		{StateLocked, open, RouteRelayer, "displayPlaylist", ""},
		{StateLocked, open, RouteRelayer, "factoryReset", CodeLocked},
		{"bogus", open, RouteRelayer, "factoryReset", CodeLocked},
		{"bogus", open, RouteLAN, "displayPlaylist", CodeLocked},
		// RouteRelayV2: the public relay admits no plaintext in any state,
		// whatever the switch says (the one exception needs its exact
		// request: TestPolicyRelayV2ShowCode).
		{StateLegacy, open, RouteRelayV2, "displayPlaylist", CodeEnrollmentRequired},
		{StateLegacy, open, RouteRelayV2, "showPairingQRCode", CodeEnrollmentRequired},
		{StateEnrolled, open, RouteRelayV2, "displayPlaylist", CodeEnvelopeRequired},
		{StateEnrolled, open, RouteRelayV2, "connect", CodeEnvelopeRequired},
		{StateLocked, open, RouteRelayV2, "displayPlaylist", CodeLocked},
		{StateLocked, open, RouteRelayV2, "factoryReset", CodeLocked},
	}
	for i, tc := range cases {
		d := Policy(tc.state, tc.lan, tc.route, tc.cmd, nil)
		if d.Allow != (tc.code == "") || d.Code != tc.code {
			t.Errorf("case %d (%s lan=%v route=%d %s): %+v, want code %q", i, tc.state, tc.lan, tc.route, tc.cmd, d, tc.code)
		}
	}
}

// §7.2 v2 column: one plaintext request is admitted on relay v2, by exact
// shape and only while the device has no owner.
func TestPolicyRelayV2ShowCode(t *testing.T) {
	const exact = `{"show":true,"purpose":"enroll"}`
	cases := []struct {
		name  string
		state State
		cmd   string
		req   string
		code  string
	}{
		{"exact shape, legacy", StateLegacy, "showPairingQRCode", exact, ""},
		{"member order and whitespace are free", StateLegacy, "showPairingQRCode", ` { "purpose" : "enroll" , "show" : true } `, ""},
		{"enrolled", StateEnrolled, "showPairingQRCode", exact, CodeEnvelopeRequired},
		{"locked", StateLocked, "showPairingQRCode", exact, CodeLocked},
		// show:false latches the pairing confirmation and hides the code.
		{"show false", StateLegacy, "showPairingQRCode", `{"show":false,"purpose":"enroll"}`, CodeEnrollmentRequired},
		{"show false, no purpose", StateLegacy, "showPairingQRCode", `{"show":false}`, CodeEnrollmentRequired},
		// Without the purpose a claimed device repaints its topic QR.
		{"no purpose", StateLegacy, "showPairingQRCode", `{"show":true}`, CodeEnrollmentRequired},
		{"other purpose", StateLegacy, "showPairingQRCode", `{"show":true,"purpose":"pair"}`, CodeEnrollmentRequired},
		{"purpose not a string", StateLegacy, "showPairingQRCode", `{"show":true,"purpose":["enroll"]}`, CodeEnrollmentRequired},
		{"show as string", StateLegacy, "showPairingQRCode", `{"show":"true","purpose":"enroll"}`, CodeEnrollmentRequired},
		{"show as number", StateLegacy, "showPairingQRCode", `{"show":1,"purpose":"enroll"}`, CodeEnrollmentRequired},
		{"extra member", StateLegacy, "showPairingQRCode", `{"show":true,"purpose":"enroll","x":1}`, CodeEnrollmentRequired},
		{"duplicate member", StateLegacy, "showPairingQRCode", `{"show":true,"purpose":"enroll","show":false}`, CodeEnrollmentRequired},
		{"case-variant member", StateLegacy, "showPairingQRCode", `{"show":true,"purpose":"enroll","Show":false}`, CodeEnrollmentRequired},
		{"empty request", StateLegacy, "showPairingQRCode", ``, CodeEnrollmentRequired},
		{"null request", StateLegacy, "showPairingQRCode", `null`, CodeEnrollmentRequired},
		{"array request", StateLegacy, "showPairingQRCode", `[true,"enroll"]`, CodeEnrollmentRequired},
		{"another command with the shape", StateLegacy, "connect", exact, CodeEnrollmentRequired},
		{"factoryReset", StateLegacy, "factoryReset", exact, CodeEnrollmentRequired},
	}
	for _, tc := range cases {
		d := Policy(tc.state, true, RouteRelayV2, tc.cmd, json.RawMessage(tc.req))
		if d.Allow != (tc.code == "") || d.Code != tc.code {
			t.Errorf("%s: %+v, want code %q", tc.name, d, tc.code)
		}
	}
	// The authority applies the same rule, and its retired exception never
	// reaches v2.
	dev := newTestDevice(t)
	if !dev.a.Policy(RouteRelayV2, "showPairingQRCode", json.RawMessage(exact)).Allow {
		t.Fatal("legacy authority refused the show-code request on v2")
	}
	owned, _, _ := ownerDevice(t)
	if d := owned.a.Policy(RouteRelayV2, "showPairingQRCode", json.RawMessage(exact)); d.Code != CodeEnvelopeRequired {
		t.Fatalf("owned authority: %+v", d)
	}
}

func TestAuthorityPolicyUsesStateAndSwitch(t *testing.T) {
	d := newTestDevice(t)
	if d.a.Policy(RouteLAN, "sshAccess", nil).Code != CodeEnrollmentRequired {
		t.Fatal("legacy admitted owner-tier plaintext")
	}
	if !d.a.Policy(RouteLAN, "displayPlaylist", nil).Allow {
		t.Fatal("legacy refused controller-tier plaintext")
	}
	d, owner, _ := ownerDevice(t)
	if !d.a.Policy(RouteLAN, "displayPlaylist", nil).Allow {
		t.Fatal("enrolled, switch open: LAN plaintext refused")
	}
	d.expectOK(owner, d.cmdClaims(owner, CmdSetLanControl, `{"open":false}`))
	if d.a.Policy(RouteLAN, "displayPlaylist", nil).Code != CodeEnrollmentRequired {
		t.Fatal("enrolled, switch closed: LAN plaintext allowed")
	}
}

// isPlayCastShape is the display grant's one request (§5.7).
func TestIsPlayCastShape(t *testing.T) {
	for raw, want := range map[string]bool{
		`{"intent":{"action":"now_display"},"dp1_call":{"items":[]}}`:     true,
		`{"intent":{"action":"now_display"},"dp1_call":{},"x":1}`:         false,
		`{"intent":{"action":"queue"},"dp1_call":{}}`:                     false,
		`{"intent":{"action":"now_display","x":1},"dp1_call":{}}`:         false,
		`{"intent":{"action":"now_display"},"dp1_call":null}`:             false,
		`{"intent":{"action":"now_display"},"dp1_call":{},"dp1_call":{}}`: false,
		`{"intent":{"action":"now_display"}}`:                             false,
	} {
		if got := isPlayCastShape(json.RawMessage(raw)); got != want {
			t.Errorf("isPlayCastShape(%s) = %v", raw, got)
		}
	}
}
