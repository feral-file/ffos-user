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
