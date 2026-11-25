package contentpath

import (
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"strconv"
	"strings"
)

// web3:// is ERC-4804 (with ERC-6860's corrections): a work that is the return
// value of a contract's view function has a URL any player can resolve with
// one eth_call and no knowledge of the artist.
//
//	web3://<contract>[:<chainid>][/<method>[/<arg>…]][?<query>][#<fragment>]
//
// Han Saglam's Digital Soul is `web3://0x4f32740cf412d647a622ba27c36fa4b76f71601f:1/artpiece`
// and Art Blocks' generator is
// `web3://0x953d288708bb771f969fcfd9ba0819ef506ac718:1/getTokenHtml/<core>/<tokenId>`.
// The player holds no function name: the URL carries the method and the
// standard's auto-mode rules type the arguments (feral-file#3599).
//
// Unlike ipfs, ar and onchfs there is no content address here, so there is
// nothing to verify the bytes against: the guarantee is the chain's
// consensus, and so the node's. See the web3 package for what the device
// does about that (a pinned block and a second node's opinion where one
// answers).
//
// As for onchfs, the kiosk loads http(s) only, so a web3 work is given a
// device-local source URL that names the same call:
//
//	https://<base32(contract)>.<chainid>.web3.invalid/<method>/<arg>…?<query>#<fragment>
//
//   - The host is a pure function of the contract and the chain, and the
//     method and arguments stay in the path, so ParseWeb3SourceURL recovers
//     the exact call from the URL alone. Nothing needs a side table.
//   - `.invalid` is reserved (RFC 6761) and never resolves: a request replay
//     does not answer fails at once and reaches no network.
//   - base32 keeps the 20-byte contract inside one DNS label (32 characters,
//     where hex would need 40 and the limit is 63).
//   - The chain id is always written, so a URI that omitted it (ERC-4804's
//     default of 1) and one that wrote `:1` are the same work and the same
//     cache key.
//   - Path, query and fragment are carried byte for byte, and so is whether
//     the path separator was there at all: a URL with a query and no path
//     (`web3://0x…:1?seed=1`) becomes `https://<host>?seed=1`, with no "/",
//     because under ERC-6860's manual mode that URL's calldata is `?seed=1`
//     where `…:1/?seed=1`'s is `/?seed=1`. Two calls, two works, two cache
//     keys (Web3.HasPath).
//
// One origin per contract-and-chain, not per work: two editions of one
// contract (Art Blocks' getTokenHtml/<core>/<tokenId>) share an origin and so
// share localStorage and service workers. That is the cost of keeping the
// host invertible; onchfs can afford an origin per work because its whole
// identity is one 32-byte root (owner's call, feral-file#3599).
//
// ParseWeb3 and ParseWeb3SourceURL are each other's inverse for every URI
// ParseWeb3 accepts, and that is checked rather than reasoned about:
// ParseWeb3 canonicalises the path to the spelling a browser will ask for
// (canonicalWeb3Path), then refuses any URI whose source URL does not read
// back as itself (roundTrips). The query and the fragment are written
// verbatim, so the bytes net/url will not carry there are refused rather
// than rewritten into a source nothing recognises. Nothing else may build or
// parse either form.

// Web3HostSuffix ends every device-local web3 source host.
const Web3HostSuffix = ".web3.invalid"

// web3Schemes are the scheme prefixes ERC-6860 gives this URL form. Both map
// to the same canonical URI, so a playlist that spells one and a playlist
// that spells the other name one work and one cache key.
var web3Schemes = []string{"web3://", "w3://"}

// Web3DefaultChainID is the chain ERC-4804 reads when the URL names none.
const Web3DefaultChainID = 1

// web3Base32 is RFC 4648 base32 in lower case without padding, as for onchfs:
// hosts are case-insensitive and Chromium lower-cases them.
var web3Base32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// web3ContractLabelLen is base32's length for 20 bytes.
const web3ContractLabelLen = 32

