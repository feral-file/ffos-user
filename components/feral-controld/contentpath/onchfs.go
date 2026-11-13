package contentpath

import (
	"encoding/base32"
	"encoding/hex"
	"net/url"
	"path"
	"strings"
)

// onchfs is the on-chain file system fxhash wrote generative works into
// (Ethereum, Base, Tezos). A work is addressed `onchfs://<root>[/path]
// [?query][#fragment]`, where root is the 64-hex keccak address of its root
// inode, optionally preceded by a CAIP-2 authority naming the chain
// (`onchfs://eip155:1/<root>`, `onchfs://tezos:NetXdQprcVkpaWU:KT1…/<root>`;
// fxhash/onchfs doc/docs/concepts/uris.md). No host serves it: fxhash's
// gateway went with fxhash on 2026-08-17 (ffos-user#427), so the device reads
// the tree from chain and serves it to the kiosk itself through offline-cache
// replay.
//
// The kiosk can only load http(s), so a work is given a device-local source
// URL that names the same tree:
//
//	https://<base32(root)>[.<chain>].onchfs.invalid/<path>?<query>#<fragment>
//
//   - One host per root gives every work its own origin, so one work's storage
//     and service workers are never another's.
//   - `.invalid` is reserved (RFC 6761) and never resolves: a request the
//     replay does not answer fails at once and reaches no network.
//   - base32 (52 characters) keeps the root inside one DNS label (63 max);
//     hex would need 64.
//   - path, query and fragment are carried byte for byte. fxhash works read
//     their seed from the query (`?fxhash=…&fxiteration=…`).
//
// The mapping is a pure function of the URI, so the offline cache, which keys
// a work by sha256 of its exact source string, sees one stable key per work.
// ParseOnchfs and ParseOnchfsSourceURL are each other's inverse; nothing else
// may build or parse either form.

// OnchfsHostSuffix ends every device-local onchfs source host.
const OnchfsHostSuffix = ".onchfs.invalid"

const onchfsScheme = "onchfs://"

// OnchfsChain is a chain the device can read onchfs from.
type OnchfsChain struct {
	CAIP2    string // e.g. "eip155:1"
	Label    string // the host label carrying an explicit authority
	Contract string // the onchfs file system contract fxhash deployed there
}

// OnchfsChains are the mainnets fxhash deployed onchfs on (onchfs-js
// DEFAULT_CONTRACTS). Testnets are not played by a wall.
var OnchfsChains = []OnchfsChain{
	{CAIP2: "eip155:1", Label: "eth", Contract: "0x9e0f2864c6f125bbf599df6ca6e6c3774c5b2e04"},
	{CAIP2: "eip155:8453", Label: "base", Contract: "0x2983008f292a43f208bba0275afd7e9b3d39af3b"},
	{CAIP2: "tezos:NetXdQprcVkpaWU", Label: "tezos", Contract: "KT1Ae7dT1gsLw2tRnUMXSCmEyF74KVkM6LUo"},
}

// onchfsBase32 is RFC 4648 base32 in lower case without padding: hosts are
// case-insensitive and Chromium lower-cases them.
var onchfsBase32 = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// Onchfs is one parsed onchfs reference.
type Onchfs struct {
	// Root is the 64-hex address of the root inode, lower case.
	Root string
	// Chain is the CAIP-2 id an explicit authority named, "" when the URI
	// carries none (the usual case: the indexer stores the bare form) and
	// the chain has to be found by asking each one.
	Chain string
	// Path is the escaped path inside the tree, without a leading slash; ""
	// is the root (a directory serves its index.html).
	Path string
	// Query and Fragment are raw, without "?" or "#". HasQuery and
	// HasFragment tell an empty one from an absent one, so the round trip
	// is byte for byte.
	Query       string
	HasQuery    bool
	Fragment    string
	HasFragment bool
}

// ParseOnchfs parses an `onchfs://` URI. ok is false for anything that is not
// a well-formed reference to a tree the device can read: a root that is not
// 64 hex, a path that climbs out of the tree, an authority naming a chain or
// contract other than OnchfsChains.
func ParseOnchfs(raw string) (Onchfs, bool) {
	if len(raw) < len(onchfsScheme) || !strings.EqualFold(raw[:len(onchfsScheme)], onchfsScheme) {
		return Onchfs{}, false
	}
	var o Onchfs
	rest := raw[len(onchfsScheme):]
	rest, o.Fragment, o.HasFragment = strings.Cut(rest, "#")
	rest, o.Query, o.HasQuery = strings.Cut(rest, "?")
	first, after, _ := strings.Cut(rest, "/")
	if !isHex64(first) {
		chain, ok := onchfsAuthority(first)
		if !ok {
			return Onchfs{}, false
		}
		o.Chain = chain
		first, after, _ = strings.Cut(after, "/")
	}
	if !isHex64(first) {
		return Onchfs{}, false
	}
	o.Root = strings.ToLower(first)
	if !cleanTreePath(after) {
		return Onchfs{}, false
	}
	o.Path = after
	return o, true
}

// URI is the canonical `onchfs://` form: lower-case root, the authority only
// when one was given (as its CAIP-2 chain), path, query and fragment as
// parsed.
func (o Onchfs) URI() string {
	var b strings.Builder
	b.WriteString(onchfsScheme)
	if o.Chain != "" {
		b.WriteString(o.Chain)
		b.WriteByte('/')
	}
	b.WriteString(o.Root)
	b.WriteByte('/')
	b.WriteString(o.Path)
	o.writeTail(&b)
	return b.String()
}

