package web3_test

import (
	"bytes"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/web3"
)

const reviewContract = "0x4f32740cf412d647a622ba27c36fa4b76f71601f"

func planFor(t *testing.T, path, query, mode string) (web3.Plan, error) {
	t.Helper()
	return web3.NewPlan(web3.Request{Contract: reviewContract, ChainID: 1, Path: path, Query: query}, mode)
}

func wantCalldata(t *testing.T, sig string, args ...string) []byte {
	t.Helper()
	out := web3.SelectorForTest(sig)
	for _, a := range args {
		b, err := hex.DecodeString(a)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, b...)
	}
	return out
}

func pad32(n string) string { return strings.Repeat("0", 64-len(n)) + n }

// An integer outside its type must be refused, not panic in word().
func TestIntegersAreRangeCheckedAgainstTheirType(t *testing.T) {
	two256 := "115792089237316195423570985008687907853269984665640564039457584007913129639936"
	for _, path := range []string{
		"render/" + two256,
		"render/uint8!256",
		"render/uint256!-1",
		"render/int8!128",
		"render/int8!-129",
		"render/int256!57896044618658097711785492504343953926634992332820282019728792003956564819968",
		"render/uint!0xzz",
		"render/uint!",
	} {
		t.Run(path, func(t *testing.T) {
			if _, err := planFor(t, path, "", web3.ModeAuto); !errors.Is(err, web3.ErrUnsupported) {
				t.Fatalf("err = %v, want ErrUnsupported", err)
			}
		})
	}
	for path, arg := range map[string]string{
		"render/uint8!255": pad32("ff"),
		"render/int8!-128": strings.Repeat("f", 62) + "80",
		"render/uint!0x10": pad32("10"),
	} {
		typ := strings.Split(strings.Split(path, "/")[1], "!")[0]
		if typ == "uint" {
			typ = "uint256"
		}
		p, err := planFor(t, path, "", web3.ModeAuto)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want := wantCalldata(t, "render("+typ+")", arg); !bytes.Equal(p.Calldata, want) {
			t.Errorf("%s calldata = %x, want %x", path, p.Calldata, want)
		}
	}
}

// Decimal digit strings are base ten whatever their leading zeros.
func TestDecimalArgumentsAreBaseTen(t *testing.T) {
	for path, n := range map[string]string{"render/010": "a", "render/08": "8", "render/0": "0"} {
		p, err := planFor(t, path, "", web3.ModeAuto)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if want := wantCalldata(t, "render(uint256)", pad32(n)); !bytes.Equal(p.Calldata, want) {
			t.Errorf("%s calldata = %x, want %x", path, p.Calldata, want)
		}
	}
}

// true/false are booleans, and the uint/int aliases hash as their canonical
// types, so the selector names the function the contract actually has.
func TestBooleansAndIntegerAliasesAreCanonical(t *testing.T) {
	cases := []struct{ path, sig, arg string }{
		{"render/true", "render(bool)", pad32("1")},
		{"render/false", "render(bool)", pad32("0")},
		{"render/uint!1", "render(uint256)", pad32("1")},
		{"render/int!1", "render(int256)", pad32("1")},
	}
	for _, c := range cases {
		p, err := planFor(t, c.path, "", web3.ModeAuto)
		if err != nil {
			t.Fatalf("%s: %v", c.path, err)
		}
		if want := wantCalldata(t, c.sig, c.arg); !bytes.Equal(p.Calldata, want) {
			t.Errorf("%s calldata = %x, want %x", c.path, p.Calldata, want)
		}
	}
}