// Web3 is one parsed web3:// reference.
type Web3 struct {
	// Contract is the 40-hex contract address, lower case, without "0x".
	Contract string
	// ChainID is the EIP-155 chain the call is made on, never 0:
	// Web3DefaultChainID when the URL named none.
	ChainID uint64
	// Path is the escaped path of the call, without a leading slash: the
	// method name, then its arguments, as ERC-4804 spells them ("" is a
	// call with empty calldata).
	Path string
	// HasPath says the URL carried the path separator, so ERC-6860's mPath
	// begins with "/" rather than being empty. It is what tells
	// `web3://0x…:1?seed=1` (mPath empty, mPathQuery "?seed=1") from
	// `web3://0x…:1/?seed=1` (mPath "/", mPathQuery "/?seed=1"), which under
	// manual mode are two different calldatas and so two different works.
	//
	// It is canonicalised, not recorded raw: when there is no query the two
	// spellings produce the same calldata ("/" either way, by ERC-6860's
	// empty-mPathQuery rule), so both are stored as HasPath — one work, one
	// cache key, exactly as a URI that omitted the default chain id and one
	// that wrote `:1` are one work. HasPath is therefore false only for the
	// bare-authority-with-query form, the one shape where the separator
	// changes what is sent.
	HasPath bool
	// Query and Fragment are raw, without "?" or "#". HasQuery and
	// HasFragment tell an empty one from an absent one, so the round trip is
	// byte for byte. The query carries ERC-4804's `returns=` attribute.
	Query       string
	HasQuery    bool
	Fragment    string
	HasFragment bool
}

// web3HasPath canonicalises mPath's presence from the separator the URL
// actually carried, and is the one rule both ParseWeb3 and
// ParseWeb3SourceURL use: a device-local source has to spell the distinction
// the same way the URI did, or the two stop being inverses.
//
// See Web3.HasPath for why an absent separator with no query is stored as
// present.
func web3HasPath(separator, hasQuery bool) bool { return separator || !hasQuery }

// ParseWeb3 parses a `web3://` (or `w3://`) URL. ok is false for anything the
// device cannot turn into one eth_call: a contract that is not a literal
// 0x-prefixed 40-hex address (a name-service lookup is a second trusted
// system and is not implemented), a chain id that is not a positive decimal,
// a userinfo prefix (`web3://anything@0x…` — a look-alike no standard form
// needs), or a path that does not clean to itself.
func ParseWeb3(raw string) (Web3, bool) {
	rest, ok := cutWeb3Scheme(raw)
	if !ok {
		return Web3{}, false
	}
	var w Web3
	rest, w.Fragment, w.HasFragment = strings.Cut(rest, "#")
	rest, w.Query, w.HasQuery = strings.Cut(rest, "?")
	authority, after, hasSeparator := strings.Cut(rest, "/")
	if strings.Contains(authority, "@") {
		return Web3{}, false
	}
	contract, chain, hasChain := strings.Cut(authority, ":")
	if !isHex40Address(contract) {
		return Web3{}, false
	}
	// isHex40Address accepts a "0X" prefix as well as "0x", so remove the
	// validated two characters rather than only the lower-case spelling.
	w.Contract = strings.ToLower(contract[2:])
	w.ChainID = Web3DefaultChainID
	if hasChain {
		// Leading zeros would give one chain two spellings and so one work
		// two cache keys; a decimal with none is the only form accepted.
		id, err := strconv.ParseUint(chain, 10, 64)
		if err != nil || id == 0 || chain != strconv.FormatUint(id, 10) {
			return Web3{}, false
		}
		w.ChainID = id
	}
	if !cleanWeb3Path(after) {
		return Web3{}, false
	}
	w.Path = canonicalWeb3Path(after)
	w.HasPath = web3HasPath(hasSeparator, w.HasQuery)
	if !roundTrips(w) {
		return Web3{}, false
	}
	return w, true
}

