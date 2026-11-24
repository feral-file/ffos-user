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
//   - Path, query and fragment are carried byte for byte.
//
// One origin per contract-and-chain, not per work: two editions of one
// contract (Art Blocks' getTokenHtml/<core>/<tokenId>) share an origin and so
// share localStorage and service workers. That is the cost of keeping the
// host invertible; onchfs can afford an origin per work because its whole
// identity is one 32-byte root (owner's call, feral-file#3599).
//
// ParseWeb3 and ParseWeb3SourceURL are each other's inverse; nothing else may
// build or parse either form.

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
	// Query and Fragment are raw, without "?" or "#". HasQuery and
	// HasFragment tell an empty one from an absent one, so the round trip is
	// byte for byte. The query carries ERC-4804's `returns=` attribute.
	Query       string
	HasQuery    bool
	Fragment    string
	HasFragment bool
}

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
	authority, after, _ := strings.Cut(rest, "/")
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
	w.Path = after
	return w, true
}

// URI is the canonical `web3://` form: lower-case contract, the chain id
// always written, then path, query and fragment as parsed.
func (w Web3) URI() string {
	var b strings.Builder
	b.WriteString("web3://0x")
	b.WriteString(w.Contract)
	b.WriteByte(':')
	b.WriteString(strconv.FormatUint(w.ChainID, 10))
	b.WriteByte('/')
	b.WriteString(w.Path)
	w.writeTail(&b)
	return b.String()
}

// SourceURL is the device-local https URL the kiosk loads for w (see the
// package comment above).
func (w Web3) SourceURL() string {
	var b strings.Builder
	b.WriteString("https://")
	b.WriteString(w.Host())
	b.WriteByte('/')
	b.WriteString(w.Path)
	w.writeTail(&b)
	return b.String()
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
	p := strings.TrimPrefix(u.EscapedPath(), "/")
	if !cleanWeb3Path(p) {
		return Web3{}, false
	}
	w.Path = p
	w.Query, w.HasQuery = u.RawQuery, u.ForceQuery || u.RawQuery != ""
	// The fragment is taken from the raw string, not u.EscapedFragment, so it
	// round-trips exactly as SourceURL wrote it.
	_, w.Fragment, w.HasFragment = strings.Cut(raw, "#")
	return w, true
}

// ParseWeb3Host parses a device-local web3 host into its contract and chain.
// Path, query and fragment of the result are empty.
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
	return Web3{Contract: hex.EncodeToString(contract), ChainID: id}, true
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
