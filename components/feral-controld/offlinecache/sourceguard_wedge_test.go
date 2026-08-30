package offlinecache

// White-box tests (package offlinecache, like the other *_wedge_test.go
// files) because the enforcement they cover is unexported: the guard's
// transport hook, its redirect policy, and the client that carries them.
//
// What these pin is the difference between checking a URL and checking a
// SOCKET. sourceGuard.check runs once, on the source URL, before anything
// is queued — and on its own that is bypassed by a plain 302 or by a name
// whose answer changes between the check and the dial. Neither needs a
// hostile DNS server to be interesting; the redirect needs nothing but an
// attacker-controlled origin.

import (
	"context"
	"errors"
	"net"
	go_http "net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// loopbackIsPublic is the isReserved override these tests inject. Every
// httptest server lives on 127.0.0.1, which the real predicate rejects —
// so without this the scenario the guard exists to stop (a permitted
// origin redirecting somewhere forbidden) could not be staged at all,
// because the permitted origin could not exist. It relaxes loopback ONLY;
// everything else still goes through isReservedAddr, so the rejection
// under test is the production one.
func loopbackIsPublic(ip net.IP) bool {
	if ip.IsLoopback() {
		return false
	}
	return isReservedAddr(ip)
}

// flipResolver answers with one address the first time and another after
// that — DNS rebinding in its simplest form.
type flipResolver struct {
	first, rest string
	calls       atomic.Int32
}

func (r *flipResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	ip := r.rest
	if r.calls.Add(1) == 1 {
		ip = r.first
	}
	return []net.IPAddr{{IP: net.ParseIP(ip)}}, nil
}

type staticResolver struct{ ip string }

func (r staticResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return []net.IPAddr{{IP: net.ParseIP(r.ip)}}, nil
}

// TestSourceGuard_RedirectToReservedAddressIsBlocked is the finding this
// enforcement exists for: a source that passes the pre-flight check and
// then redirects somewhere reserved. No hostile DNS is involved — the
// attacker only has to control the origin they already told us to fetch.
func TestSourceGuard_RedirectToReservedAddressIsBlocked(t *testing.T) {
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		// The classic pivot: bounce to a name that lands on the LAN.
		go_http.Redirect(w, &go_http.Request{}, "http://internal.example/api/cast", go_http.StatusFound)
	}))
	defer origin.Close()

	guard := sourceGuard{resolver: staticResolver{ip: "10.0.0.1"}, isReserved: loopbackIsPublic}
	client := newGuardedHTTPClientFor(guard, 0)

	req, err := client.NewRequest(go_http.MethodGet, origin.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "the redirect hop must not be followed")
	assert.ErrorIs(t, err, ErrUnsafeSource)
	assert.Contains(t, err.Error(), "10.0.0.1")
}

// TestSourceGuard_RebindingBetweenCheckAndDialIsBlocked pins the other
// half: check() sees a public answer and admits the source, then the
// dial re-resolves and gets a reserved one. A URL-time check alone cannot
// see this; the transport hook re-applies the same policy to whatever the
// second lookup returned.
func TestSourceGuard_RebindingBetweenCheckAndDialIsBlocked(t *testing.T) {
	resolver := &flipResolver{first: "93.184.216.34", rest: "127.0.0.1"}
	guard := sourceGuard{resolver: resolver}

	// Pre-flight: the first answer is public, so the source is admitted.
	require.NoError(t, guard.check(context.Background(), "https://rebind.example/art.html"),
		"the first answer is public, so the URL check must pass")

	// The dial re-resolves and gets loopback — and is refused.
	_, err := guard.dialContext(context.Background(), "tcp", "rebind.example:443")
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsafeSource)
	assert.Contains(t, err.Error(), "127.0.0.1")
	assert.GreaterOrEqual(t, resolver.calls.Load(), int32(2),
		"the dial must resolve for itself rather than trust the check's answer")
}

