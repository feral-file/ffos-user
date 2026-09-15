package commandrouter

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

func TestRecentPlayerReply_ClassifiesOldPlayerAsUnsupported(t *testing.T) {
	got := recentPlayerReply(map[string]interface{}{
		"message": map[string]interface{}{"ok": false},
	})
	message := got["message"].(map[string]interface{})
	if message["status"] != "unsupported" {
		t.Fatalf("status = %v, want unsupported", message["status"])
	}
}

func TestRecentPlayerReply_PreservesExplicitEmpty(t *testing.T) {
	got := recentPlayerReply(map[string]interface{}{
		"message": map[string]interface{}{
			"ok": true, "status": "empty", "records": []interface{}{},
		},
	})
	message := got["message"].(map[string]interface{})
	if message["status"] != "empty" {
		t.Fatalf("status = %v, want empty", message["status"])
	}
}

// Only a TRULY bare false reply identifies a pre-#729 player. A modern player
// that failed and said why must keep its own error: calling an evicted or
// malformed record "unsupported" would tell the app the device cannot do this
// at all, when the truth is a real, possibly retryable failure.
func TestRecentPlayerReply_PreservesAModernErrorWithoutStatus(t *testing.T) {
	got := recentPlayerReply(map[string]interface{}{
		"message": map[string]interface{}{
			"ok": false, "error": "Recently played record is unavailable",
		},
	})
	message := got["message"].(map[string]interface{})
	if message["status"] != "error" {
		t.Fatalf("status = %v, want error", message["status"])
	}
	if message["error"] != "Recently played record is unavailable" {
		t.Fatalf("error = %v, want the player's own error", message["error"])
	}
}

func TestRecentPlayerReply_PreservesAResultBearingFailure(t *testing.T) {
	got := recentPlayerReply(map[string]interface{}{
		"message": map[string]interface{}{
			"ok": false, "records": []interface{}{},
		},
	})
	message := got["message"].(map[string]interface{})
	if message["status"] != "error" {
		t.Fatalf("status = %v, want error", message["status"])
	}
	if _, rewritten := message["error"]; rewritten {
		t.Fatalf("a result-bearing failure must not be given an invented error: %v", message["error"])
	}
}

// A result-bearing failure is a modern player failing for a real reason; only
// an EXACTLY bare {"ok":false} is a pre-feature player. A deny-list of known
// explanatory keys would silently mislabel the next field someone adds.
func TestRecentPlayerReply_PreservesAnUnknownExplanatoryField(t *testing.T) {
	got := recentPlayerReply(map[string]interface{}{
		"message": map[string]interface{}{
			"ok": false, "result": map[string]interface{}{"retryAfterMs": float64(500)},
		},
	})
	message := got["message"].(map[string]interface{})
	if message["status"] != "error" {
		t.Fatalf("status = %v, want error", message["status"])
	}
	if _, invented := message["error"]; invented {
		t.Fatalf("a result-bearing failure must keep its own shape: %v", message)
	}
}

func TestIsBareLegacyFailure_RequiresTheExactShape(t *testing.T) {
	cases := map[string]struct {
		message map[string]interface{}
		want    bool
	}{
		"exactly bare":   {map[string]interface{}{"ok": false}, true},
		"ok true":        {map[string]interface{}{"ok": true}, false},
		"missing ok":     {map[string]interface{}{"error": "x"}, false},
		"with error":     {map[string]interface{}{"ok": false, "error": "x"}, false},
		"with code":      {map[string]interface{}{"ok": false, "code": "busy"}, false},
		"with result":    {map[string]interface{}{"ok": false, "result": 1}, false},
		"with any field": {map[string]interface{}{"ok": false, "somethingNew": 1}, false},
		"non-bool ok":    {map[string]interface{}{"ok": "false"}, false},
	}
	for name, tc := range cases {
		if got := isBareLegacyFailure(tc.message); got != tc.want {
			t.Fatalf("%s: got %v want %v", name, got, tc.want)
		}
	}
}

