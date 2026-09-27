// Package uarewrite decides which kiosk requests must have their outgoing
// User-Agent replaced before they leave the device.
//
// Why this exists: some artwork origins sit behind a bot-mitigation layer
// that challenges any request presenting a BROWSER User-Agent. The
// challenge is an HTML page, not the artwork, and it is served with
// `Cross-Origin-Resource-Policy: same-origin`, so Chromium rejects it
// outright (`ERR_BLOCKED_BY_RESPONSE.NotSameOrigin`) before ff-player ever
// sees bytes. A challenge can only be solved by executing its JavaScript in
// a top-level document; an <img>/fetch() subresource has nowhere to run it,
// so the kiosk can never obtain the clearance cookie and the item fails
// forever, retrying on every playlist tick. Measured on `ipfs.io`
// (feral-file/ffos-user#296): identical request, browser UA -> 403 with
// `cf-mitigated: challenge`, any non-browser UA -> 200 with the image.
//
// The rewrite is deliberately NOT applied to every request. Some origins do
// the opposite of ipfs.io and reject UNRECOGNIZED agents, so a blanket
// rewrite would trade this bug for a harder one: artworks that render today
// would start failing, and the cause would be invisible. Scope is therefore
// an explicit, operator-editable host list — new hostile gateways are a
// config edit, not a release.
//
// A second rule rides the same host list. In September 2026 the same two
// gateways finished retiring: `ipfs.io` and `dweb.link` now answer a native
// fetch with 429 and the body "This IPFS gateway is switching to a service
// worker gateway only", and a browser User-Agent with 403 — content is only
// reachable through their in-browser service-worker page, which an <img>,
// <video>, or iframe navigation can never run. No header can fix that, so
// for a matching host whose path is a content-addressed `/ipfs/<cid>...` the
// policy instead hands Chromium a REPLACEMENT URL on a working gateway (see
// RewriteURL). The DP-1 item source stays byte-for-byte what the app cast:
// `Fetch.continueRequest`'s `url` override is invisible to the page, so
// ff-player, the offline-cache identity (`sha256(item.source)`), and every
// log still see the original URL. Anything on a matching host that is NOT a
// CID path keeps the User-Agent rule above.
//
// This package is intentionally pure: it holds no CDP connection, performs
// no I/O, and knows nothing about how interception is armed. That keeps the
// policy independently testable and lets whichever component owns kiosk
// `Fetch` interception consume it without inheriting a dependency on this
// one's transport.
package uarewrite

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"
)

// DefaultUserAgent is the token sent in place of Chromium's own.
//
// It does NOT impersonate another browser, and must not be "fixed" into one.
// Two reasons: impersonation is what the mitigation layer is looking for, so
// a fake browser UA is the one value guaranteed to be challenged again; and
// an honest token keeps the device identifiable in origin logs, which is the
// behavior an operator would expect from a device fetching art on their
// behalf. Verified sufficient on device: `feral-player/2.0` returned 200
// where the Chromium UA returned 403.
const DefaultUserAgent = "feral-player/2.0"

// DefaultHosts are the origins known to challenge browser User-Agents.
//
// Both are IPFS gateways fronted by the same mitigation provider and both
// were observed returning 403 + `cf-mitigated: challenge` to a browser UA on
// 2026-08-19. This list is a STARTING POINT, not a closed set: it is
// overridable from config precisely because the next hostile gateway must
// not require a daemon release. Keep entries bare hosts — no scheme, no
// port, no path.
var DefaultHosts = []string{
	"ipfs.io",
	"dweb.link",
}

// DefaultReplacementGateway is where `/ipfs/<cid>` requests bound for a
// retired host are sent instead.
//
// Filebase is the FETCHING public gateway the indexer's own pool falls back
// to (ff-indexer-v2 `FF_INDEXER_URI_IPFS_GATEWAYS`), and it is the one
// verified to return the bytes to a native fetch on 2026-09-26 while ipfs.io
// returned 429. It is deliberately NOT `ipfs.feralfile.com`: that node runs
// `Gateway.NoFetch=true`, so it can only serve what Feral File itself pinned,
// and the retired-gateway URLs on a device are overwhelmingly external
// tokens (objkt, fxhash) that it does not hold (feral-file/feral-file#3500
// records that decision). Overridable from config for the same reason the
// host list is: the next gateway change must not need a release.
const DefaultReplacementGateway = "https://ipfs.filebase.io"

