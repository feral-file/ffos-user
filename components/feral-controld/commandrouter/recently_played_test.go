package commandrouter

import "testing"

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
