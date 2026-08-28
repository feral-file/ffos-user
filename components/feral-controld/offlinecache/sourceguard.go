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
// therefore untrusted input, and four separate paths in this package
// dial it:
//
//   - classify.go's HEAD / ranged-GET probe,
//   - probe.go's cast-time liveness preflight (#304),
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
// second lookup can substitute another. That guarantee also requires the
// transport to have NO proxy — see transport() — because a proxied
// request dials the proxy and lets IT reach the origin, which would put
// the destination back out of reach of the check. checkRedirect adds the
// one thing a dialer cannot see — the scheme — plus a hop cap. This check
// remains in front of all of it because it is what keeps a bad source
// from ever being queued, and because it produces the error the caller
// reports.
//
// capture.go's headless browser IS covered, by a different mechanism:
// Chromium resolves and dials on its own, so no Go transport can reach
// it. Instead the capture CDP session enables Fetch and answers every
// paused request against this same address policy, extended to
// auto-attached child targets (OOPIFs and workers) because those run in
// their own targets whose requests never reach the root handler. See
// capturer.attachSourceGuard and enableGuardedAutoAttach.
//
// Two residuals remain there, and they are an ACCEPTED, recorded decision
// rather than an oversight — see the accepted-risk record in
// docs/offline-artwork-capture.md §9 before re-reporting either:
//
//   - DNS rebinding. The capture-side check is URL-time; Chromium
//     resolves the host itself after the request is continued and may get
//     a different answer than we did.
//   - WebSocket. CDP Fetch does not intercept ws:// handshakes at all.
//
// Both need Chromium's egress removed entirely (a loopback filtering
// proxy it must dial through) rather than more interception, and both
// are sequenced with #3471, which closes the unauthenticated :1111
// surface these paths lead to.
// NOTE FOR ANY NEW ERROR ON THIS PATH: every untrusted string embedded in
// a guard error MUST go through truncateSourceForLog. These errors are
// logged — by Classify's caller and by the capture-side blocker, both
// alongside an already-truncated URL — so an untruncated host, scheme or
// wrapped parse error re-opens exactly the unbounded-log hole that
// truncation closes. url.Parse accepts a 100,000-character hostname
// without complaint, so "a hostname is short" is not a bound.
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
		return fmt.Errorf("%w: unparseable URL: %s", ErrUnsafeSource, truncateSourceForLog(err.Error()))
	}

	// Scheme allowlist, not a denylist: the set of schemes a browser or
	// http.Client will act on (file, ftp, gopher, ws, chrome, devtools,
	// about, blob, javascript, ...) is long and grows, so anything not
	// explicitly http(s) is refused.
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("%w: scheme %q is not http or https", ErrUnsafeSource, truncateSourceForLog(u.Scheme))
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
		return nil, fmt.Errorf("offline cache: resolve source host %s: %w", truncateSourceForLog(host), err)
	}
	if len(addrs) == 0 {
		return nil, fmt.Errorf("offline cache: source host %s resolved to no addresses", truncateSourceForLog(host))
	}
	// EVERY answer must be safe, not merely the first: a name that
	// returns one public and one loopback address would otherwise be
	// admitted here and then dialed round-robin at fetch time.
	ips := make([]net.IP, 0, len(addrs))
	for _, addr := range addrs {
		if g.reserved(addr.IP) {
			return nil, fmt.Errorf("%w: host %s resolves to reserved address %s", ErrUnsafeSource, truncateSourceForLog(host), addr.IP)
		}
		ips = append(ips, addr.IP)
	}
	return ips, nil
}

// maxSourceRedirects caps redirect hops on a source fetch. net/http's own
// default is also ten; restating it makes the limit ours to reason about
// and lets the refusal carry ErrUnsafeSource like every other one here.
const maxSourceRedirects = 10

