package contentpath_test

import (
	"strings"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
)

// digitalSoul is Han Saglam's Digital Soul, the work ffos-user#427's
// follow-up (feral-file#3599) names: the return of artpiece() on Ethereum.
const (
	digitalSoulContract = "0x4f32740cf412d647a622ba27c36fa4b76f71601f"
	// artBlocksGenerator takes two arguments, which is what proves the
	// method and its arguments stay in the path.
	artBlocksGenerator = "0x953d288708bb771f969fcfd9ba0819ef506ac718"
	artBlocksCore      = "0x059edd72cd353df5106d2b9cc5ab83a52287ac3a"
)

func TestParseWeb3(t *testing.T) {
	cases := []struct {
		name     string
		raw      string
		contract string
		chain    uint64
		path     string
		query    string
		hasQuery bool
	}{
		{"method only", "web3://" + digitalSoulContract + ":1/artpiece", digitalSoulContract[2:], 1, "artpiece", "", false},
		{"no chain id defaults to 1", "web3://" + digitalSoulContract + "/artpiece", digitalSoulContract[2:], 1, "artpiece", "", false},
		{"w3 alias", "w3://" + digitalSoulContract + ":1/artpiece", digitalSoulContract[2:], 1, "artpiece", "", false},
		{"scheme case is ignored", "WEB3://" + digitalSoulContract + ":1/artpiece", digitalSoulContract[2:], 1, "artpiece", "", false},
		{"contract case is folded", "web3://0x" + strings.ToUpper(digitalSoulContract[2:]) + ":1/artpiece", digitalSoulContract[2:], 1, "artpiece", "", false},
		{"two arguments", "web3://" + artBlocksGenerator + ":1/getTokenHtml/" + artBlocksCore + "/0", artBlocksGenerator[2:], 1, "getTokenHtml/" + artBlocksCore + "/0", "", false},
		{"base", "web3://" + digitalSoulContract + ":8453/artpiece", digitalSoulContract[2:], 8453, "artpiece", "", false},
		{"returns attribute", "web3://" + digitalSoulContract + ":1/tokenURI/1?returns=(string)", digitalSoulContract[2:], 1, "tokenURI/1", "returns=(string)", true},
		{"no path", "web3://" + digitalSoulContract + ":1", digitalSoulContract[2:], 1, "", "", false},
		{"empty query is not an absent one", "web3://" + digitalSoulContract + ":1/artpiece?", digitalSoulContract[2:], 1, "artpiece", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, ok := contentpath.ParseWeb3(tc.raw)
			if tc.contract == "" {
				if ok {
					t.Fatalf("ParseWeb3(%q) = %+v, want refused", tc.raw, w)
				}
				return
			}
			if !ok {
				t.Fatalf("ParseWeb3(%q) refused", tc.raw)
			}
			if w.Contract != tc.contract || w.ChainID != tc.chain || w.Path != tc.path {
				t.Fatalf("ParseWeb3(%q) = contract %q chain %d path %q, want %q %d %q",
					tc.raw, w.Contract, w.ChainID, w.Path, tc.contract, tc.chain, tc.path)
			}
			if w.Query != tc.query || w.HasQuery != tc.hasQuery {
				t.Fatalf("ParseWeb3(%q) query = %q/%v, want %q/%v", tc.raw, w.Query, w.HasQuery, tc.query, tc.hasQuery)
			}
		})
	}
}

func TestParseWeb3Refuses(t *testing.T) {
	// Each of these is a URL the device must not turn into a call. A name
	// service needs a second trusted system; a userinfo prefix is a
	// look-alike no standard form needs; a chain id with a leading zero
	// would give one work two cache keys.
	for _, raw := range []string{
		"",
		"web3://",
		"onchfs://" + strings.Repeat("ab", 32),
		"https://" + digitalSoulContract + "/artpiece",
		"web3://example.eth/artpiece",
		"web3://" + digitalSoulContract[:40] + "/artpiece",
		"web3://" + digitalSoulContract + "ff/artpiece",
		"web3://" + digitalSoulContract + ":0/artpiece",
		"web3://" + digitalSoulContract + ":01/artpiece",
		"web3://" + digitalSoulContract + ":abc/artpiece",
		"web3://" + digitalSoulContract + ":1:2/artpiece",
		"web3://someone@" + digitalSoulContract + ":1/artpiece",
		"web3://" + digitalSoulContract + ":1/../escape",
		"web3://" + digitalSoulContract + ":1/a/../../escape",
		"web3://" + digitalSoulContract + `:1/back\slash`,
	} {
		if w, ok := contentpath.ParseWeb3(raw); ok {
			t.Errorf("ParseWeb3(%q) = %+v, want refused", raw, w)
		}
	}
}