// bytesN is left-aligned in its word; the rest is padding.
func TestFixedBytesReturnsDropThePadding(t *testing.T) {
	p, err := planFor(t, "f", "returns=(bytes4,bytes32)", web3.ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	w2 := strings.Repeat("ab", 32)
	body, err := p.Decode(mustHex(t, "deadbeef"+strings.Repeat("00", 28)+w2))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf(`["0xdeadbeef","0x%s"]`, w2); string(body) != want {
		t.Errorf("body = %s, want %s", body, want)
	}
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Many offsets pointing at one large value must be charged cumulatively,
// before any copy, and escaping expansion counts too.
func TestDecodeChargesAnAggregateBudget(t *testing.T) {
	const n = 32
	types := strings.TrimSuffix(strings.Repeat("string,", n), ",")
	p, err := planFor(t, "f", "returns=("+types+")", web3.ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	for name, fill := range map[string]byte{"plain": 'a', "escaped": '<'} {
		t.Run(name, func(t *testing.T) {
			const strLen = 1 << 20
			head := n * 32
			out := make([]byte, 0, head+32+strLen)
			for i := 0; i < n; i++ {
				out = append(out, mustHex(t, pad32(fmt.Sprintf("%x", head)))...)
			}
			out = append(out, mustHex(t, pad32(fmt.Sprintf("%x", strLen)))...)
			out = append(out, bytes.Repeat([]byte{fill}, strLen)...)
			if _, err := p.Decode(out); !errors.Is(err, web3.ErrTooLarge) {
				t.Fatalf("err = %v, want ErrTooLarge", err)
			}
		})
	}
}

// Manual mode hands the query to the contract untouched and decodes ABI
// bytes; the type comes from the whole path.
func TestManualModeIsNotReadAsAutoMode(t *testing.T) {
	for _, q := range []string{"returns=hello", "returns=(uint256)", "returns=(bogus"} {
		p, err := planFor(t, "index.html", q, web3.ModeManual)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if want := "/index.html?" + q; string(p.Calldata) != want {
			t.Errorf("calldata = %q, want %q", p.Calldata, want)
		}
		if p.HasReturns || p.ContentType != "text/html" {
			t.Errorf("query %q: HasReturns=%v ContentType=%q", q, p.HasReturns, p.ContentType)
		}
	}
	p, err := planFor(t, "image.svg", "", web3.ModeManual)
	if err != nil {
		t.Fatal(err)
	}
	if p.ContentType != "image/svg+xml" {
		t.Errorf("manual /image.svg ContentType = %q, want image/svg+xml", p.ContentType)
	}
}

// ERC-6860 auto mode renders a bool as a JSON boolean, not a hex word
// (round 5 review, F9). A non-canonical word is refused, not rounded.
func TestBoolReturnsAreNativeJSONBooleans(t *testing.T) {
	p, err := planFor(t, "flag", "returns=(bool,uint256,bool)", web3.ModeAuto)
	if err != nil {
		t.Fatal(err)
	}
	out := mustHex(t, pad32("1")+pad32("1")+pad32("0"))
	body, err := p.Decode(out)
	if err != nil {
		t.Fatal(err)
	}
	if want := `[true,"0x1",false]`; string(body) != want {
		t.Errorf("body = %s, want %s", body, want)
	}
	for _, word := range []string{pad32("2"), "01" + strings.Repeat("00", 31)} {
		q, err := planFor(t, "flag", "returns=(bool)", web3.ModeAuto)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := q.Decode(mustHex(t, word)); !errors.Is(err, web3.ErrUnsupported) {
			t.Errorf("word %s: err = %v, want ErrUnsupported", word, err)
		}
	}
}

// An empty returns/returnTypes attribute is the default decoding, and is not
// `returns=()` (round 5 review, F10).
func TestAnEmptyReturnsAttributeIsDefaultDecoding(t *testing.T) {
	for _, q := range []string{"returns=", "returnTypes=", "returns=&x=1", "returns=%20"} {
		p, err := planFor(t, "artpiece", q, web3.ModeAuto)
		if err != nil {
			t.Fatalf("query %q: %v", q, err)
		}
		if p.HasReturns {
			t.Errorf("query %q: HasReturns = true, want default decoding", q)
		}
	}
	p, err := planFor(t, "artpiece", "returns=()", web3.ModeAuto)
	if err != nil || !p.HasReturns || len(p.Returns) != 0 {
		t.Errorf("returns=(): HasReturns=%v Returns=%v err=%v, want the raw-result mode", p.HasReturns, p.Returns, err)
	}
}

// ERC-6860 makes returnTypes an alias of returns and the last attribute wins,
// whichever name it used (round 6 review, F13).
func TestTheLastReturnsAliasWinsInQueryOrder(t *testing.T) {
	for _, c := range []struct {
		query string
		want  []string
		has   bool
	}{
		{"returns=(uint256)&returnTypes=(bool)", []string{"bool"}, true},
		{"returnTypes=(bool)&returns=(uint256)", []string{"uint256"}, true},
		{"returns=(uint256)&returnTypes=", nil, false},
		{"returnTypes=(bool)&returns=(uint256)&returns=(address)", []string{"address"}, true},
	} {
		p, err := planFor(t, "artpiece", c.query, web3.ModeAuto)
		if err != nil {
			t.Fatalf("query %q: %v", c.query, err)
		}
		if p.HasReturns != c.has || strings.Join(p.Returns, ",") != strings.Join(c.want, ",") {
			t.Errorf("query %q: HasReturns=%v Returns=%v, want %v %v", c.query, p.HasReturns, p.Returns, c.has, c.want)
		}
	}
}
