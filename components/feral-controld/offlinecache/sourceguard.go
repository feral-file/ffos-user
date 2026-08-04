package offlinecache

import (
	"context"
	"errors"
	"fmt"
	"net"
	go_http "net/http"
	go_url "net/url"
	"strings"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// ErrUnsafeSource is returned by Classify when a playlist item's source
// URL points somewhere this daemon must never dial on a playlist's
// behalf.
//
// Threat model. A playlist body reaches this service from the LAN hub —
// which binds 0.0.0.0:1111 and is unauthenticated (see the component
// AGENTS.md) — and from the relayer. Every source URL inside it is
// therefore untrusted input, and three separate paths in this package
// dial it:
//
//   - classify.go's HEAD / ranged-GET probe,
//   - mediacapture.go's direct body download,
//   - capture.go's Page.navigate in the headless browser.
//
// The device runs privileged, unauthenticated services the loopback
// interface would happily hand to any of those: Chromium's DevTools
// endpoints on 127.0.0.1:9222 and :9223 (which can open tabs, read any
// page, and drive the kiosk), the hub itself on :1111, sys-monitord on
// :9001, and the blob static server. Without this guard a source of
// "file:///etc/shadow" is local file disclosure through the headless
// browser, and "http://127.0.0.1:9222/json/new?..." is full control of
// the kiosk browser — both reachable by anyone who can put a playlist
// on the wire.
//
// Guarding inside Classify is what makes that reachable-once: Classify
// is the single function both enqueue paths (DownloadItem and
// DownloadPlaylist) call before any I/O and before any job is queued, so
// a rejected source is never probed, never downloaded, and never
// navigated to.
//
// A pre-flight check on the source URL is NOT sufficient on its own, for
// two reasons that need no hostile DNS at all:
//
//   - Redirects. http.Client follows up to ten hops by default, and only
//     the FIRST URL ever reached this check. A source that passes and
//     then 302s to http://127.0.0.1:1111/ walked straight to the
//     unauthenticated hub.
//   - Rebinding. This check resolves a name and then throws the
//     addresses away; the real dial resolves again, so an answer that
//     flips to a reserved address in between was never re-examined.
//
// Both are closed by enforcing the SAME policy at the socket instead of
// at the URL: newGuardedHTTPClient puts addrsFor in the transport's
// DialContext, which every hop and every re-resolution must pass
// through, and then dials the exact address it just validated so no
// second lookup can substitute another. checkRedirect adds the one thing
// a dialer cannot see — the scheme — plus a hop cap. This check remains
// in front of all of it because it is what keeps a bad source from ever
// being queued, and because it produces the error the caller reports.
//
// Residual gap, stated plainly rather than papered over: capture.go's
// headless browser is NOT covered. Chromium resolves and dials on its
// own, and the page it navigates is untrusted artwork that may request
// arbitrary subresources, so an artwork can still reach a loopback
// service from inside the browser. Closing that needs request-level
// interception on the capture CDP session (the machinery cdpsession.go
// already uses for replay), not a flag; it is deliberately a separate
// change and is tracked in docs/offline-artwork-capture.md.
var ErrUnsafeSource = errors.New("offline cache: source URL is not permitted")

// AddrResolver is the DNS seam sourceGuard needs, owned here (rather
// than taken from wrapper) because it is the only lookup this package
// performs and tests must be able to return reserved addresses for a
// name without touching real DNS. net.DefaultResolver satisfies it.
type AddrResolver interface {
	LookupIPAddr(ctx context.Context, host string) ([]net.IPAddr, error)
}

// sourceGuard decides whether an untrusted source URL may be dialed.
type sourceGuard struct {
	resolver AddrResolver
	// isReserved overrides the address policy. It exists for ONE reason:
	// a faithful public->private redirect test needs two httptest
	// servers, and every httptest server is on loopback, which the real
	// predicate rejects at both ends — so the scenario the guard exists
	// to stop could not otherwise be exercised end to end. nil means
	// isReservedAddr, and nothing outside tests ever sets it.
	isReserved func(net.IP) bool
}

// reserved applies the address policy, honoring the test override.
func (g sourceGuard) reserved(ip net.IP) bool {
	if g.isReserved != nil {
		return g.isReserved(ip)
	}
	return isReservedAddr(ip)
}

// check reports whether rawURL is safe for this daemon to dial.
//
// It deliberately does NOT accept data: URIs: those carry their own
// bytes and are never dialed at all, so Classify short-circuits them
// before reaching this guard. Letting them through here too would be a
// second, silent way for a non-http scheme to pass.
func (g sourceGuard) check(ctx context.Context, rawURL string) error {
	u, err := go_url.Parse(rawURL)
	if err != nil {
		// Both wrapped: callers branch on ErrUnsafeSource, and the parse
		// error still carries why the URL was rejected.
		return fmt.Errorf("%w: unparseable URL: %w", ErrUnsafeSource, err)
	}

	// Scheme allowlist, not a denylist: the set of schemes a browser or
	// http.Client will act on (file, ftp, gopher, ws, chrome, devtools,
	// about, blob, javascript, ...) is long and grows, so anything not
	// explicitly http(s) is refused.
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q is not http or https", ErrUnsafeSource, u.Scheme)
	}

	// Hostname() strips any userinfo and port, so the credential-prefix
	// trick ("http://cdn.example.com@127.0.0.1/") is resolved to the
	// real target host here rather than being read off the raw string.
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("%w: URL has no host", ErrUnsafeSource)
	}

	_, err = g.addrsFor(ctx, host)
	return err
}