// minCIDLength is the shortest path segment RewriteURL treats as a CID.
// CIDv0 is exactly 46 base58 characters and a CIDv1 over a sha2-256 digest
// is longer in every multibase a gateway path uses, so anything shorter
// under `/ipfs/` is not content-addressed and is left alone — mirrors
// ff-player's `isContentAddressed` so the two agree on what a CID path is.
// A CIDv1 over an identity multihash can be shorter; those inline their
// bytes and are not what an artwork source carries, so they stay on the
// header rule rather than loosening the check for every API-ish path.
const minCIDLength = 46

// Policy answers "does this URL need its User-Agent replaced, and with
// what". It is immutable after construction and safe for concurrent use;
// callers on the CDP request path hold no lock.
type Policy struct {
	// hosts is the lower-cased match set. Membership is exact on the
	// URL's hostname — see Matches for why subdomains are excluded.
	hosts map[string]struct{}
	// userAgent is the replacement token. Never empty for a Policy
	// returned by New.
	userAgent string
	// replacement is the gateway RewriteURL sends CID paths to, reduced to
	// scheme and host. Never nil for a Policy returned by New.
	replacement *url.URL
}

// New builds a Policy from an operator-supplied host list and User-Agent.
//
// Empty hosts / userAgent fall back to the package defaults rather than
// producing a Policy that silently matches nothing: a half-configured block
// that disables the fix without saying so is the failure mode this
// package's whole reason for existing makes expensive to diagnose. To turn
// the behavior OFF, callers must not construct a Policy at all (config's
// Enabled flag), which is a decision visible at the wiring site.
//
// Entries are normalized: surrounding space trimmed, lower-cased, and an
// accidental scheme or port stripped, since "https://ipfs.io" and
// "ipfs.io:443" are the shapes an operator most plausibly writes by hand and
// silently ignoring them would look identical to the bug being fixed. An
// entry that still cannot yield a bare host after normalization is a
// configuration error and is reported, not dropped — see the returned error.
//
// replacementGateway is where CID paths on a matching host are sent (see
// RewriteURL); empty uses DefaultReplacementGateway. It must parse as an
// http(s) URL whose host passes the same literal-host check as hosts. It MAY
// itself be a listed host: an operator who had already listed
// `ipfs.filebase.io` for the User-Agent rule keeps that rule for it, and
// RewriteURL never redirects a request to the host it is already on, so the
// policy cannot loop. Rejecting that shape instead was tried and is worse —
// a config that was valid before this rule existed would have switched the
// whole interceptor off (main disables it on a construction error), taking
// the ipfs.io User-Agent handling down with it.
func New(hosts []string, userAgent string, replacementGateway string) (*Policy, error) {
	if len(hosts) == 0 {
		hosts = DefaultHosts
	}
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		ua = DefaultUserAgent
	}
	replacement, err := normalizeReplacementGateway(replacementGateway)
	if err != nil {
		return nil, fmt.Errorf("uarewrite: invalid replacement gateway %q: %w", replacementGateway, err)
	}

	set := make(map[string]struct{}, len(hosts))
	for _, raw := range hosts {
		host, err := normalizeHost(raw)
		if err != nil {
			return nil, fmt.Errorf("uarewrite: invalid host %q: %w", raw, err)
		}
		set[host] = struct{}{}
	}
	if len(set) == 0 {
		return nil, fmt.Errorf("uarewrite: host list resolved to no usable entries")
	}

	return &Policy{hosts: set, userAgent: ua, replacement: replacement}, nil
}

