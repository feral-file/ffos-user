package web3

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
)

// ABI encoding for the argument types ERC-4804's auto mode can produce, and
// decoding for the result forms a work comes back in. This is deliberately
// not a general ABI codec: auto mode types arguments from how they are
// spelled in a URL, so the set is small and closed, and a general codec would
// be a much larger surface for a hostile node's answer to be read through.
//
// Every decoder treats the node's answer as hostile, as the onchfs ABI
// decoder does (ffos-user#427): no length may exceed the answer it came from,
// bounds are checked by subtraction so nothing can overflow, and bytes are
// copied out of the answer rather than aliased into it.

const wordBytes = 32

// fixedBytesType matches bytes1 … bytes32.
var fixedBytesType = regexp.MustCompile(`^bytes([1-9]|[12][0-9]|3[0-2])$`)

// intType matches uint/int with an optional bit width.
var intType = regexp.MustCompile(`^(u?int)(8|16|24|32|40|48|56|64|72|80|88|96|104|112|120|128|136|144|152|160|168|176|184|192|200|208|216|224|232|240|248|256)?$`)

// argument is one typed call argument.
type argument struct {
	typ string
	val string
}

// typeArgument applies ERC-4804's auto-mode typing rules to one path
// segment. `type!value` gives the type explicitly; otherwise the spelling
// decides, longest-specific first, so an address is never read as bytes.
func typeArgument(raw string) (argument, error) {
	if t, v, ok := strings.Cut(raw, "!"); ok {
		if t == "" {
			return argument{}, fmt.Errorf("%w: argument %q has an empty type", ErrUnsupported, raw)
		}
		return argument{typ: t, val: v}, nil
	}
	switch {
	case isDecimalDigits(raw):
		return argument{typ: "uint256", val: raw}, nil
	case hasHexPrefix(raw) && len(raw) == 66 && isHexDigits(raw[2:]):
		return argument{typ: "bytes32", val: raw}, nil
	case hasHexPrefix(raw) && len(raw) == 42 && isHexDigits(raw[2:]):
		return argument{typ: "address", val: raw}, nil
	case hasHexPrefix(raw) && len(raw)%2 == 0 && (len(raw) == 2 || isHexDigits(raw[2:])):
		return argument{typ: "bytes", val: raw}, nil
	}
	return argument{typ: "string", val: raw}, nil
}

func hasHexPrefix(s string) bool {
	return len(s) >= 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X')
}

// abiEncode encodes args as a function's argument tuple: a head word each,
// with dynamic values' bytes in the tail and an offset in the head.
func abiEncode(args []argument) ([]byte, error) {
	heads := make([][]byte, len(args))
	tails := make([][]byte, len(args))
	for i, a := range args {
		head, tail, err := encodeArgument(a)
		if err != nil {
			return nil, err
		}
		heads[i], tails[i] = head, tail
	}
	out := make([]byte, 0, wordBytes*len(args))
	var tail []byte
	base := wordBytes * len(args)
	for i := range args {
		if heads[i] != nil {
			out = append(out, heads[i]...)
			continue
		}
		out = append(out, word(big.NewInt(int64(base+len(tail))))...)
		tail = append(tail, tails[i]...)
	}
	return append(out, tail...), nil
}

// encodeArgument returns either a static head word or a dynamic tail, never
// both.
func encodeArgument(a argument) (head, tail []byte, err error) {
	switch {
	case intType.MatchString(a.typ):
		// A negative int is accepted and taken two's complement, as the
		// ABI does; the URL form for one is `int256!-1`.
		n, ok := new(big.Int).SetString(a.val, 0)
		if !ok {
			return nil, nil, fmt.Errorf("%w: %s value %q is not an integer", ErrUnsupported, a.typ, a.val)
		}
		return word(n), nil, nil
	case a.typ == "address":
		b, err := decodeFixedHex(a.val, 20)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: address %q: %w", ErrUnsupported, a.val, err)
		}
		// An address is right-aligned in its word, unlike bytesN.
		h := make([]byte, wordBytes)
		copy(h[wordBytes-20:], b)
		return h, nil, nil
	case a.typ == "bool":
		v := int64(0)
		switch strings.ToLower(a.val) {
		case "true", "1":
			v = 1
		case "false", "0":
		default:
			return nil, nil, fmt.Errorf("%w: bool value %q", ErrUnsupported, a.val)
		}
		return word(big.NewInt(v)), nil, nil
	case fixedBytesType.MatchString(a.typ):
		size, _ := strconv.Atoi(strings.TrimPrefix(a.typ, "bytes"))
		b, err := decodeFixedHex(a.val, size)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: %s %q: %w", ErrUnsupported, a.typ, a.val, err)
		}
		// bytesN is left-aligned in its word.
		h := make([]byte, wordBytes)
		copy(h, b)
		return h, nil, nil
	case a.typ == "bytes":
		b, err := decodeHexBytes(a.val)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: bytes %q: %w", ErrUnsupported, a.val, err)
		}
		return nil, dynamicTail(b), nil
	case a.typ == "string":
		return nil, dynamicTail([]byte(a.val)), nil
	}
	return nil, nil, fmt.Errorf("%w: argument type %q", ErrUnsupported, a.typ)
}

// dynamicTail is a length word then the bytes, padded to a whole word.
func dynamicTail(b []byte) []byte {
	out := append(word(big.NewInt(int64(len(b)))), b...)
	if pad := len(b) % wordBytes; pad != 0 {
		out = append(out, make([]byte, wordBytes-pad)...)
	}
	return out
}