func TestSourceGuard_DialContextRejectsReservedTargets(t *testing.T) {
	guard := sourceGuard{resolver: staticResolver{ip: "192.168.1.10"}}
	for _, tc := range []struct{ name, addr string }{
		{"loopback literal", "127.0.0.1:1111"},
		{"ipv6 loopback literal", "[::1]:9222"},
		{"ipv4-mapped loopback", "[::ffff:127.0.0.1]:9223"},
		{"link-local metadata", "169.254.169.254:80"},
		{"private literal", "10.1.2.3:80"},
		{"name resolving private", "cdn.example:443"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := guard.dialContext(context.Background(), "tcp", tc.addr)
			require.Error(t, err)
			assert.ErrorIs(t, err, ErrUnsafeSource)
		})
	}
}

// A public target still dials — the guard must not be a blanket denial.
func TestSourceGuard_DialContextAllowsPublicTarget(t *testing.T) {
	server := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	defer server.Close()

	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}, isReserved: loopbackIsPublic}
	client := newGuardedHTTPClientFor(guard, 0)
	req, err := client.NewRequest(go_http.MethodGet, server.URL, nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, go_http.StatusOK, resp.StatusCode)
}

func TestSourceGuard_CheckRedirect(t *testing.T) {
	guard := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}}

	t.Run("non-http scheme is refused", func(t *testing.T) {
		// dialContext never sees a scheme — by the time a socket is
		// opened the URL is gone — so this is the only place a redirect
		// to file:// or ftp:// can be caught.
		for _, raw := range []string{"file:///etc/shadow", "ftp://example.com/x", "gopher://example.com"} {
			req, err := go_http.NewRequest(go_http.MethodGet, raw, nil)
			require.NoError(t, err)
			err = guard.checkRedirect(req, nil)
			require.Error(t, err, raw)
			assert.ErrorIs(t, err, ErrUnsafeSource)
		}
	})

	t.Run("hop cap is enforced", func(t *testing.T) {
		req, err := go_http.NewRequest(go_http.MethodGet, "https://example.com/", nil)
		require.NoError(t, err)
		via := make([]*go_http.Request, maxSourceRedirects)
		err = guard.checkRedirect(req, via)
		require.Error(t, err)
		assert.ErrorIs(t, err, ErrUnsafeSource)
	})

	t.Run("an ordinary https hop passes", func(t *testing.T) {
		req, err := go_http.NewRequest(go_http.MethodGet, "https://example.com/next", nil)
		require.NoError(t, err)
		assert.NoError(t, guard.checkRedirect(req, []*go_http.Request{req}))
	})
}

// A resolver failure must stay a transient network error, never a
// security refusal — an offline device must not look like it is under
// attack. Mirrors check()'s contract for the same condition.
func TestSourceGuard_DialContextResolverFailureIsNotUnsafe(t *testing.T) {
	guard := sourceGuard{resolver: erroringResolver{}}
	_, err := guard.dialContext(context.Background(), "tcp", "cdn.example:443")
	require.Error(t, err)
	assert.NotErrorIs(t, err, ErrUnsafeSource)
}

type erroringResolver struct{}

func (erroringResolver) LookupIPAddr(_ context.Context, _ string) ([]net.IPAddr, error) {
	return nil, errors.New("dns is down")
}

// TestSourceGuard_TransportHasNoProxy pins the property the whole
// socket-level guarantee rests on. With a proxy configured, Transport
// dials the PROXY and hands it the origin host, so dialContext would
// check the proxy's address while the proxy resolved and connected to
// whatever the URL named — the destination would never be examined at
// all. An earlier version of transport() copied http.DefaultTransport's
// settings wholesale and inherited ProxyFromEnvironment with them.
//
// Asserted on the field rather than through the environment because
// http.ProxyFromEnvironment caches HTTP_PROXY behind a sync.Once, which
// would make an env-driven assertion depend on whether some earlier test
// in the binary had already tripped it.
func TestSourceGuard_TransportHasNoProxy(t *testing.T) {
	tr := sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}}.transport()
	assert.Nil(t, tr.Proxy, "a proxied request would put the destination out of reach of dialContext")
	require.NotNil(t, tr.DialContext, "the guard's dialer must be the one in use")
}

