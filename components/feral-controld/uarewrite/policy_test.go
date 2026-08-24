package uarewrite

import (
	"net/url"
	"reflect"
	"sort"
	"strings"
	"testing"
)

func TestNewDefaults(t *testing.T) {
	t.Parallel()

	p, err := New(nil, "")
	if err != nil {
		t.Fatalf("New(nil, \"\") = %v, want no error", err)
	}
	if got := p.UserAgent(); got != DefaultUserAgent {
		t.Errorf("UserAgent() = %q, want %q", got, DefaultUserAgent)
	}
	if got := p.Hosts(); !reflect.DeepEqual(got, []string{"dweb.link", "ipfs.io"}) {
		t.Errorf("Hosts() = %v, want the sorted default set", got)
	}
}

// The replacement token must not look like a browser: a browser UA is
// exactly what the mitigation layer challenges, so an "improvement" that
// impersonates Chrome would silently reinstate the bug this package fixes.
func TestDefaultUserAgentIsNotBrowserShaped(t *testing.T) {
	t.Parallel()

	for _, bad := range []string{"Mozilla", "Chrome", "Safari", "AppleWebKit", "Gecko"} {
		if strings.Contains(DefaultUserAgent, bad) {
			t.Errorf("DefaultUserAgent %q contains browser token %q", DefaultUserAgent, bad)
		}
	}
}