// NewFromOperatorHosts builds a Policy from a HAND-EDITED host list,
// salvaging the usable entries instead of failing the whole list on the
// first bad one. It returns the rejected entries so the caller can name them
// in a log; an empty result is not an error.
//
// This exists because New's all-or-nothing contract is wrong for the one
// input the design expects operators to edit. The config block's whole
// premise is that the next hostile gateway is a config edit, not a release,
// and validateLiteralHost's own doc names "*.ipfs.io" as the likely
// real-world spelling of that edit going wrong. Under New, appending one
// such entry to a working list took ipfs.io and dweb.link down with it —
// re-opening #296 on a headless device, with the cause visible only in a
// daemon log. Meanwhile an UNREADABLE config block falls back to the
// built-in defaults and keeps working (see config.GatewayUserAgentTuning).
// One operator error class must not have two opposite outcomes, and the
// more likely spelling must not take the worse path.
//
// Entries that survive are honored exactly as written: an operator who
// deliberately narrowed the scope keeps their narrowing. Only when NOTHING
// survives does this land on DefaultHosts, which is precisely where the
// unreadable-block path lands. Keep New strict — it is the pure contract the
// policy tests pin, and the tolerance belongs here where "what a host is" is
// already decided, not scattered across the wiring.
//
// The replacement gateway gets the same tolerance for the same reason: a
// mistyped gateway must not switch the CID rewrite off (which would put the
// retired gateway's 429 back on screen with only a log line to say why), so
// an unusable value is reported in the rejected list, spelled
// "replacementGateway=<raw>", and the built-in default is used instead. The
// host-list error path is unchanged: only an unusable BUILT-IN list can
// reach it, since a replacement that names a listed host is valid (see New).
func NewFromOperatorHosts(hosts []string, userAgent string, replacementGateway string) (*Policy, []string, error) {
	kept := make([]string, 0, len(hosts))
	var rejected []string
	for _, raw := range hosts {
		if _, err := normalizeHost(raw); err != nil {
			rejected = append(rejected, raw)
			continue
		}
		kept = append(kept, raw)
	}

	// New maps an empty list to DefaultHosts, so the all-rejected case needs
	// no special handling here. An error now means the BUILT-IN list is
	// unusable, which is a programming error, not operator input.
	policy, err := New(kept, userAgent, replacementGateway)
	if err != nil && strings.TrimSpace(replacementGateway) != "" {
		// Retry with the default before concluding the host list itself is
		// broken: the salvage contract is "one bad operator value drops
		// only that value", and the gateway is an operator value too.
		if retry, rerr := New(kept, userAgent, ""); rerr == nil {
			rejected = append(rejected, "replacementGateway="+replacementGateway)
			return retry, rejected, nil
		}
	}
	return policy, rejected, err
}

// UserAgent is the replacement token to send for a matching request.
func (p *Policy) UserAgent() string { return p.userAgent }

// Hosts returns the normalized match set, sorted for stable logging and
// test assertions. Callers must treat it as read-only.
func (p *Policy) Hosts() []string {
	out := make([]string, 0, len(p.hosts))
	for h := range p.hosts {
		out = append(out, h)
	}
	sort.Strings(out)
	return out
}

// ReplacementGateway is the origin CID paths are redirected to, as
// "scheme://host", for logging and test assertions.
func (p *Policy) ReplacementGateway() string {
	return p.replacement.Scheme + "://" + p.replacement.Host
}

// RewriteURL returns the URL a matching, content-addressed request should be
// sent to instead, and whether such a rewrite applies.
//
// It applies when the host Matches AND the path is `/ipfs/<cid>` with an
// optional remainder: that is the one shape whose bytes any gateway can
// serve, because the CID names the content rather than the host. The path
// and query are carried over byte-for-byte (a generative artwork's
// `?fxhash=...` parameters and ff-player's own `display_mode` hint live
// there); the fragment is dropped because it never leaves the client. The
// only thing that changes is scheme and authority.
//
// Everything else on a matching host — `/ipns/` names, API paths, the
// gateway's own static assets — is left for the User-Agent rule: those
// requests are not content-addressed, so another gateway is not guaranteed
// to answer them, and guessing would turn one failure shape into another.
// A retired-gateway `/ipns/` source therefore still fails today; that is an
// honest gap, not an oversight, and the indexer treats those the same way
// (ff-indexer-v2 `gateway_retired` is not migrated for IPNS either).
//
// The caller must consult this BEFORE RewriteHeaders and use exactly one of
// them: a redirected request keeps Chromium's own headers, because the
// replacement gateway is an ordinary origin we have no evidence of
// challenging browsers, and the whole reason the UA rule is scoped is that
// sending an unusual agent to an origin nobody measured is how artworks
// that render today start failing.
func (p *Policy) RewriteURL(rawURL string) (string, bool) {
	if !p.Matches(rawURL) {
		return "", false
	}
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", false
	}
	if !isCIDPath(u.EscapedPath()) {
		return "", false
	}
	if strings.EqualFold(u.Hostname(), p.replacement.Hostname()) {
		// Already on the replacement gateway (an operator listed it for
		// the User-Agent rule). Redirecting it to itself would be a no-op
		// at best; let the header rule handle it.
		return "", false
	}
	out := url.URL{
		Scheme:     p.replacement.Scheme,
		Host:       p.replacement.Host,
		Path:       u.Path,
		RawPath:    u.RawPath,
		RawQuery:   u.RawQuery,
		ForceQuery: u.ForceQuery,
	}
	return out.String(), true
}