// MaxSourceURLBytes bounds a playlist ITEM's dialable source URL.
//
// The length matters because an item source is retained and re-emitted,
// not merely dialed once: it is the cache key, it is held in sourceByKey
// and in every queued job, it is echoed in offline_cache_status, and
// service.process puts the capture error — which carries it — into a
// notify() Coverage.Reason that goes out over the relayer WebSocket. The
// hub accepts a 4 MiB request unauthenticated, so without a bound one
// hostile playlist turns into multi-megabyte state and repeated
// multi-megabyte notifications on a constrained device.
//
// Bounding HERE, at admission, rather than sanitizing at each log and
// wire site is deliberate. Every path downstream — classify's *url.Error
// (which quotes the whole URL), mediacapture's raw %s interpolation, the
// notification payload, the status response — becomes bounded by
// construction, and none of them has to remember to do anything.
// truncateSourceForLog stays as defense in depth for the paths that
// predate this and for sources already on disk.
//
// 2048 is the de-facto ceiling browsers, proxies and servers converged
// on. Two things are exempt:
//
//   - data: URIs. An inline item's bytes ARE its source, legitimately
//     large, and it is never dialed.
//   - subresource URLs during capture. Signed CDN links routinely run
//     long, and capping them would fail real artwork rather than protect
//     anything. Note this is NOT because they are transient — an earlier
//     version of this comment claimed that and was wrong: captured
//     subresource URLs are persisted in ItemRecord.Resources and logged
//     at replay. The actual reason is narrower: they are a hostile
//     artwork's own subresources, practically bounded by what a real
//     origin will serve, and they are never a lookup key a client can
//     reflect through an RPC.
const MaxSourceURLBytes = 2048

// ErrSourceTooLong is returned for an item source past MaxSourceURLBytes.
// Distinct from ErrUnsafeSource because the destination may be perfectly
// safe — the objection is what holding the string costs, not where it
// points — and a caller may want to report the two differently.
var ErrSourceTooLong = errors.New("offline cache: item source URL is too long")

// isInlineSource reports whether source carries its own bytes rather than
// naming somewhere to dial. Such a source is exempt from the length bound
// (see MaxSourceURLBytes) and is classified without any network access.
func isInlineSource(source string) bool {
	return strings.HasPrefix(strings.ToLower(source), "data:")
}

// CheckSourceKeyLength enforces MaxSourceURLBytes on a caller-supplied
// LOOKUP KEY — a source at the clear and status RPCs, or a playlistId at
// clearPlaylistCache — rather than on something to dial and store.
//
// Unlike checkSourceLength this has NO data: exemption, and the reason is
// specific rather than stylistic: an inline source can never be a cached
// key at all. Classify returns ClassInline for data:, DownloadItem then
// returns ErrItemInlineNotQueued, and nothing is ever enqueued or
// recorded — so a data: source at these boundaries can only ever answer
// not_found/not_cached, and bounding it changes no reachable outcome
// while closing the same reflection hole.
//
// This exists because the admission bound alone does not cover these
// paths: clear and status accept a key from the same unauthenticated hub
// without it ever passing through DownloadItem, and every one of them
// echoes it back — the clear handlers in their ok responses, status as a
// not_cached entry — so an unbounded value is reflected into a daemon
// response and, for status, up to MaxStatusSources times in one request.
// A playlistId additionally reaches the filesystem: an oversized one
// fails ReadFile with ENAMETOOLONG rather than IsNotExist, so the full
// submitted string comes back inside the error text.
func CheckSourceKeyLength(source string) error {
	if len(source) <= MaxSourceURLBytes {
		return nil
	}
	return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit",
		ErrSourceTooLong, len(source), MaxSourceURLBytes)
}