func TestNewHostNormalization(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		in    []string
		want  []string
		isErr bool
	}{
		{name: "bare hosts", in: []string{"ipfs.io"}, want: []string{"ipfs.io"}},
		{name: "strips scheme", in: []string{"https://ipfs.io"}, want: []string{"ipfs.io"}},
		{name: "strips port", in: []string{"ipfs.io:443"}, want: []string{"ipfs.io"}},
		{name: "strips scheme and path", in: []string{"https://ipfs.io/ipfs/"}, want: []string{"ipfs.io"}},
		{name: "lower-cases", in: []string{"IPFS.IO"}, want: []string{"ipfs.io"}},
		{name: "trims space", in: []string{"  ipfs.io  "}, want: []string{"ipfs.io"}},
		{name: "dedupes", in: []string{"ipfs.io", "https://ipfs.io", "IPFS.IO:443"}, want: []string{"ipfs.io"}},
		{name: "sorted output", in: []string{"z.example", "a.example"}, want: []string{"a.example", "z.example"}},
		{name: "empty entry is an error", in: []string{"ipfs.io", "  "}, isErr: true},
		{name: "scheme with no host is an error", in: []string{"https://"}, isErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, err := New(tt.in, "ua/1")
			if tt.isErr {
				if err == nil {
					t.Fatalf("New(%v) = nil error, want an error", tt.in)
				}
				return
			}
			if err != nil {
				t.Fatalf("New(%v) = %v, want no error", tt.in, err)
			}
			if got := p.Hosts(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Hosts() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatches(t *testing.T) {
	t.Parallel()

	p, err := New([]string{"ipfs.io", "dweb.link"}, "ua/1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name string
		url  string
		want bool
	}{
		{name: "listed host https", url: "https://ipfs.io/ipfs/QmAbc", want: true},
		{name: "listed host http", url: "http://ipfs.io/ipfs/QmAbc", want: true},
		{name: "listed host with port", url: "https://ipfs.io:8443/ipfs/QmAbc", want: true},
		{name: "listed host upper case", url: "https://IPFS.IO/ipfs/QmAbc", want: true},
		{name: "second listed host", url: "https://dweb.link/ipfs/QmAbc", want: true},
		{name: "query string does not matter", url: "https://ipfs.io/x?a=1&b=2", want: true},

		// Scope guards: everything below must be left alone, because a
		// rewritten UA on an origin we have not reasoned about can break
		// artworks that render correctly today.
		{name: "unlisted host", url: "https://gateway.pinata.cloud/ipfs/QmAbc", want: false},
		{name: "player shell on loopback", url: "http://127.0.0.1:8080/playlist", want: false},
		{name: "subdomain does not match", url: "https://cdn.ipfs.io/ipfs/QmAbc", want: false},
		{name: "host merely containing the entry", url: "https://ipfs.io.attacker.example/x", want: false},
		{name: "entry as a path segment", url: "https://evil.example/ipfs.io/x", want: false},

		// No remote origin to rewrite for.
		{name: "data uri", url: "data:image/png;base64,iVBORw0KGgo=", want: false},
		{name: "blob uri", url: "blob:http://127.0.0.1:8080/abc-def", want: false},
		{name: "relative url", url: "/ipfs/QmAbc", want: false},
		{name: "empty string", url: "", want: false},
		{name: "unparseable", url: "://%%%", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := p.Matches(tt.url); got != tt.want {
				t.Errorf("Matches(%q) = %v, want %v", tt.url, got, tt.want)
			}
		})
	}
}

func TestFetchPatterns(t *testing.T) {
	t.Parallel()

	p, err := New([]string{"ipfs.io"}, "ua/1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	got := p.FetchPatterns()
	want := []map[string]interface{}{
		{"urlPattern": "http://ipfs.io/*", "requestStage": "Request"},
		{"urlPattern": "http://ipfs.io:*/*", "requestStage": "Request"},
		{"urlPattern": "https://ipfs.io/*", "requestStage": "Request"},
		{"urlPattern": "https://ipfs.io:*/*", "requestStage": "Request"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FetchPatterns() = %v, want %v", got, want)
	}
}

// acceptedHostShapes enumerates EVERY host spelling validateLiteralHost
// admits, with the URL text a real request would carry for each.
//
// It is the fixture the coverage tests below are driven from, and that is the
// point: the IPv6 defect existed because the coverage test was fed one
// hand-picked DNS host while `New` had quietly grown support for IP literals.
// A shape added to the validator must be added here, and it is then checked
// end to end automatically instead of depending on whoever widens the grammar
// remembering to widen the assertions too.
var acceptedHostShapes = []struct {
	name   string
	config string   // what an operator writes in gatewayUserAgent.hosts
	stored string   // the canonical form Hosts() must report
	urls   []string // real URL text that must be in scope, written literally
}{
	{
		name: "dns name", config: "ipfs.io", stored: "ipfs.io",
		urls: []string{"https://ipfs.io/ipfs/QmAbc", "http://ipfs.io/x", "https://ipfs.io:8443/x"},
	},
	{
		name: "ipv4 literal", config: "127.0.0.1", stored: "127.0.0.1",
		urls: []string{"http://127.0.0.1/x", "http://127.0.0.1:8080/x"},
	},
	{
		// Bracketed in URL text, unbracketed once stored — the exact
		// asymmetry that produced `https://::1/*` and matched nothing.
		name: "ipv6 loopback", config: "[::1]", stored: "::1",
		urls: []string{"https://[::1]/x", "https://[::1]:8443/x"},
	},
	{
		name: "ipv6 full", config: "[2001:db8::1]", stored: "2001:db8::1",
		urls: []string{"https://[2001:db8::1]/x", "http://[2001:db8::1]:8080/x"},
	},
}

// Matches ignores the port, so the emitted patterns must cover a port-bearing
// URL too. More generally: whenever Matches calls a URL in scope, some Fetch
// pattern must actually pause it. When the two disagree the rewrite silently
// does not happen and the artwork fails exactly as if the fix were absent —
// with nothing logged to say why. Ports and IPv6 brackets have each broken
// this once already.
func TestFetchPatternsCoverEveryURLThatMatches(t *testing.T) {
	t.Parallel()

	for _, shape := range acceptedHostShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			p, err := New([]string{shape.config}, "ua/1")
			if err != nil {
				t.Fatalf("New(%q): %v", shape.config, err)
			}
			if got := p.Hosts(); len(got) != 1 || got[0] != shape.stored {
				t.Fatalf("Hosts() = %v, want [%q]", got, shape.stored)
			}

			patterns := make([]string, 0, 4)
			for _, pat := range p.FetchPatterns() {
				patterns = append(patterns, pat["urlPattern"].(string))
			}

			for _, u := range shape.urls {
				if !p.Matches(u) {
					t.Errorf("Matches(%q) = false, want true", u)
					continue
				}
				if !anyGlobMatches(patterns, u) {
					t.Errorf("Matches(%q) is true but no Fetch pattern covers it (patterns: %v)", u, patterns)
				}
			}
		})
	}
}

// The property that keeps Matches and FetchPatterns in agreement: rendering a
// stored host into URL text and parsing it back must return that same stored
// host. Matches reads url.Hostname(); patterns are built from authorityFor.
// If those two are not inverses, one half is scoped to something the other
// cannot see — which is precisely how the IPv6 form came to match nothing.
func TestAuthorityRoundTripsThroughURLParsing(t *testing.T) {
	t.Parallel()

	for _, shape := range acceptedHostShapes {
		t.Run(shape.name, func(t *testing.T) {
			t.Parallel()

			p, err := New([]string{shape.config}, "ua/1")
			if err != nil {
				t.Fatalf("New(%q): %v", shape.config, err)
			}
			stored := p.Hosts()[0]

			for _, scheme := range []string{"http", "https"} {
				raw := scheme + "://" + authorityFor(stored) + "/x"
				u, err := url.Parse(raw)
				if err != nil {
					t.Fatalf("rendered authority %q does not parse: %v", raw, err)
				}
				if got := u.Hostname(); got != stored {
					t.Errorf("round trip broke: rendered %q, parsed host %q, stored %q", raw, got, stored)
				}
			}
		})
	}
}

// anyGlobMatches models CDP's urlPattern semantics: a plain glob over the
// full URL text where "*" matches any run of characters. Kept deliberately
// small — it exists to assert pattern coverage, not to reimplement Chromium.
func anyGlobMatches(patterns []string, url string) bool {
	for _, p := range patterns {
		if globMatch(p, url) {
			return true
		}
	}
	return false
}

func globMatch(pattern, s string) bool {
	parts := strings.Split(pattern, "*")
	if !strings.HasPrefix(s, parts[0]) {
		return false
	}
	s = s[len(parts[0]):]
	for i := 1; i < len(parts); i++ {
		if parts[i] == "" {
			if i == len(parts)-1 {
				return true
			}
			continue
		}
		idx := strings.Index(s, parts[i])
		if idx < 0 {
			return false
		}
		s = s[idx+len(parts[i]):]
	}
	return strings.HasSuffix(pattern, "*") || s == ""
}

// A catch-all pattern would put a daemon round trip in front of every asset
// a generative artwork loads. Guard the scoping property explicitly so a
// later "simplification" to "*" fails here rather than in the field.
func TestFetchPatternsAreScopedNotCatchAll(t *testing.T) {
	t.Parallel()

	p, err := New([]string{"ipfs.io", "dweb.link"}, "ua/1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, pat := range p.FetchPatterns() {
		got, _ := pat["urlPattern"].(string)

		// Assert on the AUTHORITY, not on whether the whole pattern equals
		// some known catch-all literal. The earlier version of this test
		// compared against "*" and "*://*/*" and therefore sailed past
		// `https://*/*`, which is the pattern a `*` host entry actually
		// produced — a catch-all that pauses every kiosk request. Checking
		// the authority is what makes this test guard the invariant rather
		// than a spelling of its violation.
		authority := got
		if i := strings.Index(authority, "://"); i >= 0 {
			authority = authority[i+3:]
		}
		if i := strings.Index(authority, "/"); i >= 0 {
			authority = authority[:i]
		}
		host := strings.TrimSuffix(authority, ":*")
		if host != "ipfs.io" && host != "dweb.link" {
			t.Errorf("pattern %q has authority %q, which is not a configured host", got, host)
		}
		if strings.ContainsAny(host, "*?[]") {
			t.Errorf("pattern %q carries a glob metacharacter in its host", got)
		}
		if pat["requestStage"] != "Request" {
			t.Errorf("pattern %v must pause at the Request stage", pat)
		}
	}
}

// A glob metacharacter in a configured host produces a pattern that pauses
// far more than it rewrites: `*` yields `https://*/*`, which Chromium matches
// against EVERY request, while Matches still compares the literal string "*"
// and rewrites none of them. That is a daemon round trip in front of every
// kiosk asset for zero benefit, with the scoping guarantee silently gone.
// New must refuse the config outright so main.go degrades to "no rewrite".
func TestNewRejectsGlobMetacharacters(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{
		"*",
		"*.ipfs.io",
		"ipfs.*",
		"ipfs.io*",
		"if?s.io",
		"[ai]pfs.io",
		"https://*",
		"*:443",
	} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()

			p, err := New([]string{entry}, "ua/1")
			if err == nil {
				t.Fatalf("New(%q) accepted a glob host and emitted %v", entry, p.FetchPatterns())
			}
		})
	}
}