// isCIDPath reports whether an escaped URL path is `/ipfs/<cid>` optionally
// followed by `/...`. The CID check is shape-only (length and alphabet),
// not a multihash decode: the goal is to refuse `/ipfs/` API-ish paths and
// obvious junk, not to validate content addressing, which the replacement
// gateway does for real. The alphabet is the union of the multibase
// encodings a path-form gateway URL carries: base58btc (CIDv0), base32,
// base36, base16, and base64url, whose `-` and `_` are the only
// non-alphanumerics among them. A `%` (percent-encoding) or any other byte
// means the segment is not a CID.
func isCIDPath(escapedPath string) bool {
	const prefix = "/ipfs/"
	if !strings.HasPrefix(escapedPath, prefix) {
		return false
	}
	rest := escapedPath[len(prefix):]
	cid := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		cid = rest[:i]
	}
	if len(cid) < minCIDLength {
		return false
	}
	for _, r := range cid {
		isAlnum := (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !isAlnum && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// normalizeReplacementGateway reduces an operator-written gateway to scheme
// and host. A path is tolerated and dropped ("https://ipfs.filebase.io/ipfs/"
// is how a gateway is usually written down); a query or fragment is refused
// for the same reason normalizeHost refuses them — url.Parse would silently
// make them structure rather than fail.
func normalizeReplacementGateway(raw string) (*url.URL, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = DefaultReplacementGateway
	}
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return nil, fmt.Errorf("contains %q; use scheme and host only", s[i])
	}
	u, err := url.Parse(s)
	if err != nil {
		return nil, err
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, fmt.Errorf("scheme must be http or https")
	}
	if u.Hostname() == "" {
		return nil, fmt.Errorf("no host component")
	}
	if u.User != nil {
		return nil, fmt.Errorf("credentials are not supported")
	}
	// The same literal-host grammar as configured hosts: a gateway written
	// as `ipfs_filebase.io` or `*.filebase.io` must be reported and fall to
	// the default, not become the destination for every CID on the device.
	if err := validateLiteralHost(strings.ToLower(u.Hostname())); err != nil {
		return nil, err
	}
	return &url.URL{Scheme: u.Scheme, Host: strings.ToLower(u.Host)}, nil
}

// Matches reports whether rawURL's host is in the policy's set.
//
// Matching is EXACT on the hostname and deliberately does not extend to
// subdomains. A suffix match would let an unrelated `evil.ipfs.io.attacker
// .example` style host, or any subdomain an operator did not intend, pull
// the rewritten UA — and since the whole point of scoping is to avoid
// touching origins we have not reasoned about, a wildcard would quietly
// undo it. An operator who genuinely needs a subdomain lists it.
//
// Port is ignored: `ipfs.io` and `ipfs.io:8443` are the same origin for this
// decision, and requiring the port would make the common config entry wrong.
//
// A URL that does not parse, or carries no host (relative URLs, `data:`,
// `blob:`), never matches. Those cannot reach a remote origin, so rewriting
// their headers would be meaningless at best.
func (p *Policy) Matches(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return false
	}
	_, ok := p.hosts[host]
	return ok
}