// checkSourceLength enforces MaxSourceURLBytes on a dialable item source.
// The error deliberately reports only the length, never the URL: quoting
// an oversized string in the error for a check whose entire purpose is to
// stop oversized strings propagating would defeat itself.
func checkSourceLength(source string) error {
	if isInlineSource(source) || len(source) <= MaxSourceURLBytes {
		return nil
	}
	return fmt.Errorf("%w: %d bytes exceeds the %d-byte limit",
		ErrSourceTooLong, len(source), MaxSourceURLBytes)
}

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
	// Go's redirect handling sets a Referer carrying the PREVIOUS hop's
	// full URL, query string included (refererForURL strips only
	// userinfo). A signed CDN source (?X-Amz-Signature=...) that 302s
	// cross-origin would hand its credential-bearing URL to the next
	// origin and its logs — something a browser's default
	// strict-origin-when-cross-origin policy would never do, and these
	// hops are between origins a hostile playlist chose. No fetch on
	// this transport needs a Referer, so it is dropped on every hop
	// rather than policy-matched.
	req.Header.Del("Referer")
	switch strings.ToLower(req.URL.Scheme) {
	case "http", "https":
		return nil
	default:
		return fmt.Errorf("%w: redirect to scheme %q is not http or https", ErrUnsafeSource, truncateSourceForLog(req.URL.Scheme))
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
		Transport:     g.transport(),
	})
}

// newGuardedNoRedirectHTTPClientFor is the guarded client with redirect
// FOLLOWING disabled outright: the first response — a 3xx included — is
// returned as the final answer, and no second request is ever made.
//
// Built for the cast preflight, whose request-count bound must be a hard
// one request per probed source: with following enabled, every probe
// could legally fan into maxSourceRedirects further requests, turning a
// budget of N into N x 10 against attacker-chosen origins (#310 review).
// Not following also removes the probe's redirect-time SSRF surface
// entirely — there is no hop for checkRedirect to police and no
// cross-origin Referer to strip, though the dial-time guard still vets
// the FIRST request's destination. Callers treat a 3xx answer as alive
// (fail-open): the origin answered and delegates elsewhere; the kiosk's
// own fetch follows it with the browser's policies.
func newGuardedNoRedirectHTTPClientFor(g sourceGuard, timeout time.Duration) wrapper.HTTPClient {
	return wrapper.NewHTTPClientFrom(&go_http.Client{
		Timeout: timeout,
		CheckRedirect: func(*go_http.Request, []*go_http.Request) error {
			return go_http.ErrUseLastResponse
		},
		Transport: g.transport(),
	})
}

// transport builds the guarded transport. Factored out so a test can
// assert the Proxy field directly: http.ProxyFromEnvironment caches the
// environment behind a sync.Once, which makes an env-driven test
// order-dependent, and this property is too load-bearing to pin only
// indirectly.
//
// Proxy is deliberately nil, NOT http.ProxyFromEnvironment (which
// http.DefaultTransport uses, and which an earlier version of this
// function copied along with the rest of the defaults). With a proxy
// configured, Transport dials the PROXY and hands it the origin host —
// so DialContext would validate the proxy's address while the proxy
// resolved and connected to whatever the URL named, and every guarantee
// below it would be worth nothing. The one thing this transport exists
// to guarantee is that the destination is checked, so it must reach the
// destination itself.
//
// Trade-off, accepted deliberately: a deployment that can only egress
// through a proxy cannot fetch artwork on these two paths. That is the
// right way round — artwork origins are public CDNs reached directly on
// this device, and the daemon-wide client (relayer, indexer, OTA) still
// honors proxy environment variables, so only untrusted playlist-source
// fetches go direct. Supporting a proxy safely would mean enforcing the
// destination through it (CONNECT-aware checking), not re-enabling this.
func (g sourceGuard) transport() *go_http.Transport {
	return &go_http.Transport{
		Proxy:                 nil,
		DialContext:           g.dialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// Every fetch on this transport dials an untrusted, possibly
		// hostile origin, and Go's default response-header ceiling is
		// 10 MiB PER RESPONSE — so a probe wave fanning out
		// concurrently (probeConcurrency-wide on the cast preflight)
		// against a server that streams enormous header blocks and then
		// stalls could hold hundreds of MiB resident on a constrained
		// device before timeouts fire. 1 MiB is orders of magnitude
		// above any legitimate header block (real-world limits sit at
		// 8-64 KiB) while capping the wave at the size of one playlist
		// body.
		MaxResponseHeaderBytes: 1 << 20,
	}
}

