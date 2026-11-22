package hub

import (
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"
)

// httptest requests come from 192.0.2.1 (TEST-NET-1, a public range). Every
// hub test in this package treats that network as on-link, as if the device
// sat on it; the tests below pass their own address lists.
func init() {
	interfaceAddrs = func() ([]net.Addr, error) {
		return []net.Addr{mustIPNet("192.0.2.10/24")}, nil
	}
}

func mustIPNet(cidr string) *net.IPNet {
	ip, n, err := net.ParseCIDR(cidr)
	if err != nil {
		panic(err)
	}
	n.IP = ip
	return n
}

func TestIsLocalSource(t *testing.T) {
	campus := func() ([]net.Addr, error) {
		return []net.Addr{
			mustIPNet("203.0.113.40/24"),            // a campus network handing out public v4
			mustIPNet("2001:db8:1:2::40/64"),        // a home LAN's global v6 prefix
			mustIPNet("198.51.100.7/32"),            // a host route covers only itself
			&net.IPAddr{IP: net.ParseIP("1.1.1.1")}, // not a prefix: ignored
		}, nil
	}
	for _, tc := range []struct {
		remote string
		addrs  func() ([]net.Addr, error)
		local  bool
	}{
		{"127.0.0.1:5000", nil, true},
		{"[::1]:5000", nil, true},
		{"192.168.1.20:5000", nil, true},
		{"10.0.0.2:5000", nil, true},
		{"172.16.5.5:5000", nil, true},
		{"169.254.10.10:5000", nil, true},
		{"[fe80::1%wlan0]:5000", nil, true},
		{"[fd12:3456::1]:5000", nil, true},        // ULA
		{"100.101.102.103:5000", nil, true},       // CGNAT / Tailscale
		{"[::ffff:192.168.1.20]:5000", nil, true}, // mapped v4
		{"[::ffff:8.8.8.8]:5000", nil, false},
		{"8.8.8.8:5000", nil, false},
		{"[2001:4860::8888]:5000", nil, false},
		{"203.0.113.99:5000", campus, true},
		{"[2001:db8:1:2::99]:5000", campus, true},
		{"[2001:db8:1:3::99]:5000", campus, false},
		{"198.51.100.8:5000", campus, false},
		{"203.0.114.1:5000", campus, false},
		{"203.0.113.99:5000", func() ([]net.Addr, error) { return nil, errors.New("netlink") }, false},
		{"203.0.113.99:5000", func() ([]net.Addr, error) { return []net.Addr{mustIPNet("0.0.0.0/0")}, nil }, false},
		{"not-an-address", campus, false},
		{"", campus, false},
		{"[::1", nil, false},
	} {
		if got := isLocalSource(tc.remote, tc.addrs); got != tc.local {
			t.Errorf("isLocalSource(%q) = %v, want %v", tc.remote, got, tc.local)
		}
	}
}

func TestMiddlewareRefusesNonLocalSource(t *testing.T) {
	h := &hub{logger: zap.NewNop(), reqSlots: make(chan struct{}, 1)}
	served := false
	handler := h.withMiddleware("cast", func(http.ResponseWriter, *http.Request) { served = true })
	for _, tc := range []struct {
		remote string
		status int
	}{
		{"8.8.8.8:4000", http.StatusForbidden},
		{"192.168.1.5:4000", http.StatusOK},
	} {
		served = false
		req := httptest.NewRequest(http.MethodPost, "http://192.168.1.2:1111/api/cast", nil)
		req.RemoteAddr = tc.remote
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != tc.status || served != (tc.status == http.StatusOK) {
			t.Fatalf("%s: status %d served %v", tc.remote, rec.Code, served)
		}
		if tc.status == http.StatusForbidden && rec.Body.String() != "{\"error\":{\"code\":\"lan_only\"}}\n" {
			t.Fatalf("%s: body %q", tc.remote, rec.Body.String())
		}
	}
}