// SourceURL is the device-local https URL the kiosk loads for o (see the
// package comment above). The root of a tree is always "/", so a bare
// `onchfs://<root>?fxhash=…` and `onchfs://<root>/?fxhash=…` are the same
// work and the same cache key.
func (o Onchfs) SourceURL() string {
	var b strings.Builder
	b.WriteString("https://")
	b.WriteString(o.Host())
	b.WriteByte('/')
	b.WriteString(o.Path)
	o.writeTail(&b)
	return b.String()
}

// Origin is the work's origin on the device, "https://<host>".
func (o Onchfs) Origin() string { return "https://" + o.Host() }

// Host is the device-local host of o's tree.
func (o Onchfs) Host() string {
	root, _ := hex.DecodeString(o.Root)
	host := onchfsBase32.EncodeToString(root)
	if c, ok := OnchfsChainByCAIP2(o.Chain); ok {
		host += "." + c.Label
	}
	return host + OnchfsHostSuffix
}

// OnchfsChainByCAIP2 looks a chain up by its CAIP-2 id.
func OnchfsChainByCAIP2(caip2 string) (OnchfsChain, bool) {
	for _, c := range OnchfsChains {
		if c.CAIP2 == caip2 {
			return c, true
		}
	}
	return OnchfsChain{}, false
}

func (o Onchfs) writeTail(b *strings.Builder) {
	if o.HasQuery {
		b.WriteByte('?')
		b.WriteString(o.Query)
	}
	if o.HasFragment {
		b.WriteByte('#')
		b.WriteString(o.Fragment)
	}
}

// ParseOnchfsSourceURL is SourceURL's inverse. It accepts only the exact
// shape SourceURL builds: https, no userinfo, no port, a 52-character base32
// root label, an optional known chain label, then OnchfsHostSuffix. Anything
// else, including look-alikes such as `<root>.onchfs.invalid.example`, is
// not an onchfs source.
func ParseOnchfsSourceURL(raw string) (Onchfs, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.User != nil || u.Port() != "" || u.Opaque != "" {
		return Onchfs{}, false
	}
	o, ok := ParseOnchfsHost(u.Host)
	if !ok {
		return Onchfs{}, false
	}
	p := strings.TrimPrefix(u.EscapedPath(), "/")
	if !cleanTreePath(p) {
		return Onchfs{}, false
	}
	o.Path = p
	o.Query, o.HasQuery = u.RawQuery, u.ForceQuery || u.RawQuery != ""
	// The fragment is taken from the raw string, not u.EscapedFragment, so it
	// round-trips exactly as SourceURL wrote it.
	_, o.Fragment, o.HasFragment = strings.Cut(raw, "#")
	return o, true
}

// ParseOnchfsHost parses a device-local onchfs host into its root and chain.
// Path, query and fragment of the result are empty.
func ParseOnchfsHost(host string) (Onchfs, bool) {
	host = strings.ToLower(host)
	labels, ok := strings.CutSuffix(host, OnchfsHostSuffix)
	if !ok {
		return Onchfs{}, false
	}
	rootLabel, chainLabel, hasChain := strings.Cut(labels, ".")
	if len(rootLabel) != 52 {
		return Onchfs{}, false
	}
	root, err := onchfsBase32.DecodeString(rootLabel)
	if err != nil || len(root) != 32 || onchfsBase32.EncodeToString(root) != rootLabel {
		return Onchfs{}, false
	}
	o := Onchfs{Root: hex.EncodeToString(root)}
	if hasChain {
		for _, c := range OnchfsChains {
			if c.Label == chainLabel {
				o.Chain = c.CAIP2
			}
		}
		if o.Chain == "" {
			return Onchfs{}, false
		}
	}
	return o, true
}

// IsOnchfsSourceURL reports whether raw is a device-local onchfs source URL.
// Callers use it to keep these URLs away from anything that would dial them.
func IsOnchfsSourceURL(raw string) bool {
	_, ok := ParseOnchfsSourceURL(raw)
	return ok
}

// onchfsAuthority maps a URI authority to a chain the device reads. The
// ABNF allows `<namespace>[:<chain id>[:<contract>]]`. A namespace alone
// names its mainnet, as onchfs-js resolves it. A contract is accepted only
// when it is the contract fxhash deployed on that chain: the device reads no
// other, and resolving such a URI from the default contract would show a
// different work.
func onchfsAuthority(a string) (string, bool) {
	parts := strings.Split(a, ":")
	if len(parts) > 3 {
		return "", false
	}
	var caip2 string
	switch ns := strings.ToLower(parts[0]); {
	case ns == "eip155" && len(parts) == 1:
		caip2 = "eip155:1"
	case ns == "eip155":
		caip2 = "eip155:" + parts[1]
	case ns == "tezos" && (len(parts) == 1 || strings.EqualFold(parts[1], "mainnet") || parts[1] == "NetXdQprcVkpaWU"):
		caip2 = "tezos:NetXdQprcVkpaWU"
	default:
		return "", false
	}
	c, ok := OnchfsChainByCAIP2(caip2)
	if !ok {
		return "", false
	}
	if len(parts) == 3 && !strings.EqualFold(strings.TrimPrefix(parts[2], "0x"), strings.TrimPrefix(c.Contract, "0x")) {
		return "", false
	}
	return caip2, true
}

// cleanTreePath reports whether an escaped path stays inside its tree: no
// "." or ".." segment (escaped or not), no backslash, no empty segment
// except a trailing slash (`dir/` serves dir/index.html).
func cleanTreePath(escaped string) bool {
	if escaped == "" {
		return true
	}
	unescaped, err := url.PathUnescape(escaped)
	if err != nil || strings.Contains(unescaped, `\`) {
		return false
	}
	trimmed := strings.TrimSuffix(unescaped, "/")
	return trimmed != "" && path.Clean("/"+trimmed) == "/"+trimmed
}

func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}
