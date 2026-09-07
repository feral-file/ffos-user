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