// roundTrips checks the one property the rest of the device depends on, on
// the parse that produces it: that the source URL built from w reads back as
// w. It is the invariant, checked, rather than a property re-derived
// component by component — canonicalWeb3Path makes it true of the path, and
// the query and fragment are written verbatim, where net/url is stricter
// than the path escaping suggests. A C0 or DEL byte in the query makes
// url.Parse refuse the source outright (it scans every byte before the
// fragment), and an invalid percent-escape in the fragment does the same
// through setFragment's unescape.
//
// Refusing such a URI is what keeps it failing closed. The alternative — and
// what happened before this check — is worse than not playing: ParseWeb3
// accepted the URI, so dp1 rewrote the item to a `.invalid` https source,
// and then nothing recognised that source as device-local. The work was
// never resolved, the app's download command answered a classification
// failure, and media health dialed and judged a host that cannot resolve,
// which is both "a device-local source is never dialed" and "a device-held
// source is never probed or judged" broken on one work. Unmapped, it stays a
// `web3://` source the kiosk simply cannot load (round 5 review, F1).
//
// Canonicalising the query and the fragment as well — a derived escape set
// each, as for the path — is what would let these works play. It is two more
// tables to keep in step with net/url for two components whose rules differ,
// and no measured work needs one of those bytes, so it is left for the first
// that does.
func roundTrips(w Web3) bool {
	back, ok := ParseWeb3SourceURL(w.SourceURL())
	return ok && back == w
}

// canonicalWeb3Path spells a web3 path so that it survives being written into
// an https URL and read back out: every byte that would not come back
// unchanged is escaped, every percent triplet already written is left alone,
// and nothing else is touched.
//
// This is what makes ParseWeb3 and ParseWeb3SourceURL an inverse pair for
// every path, rather than only for paths that arrived escaped. SourceURL
// writes Path into an https URL verbatim and ParseWeb3SourceURL reads it back
// through url.URL.EscapedPath, which re-escapes — so a path carrying a raw
// space, quote, angle bracket or brace used to be stored under one spelling
// and requested by Chromium under another. Replay matches method and URL
// exactly, so the entry document missed on every play and the work never
// appeared; nothing reported it either, because a device-local source is
// exempt from every media-health verdict (round 4 review, F2).
//
// A `string` argument containing a space is the ordinary case under
// ERC-4804's auto-mode typing, so this is not an edge shape.
//
// Existing triplets are preserved rather than normalised, so a URI that
// encoded an argument on purpose keeps its exact source string and its cache
// key. It is idempotent: escaping an already-escaped path changes nothing, so
// one work has one cache key however it was spelled.
//
// **The auto-mode call is unaffected; the manual-mode call is not, and that
// is deliberate.** web3.pathSegments unescapes every segment before it builds
// auto mode's calldata, so there escaping is only a different spelling of the
// same bytes. Manual mode has no segments to unescape: ERC-6860 sends
// mPathQuery as the calldata and the contract parses it itself, so a
// manual-mode work is called with the canonicalised spelling
// (`/my%20work`, not `/my work`). A conforming URI cannot carry one of these
// bytes raw — the ones escaped here are the ones a URL path may not hold —
// and neither measured work is manual mode, so the device keeps one spelling
// for the source, the cache key and the call rather than carrying a second
// path through web3.Request for a shape no named work has. The cost is
// recorded on the branch's `Out of scope` instead of being left implied
// (round 5 review, F3).
//
// Only web3 is canonicalised here. Onchfs.SourceURL writes its path verbatim
// the same way and is pre-existing on the base branch; see the branch's
// `Root cause` field for where that is recorded.
func canonicalWeb3Path(escaped string) string {
	var b strings.Builder
	for i := 0; i < len(escaped); {
		c := escaped[i]
		// An existing, valid percent-triplet is left exactly as written. It
		// is already a spelling a browser asks for, and rewriting it would
		// change the source string — and so the cache key — for a work whose
		// URI deliberately encoded an argument (a `string!` separator, a
		// slash inside an argument).
		if c == '%' && i+2 < len(escaped) && isHexDigit(escaped[i+1]) && isHexDigit(escaped[i+2]) {
			b.WriteString(escaped[i : i+3])
			i += 3
			continue
		}
		if web3MustEscape[c] {
			b.WriteString("%")
			b.WriteByte(upperHex[c>>4])
			b.WriteByte(upperHex[c&0x0f])
		} else {
			b.WriteByte(c)
		}
		i++
	}
	return b.String()
}