// isReservedAddr reports whether ip is somewhere a playlist source must
// never point. The contract it enforces is "dial only clearly-public
// space" — NOT "block the ranges that happen to be reachable on this
// hardware today". Those differ, and the difference matters: IPv6 is
// disabled on the FF1 right now and 0.0.0.x times out rather than
// reaching loopback, so several of the forms below are not live bypasses
// on that device. They are refused anyway, because a predicate that
// admits non-public addresses on the grounds that some current kernel
// does not route them is one config change away from being wrong, and
// because lab and overlay networks routinely DO route the special-use
// ranges internally.
//
// The two families get opposite treatments, deliberately:
//
//   - IPv4 is a denylist, because public v4 is fragmented across the
//     whole space with no single prefix to allow. It enumerates the IANA
//     Special-Purpose Address Registry entries that are not globally
//     reachable; see isReservedIPv4.
//   - IPv6 is an ALLOWLIST (see isReservedIPv6), because there the
//     public space IS one prefix. That inversion is what stops this
//     predicate from needing a new branch every time someone notices
//     another special-use v6 block.
//
// The IPv4-mapped IPv6 form (::ffff:127.0.0.1 and friends) needs no
// special case: To4 normalizes it to its v4 shape, so it takes exactly
// the same branches as the literal v4 address.
func isReservedAddr(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if v4 := ip.To4(); v4 != nil {
		return isReservedIPv4(v4)
	}
	if len(ip) != net.IPv6len {
		// Neither a v4 nor a well-formed v6 address. Unreachable via
		// net.ParseIP, but a hand-built net.IP can be any length, and
		// the safe answer to "what is this" is never "dial it".
		return true
	}

	// IPv6 forms that WRAP a v4 address must be unwrapped and judged by
	// the v4 policy before the allowlist below sees them, in both
	// directions. NAT64 (64:ff9b::/96) sits outside the global-unicast
	// prefix, so without this a v6-only deployment reaching a perfectly
	// public origin through its NAT64 gateway would be refused; 6to4
	// (2002::/16) sits inside it, so without this a 6to4-wrapped
	// loopback address would be admitted.
	if embedded := embeddedIPv4(ip); embedded != nil {
		return isReservedIPv4(embedded)
	}
	if compat := ipv4Compatible(ip); compat != nil {
		return isReservedIPv4(compat)
	}
	return isReservedIPv6(ip)
}