// addrsFor resolves host and returns its addresses only if EVERY one of
// them is outside the reserved ranges. Shared deliberately by check (URL
// time) and dialContext (socket time) so the two can never drift into
// disagreeing about what "reserved" means.
//
// Returning the addresses, rather than just a verdict, is what lets
// dialContext connect to the exact ones it validated — the alternative,
// handing the hostname back to the dialer, would re-resolve and reopen
// the very window this closes.
func (g sourceGuard) addrsFor(ctx context.Context, host string) ([]net.IP, error) {
	// A literal address needs no DNS round trip — and must not get one,
	// since LookupIPAddr on a literal simply echoes it back.
	if ip := net.ParseIP(host); ip != nil {
		if g.reserved(ip) {
			return nil, fmt.Errorf("%w: address %s is in a reserved range", ErrUnsafeSource, ip)
		}
		return []net.IP{ip}, nil
	}

	addrs, err := g.resolver.LookupIPAddr(ctx, host)
	if err != nil {
		// NOT tagged ErrUnsafeSource: a lookup failure is a transient
		// network fault, and reporting it as a security rejection would
		// make an offline device look like it is under attack.
		return nil, fmt.Errorf("offline cache: resolve source host %s: %w", host, err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("offline cache: source host %s resolved to no addresses", host)
	}
	// EVERY answer must be safe, not merely the first: a name that
	// returns one public and one loopback address would otherwise be
	// admitted here and then dialed round-robin at fetch time.
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if g.reserved(addr.IP) {
			return nil, fmt.Errorf("%w: host %s resolves to reserved address %s", ErrUnsafeSource, host, addr.IP)
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

// maxSourceRedirects caps redirect hops on a source fetch. net/http's own
// default is also ten; restating it makes the limit ours to reason about
// and lets the refusal carry ErrUnsafeSource like every other one here.
const maxSourceRedirects = 10

// dialContext is the transport hook that makes this guard real. Every
// connection the client opens — the first one, each redirect hop, and
// each re-resolution of a name whose answer changed — arrives here, and
// nothing is dialed until addrsFor has cleared it.
func (g sourceGuard) dialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	ips, err := g.addrsFor(ctx, host)
	if err != nil {
		return nil, err
	}
	// Dial the validated addresses themselves, never the hostname: TLS
	// still verifies against the original host from the URL, so pinning
	// the address costs nothing and removes the second lookup a hostile
	// resolver would answer differently.
	var dialer net.Dialer
	var lastErr error
	for _, ip := range ips {
		conn, dialErr := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
		if dialErr == nil {
			return conn, nil
		}
		lastErr = dialErr
	}
	return nil, lastErr
}

// checkRedirect covers the one thing dialContext cannot: by the time a
// connection is dialed the URL is gone, so a redirect to a non-http(s)
// scheme has to be refused here.
func (g sourceGuard) checkRedirect(req *go_http.Request, via []*go_http.Request) error {
	if len(via) >= maxSourceRedirects {
		return fmt.Errorf("%w: stopped after %d redirects", ErrUnsafeSource, len(via))
	}
	switch strings.ToLower(req.URL.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("%w: redirect to scheme %q is not http or https", ErrUnsafeSource, req.URL.Scheme)
	}
}

// newGuardedHTTPClient builds the client every untrusted-source fetch
// must use — classify.go's probe and mediacapture.go's body download.
// timeout <= 0 leaves http.Client.Timeout unset, which the body download
// requires (see bootstrap.go's bodyClient comment: a whole-request
// timeout kills large artwork transfers).
//
// The transport mirrors http.DefaultTransport apart from DialContext, so
// the only behavior this changes is where connections are allowed to go.
func newGuardedHTTPClient(resolver AddrResolver, timeout time.Duration) wrapper.HTTPClient {
	return newGuardedHTTPClientFor(sourceGuard{resolver: resolver}, timeout)
}

// newGuardedHTTPClientFor is newGuardedHTTPClient with the guard supplied
// directly, so a test can hand in one carrying the isReserved override
// its own httptest servers need.
func newGuardedHTTPClientFor(g sourceGuard, timeout time.Duration) wrapper.HTTPClient {
	return wrapper.NewHTTPClientFrom(&go_http.Client{
		Timeout:       timeout,
		CheckRedirect: g.checkRedirect,
		Transport: &go_http.Transport{
			Proxy:                 go_http.ProxyFromEnvironment,
			DialContext:           g.dialContext,
			ForceAttemptHTTP2:     true,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	})
}

// isReservedAddr reports whether ip is somewhere a playlist source must
// never point. It covers the ranges that reach either this device's own
// services or the LAN around it — the two things an SSRF through a
// playlist would be aimed at.
//
// The IPv4-mapped IPv6 forms (::ffff:127.0.0.1 and friends) are covered
// without a special case: To4 normalizes them to their v4 shape first,
// so they take exactly the same branches as the literal v4 address.
func isReservedAddr(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		ip = v4
	}

	// net's own predicates cover loopback (127/8, ::1), RFC 1918 and
	// IPv6 ULA (fc00::/7), link-local (169.254/16, fe80::/10), every
	// multicast scope, and the unspecified address (0.0.0.0, ::).
	if ip.IsUnspecified() || ip.IsLoopback() || ip.IsPrivate() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() ||
		ip.IsInterfaceLocalMulticast() || ip.IsMulticast() {
		return true
	}

	if v4 := ip.To4(); v4 != nil {
		switch {
		case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
			return true // 100.64.0.0/10 CGNAT — carrier-side, not a public origin
		case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
			return true // 192.0.0.0/24 IETF protocol assignments
		case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
			return true // 198.18.0.0/15 benchmarking
		case v4[0] >= 240:
			return true // 240.0.0.0/4 reserved, and 255.255.255.255 broadcast
		}
		return false
	}

	// IPv6 forms that wrap a v4 address the checks above would have
	// caught in its native shape: NAT64 (64:ff9b::/96) and 6to4
	// (2002::/16) both embed one. Unwrap and re-test rather than
	// trusting the outer prefix to look public.
	if embedded := embeddedIPv4(ip); embedded != nil {
		return isReservedAddr(embedded)
	}
	return false
}

// embeddedIPv4 extracts the IPv4 address carried inside a NAT64 or 6to4
// IPv6 address, or nil when ip is neither.
func embeddedIPv4(ip net.IP) net.IP {
	if len(ip) != net.IPv6len {
		return nil
	}
	// NAT64 well-known prefix 64:ff9b::/96 — v4 sits in the last 4 bytes.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b {
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// 6to4 2002::/16 — v4 sits in bytes 2..5.
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	}
	return nil
}
