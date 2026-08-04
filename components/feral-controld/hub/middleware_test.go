package hub

import "testing"

// TestContactObserverRouteAndLoopbackFilter pins the §4.1 contact signal's
// exclusions (load-bearing, not hygiene): only the control-plane routes
// count, and loopback sources never do — feral-vmagent scrapes /metrics over
// 127.0.0.1 every 60s on every device, and any loopback poller of a counted
// endpoint would otherwise pin the escape policy's deferral permanently.
func TestContactObserverRouteAndLoopbackFilter(t *testing.T) {
	for _, tt := range []struct {
		route  string
		remote string
		want   bool
	}{
		{"cast", "192.168.1.10:41000", true},
		{"status", "192.168.1.10:41000", true},
		{"status_v2", "192.168.1.10:41000", true},
		{"metrics", "192.168.1.10:41000", false},
		{"notification", "192.168.1.10:41000", false},
		{"unmatched", "192.168.1.10:41000", false},
		{"cast", "127.0.0.1:41000", false},
		{"cast", "[::1]:41000", false},
		{"cast", "not-an-addr", false}, // unparseable must never fabricate contact
	} {
		got := countsAsContact(tt.route) && !isLoopbackAddr(tt.remote)
		if got != tt.want {
			t.Fatalf("route=%s remote=%s: contact=%v, want %v", tt.route, tt.remote, got, tt.want)
		}
	}
}