// TestSourceGuard_ProxyEnvIsIgnored is the behavioral half the reviewer
// asked for: with HTTP_PROXY/HTTPS_PROXY pointing at a recording server,
// a source whose host resolves into reserved space must be refused
// locally and the proxy must never be told about it. A client that
// honored the proxy would hand it "internal.example" and let it do the
// resolving, which is precisely the bypass.
func TestSourceGuard_ProxyEnvIsIgnored(t *testing.T) {
	var proxied atomic.Int32
	proxy := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, _ *go_http.Request) {
		proxied.Add(1)
		w.WriteHeader(go_http.StatusOK)
	}))
	defer proxy.Close()

	t.Setenv("HTTP_PROXY", proxy.URL)
	t.Setenv("HTTPS_PROXY", proxy.URL)
	t.Setenv("NO_PROXY", "")

	// Loopback counts as public here only so the proxy could be reached
	// at all if the client tried; the destination stays genuinely
	// reserved, so the refusal under test is the production one.
	guard := sourceGuard{resolver: staticResolver{ip: "10.0.0.1"}, isReserved: loopbackIsPublic}
	client := newGuardedHTTPClientFor(guard, 0)

	req, err := client.NewRequest(go_http.MethodGet, "http://internal.example/api/cast", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrUnsafeSource)
	assert.Zero(t, proxied.Load(), "the proxy must never be handed an unsafe destination")
}

// TestSourceGuard_ErrorsAreBoundedForLogging closes a bypass of the
// source-log cap. The guard's errors are LOGGED — by Classify's caller
// and by the capture-side blocker, both next to an already-truncated URL
// — so any untrusted string they embed raw defeats that truncation
// entirely. url.Parse accepts a 100,000-character hostname without
// complaint, so an unauthenticated LAN client could force multi-megabyte
// entries into controld.log through the error text alone.
func TestSourceGuard_ErrorsAreBoundedForLogging(t *testing.T) {
	huge := strings.Repeat("a", maxLoggedSourceBytes*40)

	for _, tc := range []struct {
		name string
		err  error
	}{
		{
			name: "hostname that fails to resolve",
			err: sourceGuard{resolver: erroringResolver{}}.
				check(context.Background(), "https://"+huge+"/x"),
		},
		{
			name: "hostname resolving to a reserved address",
			err: sourceGuard{resolver: staticResolver{ip: "127.0.0.1"}}.
				check(context.Background(), "https://"+huge+"/x"),
		},
		{
			name: "unsupported scheme",
			err: sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}}.
				check(context.Background(), huge+"://example.com/x"),
		},
		{
			name: "unparseable URL",
			err: sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}}.
				check(context.Background(), "http://["+huge),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Error(t, tc.err)
			assert.Less(t, len(tc.err.Error()), len(huge),
				"a guard error must not carry the untrusted input at full length — it is logged")
		})
	}
}

// The capture-side blocker builds its own errors and is logged the same
// way, so it needs the same bound.
func TestCapturer_SourceGuard_BlockErrorsAreBoundedForLogging(t *testing.T) {
	huge := strings.Repeat("b", maxLoggedSourceBytes*40)
	c := &capturer{
		json:   wrapper.NewJSON(),
		logger: zaptest.NewLogger(t),
		guard:  sourceGuard{resolver: staticResolver{ip: "127.0.0.1"}},
	}
	for _, raw := range []string{
		"https://" + huge + "/x",
		huge + "://example.com/x",
		"http://[" + huge,
	} {
		err := c.pausedRequestAllowed(context.Background(), raw)
		require.Error(t, err, raw[:40])
		assert.Less(t, len(err.Error()), len(huge),
			"a blocked-request error must not carry the untrusted URL at full length")
	}
}

