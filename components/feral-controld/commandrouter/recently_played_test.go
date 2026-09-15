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
