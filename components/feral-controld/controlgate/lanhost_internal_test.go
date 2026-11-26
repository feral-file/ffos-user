package controlgate

import (
	"net"
	"testing"
)

func TestLANIPv4Literal(t *testing.T) {
	tests := []struct{ ip, want string }{
		{"192.168.1.23", "192.168.1.23"},
		{"10.0.0.7", "10.0.0.7"},
		{"172.16.4.4", "172.16.4.4"},
		{"169.254.10.2", "169.254.10.2"},
		{"203.0.113.9", ""}, // public: a controller would refuse it
		{"100.64.0.3", ""},  // CGNAT / Tailscale, not the LAN
		{"127.0.0.1", ""},   // loopback
		{"fd00::1", ""},     // IPv6
		{"::ffff:10.1.2.3", "10.1.2.3"},
	}
	for _, tc := range tests {
		if got := lanIPv4Literal(net.ParseIP(tc.ip)); got != tc.want {
			t.Errorf("lanIPv4Literal(%s) = %q, want %q", tc.ip, got, tc.want)
		}
	}
	if got := lanIPv4Literal(nil); got != "" {
		t.Errorf("nil ip = %q", got)
	}
}