// isReservedIPv4 applies the v4 policy. ip may be in either 4-byte or
// 16-byte form; it is normalized here so callers that unwrap an embedded
// address (which net.IPv4 returns in 16-byte form) need not think about
// it.
//
// The switch enumerates the IANA IPv4 Special-Purpose Address Registry
// rows whose "Globally Reachable" column is False, minus the ones net's
// own predicates already cover. Rows marked globally reachable — the
// AS112 delegations (192.31.196.0/24, 192.175.48.0/24) and AMT
// (192.52.193.0/24) — are deliberately NOT listed: they are real public
// destinations, and blocking them would be a functional regression with
// no security gain.
func isReservedIPv4(ip net.IP) bool {
	v4 := ip.To4()
	if v4 == nil {
		return true
	}

	// net's own predicates cover loopback (127/8), RFC 1918, link-local
	// (169.254/16), every multicast scope (224/4), and 0.0.0.0.
	if v4.IsUnspecified() || v4.IsLoopback() || v4.IsPrivate() ||
		v4.IsLinkLocalUnicast() || v4.IsLinkLocalMulticast() ||
		v4.IsInterfaceLocalMulticast() || v4.IsMulticast() {
		return true
	}

	switch {
	case v4[0] == 0:
		// 0.0.0.0/8 "this network" (RFC 1122). IsUnspecified covers
		// only 0.0.0.0 itself; the rest of the /8 is non-public space
		// whose handling varies by stack, and a guard that admits only
		// clearly-public destinations has no reason to reason about
		// which of those stacks routes it where.
		return true
	case v4[0] == 100 && v4[1] >= 64 && v4[1] <= 127:
		return true // 100.64.0.0/10 CGNAT — carrier-side, not a public origin
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 0:
		return true // 192.0.0.0/24 IETF protocol assignments
	case v4[0] == 192 && v4[1] == 0 && v4[2] == 2:
		return true // 192.0.2.0/24 TEST-NET-1 (RFC 5737)
	case v4[0] == 192 && v4[1] == 88 && v4[2] == 99:
		// 192.88.99.0/24, the deprecated 6to4 relay anycast prefix
		// (RFC 7526). A packet sent here is forwarded by whichever
		// relay answers the anycast, to a v6 destination this guard
		// never got to inspect — a pivot, not an origin.
		return true
	case v4[0] == 198 && (v4[1] == 18 || v4[1] == 19):
		return true // 198.18.0.0/15 benchmarking
	case v4[0] == 198 && v4[1] == 51 && v4[2] == 100:
		return true // 198.51.100.0/24 TEST-NET-2 (RFC 5737)
	case v4[0] == 203 && v4[1] == 0 && v4[2] == 113:
		return true // 203.0.113.0/24 TEST-NET-3 (RFC 5737)
	case v4[0] >= 240:
		return true // 240.0.0.0/4 reserved, and 255.255.255.255 broadcast
	}
	return false
}

// isReservedIPv6 applies the v6 policy to an address that carries no
// embedded v4 (its callers unwrap those first).
//
// This is an ALLOWLIST, and that is the whole point. 2000::/3 is the
// only block IANA has ever allocated for global unicast, so every other
// v6 address — ULA, link-local, the deprecated site-local range,
// multicast, the discard prefix, local-use NAT64, the SRv6 SID block,
// and the vast unallocated remainder — is refused by default rather than
// by enumeration. Earlier revisions of this file listed the non-public
// v6 ranges one at a time and were repeatedly found to be missing
// another one; a denylist over a mostly-unallocated address space cannot
// be completed, so it was inverted instead.
//
// Trade-off, accepted: if IANA ever opens a second global-unicast block,
// artwork hosted there is refused until this predicate learns about it.
// That is a documented, one-line fix on a multi-year timeline, and it
// fails closed rather than open.
func isReservedIPv6(ip net.IP) bool {
	if ip[0]&0xe0 != 0x20 {
		return true
	}

	// Special-purpose blocks that sit INSIDE 2000::/3 and so have to be
	// carved back out by hand.
	switch {
	case ip[0] == 0x20 && ip[1] == 0x01 && ip[2]&0xfe == 0x00:
		// 2001::/23, the IETF protocol-assignments block: Teredo
		// (2001::/32, which tunnels to an embedded v4 endpoint),
		// benchmarking, AMT, ORCHIDv2. Note this stops at 2001:01ff —
		// the RIR allocations start at 2001:0200::/23 (APNIC) and are
		// untouched.
		//
		// Refusing the whole /23 is deliberately BROADER than the v4
		// policy above, which goes out of its way to admit the AS112
		// delegations because the registry marks them globally
		// reachable. The few globally-reachable rows in here — AS112-v6
		// (2001:4:112::/48), PCP anycast (2001:1::1), TURN anycast
		// (2001:1::2) — get no such exemption, because none of them is
		// ever an artwork origin and carving them back out would
		// reintroduce exactly the per-prefix enumeration this allowlist
		// exists to end.
		return true
	case ip[0] == 0x20 && ip[1] == 0x01 && ip[2] == 0x0d && ip[3] == 0xb8:
		return true // 2001:db8::/32 documentation (RFC 3849)
	case ip[0] == 0x3f && ip[1] == 0xff && ip[2]&0xf0 == 0x00:
		// 3fff::/20 documentation (RFC 9637). A /20 fixes bytes 0-1
		// plus the TOP NIBBLE of byte 2 — testing only ip[1]&0xf0
		// would be a /12, refusing all of 3ff0:: upward. That would
		// fail closed rather than open, but it also contradicts the
		// allowlist's own logic: every other unallocated slice of
		// 2000::/3 is admitted, so this one must not be special.
		return true
	}
	return false
}

