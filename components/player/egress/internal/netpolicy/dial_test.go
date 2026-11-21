package netpolicy

import (
	"net/netip"
	"testing"
)

func TestDestinationPolicy(t *testing.T) {
	for _, s := range []string{"0.0.0.0", "127.0.0.1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "100.64.0.1", "169.254.169.254", "224.0.0.251", "255.255.255.255", "::1", "::ffff:192.168.1.1", "fd00::1", "fe80::1", "ff02::fb", "64:ff9b::1", "2002::1"} {
		if public(netip.MustParseAddr(s)) {
			t.Errorf("allowed %s", s)
		}
	}
	for _, s := range []string{"1.1.1.1", "2606:4700:4700::1111"} {
		if !public(netip.MustParseAddr(s)) {
			t.Errorf("denied %s", s)
		}
	}
}

func TestDefaultRoute(t *testing.T) {
	for raw, want := range map[string]bool{
		`[{"dst":"default","gateway":"192.168.1.1"}]`:    true,
		`[{"dst":"default","dev":"tun0"}]`:               true,
		`[{"dst":"2000:aaaa::/64","gateway":"fe80::1"}]`: false,
		`[{"dst":"default","type":"unreachable"}]`:       false,
		`[]`: false,
	} {
		if permittedRoute([]byte(raw), false) != want {
			t.Errorf("route %s, want %v", raw, want)
		}
	}
}

func TestLANRoutes(t *testing.T) {
	for raw, want := range map[string]bool{
		`[{"dst":"192.168.1.0/24"}]`:                   true,
		`[{"dst":"10.0.0.8","type":"local"}]`:          false,
		`[{"dst":"192.168.1.255","type":"broadcast"}]`: false,
		`[{}]`: false,
	} {
		if permittedRoute([]byte(raw), true) != want {
			t.Errorf("route %s, want %v", raw, want)
		}
	}
}
