package playerresponse

import (
	"encoding/json"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/ffplayerfixtures"
)

// decodeFixture simulates the ONE step this package's tests don't otherwise
// exercise: the real cdp client's decode of a Runtime.evaluate result that
// is itself a JSON string (window.handleCDPRequest's return value is always
// `JSON.stringify(...)` — see ffplayerfixtures' package doc). Every other
// package in this module models that decode step identically (see
// setupui_test.go's fakeCDP doc: "a JSON-string expression comes back as
// its unmarshaled map"); this is the same simulation, not a dependency on
// the cdp package itself.
func decodeFixture(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture is not valid JSON: %v\nraw: %s", err, raw)
	}
	return decoded
}

// TestFFPlayerCompat_RefusalReplies drives the LITERAL refreshArtwork refusal
// replies the shipped ff-player produces through the real Refusal()
// classifier — the same boundary decode boot recovery's evaluateRefreshArtwork
// depends on. A drift in ff-player's error string, code, or envelope shape
// fails HERE instead of only inside a hand-built mock.
func TestFFPlayerCompat_RefusalReplies(t *testing.T) {
	cases := []struct {
		name       string
		raw        string
		wantReason string
		wantCode   string
		wantOK     bool
	}{
		{
			name:       "handler_pending",
			raw:        ffplayerfixtures.RefusalHandlerPending,
			wantReason: "No playlist handler registered yet",
			wantCode:   CodeHandlerPending,
			wantOK:     true,
		},
		{
			name:       "no_artwork",
			raw:        ffplayerfixtures.RefusalNoArtwork,
			wantReason: "No active artwork to refresh",
			wantCode:   CodeNoArtwork,
			wantOK:     true,
		},
		{
			name:       "preview_update_failed",
			raw:        ffplayerfixtures.RefusalPreviewUpdateFailed,
			wantReason: "Playlist handler could not update preview URL",
			wantCode:   CodePreviewUpdateFailed,
			wantOK:     true,
		},
		{
			name:       "old player bare {ok:false}",
			raw:        ffplayerfixtures.RefusalOldPlayerBareNotOK,
			wantReason: "",
			wantCode:   "", // unclassifiable — boot recovery's capability fuse decides what happens next
			wantOK:     true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			decoded := decodeFixture(t, tc.raw)
			reason, code, ok := Refusal(decoded)
			if ok != tc.wantOK {
				t.Fatalf("Refusal() ok = %v, want %v", ok, tc.wantOK)
			}
			if reason != tc.wantReason {
				t.Fatalf("Refusal() reason = %q, want %q", reason, tc.wantReason)
			}
			if code != tc.wantCode {
				t.Fatalf("Refusal() code = %q, want %q", code, tc.wantCode)
			}
			// A refusal (ok:false in the reply) must never read as an ACK.
			if OK(decoded) {
				t.Fatalf("OK() reported true for a refusal reply")
			}
		})
	}
}