// ipv4Compatible extracts the IPv4 address from a deprecated
// IPv4-compatible IPv6 address (::a.b.c.d, RFC 4291 2.5.5.1), or nil.
//
// This form needs its own unwrap because net.IP.To4 does NOT normalize
// it — To4 recognizes only the IPv4-MAPPED shape (::ffff:a.b.c.d) — so
// ::127.0.0.1 otherwise reaches the IPv6 branches, where IsLoopback
// compares against ::1 and does not match. It would then be judged
// public. Verified: before this, isReservedAddr("::127.0.0.1") was false.
//
// :: and ::1 are excluded so they are judged as the v6 addresses they
// are: unwrapping ::1 would otherwise yield 0.0.0.1 and re-enter the v4
// policy, which is a confusing way to reach the same verdict. Both are
// outside 2000::/3, so isReservedIPv6 refuses them.
func ipv4Compatible(ip net.IP) net.IP {
	if len(ip) != net.IPv6len || ip.To4() != nil {
		return nil
	}
	for _, b := range ip[:12] {
		if b != 0 {
			return nil
		}
	}
	// ::0 and ::1 are handled by their own predicates, not here.
	if ip[12] == 0 && ip[13] == 0 && ip[14] == 0 && ip[15] <= 1 {
		return nil
	}
	return net.IPv4(ip[12], ip[13], ip[14], ip[15])
}

// embeddedIPv4 extracts the IPv4 address carried inside a NAT64 or 6to4
// IPv6 address, or nil when ip is neither.
func embeddedIPv4(ip net.IP) net.IP {
	if len(ip) != net.IPv6len {
		return nil
	}
	// NAT64 well-known prefix 64:ff9b::/96 — v4 sits in the last 4 bytes.
	//
	// The /96 SHAPE must be verified, not just the 64:ff9b prefix.
	// RFC 6052 §3.1 permits the well-known prefix only at /96, but
	// 64:ff9b:1::/48 (RFC 8215, local-use NAT64) shares those first two
	// octets and embeds its v4 somewhere else entirely — at /64 the
	// address lands in bytes 9-12. Matching on the prefix alone would
	// therefore read the WRONG four bytes and judge them public:
	// 64:ff9b:1:0:7f:0:100:0 translates to 127.0.0.1 but unwraps here
	// as 1.0.0.0. Requiring bytes 4-11 to be zero costs nothing the RFC
	// allows and drops every other 64:ff9b:* form through to
	// isReservedIPv6, which refuses it for being outside 2000::/3.
	if ip[0] == 0x00 && ip[1] == 0x64 && ip[2] == 0xff && ip[3] == 0x9b {
		for _, b := range ip[4:12] {
			if b != 0 {
				return nil
			}
		}
		return net.IPv4(ip[12], ip[13], ip[14], ip[15])
	}
	// 6to4 2002::/16 — v4 sits in bytes 2..5.
	if ip[0] == 0x20 && ip[1] == 0x02 {
		return net.IPv4(ip[2], ip[3], ip[4], ip[5])
	}
	return nil
}