// TestIsReservedAddr_NonPublicFormsTheV4NormalizationMisses covers the
// shapes net.IP's own predicates leave uncovered. Each was verified to
// return false — i.e. "public, go ahead and dial" — before the fix.
//
// Reachability is deliberately NOT the criterion here. On the FF1 today
// IPv6 is disabled entirely and 0.0.0.x times out rather than reaching
// loopback, so none of these is a live bypass on that hardware. They are
// rejected because the guard's contract is "dial only clearly-public
// space", and a predicate that admits non-public addresses on the grounds
// that some current kernel happens not to route them is one config change
// away from being wrong.
func TestIsReservedAddr_NonPublicFormsTheV4NormalizationMisses(t *testing.T) {
	tests := []struct {
		name string
		ip   string
		want bool
		why  string
	}{
		{"ipv4-compatible loopback", "::127.0.0.1", true,
			"To4 normalizes only ::ffff: mapped form, so this reached the IPv6 branch where IsLoopback compares against ::1"},
		{"ipv4-compatible loopback, hex form", "::7f00:1", true,
			"same address, written the way a scanner would emit it"},
		{"ipv4-compatible private", "::192.168.1.1", true,
			"unwrapping must re-apply the whole v4 policy, not just loopback"},
		{"this-network low", "0.0.0.1", true,
			"IsUnspecified covers only 0.0.0.0; the rest of 0/8 is non-public"},
		{"this-network high", "0.255.255.255", true, "upper end of 0/8"},
		{"ipv6 site-local", "fec0::1", true, "deprecated RFC 3879 LAN scope; net has no predicate"},
		{"ipv6 site-local upper", "feff::1", true, "fec0::/10 runs to feff:"},

		// RFC 5737 / RFC 3849 / RFC 9637 documentation ranges. Not
		// globally routable, but lab and overlay networks route them
		// internally, which is exactly where this daemon runs.
		{"TEST-NET-1", "192.0.2.10", true, "RFC 5737 documentation range"},
		{"TEST-NET-2", "198.51.100.10", true, "RFC 5737 documentation range"},
		{"TEST-NET-3", "203.0.113.10", true, "RFC 5737 documentation range"},
		{"v6 documentation", "2001:db8::1", true, "RFC 3849 documentation range"},
		{"v6 documentation, new block", "3fff::1", true, "RFC 9637 documentation range"},
		{"6to4 relay anycast", "192.88.99.1", true,
			"deprecated RFC 7526 anycast; forwards to a v6 destination this guard never inspected"},

		// The rest of the IANA special-purpose space, refused by the
		// 2000::/3 allowlist rather than one branch each.
		{"v6 discard prefix", "100::1", true, "RFC 6666, outside 2000::/3"},
		{"local-use NAT64", "64:ff9b:1::1", true, "RFC 8215 local-use prefix, outside 2000::/3"},
		// THE discriminating NAT64 row. The one above passes for an
		// accidental reason — its low 32 bits are 0.0.0.1, which the v4
		// policy refuses however it is unwrapped — so it would still
		// pass with the shape check gone. This address translates to
		// 127.0.0.1 under a /64 NAT64 prefix (v4 in bytes 9-12) while
		// its LAST four bytes read 1.0.0.0, i.e. public: matching the
		// 64:ff9b prefix without verifying the /96 shape admits it.
		{"local-use NAT64 wrapping loopback", "64:ff9b:1:0:7f:0:100:0", true,
			"translates to 127.0.0.1 at /64; unwrapping the last 4 bytes would read 1.0.0.0 and admit it"},
		{"local-use NAT64 wrapping RFC 1918", "64:ff9b:1:0:c0:a801:100:0", true,
			"same shape, translating to 192.168.1.1"},
		{"SRv6 SID block", "5f00::1", true, "RFC 9602, outside 2000::/3"},
		{"teredo", "2001:0:4136:e378:8000:63bf:3fff:fdd2", true,
			"2001::/23 IETF protocol assignments; Teredo tunnels to an embedded v4 endpoint"},
		{"v6 benchmarking", "2001:2::1", true, "2001:2::/48, inside the 2001::/23 carve-out"},

		// Guardrails: neither the unwrap nor the allowlist may over-reject.
		{"unspecified v6", "::", true, "already covered by IsUnspecified, must stay covered"},
		{"loopback v6", "::1", true, "must not be unwrapped to 0.0.0.1 and re-judged"},
		{"public v4", "8.8.8.8", false, "must stay dialable"},
		{"public v6", "2001:4860:4860::8888", false, "must stay dialable"},
		{"public v6 in APNIC space", "2001:200::1", false,
			"the 2001::/23 carve-out must stop at 2001:01ff — 2001:0200::/23 is a live RIR allocation"},
		{"public v6, ARIN", "2600::1", false, "live RIR allocation"},
		{"public v6, RIPE", "2a00::1", false, "live RIR allocation"},
		{"public v6, Cloudflare", "2606:4700::1111", false, "a real artwork-CDN neighbor"},
		{"public v6, AFRINIC", "2c0f::1", false, "live RIR allocation (2c00::/12)"},
		{"unallocated inside 2000::/3", "3000::1", false,
			"the allowlist admits unallocated space inside 2000::/3 by design; only the named " +
				"special-purpose blocks are carved out"},
		{"6bone, returned to IANA", "3ffe::1", false,
			"discriminates the 3fff::/20 carve-out from a 3ff0::/12 one: a /20 fixes bytes 0-1 plus " +
				"the top nibble of byte 2, so 3ffe:: is outside it and must stay admitted like any " +
				"other unallocated slice"},
		{"AS112 delegation", "192.175.48.1", false,
			"listed globally reachable in the IANA registry; blocking it would be a functional regression"},
		{"public v4 near TEST-NET-2", "198.51.101.1", false, "the /24 carve-out must not widen to a /16"},
		{"public v4 near TEST-NET-3", "203.0.114.1", false, "the /24 carve-out must not widen to a /16"},
		{"NAT64-wrapped public v4", "64:ff9b::8.8.8.8", false,
			"a v6-only deployment reaches public origins through NAT64; unwrap must run before the allowlist"},
		{"fe00::1 is not site-local, but is still not public", "fe00::1", true,
			"outside 2000::/3. This row asserted false until the v6 policy became an allowlist: it was " +
				"correct that fe00::/9 escapes the fec0::/10 mask, and wrong that escaping it makes an " +
				"unallocated address dialable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ip := net.ParseIP(tt.ip)
			require.NotNil(t, ip, "test bug: %q did not parse", tt.ip)
			require.Equal(t, tt.want, isReservedAddr(ip), tt.why)
		})
	}
}