// word is n as one big-endian 32-byte word, two's complement for a negative.
func word(n *big.Int) []byte {
	out := make([]byte, wordBytes)
	if n.Sign() >= 0 {
		n.FillBytes(out)
		return out
	}
	// 2^256 + n, which is n's two's complement in 32 bytes.
	mod := new(big.Int).Lsh(big.NewInt(1), 256)
	new(big.Int).Add(mod, n).FillBytes(out)
	return out
}

func decodeFixedHex(s string, size int) ([]byte, error) {
	if !hasHexPrefix(s) {
		return nil, fmt.Errorf("not 0x-prefixed")
	}
	b, err := hex.DecodeString(s[2:])
	if err != nil {
		return nil, err
	}
	if len(b) != size {
		return nil, fmt.Errorf("%d bytes, want %d", len(b), size)
	}
	return b, nil
}

func decodeHexBytes(s string) ([]byte, error) {
	if !hasHexPrefix(s) {
		return nil, fmt.Errorf("not 0x-prefixed")
	}
	return hex.DecodeString(s[2:])
}

// readWord returns the i-th word of out as a length or an offset into out.
//
// It returns an int, not a uint64, deliberately: every value a well-formed
// answer can carry here is bounded by len(out), which is an int, so the
// bound is checked once at the boundary and no caller has to convert — a
// conversion is exactly where a hostile 2^64-1 would become a negative
// index. A word whose high 24 bytes are not zero, or whose value exceeds
// len(out), is refused: neither is a length or an offset into an answer this
// size, so it is a hostile or broken node.
func readWord(out []byte, i int) (int, error) {
	start := i * wordBytes
	if i < 0 || start < 0 || start+wordBytes > len(out) {
		return 0, fmt.Errorf("%w: word %d is past the %d-byte answer", ErrUnsupported, i, len(out))
	}
	w := out[start : start+wordBytes]
	for _, b := range w[:wordBytes-8] {
		if b != 0 {
			return 0, fmt.Errorf("%w: word %d does not fit an offset or length", ErrUnsupported, i)
		}
	}
	var n uint64
	for _, b := range w[wordBytes-8:] {
		n = n<<8 | uint64(b)
	}
	if n > uint64(len(out)) {
		return 0, fmt.Errorf("%w: word %d is %d, past the %d-byte answer", ErrUnsupported, i, n, len(out))
	}
	// Safe by the check above: n <= len(out), and len(out) is an int.
	return int(n), nil
}

// abiDynamic reads the dynamic value whose offset is in head word i: the
// offset points at a length word, and the bytes follow it. The result is
// copied out of out, so nothing holds the node's buffer alive.
func abiDynamic(out []byte, i int) ([]byte, error) {
	off, err := readWord(out, i)
	if err != nil {
		return nil, err
	}
	if off%wordBytes != 0 {
		return nil, fmt.Errorf("%w: value %d is at a %d-byte offset, not a word boundary", ErrUnsupported, i, off)
	}
	length, err := readWord(out, off/wordBytes)
	if err != nil {
		return nil, err
	}
	start := off + wordBytes
	// Checked by subtraction: start and length are both already bounded by
	// len(out), so neither this sum nor the slice can overflow.
	if start > len(out) || length > len(out)-start {
		return nil, fmt.Errorf("%w: a %d-byte value at offset %d is past the %d-byte answer", ErrUnsupported, length, start, len(out))
	}
	if length > MaxWorkBytes {
		return nil, fmt.Errorf("%w: %d bytes, over %d", ErrTooLarge, length, MaxWorkBytes)
	}
	body := make([]byte, length)
	copy(body, out[start:start+length])
	return body, nil
}

// hexNumber renders a word the way the reference implementation's hex()
// does: no leading zeros, and "0x0" rather than a bare "0x" for zero.
func hexNumber(w []byte) string {
	s := strings.TrimLeft(hex.EncodeToString(w), "0")
	if s == "" {
		s = "0"
	}
	return "0x" + s
}

// abiDecodeValues decodes out under an explicit `returns=` type list. The
// values are rendered the way the reference implementation renders them, so
// a work that reads its own JSON sees the same shape on the device.
func abiDecodeValues(out []byte, types []string) ([]string, error) {
	if len(types) == 0 {
		// `returns=()`: hand back the raw result, hex-encoded.
		return []string{"0x" + hex.EncodeToString(out)}, nil
	}
	values := make([]string, 0, len(types))
	for i, t := range types {
		switch {
		case t == "string" || t == "bytes":
			b, err := abiDynamic(out, i)
			if err != nil {
				return nil, err
			}
			if t == "string" {
				values = append(values, string(b))
			} else {
				values = append(values, "0x"+hex.EncodeToString(b))
			}
		case intType.MatchString(t) || t == "bool":
			start := i * wordBytes
			if start+wordBytes > len(out) {
				return nil, fmt.Errorf("%w: value %d is past the %d-byte answer", ErrUnsupported, i, len(out))
			}
			values = append(values, hexNumber(out[start:start+wordBytes]))
		case t == "address":
			start := i * wordBytes
			if start+wordBytes > len(out) {
				return nil, fmt.Errorf("%w: value %d is past the %d-byte answer", ErrUnsupported, i, len(out))
			}
			values = append(values, "0x"+hex.EncodeToString(out[start+12:start+wordBytes]))
		case fixedBytesType.MatchString(t):
			start := i * wordBytes
			if start+wordBytes > len(out) {
				return nil, fmt.Errorf("%w: value %d is past the %d-byte answer", ErrUnsupported, i, len(out))
			}
			values = append(values, "0x"+hex.EncodeToString(out[start:start+wordBytes]))
		default:
			return nil, fmt.Errorf("%w: return type %q", ErrUnsupported, t)
		}
	}
	return values, nil
}