const upperHex = "0123456789ABCDEF"

// web3MustEscape is the set of bytes that have to be percent-escaped for a
// device-local path to survive the round trip, and nothing more.
//
// It is DERIVED, not written down: for each byte, the URL is built with that
// byte literal in its path and asked whether net/url hands the same path
// back. A hand-kept table of "what a browser escapes" would be a second
// source of truth for the one property that matters here — that SourceURL
// writes exactly what ParseWeb3SourceURL will read — and would drift from
// net/url on the next Go release. Deriving it also keeps the escaping
// minimal: a byte a URL path may carry literally is left literal, so a URI
// that spelled an argument one way keeps that spelling and its cache key.
var web3MustEscape = func() [256]bool {
	var set [256]bool
	for c := 0; c < 256; c++ {
		if c == '/' {
			continue // the segment separator, written literally
		}
		raw := "https://h" + Web3HostSuffix + "/x" + string(rune(c))
		u, err := url.Parse(raw)
		set[c] = err != nil || u.EscapedPath() != "/x"+string(rune(c))
	}
	return set
}()

func isHexDigit(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

// URI is the canonical `web3://` form: lower-case contract, the chain id
// always written, then path, query and fragment as parsed. The path separator
// is written only when the URL carried one (Web3.HasPath), so the bare
// authority form of a URL with a query keeps the mPath ERC-6860 gives it.
func (w Web3) URI() string {
	var b strings.Builder
	b.WriteString("web3://0x")
	b.WriteString(w.Contract)
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(w.ChainID, 10))
	w.writePath(&b)
	w.writeTail(&b)
	return b.String()
}

// SourceURL is the device-local https URL the kiosk loads for w (see the
// package comment above).
func (w Web3) SourceURL() string {
	var b strings.Builder
	b.WriteString("https://")
	b.WriteString(w.Host())
	w.writePath(&b)
	w.writeTail(&b)
	return b.String()
}

// writePath writes mPath: "/" then the path when the URL carried a
// separator, nothing at all when it did not. An https URL with no path is
// what makes the distinction survive into the source string — and so into
// the sha256 cache key — because url.URL.EscapedPath tells "" from "/" and
// nothing else in the URL can carry it (Web3.HasPath). Chromium still
// *requests* such a URL with a "/", which is why the stored entry URL is
// respelled rather than taken from the source (offlinecache/web3.go).
func (w Web3) writePath(b *strings.Builder) {
	if !w.HasPath {
		return
	}
	b.WriteByte('/')
	b.WriteString(w.Path)
}

// Origin is the work's origin on the device, "https://<host>".
func (w Web3) Origin() string { return "https://" + w.Host() }

// Host is the device-local host of w's contract and chain.
func (w Web3) Host() string {
	contract, _ := hex.DecodeString(w.Contract)
	return web3Base32.EncodeToString(contract) + "." + strconv.FormatUint(w.ChainID, 10) + Web3HostSuffix
}

// ContractAddress is the contract in the 0x-prefixed form eth_call takes.
func (w Web3) ContractAddress() string { return "0x" + w.Contract }

func (w Web3) writeTail(b *strings.Builder) {
	if w.HasQuery {
		b.WriteByte('?')
		b.WriteString(w.Query)
	}
	if w.HasFragment {
		b.WriteByte('#')
		b.WriteString(w.Fragment)
	}
}