// The daemon owns this reply shape. Item sources and the full retained DP-1
// item are device-local — they can be signed URLs carrying credentials — and
// this query is reachable from the unauthenticated LAN hub, so the reply is
// rebuilt from an allow-list rather than forwarded as the player returned it.
func TestBoundedRecentlyPlayedReply_DropsEverythingOutsideTheContract(t *testing.T) {
	got := boundedRecentlyPlayedReply(map[string]interface{}{
		"message": map[string]interface{}{
			"ok": true, "status": "ok",
			"activeOccurrenceKnown": true, "incomplete": false,
			"secretDeviceField": "leaked",
			"records": []interface{}{map[string]interface{}{
				"recordId": "rp-1", "playedAtMs": float64(17), "isActive": true,
				"itemId": "work-1", "title": "T", "artist": "A", "thumbnailUrl": "https://thumb",
				"source":         "https://cdn.example/signed?token=secret",
				"item":           map[string]interface{}{"source": "https://cdn.example/signed?token=secret"},
				"inlineManifest": map[string]interface{}{"big": "payload"},
			}},
		},
	})
	message := got["message"].(map[string]interface{})
	if _, leaked := message["secretDeviceField"]; leaked {
		t.Fatalf("unknown top-level field forwarded: %v", message)
	}
	records := message["records"].([]interface{})
	if len(records) != 1 {
		t.Fatalf("records = %v", records)
	}
	record := records[0].(map[string]interface{})
	for _, forbidden := range []string{"source", "item", "inlineManifest"} {
		if _, leaked := record[forbidden]; leaked {
			t.Fatalf("%q reached the controller: %v", forbidden, record)
		}
	}
	for key, want := range map[string]interface{}{
		"recordId": "rp-1", "playedAtMs": float64(17), "isActive": true,
		"itemId": "work-1", "title": "T", "artist": "A", "thumbnailUrl": "https://thumb",
	} {
		if record[key] != want {
			t.Fatalf("%s = %v, want %v", key, record[key], want)
		}
	}
	if message["activeOccurrenceKnown"] != true || message["incomplete"] != false || message["status"] != "ok" {
		t.Fatalf("documented fields lost: %v", message)
	}
}

func TestBoundedRecentlyPlayedReply_BoundsAndSkipsUnusableRows(t *testing.T) {
	raw := make([]interface{}, 0, maxRecentlyPlayedRecords+10)
	raw = append(raw, "not a record", map[string]interface{}{"title": "no id"})
	for i := 0; i < maxRecentlyPlayedRecords+5; i++ {
		raw = append(raw, map[string]interface{}{"recordId": "rp"})
	}
	got := boundedRecentlyPlayedReply(map[string]interface{}{
		"message": map[string]interface{}{"ok": true, "records": raw},
	})
	records := got["message"].(map[string]interface{})["records"].([]interface{})
	if len(records) != maxRecentlyPlayedRecords {
		t.Fatalf("records = %d, want the cap %d", len(records), maxRecentlyPlayedRecords)
	}
}

// Failures are recentPlayerReply's job; this must leave them exactly as
// classified rather than rebuilding them into a success shape.
func TestBoundedRecentlyPlayedReply_LeavesFailuresAlone(t *testing.T) {
	in := map[string]interface{}{"message": map[string]interface{}{
		"ok": false, "status": "unsupported", "error": "Recently played is not supported by this player",
	}}
	got := boundedRecentlyPlayedReply(in)
	message := got["message"].(map[string]interface{})
	if message["status"] != "unsupported" || message["error"] == nil {
		t.Fatalf("failure reply altered: %v", message)
	}
}

// Rows alone are not a bound: the LAN hub accepts a 4 MiB inline playlist from
// an unauthenticated caller, its metadata becomes retained history labels, and
// those come back through this reply — so 200 rows can still be megabytes.
func TestBoundedRecentlyPlayedReply_BoundsLabelBytes(t *testing.T) {
	huge := strings.Repeat("A", 64*1024)
	raw := make([]interface{}, 0, 8)
	for i := 0; i < 8; i++ {
		raw = append(raw, map[string]interface{}{
			"recordId": "rp", "title": huge, "artist": huge, "thumbnailUrl": huge, "itemId": huge,
		})
	}
	got := boundedRecentlyPlayedReply(map[string]interface{}{
		"message": map[string]interface{}{"ok": true, "records": raw},
	})
	records := got["message"].(map[string]interface{})["records"].([]interface{})

	total := 0
	for _, entry := range records {
		record := entry.(map[string]interface{})
		for _, key := range []string{"recordId", "itemId", "title", "artist", "thumbnailUrl"} {
			label, _ := record[key].(string)
			if len(label) > maxRecentlyPlayedLabelBytes {
				t.Fatalf("%s is %d bytes, over the per-field cap %d", key, len(label), maxRecentlyPlayedLabelBytes)
			}
			total += len(label)
		}
	}
	if total > maxRecentlyPlayedReplyBytes+maxRecentlyPlayedLabelBytes*5 {
		t.Fatalf("aggregate label payload %d exceeds the cap %d", total, maxRecentlyPlayedReplyBytes)
	}
	if len(records) == 0 {
		t.Fatal("bounding must not empty the list")
	}
}