func TestWeb3SourceURLRoundTrips(t *testing.T) {
	// SourceURL and ParseWeb3SourceURL are each other's inverse: the offline
	// cache keys a work by sha256 of its exact source string, and replay
	// recovers the call from that string alone, with no side table.
	for _, raw := range []string{
		"web3://" + digitalSoulContract + ":1/artpiece",
		"web3://" + digitalSoulContract + "/artpiece",
		"web3://" + artBlocksGenerator + ":1/getTokenHtml/" + artBlocksCore + "/0",
		"web3://" + digitalSoulContract + ":8453/artpiece?seed=0x06ec&n=2",
		"web3://" + digitalSoulContract + ":1/tokenURI/1?returns=(string)",
		"web3://" + digitalSoulContract + ":1/artpiece#frag",
		"web3://" + digitalSoulContract + ":1/artpiece?",
		"web3://" + digitalSoulContract + ":1",
	} {
		w, ok := contentpath.ParseWeb3(raw)
		if !ok {
			t.Fatalf("ParseWeb3(%q) refused", raw)
		}
		source := w.SourceURL()
		if !strings.HasSuffix(strings.SplitN(strings.TrimPrefix(source, "https://"), "/", 2)[0], contentpath.Web3HostSuffix) {
			t.Fatalf("SourceURL(%q) = %q, host is not under %q", raw, source, contentpath.Web3HostSuffix)
		}
		back, ok := contentpath.ParseWeb3SourceURL(source)
		if !ok {
			t.Fatalf("ParseWeb3SourceURL(%q) refused (from %q)", source, raw)
		}
		if back != w {
			t.Fatalf("round trip of %q: %+v != %+v", raw, back, w)
		}
		if back.SourceURL() != source {
			t.Fatalf("round trip of %q is not idempotent: %q != %q", raw, back.SourceURL(), source)
		}
	}
}

func TestWeb3ChainDefaultIsOneCacheKey(t *testing.T) {
	// ERC-4804 defaults the chain to 1, so a URI that omitted it and one
	// that wrote it name one work. If these differed the device would read
	// the same work twice and store it twice.
	bare, ok := contentpath.ParseWeb3("web3://" + digitalSoulContract + "/artpiece")
	if !ok {
		t.Fatal("bare form refused")
	}
	explicit, ok := contentpath.ParseWeb3("web3://" + digitalSoulContract + ":1/artpiece")
	if !ok {
		t.Fatal("explicit form refused")
	}
	if bare.SourceURL() != explicit.SourceURL() {
		t.Fatalf("one work, two sources: %q and %q", bare.SourceURL(), explicit.SourceURL())
	}
	if bare.URI() != explicit.URI() {
		t.Fatalf("one work, two URIs: %q and %q", bare.URI(), explicit.URI())
	}
}

func TestWeb3HostIsOneLabelPerContract(t *testing.T) {
	w, ok := contentpath.ParseWeb3("web3://" + digitalSoulContract + ":1/artpiece")
	if !ok {
		t.Fatal("refused")
	}
	host := w.Host()
	labels := strings.Split(strings.TrimSuffix(host, contentpath.Web3HostSuffix), ".")
	if len(labels) != 2 {
		t.Fatalf("host %q has %d labels before the suffix, want contract and chain", host, len(labels))
	}
	// 63 is the DNS label limit; base32 of 20 bytes is 32 characters, where
	// hex would need 40.
	if len(labels[0]) != 32 {
		t.Fatalf("contract label %q is %d characters, want 32", labels[0], len(labels[0]))
	}
	if labels[1] != "1" {
		t.Fatalf("chain label = %q, want 1", labels[1])
	}
	if w.ContractAddress() != digitalSoulContract {
		t.Fatalf("ContractAddress() = %q, want %q", w.ContractAddress(), digitalSoulContract)
	}
	if w.Origin() != "https://"+host {
		t.Fatalf("Origin() = %q, want https://%s", w.Origin(), host)
	}
}

