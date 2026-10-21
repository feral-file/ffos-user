package control

import (
	"encoding/json"
	"testing"
)

func TestPolicy(t *testing.T) {
	play := json.RawMessage(`{"intent":{"action":"now_display"},"dp1_call":{"items":[]}}`)
	cases := []struct {
		state State
		route Route
		cmd   string
		req   json.RawMessage
		code  string // "" = allow
	}{
		{StateLegacy, RouteLAN, "sshAccess", nil, ""},
		{StateLegacy, RouteRelayer, "factoryReset", nil, ""},
		{StateEnrolled, RouteLAN, "displayPlaylist", play, CodeEnrollmentRequired},
		{StateEnrolled, RouteRelayer, "displayPlaylist", play, ""},
		{StateEnrolled, RouteRelayer, "rotate", nil, ""},
		// connect starts every app cast; the executor keeps the claim owner-only.
		{StateEnrolled, RouteRelayer, "connect", nil, ""},
		{StateEnforced, RouteRelayer, "connect", nil, CodeEnvelopeRequired},
		{StateEnrolled, RouteRelayer, "sshAccess", nil, CodeEnvelopeRequired},
		{StateEnrolled, RouteRelayer, "unknownThing", nil, CodeEnvelopeRequired},
		{StateEnrolled, RouteRelayer, "ping", nil, CodeEnvelopeRequired},
		{StateEnforced, RouteLAN, "displayPlaylist", play, CodeEnrollmentRequired},
		{StateEnforced, RouteRelayer, "displayPlaylist", play, ""},
		{StateEnforced, RouteRelayer, "rotate", nil, CodeEnvelopeRequired},
		{StateEnforced, RouteRelayer, "displayPlaylist", json.RawMessage(`{"intent":{"action":"now_display"},"dp1_call":{},"x":1}`), CodeEnvelopeRequired},
		{StateEnforced, RouteRelayer, "displayPlaylist", json.RawMessage(`{"intent":{"action":"queue"},"dp1_call":{}}`), CodeEnvelopeRequired},
		{StateEnforced, RouteRelayer, "displayPlaylist", json.RawMessage(`{"intent":{"action":"now_display","x":1},"dp1_call":{}}`), CodeEnvelopeRequired},
		{StateEnforced, RouteRelayer, "displayPlaylist", json.RawMessage(`{"intent":{"action":"now_display"},"dp1_call":null}`), CodeEnvelopeRequired},
		{StateEnforced, RouteRelayer, "displayPlaylist", json.RawMessage(`{"intent":{"action":"now_display"},"dp1_call":{},"dp1_call":{}}`), CodeEnvelopeRequired},
		{StateEnforced, RouteRelayer, "displayPlaylist", json.RawMessage(`{"intent":{"action":"now_display"}}`), CodeEnvelopeRequired},
		{StateLocked, RouteLAN, "displayPlaylist", play, CodeLocked},
		{StateLocked, RouteRelayer, "displayPlaylist", play, ""},
		{StateLocked, RouteRelayer, "factoryReset", nil, CodeLocked},
		{"bogus", RouteRelayer, "factoryReset", nil, CodeLocked},
		// RouteRelayV2: the public device-ID relay admits no plaintext in
		// any state, not even what legacy or the Play shape allow elsewhere.
		{StateLegacy, RouteRelayV2, "sshAccess", nil, CodeEnrollmentRequired},
		{StateLegacy, RouteRelayV2, "displayPlaylist", play, CodeEnrollmentRequired},
		{StateEnrolled, RouteRelayV2, "displayPlaylist", play, CodeEnvelopeRequired},
		{StateEnrolled, RouteRelayV2, "connect", nil, CodeEnvelopeRequired},
		{StateEnforced, RouteRelayV2, "displayPlaylist", play, CodeEnvelopeRequired},
		{StateLocked, RouteRelayV2, "displayPlaylist", play, CodeLocked},
		{StateLocked, RouteRelayV2, "factoryReset", nil, CodeLocked},
	}
	for i, tc := range cases {
		d := Policy(tc.state, tc.route, tc.cmd, tc.req)
		if d.Allow != (tc.code == "") || d.Code != tc.code {
			t.Errorf("case %d (%s %d %s): %+v, want code %q", i, tc.state, tc.route, tc.cmd, d, tc.code)
		}
	}
}

func TestAuthorityPolicyUsesState(t *testing.T) {
	d := newTestDevice(t)
	if !d.a.Policy(RouteLAN, "sshAccess", nil).Allow {
		t.Fatal("legacy refused plaintext")
	}
	d, _, _ = ownerDevice(t)
	if d.a.Policy(RouteLAN, "displayPlaylist", nil).Code != CodeEnrollmentRequired {
		t.Fatal("enrolled LAN plaintext allowed")
	}
}