// Truncation must not produce invalid UTF-8 on the wire.
func TestTruncateLabel_KeepsValidUTF8(t *testing.T) {
	label := strings.Repeat("é", maxRecentlyPlayedLabelBytes)
	got := truncateLabel(label)
	if len(got) > maxRecentlyPlayedLabelBytes {
		t.Fatalf("truncated to %d bytes, over the cap", len(got))
	}
	if !utf8.ValidString(got) {
		t.Fatal("truncation split a rune")
	}
	short := "ok"
	if truncateLabel(short) != short {
		t.Fatal("a short label must be untouched")
	}
}

// recordId is the opaque replay handle, not a label: playRecentlyPlayed
// forwards it verbatim to the player's resolver. Truncating it would advertise
// a row that deterministically fails to play, so it is passed through
// losslessly and an implausibly long one drops its row instead.
func TestBoundedRecentlyPlayedReply_NeverTruncatesTheReplayHandle(t *testing.T) {
	oversized := "rp-" + strings.Repeat("9", maxRecentlyPlayedRecordIDBytes)
	got := boundedRecentlyPlayedReply(map[string]interface{}{
		"message": map[string]interface{}{"ok": true, "records": []interface{}{
			map[string]interface{}{"recordId": "rp-1788892946764001", "title": "ok"},
			map[string]interface{}{"recordId": oversized, "title": "unusable"},
		}},
	})
	records := got["message"].(map[string]interface{})["records"].([]interface{})
	if len(records) != 1 {
		t.Fatalf("an unusable handle must drop its row, got %d rows", len(records))
	}
	if records[0].(map[string]interface{})["recordId"] != "rp-1788892946764001" {
		t.Fatalf("handle altered: %v", records[0])
	}
}

// Failure replies are rebuilt from the allow-list too. recentPlayerReply only
// CLASSIFIES a failure, so a modern player's failure could otherwise carry a
// retained item, record or diagnostic field out through the unauthenticated LAN
// API — the same signed-URL exposure the success path is bounded for.
func TestBoundedRecentlyPlayedReply_BoundsFailuresToo(t *testing.T) {
	const signed = "https://cdn.example/work.html?token=secret&sig=deadbeef"
	got := boundedRecentlyPlayedReply(map[string]interface{}{
		"message": map[string]interface{}{
			"ok": false, "status": "error",
			"error":       "could not replay " + signed,
			"item":        map[string]interface{}{"source": signed},
			"records":     []interface{}{map[string]interface{}{"source": signed}},
			"diagnostics": map[string]interface{}{"lastSource": signed},
		},
	})
	message := got["message"].(map[string]interface{})
	for _, forbidden := range []string{"item", "records", "diagnostics"} {
		if _, leaked := message[forbidden]; leaked {
			t.Fatalf("%q escaped a FAILURE reply: %v", forbidden, message)
		}
	}
	if message["status"] != "error" {
		t.Fatalf("the classification must survive: %v", message)
	}
	// The player's explanation is kept, but its credentials are not.
	reason, _ := message["error"].(string)
	if !strings.Contains(reason, "could not replay") {
		t.Fatalf("the player's explanation must survive: %q", reason)
	}
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "token=secret") {
		t.Fatalf("a signed URL's credentials escaped a failure reply: %s", encoded)
	}
}

func TestSanitizeErrorText_RedactsCredentialsAndKeepsThePath(t *testing.T) {
	got := sanitizeErrorText("cannot load https://cdn.example/a/b.html?token=secret&sig=x after 3 tries")
	if strings.Contains(got, "token=secret") {
		t.Fatalf("credentials survived: %q", got)
	}
	for _, keep := range []string{"cannot load", "https://cdn.example/a/b.html", "after 3 tries"} {
		if !strings.Contains(got, keep) {
			t.Fatalf("%q lost from %q", keep, got)
		}
	}
	// Text with no URL is untouched apart from whitespace normalization.
	if sanitizeErrorText("Recently played record is unavailable") != "Recently played record is unavailable" {
		t.Fatal("plain text must not be mangled")
	}
}
