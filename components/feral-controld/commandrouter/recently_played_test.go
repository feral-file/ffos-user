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
