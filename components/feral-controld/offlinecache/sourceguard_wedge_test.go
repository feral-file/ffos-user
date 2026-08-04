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
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