// ParseWeb3SourceURL is SourceURL's inverse. It accepts only the exact shape
// SourceURL builds: https, no userinfo, no port, a 32-character base32
// contract label, a decimal chain-id label, then Web3HostSuffix. Anything
// else, including look-alikes such as `<contract>.1.web3.invalid.example`, is
// not a web3 source.
func ParseWeb3SourceURL(raw string) (Web3, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return Web3{}, false
	}
	w, ok := ParseWeb3Host(u.Host)
	if !ok {
		return Web3{}, false
	}
	escaped := u.EscapedPath()
	p := strings.TrimPrefix(escaped, "/")
	if !cleanWeb3Path(p) {
		return Web3{}, false
	}
	w.Path = p
	w.Query, w.HasQuery = u.RawQuery, u.ForceQuery || u.RawQuery != ""
	// "" is the bare-authority form SourceURL writes for an absent mPath,
	// "/" is a present but empty one. The same canonicalisation as ParseWeb3
	// applies, so a source with neither a path nor a query — a shape
	// SourceURL never writes — reads back as the "/" it would be written as.
	w.HasPath = web3HasPath(escaped != "", w.HasQuery)
	// The fragment is taken from the raw string, not u.EscapedFragment, so it
	// round-trips exactly as SourceURL wrote it.
	_, w.Fragment, w.HasFragment = strings.Cut(raw, "#")
	return w, true
}

// ParseWeb3Host parses a device-local web3 host into its contract and chain.
// Path, query and fragment of the result are empty; HasPath is set, because
// with no query that is the canonical spelling (Web3.HasPath), so the
// result's SourceURL is the origin root rather than a bare authority.
func ParseWeb3Host(host string) (Web3, bool) {
	host = strings.ToLower(host)
	labels, ok := strings.CutSuffix(host, Web3HostSuffix)
	if !ok {
		return Web3{}, false
	}
	contractLabel, chainLabel, hasChain := strings.Cut(labels, ".")
	if !hasChain || len(contractLabel) != web3ContractLabelLen {
		return Web3{}, false
	}
	contract, err := web3Base32.DecodeString(contractLabel)
	// The round-trip check refuses a label that decodes but is not what
	// Host would have written (base32's trailing bits).
	if err != nil || len(contract) != 20 || web3Base32.EncodeToString(contract) != contractLabel {
		return Web3{}, false
	}
	id, err := strconv.ParseUint(chainLabel, 10, 64)
	if err != nil || id == 0 || chainLabel != strconv.FormatUint(id, 10) {
		return Web3{}, false
	}
	return Web3{Contract: hex.EncodeToString(contract), ChainID: id, HasPath: true}, true
}

// IsWeb3SourceURL reports whether raw is a device-local web3 source URL.
func IsWeb3SourceURL(raw string) bool {
	_, ok := ParseWeb3SourceURL(raw)
	return ok
}

func cutWeb3Scheme(raw string) (string, bool) {
	for _, s := range web3Schemes {
		if len(raw) >= len(s) && strings.EqualFold(raw[:len(s)], s) {
			return raw[len(s):], true
		}
	}
	return "", false
}

// isHex40Address reports whether s is a literal 0x-prefixed 20-byte address.
// ERC-4804 also allows a name, which would mean resolving it through a second
// trusted system; the device reads literal addresses only.
func isHex40Address(s string) bool {
	if len(s) != 42 || !strings.EqualFold(s[:2], "0x") {
		return false
	}
	_, err := hex.DecodeString(s[2:])
	return err == nil
}

// cleanWeb3Path reports whether an escaped web3 path is safe to carry as a
// URL path. Unlike cleanTreePath it does not treat the path as a filesystem
// tree: the segments are a method name and its arguments (ERC-4804), and a
// percent-encoded argument may legitimately decode to "//", "a/../b" or a
// backslash, so the decoded whole is never inspected. Only what a browser
// would act on in the escaped form is refused: a literal backslash (read as a
// slash), an empty segment other than a trailing one, and a segment that is
// itself "." or ".." once decoded (a dot segment, escaped or not).
func cleanWeb3Path(escaped string) bool {
	if escaped == "" {
		return true
	}
	if strings.Contains(escaped, `\`) {
		return false
	}
	segments := strings.Split(strings.TrimSuffix(escaped, "/"), "/")
	for _, seg := range segments {
		if seg == "" {
			return false
		}
		dec, err := url.PathUnescape(seg)
		if err != nil || dec == "." || dec == ".." {
			return false
		}
	}
	return true
}