func TestParseWeb3SourceURLRefusesLookAlikes(t *testing.T) {
	w, _ := contentpath.ParseWeb3("web3://" + digitalSoulContract + ":1/artpiece")
	good := w.Host()
	label := strings.SplitN(good, ".", 2)[0]
	for _, raw := range []string{
		"https://" + good + ".example/artpiece",
		"http://" + good + "/artpiece",
		"https://user@" + good + "/artpiece",
		"https://" + good + ":8443/artpiece",
		"https://" + label + contentpath.Web3HostSuffix + "/artpiece", // no chain label
		"https://" + label + ".0" + contentpath.Web3HostSuffix + "/artpiece",
		"https://" + label + ".01" + contentpath.Web3HostSuffix + "/artpiece",
		"https://" + label + ".eth" + contentpath.Web3HostSuffix + "/artpiece",
		"https://" + label[:31] + ".1" + contentpath.Web3HostSuffix + "/artpiece",
		"https://" + strings.Repeat("a", 32) + ".1" + contentpath.Web3HostSuffix + "/../escape",
		"https://example.com/artpiece",
	} {
		if got, ok := contentpath.ParseWeb3SourceURL(raw); ok {
			t.Errorf("ParseWeb3SourceURL(%q) = %+v, want refused", raw, got)
		}
		if contentpath.IsWeb3SourceURL(raw) {
			t.Errorf("IsWeb3SourceURL(%q) = true, want false", raw)
		}
	}
}

func TestDeviceLocalSourceScheme(t *testing.T) {
	// The shared seam every scheme-agnostic caller uses (offlinecache,
	// mediahealth, commandrouter). A third scheme is a case here, not a
	// sweep of those call sites.
	onchfsRoot := strings.Repeat("ab", 32)
	onchfs, ok := contentpath.ParseOnchfs("onchfs://" + onchfsRoot + "/?fxhash=0x06ec")
	if !ok {
		t.Fatal("onchfs URI refused")
	}
	web3, ok := contentpath.ParseWeb3("web3://" + digitalSoulContract + ":1/artpiece")
	if !ok {
		t.Fatal("web3 URI refused")
	}
	cases := map[string]contentpath.DeviceLocalScheme{
		onchfs.SourceURL():                     contentpath.SchemeOnchfs,
		web3.SourceURL():                       contentpath.SchemeWeb3,
		"https://example.com/work.html":        contentpath.SchemeNone,
		"data:text/html,<p>hi":                 contentpath.SchemeNone,
		"onchfs://" + onchfsRoot:               contentpath.SchemeNone, // raw, not device-local
		"web3://" + digitalSoulContract + "/a": contentpath.SchemeNone, // raw, not device-local
		"":                                     contentpath.SchemeNone,
	}
	for source, want := range cases {
		if got := contentpath.DeviceLocalSourceScheme(source); got != want {
			t.Errorf("DeviceLocalSourceScheme(%q) = %q, want %q", source, got, want)
		}
		if got := contentpath.IsDeviceLocalSource(source); got != (want != contentpath.SchemeNone) {
			t.Errorf("IsDeviceLocalSource(%q) = %v, want %v", source, got, want != contentpath.SchemeNone)
		}
	}
}

// A percent-encoded argument is data, not a filesystem path: it may decode to
// "//", "a/../b" or a backslash and still be a valid contract argument
// (round 6 review, F12). A literal dot segment or backslash is still refused.
func TestWeb3PathAcceptsEncodedArgumentData(t *testing.T) {
	for _, p := range []string{
		"render/string!https%3A%2F%2Fexample.com",
		"render/string!a%2F..%2Fb",
		"render/string!a%5Cb",
		"render/string!..",
	} {
		raw := "web3://" + digitalSoulContract + ":1/" + p
		w, ok := contentpath.ParseWeb3(raw)
		if !ok {
			t.Errorf("ParseWeb3(%q) refused, want accepted", raw)
			continue
		}
		back, ok := contentpath.ParseWeb3SourceURL(w.SourceURL())
		if !ok || back.Path != p {
			t.Errorf("ParseWeb3SourceURL(%q) = %+v %v, want path %q", w.SourceURL(), back, ok, p)
		}
	}
	for _, p := range []string{"a/%2e%2e/b", "a//b", "a/./b"} {
		if _, ok := contentpath.ParseWeb3("web3://" + digitalSoulContract + ":1/" + p); ok {
			t.Errorf("path %q accepted, want refused", p)
		}
	}
}

// "0X" is accepted by validation, so it must be stripped like "0x"
// (round 6 review, F14).
func TestParseWeb3StripsAnUpperCasePrefix(t *testing.T) {
	w, ok := contentpath.ParseWeb3("web3://0X" + digitalSoulContract[2:] + ":1/artpiece")
	if !ok || w.Contract != digitalSoulContract[2:] {
		t.Fatalf("ParseWeb3 = %+v %v, want contract %q", w, ok, digitalSoulContract[2:])
	}
	if _, ok := contentpath.ParseWeb3SourceURL(w.SourceURL()); !ok {
		t.Fatalf("SourceURL %q is not a web3 source", w.SourceURL())
	}
}