// TestCheckSourceLength_BoundsDialableSourcesOnly pins the admission
// bound and, just as importantly, its two exemptions — a cap that also
// hit data: URIs would break inline items outright.
func TestCheckSourceLength_BoundsDialableSourcesOnly(t *testing.T) {
	long := "https://example.com/" + strings.Repeat("a", MaxSourceURLBytes)
	require.ErrorIs(t, checkSourceLength(long), ErrSourceTooLong)

	// The error must not quote the URL: a check that exists to stop
	// oversized strings propagating cannot itself propagate one.
	require.NotContains(t, checkSourceLength(long).Error(), strings.Repeat("a", 64))

	require.NoError(t, checkSourceLength("https://example.com/ok"))
	require.NoError(t, checkSourceLength("https://example.com/"+strings.Repeat("a", MaxSourceURLBytes-21)))
	require.NoError(t, checkSourceLength("data:text/html;base64,"+strings.Repeat("A", 5*MaxSourceURLBytes)),
		"inline bytes ARE the source and are never dialed; capping them would break inline items")
	require.NoError(t, checkSourceLength("DATA:text/html,"+strings.Repeat("x", 5*MaxSourceURLBytes)),
		"scheme comparison must be case-insensitive")
}

// TestSourceGuard_RedirectStripsReferer pins the redirect hook's Referer
// removal: Go's default redirect handling forwards the previous hop's full
// URL — query string (and any signed-URL credential in it) included — as
// Referer to the next origin. These hops are between origins a hostile
// playlist chose, and nothing on this transport needs a Referer.
func TestSourceGuard_RedirectStripsReferer(t *testing.T) {
	var sawReferer string
	sawRefererSet := false
	dest := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		sawReferer = r.Header.Get("Referer")
		sawRefererSet = true
		w.WriteHeader(go_http.StatusOK)
	}))
	defer dest.Close()
	origin := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		w.Header().Set("Location", dest.URL)
		w.WriteHeader(go_http.StatusFound)
	}))
	defer origin.Close()

	guard := sourceGuard{isReserved: loopbackIsPublic}
	client := newGuardedHTTPClientFor(guard, 0)

	req, err := client.NewRequest(go_http.MethodGet, origin.URL+"/signed?token=secret", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	_ = resp.Body.Close()

	require.True(t, sawRefererSet, "the redirect destination must have been reached")
	assert.Empty(t, sawReferer,
		"the signed previous-hop URL must not ride Referer to the next origin")
}
