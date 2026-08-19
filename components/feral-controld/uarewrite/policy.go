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
// This package is intentionally pure: it holds no CDP connection, performs
// no I/O, and knows nothing about how interception is armed. That keeps the
// policy independently testable and lets whichever component owns kiosk
// `Fetch` interception consume it without inheriting a dependency on this
// one's transport.
package uarewrite

import (
	"fmt"
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
func New(hosts []string, userAgent string) (*Policy, error) {
	if len(hosts) == 0 {
		hosts = DefaultHosts
	}
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		ua = DefaultUserAgent
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

	return &Policy{hosts: set, userAgent: ua}, nil
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
		for _, scheme := range []string{"http", "https"} {
			for _, authority := range []string{h, h + ":*"} {
				patterns = append(patterns, map[string]interface{}{
					"urlPattern":   scheme + "://" + authority + "/*",
					"requestStage": "Request",
				})
			}
		}
	}
	return patterns
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
	return host, nil
}