// FetchPatterns renders the policy as CDP `Fetch.enable` request patterns.
//
// This is what keeps the cost of the fix proportional to its scope: Chromium
// pauses ONLY requests matching these patterns, so every request to every
// other origin runs at full speed with no interception round trip. Arming
// `Fetch` with a catch-all "*" instead would put a daemon round trip in front
// of every asset a generative artwork loads.
//
// Patterns are emitted per host for http and https, because `urlPattern`
// matches the full URL string and a scheme wildcard would also admit schemes
// with no remote origin. `requestStage: "Request"` is required — the UA must
// be rewritten BEFORE the request is issued; pausing at the Response stage
// would only hand us the challenge page we are trying to avoid.
//
// Each scheme emits TWO patterns, one with an explicit `:*` port and one
// without. `urlPattern` is a plain glob over the URL text, so
// `https://ipfs.io/*` does NOT match `https://ipfs.io:8443/x` — without the
// port variant, Matches would report a configured host as in-scope while
// Chromium never paused the request, so the rewrite would silently not
// happen and the artwork would fail exactly as if the fix were absent. The
// two forms are needed rather than only the port one because Chromium omits
// the default port from the URL it matches against.
func (p *Policy) FetchPatterns() []map[string]interface{} {
	hosts := p.Hosts()
	patterns := make([]map[string]interface{}, 0, len(hosts)*4)
	for _, h := range hosts {
		host := authorityFor(h)
		for _, scheme := range []string{"http", "https"} {
			for _, authority := range []string{host, host + ":*"} {
				patterns = append(patterns, map[string]interface{}{
					"urlPattern":   scheme + "://" + authority + "/*",
					"requestStage": "Request",
				})
			}
		}
	}
	return patterns
}

// authorityFor renders a stored host as it appears in URL TEXT.
//
// This is the seam that stops this package's two halves from drifting apart.
// A host is STORED in the form url.Hostname() produces — which strips the
// brackets from an IPv6 literal, so `[::1]` is kept as `::1`. Matches
// compares against that stored form and is correct. But FetchPatterns builds
// a glob that Chromium evaluates against the raw URL text, where an IPv6
// authority is always bracketed. Concatenating the stored form straight into
// a pattern yielded `https://::1/*`, which matches no real URL: Matches
// called the host in scope while nothing was ever paused, so an operator's
// IPv6 gateway silently kept Chromium's challenged User-Agent.
//
// That was the FOURTH defect on this branch with one root — ports, globs,
// truncation, and this — all from the host string meaning one thing to
// Matches and another to the pattern. Rendering the authority through a
// single function is the structural answer: anything that changes how a host
// is spelled in a URL now has exactly one place to change, and
// TestAuthorityRoundTripsThroughURLParsing pins the property that makes the
// two halves agree — parsing back what this produces must return the stored
// host unchanged.
//
// Only IPv6 literals need brackets. A DNS name cannot contain a colon
// (validateLiteralHost admits only alphanumerics and hyphens per label) and
// neither can IPv4, so the colon test is exact rather than heuristic; it is
// written against net.ParseIP as well so the intent survives a future
// loosening of the label grammar.
func authorityFor(host string) string {
	if ip := net.ParseIP(host); ip != nil && strings.Contains(host, ":") {
		return "[" + host + "]"
	}
	return host
}

// RewriteHeaders returns headers with User-Agent replaced, preserving every
// other header exactly as Chromium built it.
//
// The input is CDP's `Fetch.requestPaused` header map; the output is the
// array shape `Fetch.continueRequest` expects. Replacement is
// case-insensitive on the key because CDP reports HTTP/2 headers
// lower-cased and HTTP/1.1 headers in canonical case, and emitting BOTH
// `User-Agent` and `user-agent` would send a duplicate header rather than an
// override.
//
// Only the User-Agent is touched. Client-hint headers (`sec-ch-ua*`) are
// left alone on purpose: they were not required to pass the challenge in the
// on-device measurement, and stripping headers we have no evidence about
// widens the change beyond what was verified.
func (p *Policy) RewriteHeaders(headers map[string]string) []map[string]string {
	out := make([]map[string]string, 0, len(headers)+1)
	replaced := false
	for name, value := range headers {
		if strings.EqualFold(name, "user-agent") {
			if replaced {
				// Chromium should never report the same header twice;
				// if it somehow does, collapse rather than emit a
				// duplicate that an origin would be free to interpret
				// either way.
				continue
			}
			out = append(out, map[string]string{"name": name, "value": p.userAgent})
			replaced = true
			continue
		}
		out = append(out, map[string]string{"name": name, "value": value})
	}
	if !replaced {
		// No UA on the original request (Chromium always sends one, but a
		// future request type may not). Add ours rather than returning a
		// header set that still lets the default through.
		out = append(out, map[string]string{"name": "User-Agent", "value": p.userAgent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}

// normalizeHost reduces an operator-written entry to a bare lower-cased
// host. It accepts "ipfs.io", "https://ipfs.io", "ipfs.io:443", and
// "https://ipfs.io/some/path", because those are the shapes a hand-edited
// config realistically contains.
func normalizeHost(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty entry")
	}
	// Reject query/fragment markers BEFORE parsing. url.Parse treats them as
	// structure, not as characters in a host, so it would silently truncate
	// rather than fail: "if?s.io" (a plausible typo for "ipfs.io") parses to
	// the host "if", which is then accepted as a perfectly valid literal and
	// scoped against for the rest of the daemon's life. Neither half of this
	// package can detect that afterwards — the truncation is lossless from
	// their point of view. A query string never helps identify a HOST, so
	// refusing the entry outright and naming the character is strictly more
	// useful than any salvage attempt.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		return "", fmt.Errorf(
			"contains %q, which would truncate the host at that character; "+
				"use the bare host name", s[i])
	}
	if !strings.Contains(s, "://") {
		// url.Parse treats a bare "host:port" authority as scheme:opaque,
		// so give it a scheme to parse against rather than special-casing
		// the colon here.
		s = "http://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", err
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", fmt.Errorf("no host component")
	}
	if err := validateLiteralHost(host); err != nil {
		return "", err
	}
	return host, nil
}

