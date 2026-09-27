package uarewrite

import (
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"math/big"
	"strings"
)

// isCID reports whether s is a syntactically valid IPFS CID: a bare CIDv0
// (base58btc, 46 chars starting "Qm") or a multibase-prefixed CIDv1 whose
// decoded bytes carry version 1, a codec varint, and a multihash whose
// declared digest length matches the bytes present.
//
// This is a structural parse, not a shape guess. The earlier 46-character
// floor was a stand-in for it and rejected valid CIDv1s over short
// multihashes (review F1 on #370). The parse costs nothing at request time,
// keeps API-ish `/ipfs/` paths out (they do not decode), and needs no
// dependency: the multibase alphabets a gateway path can carry are the five
// below, and the varint reader is a dozen lines.
//
// Multibase prefixes accepted: `b` base32 (the form every gateway emits),
// `v` base32hex, `k` base36, `f`/`F` base16, `u` base64url, `z` base58btc.
// Anything else is not a CID we would meet in an artwork URL and stays on
// the header rule.
func isCID(s string) bool {
	if len(s) < 2 {
		return false
	}
	var raw []byte
	var ok bool
	switch {
	case len(s) == 46 && strings.HasPrefix(s, "Qm"):
		// CIDv0: a bare base58btc sha2-256 multihash, 34 bytes.
		raw, ok = decodeBase58(s)
		return ok && len(raw) == 34 && raw[0] == 0x12 && raw[1] == 0x20
	case s[0] == 'b':
		raw, ok = decodeBase32Lower(s[1:])
	case s[0] == 'B':
		raw, ok = decodeBase32Lower(strings.ToLower(s[1:]))
	case s[0] == 'v':
		raw, ok = decodeBase32HexLower(s[1:])
	case s[0] == 'V':
		raw, ok = decodeBase32HexLower(strings.ToLower(s[1:]))
	case s[0] == 'k':
		raw, ok = decodeBigBase(s[1:], "0123456789abcdefghijklmnopqrstuvwxyz")
	case s[0] == 'f' || s[0] == 'F':
		var err error
		raw, err = hex.DecodeString(s[1:])
		ok = err == nil
	case s[0] == 'u':
		var err error
		raw, err = base64.RawURLEncoding.DecodeString(s[1:])
		ok = err == nil
	case s[0] == 'z':
		raw, ok = decodeBase58(s[1:])
	default:
		return false
	}
	if !ok {
		return false
	}
	return isCIDv1Bytes(raw)
}

// isCIDv1Bytes validates <version=1 varint><codec varint><multihash>.
func isCIDv1Bytes(b []byte) bool {
	version, n := readUvarint(b)
	if n <= 0 || version != 1 {
		return false
	}
	b = b[n:]
	if _, n = readUvarint(b); n <= 0 { // codec
		return false
	}
	b = b[n:]
	if _, n = readUvarint(b); n <= 0 { // multihash function code
		return false
	}
	b = b[n:]
	length, n := readUvarint(b)
	if n <= 0 {
		return false
	}
	b = b[n:]
	return length > 0 && uint64(len(b)) == length
}

// readUvarint decodes one unsigned LEB128 varint, returning the value and
// the bytes consumed, or n <= 0 on malformed or truncated input.
func readUvarint(b []byte) (uint64, int) {
	var x uint64
	var s uint
	for i, c := range b {
		if i >= 10 {
			return 0, -1
		}
		if c < 0x80 {
			if i == 9 && c > 1 {
				return 0, -1
			}
			return x | uint64(c)<<s, i + 1
		}
		x |= uint64(c&0x7f) << s
		s += 7
	}
	return 0, 0
}

var base32Lower = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

func decodeBase32Lower(s string) ([]byte, bool) {
	b, err := base32Lower.DecodeString(s)
	return b, err == nil
}

var base32HexLower = base32.NewEncoding("0123456789abcdefghijklmnopqrstuv").WithPadding(base32.NoPadding)

func decodeBase32HexLower(s string) ([]byte, bool) {
	b, err := base32HexLower.DecodeString(s)
	return b, err == nil
}

const base58Alphabet = "123456789ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz"

func decodeBase58(s string) ([]byte, bool) { return decodeBigBase(s, base58Alphabet) }

// decodeBigBase decodes a big-endian base-N string over alphabet, preserving
// leading zero digits as leading zero bytes (the base58/base36 convention).
func decodeBigBase(s string, alphabet string) ([]byte, bool) {
	if s == "" {
		return nil, false
	}
	base := big.NewInt(int64(len(alphabet)))
	n := new(big.Int)
	zeros := 0
	for i, r := range s {
		idx := strings.IndexRune(alphabet, r)
		if idx < 0 {
			return nil, false
		}
		if idx == 0 && i == zeros {
			zeros++
		}
		n.Mul(n, base)
		n.Add(n, big.NewInt(int64(idx)))
	}
	out := n.Bytes()
	if zeros > 0 {
		out = append(make([]byte, zeros), out...)
	}
	return out, true
}