// The guarantee that matters is behavioral, not textual: whatever New
// accepts must never yield a pattern that matches a URL Matches would reject.
// Pin it against the glob model so a future relaxation of the host grammar
// has to keep both halves in step.
func TestAcceptedHostsNeverProduceOverreachingPatterns(t *testing.T) {
	t.Parallel()

	p, err := New([]string{"ipfs.io", "dweb.link", "127.0.0.1"}, "ua/1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	patterns := make([]string, 0, 12)
	for _, pat := range p.FetchPatterns() {
		patterns = append(patterns, pat["urlPattern"].(string))
	}

	for _, u := range []string{
		"https://example.com/asset.js",
		"http://127.0.0.1:8080/playlist",
		"https://gateway.pinata.cloud/ipfs/QmAbc",
		"https://cdn.ipfs.io/ipfs/QmAbc",
		"https://ipfs.io.attacker.example/x",
	} {
		if p.Matches(u) {
			continue // legitimately in scope; nothing to prove here
		}
		if anyGlobMatches(patterns, u) {
			t.Errorf("%q is paused by a Fetch pattern but Matches() rejects it: "+
				"the request round-trips through the daemon for no rewrite", u)
		}
	}
}

func TestNewAcceptsIPLiteralHosts(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{"127.0.0.1", "192.168.31.151:8080", "[::1]", "http://[2001:db8::1]"} {
		if _, err := New([]string{entry}, "ua/1"); err != nil {
			t.Errorf("New(%q) rejected a literal IP host: %v", entry, err)
		}
	}
}

func TestNewRejectsMalformedLabels(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{"-ipfs.io", "ipfs-.io", "ipfs..io", "ipfs_gateway.io"} {
		if _, err := New([]string{entry}, "ua/1"); err == nil {
			t.Errorf("New(%q) accepted a malformed host", entry)
		}
	}
}

func TestRewriteHeaders(t *testing.T) {
	t.Parallel()

	p, err := New([]string{"ipfs.io"}, "feral-player/9.9")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	tests := []struct {
		name string
		in   map[string]string
		want []map[string]string
	}{
		{
			name: "canonical case is replaced in place",
			in:   map[string]string{"User-Agent": "Mozilla/5.0 Chrome/150", "Accept": "image/*"},
			want: []map[string]string{
				{"name": "Accept", "value": "image/*"},
				{"name": "User-Agent", "value": "feral-player/9.9"},
			},
		},
		{
			// HTTP/2 reports headers lower-cased. Emitting our own
			// canonical-case header alongside would send the UA twice.
			name: "lower case key is replaced, not duplicated",
			in:   map[string]string{"user-agent": "Mozilla/5.0 Chrome/150"},
			want: []map[string]string{
				{"name": "user-agent", "value": "feral-player/9.9"},
			},
		},
		{
			name: "absent user-agent is added",
			in:   map[string]string{"Accept": "image/*"},
			want: []map[string]string{
				{"name": "Accept", "value": "image/*"},
				{"name": "User-Agent", "value": "feral-player/9.9"},
			},
		},
		{
			name: "no headers at all still yields ours",
			in:   map[string]string{},
			want: []map[string]string{
				{"name": "User-Agent", "value": "feral-player/9.9"},
			},
		},
		{
			// Client hints are deliberately untouched: they were not
			// needed to pass the challenge on device, so stripping them
			// would widen the change past what was verified.
			name: "other headers pass through untouched",
			in: map[string]string{
				"User-Agent":      "Mozilla/5.0 Chrome/150",
				"Referer":         "http://127.0.0.1:8080/",
				"sec-ch-ua":       "\"Chromium\";v=\"150\"",
				"Sec-Fetch-Dest":  "image",
				"Accept-Encoding": "gzip",
			},
			want: []map[string]string{
				{"name": "Accept-Encoding", "value": "gzip"},
				{"name": "Referer", "value": "http://127.0.0.1:8080/"},
				{"name": "Sec-Fetch-Dest", "value": "image"},
				{"name": "User-Agent", "value": "feral-player/9.9"},
				{"name": "sec-ch-ua", "value": "\"Chromium\";v=\"150\""},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := p.RewriteHeaders(tt.in)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("RewriteHeaders(%v)\n got = %v\nwant = %v", tt.in, got, tt.want)
			}
		})
	}
}

