package web3

import (
	"encoding/hex"
	"fmt"
	"math/big"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
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
		return argument{typ: canonicalType(t), val: v}, nil
	}
	switch {
	case raw == "true" || raw == "false":
		return argument{typ: "bool", val: raw}, nil
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

// canonicalType spells the `uint`/`int` aliases as the ABI's `uint256`/
// `int256`. The function selector is the hash of the canonical signature, so
// hashing the alias verbatim would call a function that does not exist.
func canonicalType(t string) string {
	switch t {
	case "uint":
		return "uint256"
	case "int":
		return "int256"
	}
	return t
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
		n, err := parseABIInt(a.typ, a.val)
		if err != nil {
			return nil, nil, err
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

// parseABIInt reads val as a value of the integer type typ and refuses one
// the type cannot hold. word() panics on a value wider than 256 bits, and a
// value outside its declared width would be encoded as a different number, so
// the range is checked here, once, before any encoding.
//
// Decimal is the default notation: a leading zero is not an octal prefix, so
// `010` is ten. Only an explicit `0x` prefix selects hexadecimal.
func parseABIInt(typ, val string) (*big.Int, error) {
	neg := strings.HasPrefix(val, "-")
	digits := strings.TrimPrefix(val, "-")
	base := 10
	if hasHexPrefix(digits) {
		base, digits = 16, digits[2:]
	}
	// SetString would also accept a sign, underscores and a second prefix;
	// only bare digits are a URL integer.
	if digits == "" || (base == 10 && !isDecimalDigits(digits)) || (base == 16 && !isHexDigits(digits)) {
		return nil, fmt.Errorf("%w: %s value %q is not an integer", ErrUnsupported, typ, val)
	}
	n, _ := new(big.Int).SetString(digits, base)
	if neg {
		n.Neg(n)
	}
	bits := 256
	if w := strings.TrimPrefix(strings.TrimPrefix(typ, "u"), "int"); w != "" {
		bits, _ = strconv.Atoi(w)
	}
	var lo, hi *big.Int
	if strings.HasPrefix(typ, "uint") {
		lo = new(big.Int)
		hi = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(bits)), big.NewInt(1))
	} else {
		half := new(big.Int).Lsh(big.NewInt(1), uint(bits-1))
		lo = new(big.Int).Neg(half)
		hi = new(big.Int).Sub(half, big.NewInt(1))
	}
	if n.Cmp(lo) < 0 || n.Cmp(hi) > 0 {
		return nil, fmt.Errorf("%w: %s value is out of range", ErrUnsupported, typ)
	}
	return n, nil
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
	view, err := abiDynamicView(out, i)
	if err != nil {
		return nil, err
	}
	body := make([]byte, len(view))
	copy(body, view)
	return body, nil
}

// abiDynamicView is abiDynamic without the copy: the bounds are checked and
// the value is returned as a slice of out. A caller that has to charge a
// budget before it allocates reads the view first.
func abiDynamicView(out []byte, i int) ([]byte, error) {
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
	return out[start : start+length], nil
}

// jsonStringSize is the size encoding/json writes for s as a string,
// quotes included, counted without allocating. Escaping makes a value larger
// than its bytes (HTML characters, controls and invalid UTF-8 become six-byte
// \uXXXX escapes), so a budget on decoded bytes alone would let a 16 MiB
// answer marshal to about 96 MiB. Where json's exact choice is two bytes or
// six, six is counted: this is an upper bound, never an under-count.
func jsonStringSize(s []byte) int {
	n := 2
	for i := 0; i < len(s); {
		c := s[i]
		if c < utf8.RuneSelf {
			if c < 0x20 || c == '"' || c == '\\' || c == '<' || c == '>' || c == '&' {
				n += 6
			} else {
				n++
			}
			i++
			continue
		}
		r, size := utf8.DecodeRune(s[i:])
		if (r == utf8.RuneError && size == 1) || r == 0x2028 || r == 0x2029 {
			n += 6
		} else {
			n += size
		}
		i += size
	}
	return n
}

// decodeBudget charges the serialized size of every value of one
// `returns=` answer against MaxWorkBytes, before the value is copied out of
// the node's answer or marshaled. Each value passing its own bound is not
// enough: many offsets can point at the same large value, so the sum is what
// a hostile answer multiplies.
type decodeBudget struct{ used int }

func (b *decodeBudget) charge(n int) error {
	// n and used are each bounded by the budget, so the sum cannot overflow.
	if n > MaxWorkBytes-b.used {
		return fmt.Errorf("%w: decoded values exceed %d bytes", ErrTooLarge, MaxWorkBytes)
	}
	b.used += n
	return nil
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
	var budget decodeBudget
	if len(types) == 0 {
		// `returns=()`: hand back the raw result, hex-encoded.
		if err := budget.charge(2*len(out) + 6); err != nil {
			return nil, err
		}
		return []string{"0x" + hex.EncodeToString(out)}, nil
	}
	// The brackets and one comma per value.
	if err := budget.charge(2 + len(types)); err != nil {
		return nil, err
	}
	values := make([]string, 0, len(types))
	for i, t := range types {
		switch {
		case t == "string" || t == "bytes":
			view, err := abiDynamicView(out, i)
			if err != nil {
				return nil, err
			}
			if t == "string" {
				if err := budget.charge(jsonStringSize(view)); err != nil {
					return nil, err
				}
				values = append(values, string(view))
			} else {
				// 0x, two hex digits a byte, and the quotes.
				if err := budget.charge(2*len(view) + 4); err != nil {
					return nil, err
				}
				values = append(values, "0x"+hex.EncodeToString(view))
			}
		case intType.MatchString(t) || t == "bool" || t == "address" || fixedBytesType.MatchString(t):
			start := i * wordBytes
			if start+wordBytes > len(out) {
				return nil, fmt.Errorf("%w: value %d is past the %d-byte answer", ErrUnsupported, i, len(out))
			}
			w := out[start : start+wordBytes]
			var v string
			switch {
			case t == "address":
				v = "0x" + hex.EncodeToString(w[12:])
			case fixedBytesType.MatchString(t):
				// bytesN is left-aligned in its word and the rest is
				// padding, so only the first N bytes are the value.
				size, _ := strconv.Atoi(strings.TrimPrefix(t, "bytes"))
				v = "0x" + hex.EncodeToString(w[:size])
			default:
				v = hexNumber(w)
			}
			if err := budget.charge(len(v) + 2); err != nil {
				return nil, err
			}
			values = append(values, v)
		default:
			return nil, fmt.Errorf("%w: return type %q", ErrUnsupported, t)
		}
	}
	return values, nil
}
