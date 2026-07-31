package playerresponse

import "testing"

// OK is the predicate deciding whether the boot player recovery stops at a
// gentle in-app refresh or escalates to a destructive Page.reload, so every
// real reply shape the player produces is pinned here.
func TestOK(t *testing.T) {
	cases := []struct {
		name   string
		result interface{}
		want   bool
	}{
		{"wrapped ok true", map[string]interface{}{"message": map[string]interface{}{"ok": true}}, true},
		{"wrapped ok false", map[string]interface{}{"message": map[string]interface{}{"ok": false}}, false},
		{"wrapped refusal with error", map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "No active artwork to refresh"}}, false},
		{"wrapped missing ok", map[string]interface{}{"message": map[string]interface{}{}}, false},
		{"wrapped ok wrong type", map[string]interface{}{"message": map[string]interface{}{"ok": "true"}}, false},
		{"top-level ok true", map[string]interface{}{"ok": true}, true},
		{"top-level ok false", map[string]interface{}{"ok": false}, false},
		{"message not a map falls back to top-level", map[string]interface{}{"message": "done", "ok": true}, true},
		{"nil result", nil, false},
		{"non-map result", "ok", false},
		{"empty map", map[string]interface{}{}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := OK(tc.result); got != tc.want {
				t.Fatalf("OK(%#v) = %v, want %v", tc.result, got, tc.want)
			}
		})
	}
}

// TestRefusal pins the boundary classification (design doc §3.2/§4.2): every
// `error` reason string ff-player's CanvasService.refreshArtwork ships
// alongside `code` (same-release redundancy, not a legacy/pre-`code` wire
// format — see the reason* constants' doc) must classify to the same code a
// `code`-carrying reply would, and a non-refusal or unclassifiable refusal
// must be reported honestly.
func TestRefusal(t *testing.T) {
	cases := []struct {
		name       string
		result     interface{}
		wantReason string
		wantCode   string
		wantOK     bool
	}{
		{
			name:   "ok reply is not a refusal",
			result: map[string]interface{}{"message": map[string]interface{}{"ok": true}},
			wantOK: false,
		},
		{
			name:       "code-carrying reply preferred over the reason-string fallback",
			result:     map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "No active artwork to refresh", "code": "no_artwork"}},
			wantReason: "No active artwork to refresh",
			wantCode:   "no_artwork",
			wantOK:     true,
		},
		{
			name:       "reason-string fallback: handler_pending",
			result:     map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "No playlist handler registered yet"}},
			wantReason: "No playlist handler registered yet",
			wantCode:   "handler_pending",
			wantOK:     true,
		},
		{
			name:       "reason-string fallback: no_artwork",
			result:     map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "No active artwork to refresh"}},
			wantReason: "No active artwork to refresh",
			wantCode:   "no_artwork",
			wantOK:     true,
		},
		{
			name:       "reason-string fallback: preview_update_failed",
			result:     map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "Playlist handler could not update preview URL"}},
			wantReason: "Playlist handler could not update preview URL",
			wantCode:   "preview_update_failed",
			wantOK:     true,
		},
		{
			name:       "unrecognized reason is a refusal with no code",
			result:     map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "something else broke"}},
			wantReason: "something else broke",
			wantCode:   "",
			wantOK:     true,
		},
		{
			name:   "bare non-ACK with no reason is a refusal with no code",
			result: map[string]interface{}{"message": map[string]interface{}{}},
			wantOK: true,
		},
		{
			name:   "nil result is not a refusal",
			result: nil,
			wantOK: false,
		},
		{
			name:   "non-map result is not a refusal",
			result: "ok",
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			reason, code, ok := Refusal(tc.result)
			if ok != tc.wantOK {
				t.Fatalf("Refusal(%#v) ok = %v, want %v", tc.result, ok, tc.wantOK)
			}
			if !tc.wantOK {
				return
			}
			if reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", reason, tc.wantReason)
			}
			if code != tc.wantCode {
				t.Errorf("code = %q, want %q", code, tc.wantCode)
			}
		})
	}
}