// Exactly one User-Agent must reach the origin. Two would let the origin
// pick, which reinstates the challenge non-deterministically.
func TestRewriteHeadersEmitsExactlyOneUserAgent(t *testing.T) {
	t.Parallel()

	p, err := New([]string{"ipfs.io"}, "ua/1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for _, in := range []map[string]string{
		{"User-Agent": "a"},
		{"user-agent": "a"},
		{"USER-AGENT": "a"},
		{},
	} {
		count := 0
		for _, h := range p.RewriteHeaders(in) {
			if strings.EqualFold(h["name"], "user-agent") {
				count++
				if h["value"] != "ua/1" {
					t.Errorf("RewriteHeaders(%v) kept UA %q", in, h["value"])
				}
			}
		}
		if count != 1 {
			t.Errorf("RewriteHeaders(%v) emitted %d User-Agent headers, want 1", in, count)
		}
	}
}

// The caller's map must not be mutated: it comes straight off the CDP event
// and may be reused by the caller for logging after the rewrite.
func TestRewriteHeadersDoesNotMutateInput(t *testing.T) {
	t.Parallel()

	p, err := New([]string{"ipfs.io"}, "ua/1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	in := map[string]string{"User-Agent": "original", "Accept": "image/*"}
	p.RewriteHeaders(in)

	if in["User-Agent"] != "original" {
		t.Errorf("input map was mutated: User-Agent = %q", in["User-Agent"])
	}
	if len(in) != 2 {
		t.Errorf("input map grew to %d entries", len(in))
	}
}

// url.Parse treats "?" and "#" as structure rather than host characters, so
// an entry containing either is TRUNCATED rather than rejected: "if?s.io" (a
// plausible typo for "ipfs.io") yields the host "if", which then looks like a
// perfectly valid literal to every later check. Neither Matches nor
// FetchPatterns can detect that after the fact, so the entry has to be
// refused at the door.
func TestNewRejectsEntriesThatWouldTruncateSilently(t *testing.T) {
	t.Parallel()

	for _, entry := range []string{
		"if?s.io",
		"ipfs.io?x=1",
		"ipfs.io#frag",
		"https://ipfs.io/path?x=1",
	} {
		t.Run(entry, func(t *testing.T) {
			t.Parallel()

			p, err := New([]string{entry}, "ua/1")
			if err == nil {
				t.Fatalf("New(%q) accepted an entry that truncates to hosts %v", entry, p.Hosts())
			}
			if !strings.Contains(err.Error(), "truncate") {
				t.Errorf("New(%q) error %q should name the truncation hazard", entry, err)
			}
		})
	}
}

// NewFromOperatorHosts is the hand-edited-list path. New's all-or-nothing
// contract is right for a programmatic caller and wrong here: the config
// block exists so the next hostile gateway is an operator edit rather than a
// release, and one typo'd entry must not revoke the entries that work.
func TestNewFromOperatorHostsSalvagesUsableEntries(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		hosts        []string
		wantHosts    []string
		wantRejected []string
	}{
		{
			// The realistic edit: append a new gateway, spell it as a
			// wildcard. The working entries must survive untouched.
			name:         "one wildcard typo among good entries",
			hosts:        []string{"ipfs.io", "*.newgateway.link", "dweb.link"},
			wantHosts:    []string{"dweb.link", "ipfs.io"},
			wantRejected: []string{"*.newgateway.link"},
		},
		{
			// An operator who narrowed the scope keeps their narrowing;
			// salvaging must not quietly re-add the built-in hosts.
			name:         "deliberate narrowing is preserved",
			hosts:        []string{"only-mine.example", "bad host"},
			wantHosts:    []string{"only-mine.example"},
			wantRejected: []string{"bad host"},
		},
		{
			// Nothing survives, so there is no stated scope left to honor
			// and the defaults are the same landing point an unreadable
			// config block gets.
			name:         "every entry unusable falls back to defaults",
			hosts:        []string{"https://", "*.bad"},
			wantHosts:    DefaultHosts,
			wantRejected: []string{"https://", "*.bad"},
		},
		{
			name:         "a wholly valid list rejects nothing",
			hosts:        []string{"ipfs.io"},
			wantHosts:    []string{"ipfs.io"},
			wantRejected: nil,
		},
		{
			name:         "an absent list is the default scope",
			hosts:        nil,
			wantHosts:    DefaultHosts,
			wantRejected: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			p, rejected, err := NewFromOperatorHosts(tt.hosts, "feral-player/test")
			if err != nil {
				t.Fatalf("NewFromOperatorHosts: unexpected error: %v", err)
			}
			if p == nil {
				t.Fatal("NewFromOperatorHosts returned no policy")
			}

			if !reflect.DeepEqual(rejected, tt.wantRejected) {
				t.Errorf("rejected = %v, want %v", rejected, tt.wantRejected)
			}

			want := append([]string(nil), tt.wantHosts...)
			sort.Strings(want)
			if got := p.Hosts(); !reflect.DeepEqual(got, want) {
				t.Errorf("hosts = %v, want %v", got, want)
			}

			// The scoping guarantee must survive salvaging: a rejected
			// entry may never leak into the Fetch patterns, and the pattern
			// set must never widen to a catch-all.
			for _, pattern := range p.FetchPatterns() {
				urlPattern, _ := pattern["urlPattern"].(string)
				if urlPattern == "*" {
					t.Error("patterns must never be catch-all")
				}
				for _, bad := range tt.wantRejected {
					if strings.Contains(urlPattern, strings.TrimPrefix(bad, "*.")) && bad != "https://" {
						t.Errorf("rejected entry %q leaked into pattern %q", bad, urlPattern)
					}
				}
			}
		})
	}
}