// validateLiteralHost enforces the invariant that ties this package's two
// matching halves together: EVERY host accepted here must be inert under
// glob interpretation.
//
// The host string flows into two places with DIFFERENT matching semantics —
// Matches does literal set membership, FetchPatterns renders it into a CDP
// urlPattern that Chromium evaluates as a glob. A host carrying a glob
// metacharacter satisfies neither honestly: `*` yields the pattern
// `https://*/*`, which pauses EVERY kiosk request, while Matches still
// compares against the literal string "*" and rewrites none of them. The
// result is a daemon round trip in front of every asset for zero benefit,
// with the scoping guarantee silently gone and nothing in the log to say so.
// `*.ipfs.io` is the more likely real-world spelling of this mistake than a
// bare `*`, and it is worse: the operator believes they widened coverage to
// subdomains while actually disabling the rewrite for ipfs.io entirely.
//
// This is the SECOND defect of exactly that shape (the first was ports —
// see FetchPatterns' doc). Both came from the same place: a host value that
// means one thing to Matches and another to the glob. Rejecting anything
// non-literal at the door is what stops there being a third — so if a future
// change wants to support wildcards, it must teach Matches the same grammar
// in the same commit, not relax this check alone.
//
// Accepted: IPv4/IPv6 literals, and DNS names whose labels are
// alphanumeric-with-internal-hyphens. Everything else is a config error and
// is reported to the caller. What the caller does with that differs by
// entry point and the difference is deliberate: New fails the whole list
// (the strict contract the policy tests pin), while NewFromOperatorHosts —
// the hand-edited path the wiring uses — DROPS the offending entry, names
// it in an Error log, and keeps applying the rest, falling back to
// DefaultHosts only when nothing survives. A rejected host therefore never
// means "no rewrite"; see NewFromOperatorHosts for why that matters.
func validateLiteralHost(host string) error {
	// An IP literal is already unambiguous — no labels to validate, and
	// url.Hostname has stripped any IPv6 brackets.
	if net.ParseIP(host) != nil {
		return nil
	}
	if len(host) > 253 {
		return fmt.Errorf("host is longer than 253 characters")
	}
	for _, label := range strings.Split(host, ".") {
		if label == "" {
			return fmt.Errorf("empty label (leading, trailing, or doubled dot)")
		}
		if len(label) > 63 {
			return fmt.Errorf("label %q is longer than 63 characters", label)
		}
		if label[0] == '-' || label[len(label)-1] == '-' {
			return fmt.Errorf("label %q starts or ends with a hyphen", label)
		}
		for _, r := range label {
			isAlnum := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
			if !isAlnum && r != '-' {
				return fmt.Errorf(
					"label %q contains %q; hosts must be literal names or IPs, "+
						"and wildcards are not supported (see validateLiteralHost)",
					label, r)
			}
		}
	}
	return nil
}
