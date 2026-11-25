package web3_test

import (
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/web3"
)

// The findings feralfile-bot raised on this branch were each one example of a
// mechanism: one integer that panicked, one bytesN that kept its padding. The
// tests beside this file pin those examples. These tests derive the whole set
// the mechanism covers and walk it, so the sibling case is not the next
// round's finding.

// abiIntTypes derives every integer type ERC-4804's auto mode can produce:
// the bare aliases, and every width the ABI allows in both signednesses.
func abiIntTypes() []string {
	types := []string{"uint", "int"}
	for bits := 8; bits <= 256; bits += 8 {
		types = append(types, fmt.Sprintf("uint%d", bits), fmt.Sprintf("int%d", bits))
	}
	return types
}

// widthOf is the declared width of an integer type, with the bare aliases
// meaning 256 as the ABI says.
func widthOf(t string) int {
	w := strings.TrimPrefix(strings.TrimPrefix(t, "u"), "int")
	if w == "" {
		return 256
	}
	n, err := strconv.Atoi(w)
	if err != nil {
		panic("derivation is wrong: " + t)
	}
	return n
}

// TestEveryIntegerTypeIsBoundedAtItsOwnEdges walks all 66 integer types and
// checks the four values that decide the bound: the two it must hold, and the
// two just outside it that must be refused rather than encoded as something
// else — or, as the P1 finding showed, rather than panicking inside
// big.Int.FillBytes in an unrecovered goroutine.
func TestEveryIntegerTypeIsBoundedAtItsOwnEdges(t *testing.T) {
	one := big.NewInt(1)
	for _, typ := range abiIntTypes() {
		bits := widthOf(typ)
		var min, max *big.Int
		if strings.HasPrefix(typ, "uint") {
			min = big.NewInt(0)
			max = new(big.Int).Sub(new(big.Int).Lsh(one, uint(bits)), one)
		} else {
			half := new(big.Int).Lsh(one, uint(bits-1))
			min = new(big.Int).Neg(half)
			max = new(big.Int).Sub(half, one)
		}
		t.Run(typ, func(t *testing.T) {
			for _, in := range []*big.Int{min, max} {
				if _, err := planOneArg(t, typ, in.String()); err != nil {
					t.Errorf("%s!%s is inside the type and must encode, got %v", typ, in, err)
				}
			}
			for _, out := range []*big.Int{new(big.Int).Sub(min, one), new(big.Int).Add(max, one)} {
				_, err := planOneArg(t, typ, out.String())
				if !errors.Is(err, web3.ErrUnsupported) {
					t.Errorf("%s!%s is outside the type and must be refused, got %v", typ, out, err)
				}
			}
		})
	}
}

// planOneArg builds a one-argument auto-mode plan and recovers a panic as a
// test failure. The recover is the point, not boilerplate: the P1 finding was
// a panic inside big.Int.FillBytes, and an uncaught one here would take the
// whole test binary down with it, losing every other width's result in this
// sweep — exactly as it took the daemon down in a background job.
func planOneArg(t *testing.T, typ, val string) (p web3.Plan, err error) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panicked: %v", r)
		}
	}()
	return web3.NewPlan(web3.Request{
		Contract: reviewContract, ChainID: 1,
		Path: "render/" + typ + "!" + val,
	}, web3.ModeAuto)
}

// TestNoDigitStringIsReadAsAnotherBase walks every leading-zero shape a token
// id can arrive in. `010` as octal 8 was the finding; the mechanism is that a
// URL integer is decimal, so every one of these is its decimal value.
func TestNoDigitStringIsReadAsAnotherBase(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int64
	}{
		{"0", 0}, {"00", 0}, {"000", 0},
		{"7", 7}, {"07", 7}, {"007", 7},
		{"8", 8}, {"08", 8}, {"008", 8},
		{"9", 9}, {"09", 9},
		{"10", 10}, {"010", 10}, {"0010", 10},
		{"19", 19}, {"019", 19},
		{"78000000", 78000000}, {"078000000", 78000000},
	} {
		p, err := web3.NewPlan(web3.Request{
			Contract: reviewContract, ChainID: 1,
			Path: "render/" + tc.in,
		}, web3.ModeAuto)
		if err != nil {
			t.Errorf("/render/%s must encode as a decimal uint256, got %v", tc.in, err)
			continue
		}
		// The selector is render(uint256) and the one argument is the value.
		want := hex.EncodeToString(web3.SelectorForTest("render(uint256)")) +
			fmt.Sprintf("%064x", tc.want)
		if got := hex.EncodeToString(p.Calldata); got != want {
			t.Errorf("/render/%s:\n got %s\nwant %s (decimal %d)", tc.in, got, want, tc.want)
		}
	}
}

// TestEveryFixedBytesReturnWidthDropsItsPadding walks bytes1 through bytes32.
// The finding named bytes4; the mechanism is that bytesN is left-aligned in
// its word and only the first N bytes are the value.
func TestEveryFixedBytesReturnWidthDropsItsPadding(t *testing.T) {
	// A word whose every byte is distinct, so a wrong width is visible.
	var word [32]byte
	for i := range word {
		word[i] = byte(i + 1)
	}
	answer := word[:]
	for n := 1; n <= 32; n++ {
		typ := fmt.Sprintf("bytes%d", n)
		p, err := web3.NewPlan(web3.Request{
			Contract: reviewContract, ChainID: 1,
			Path: "read", Query: "returns=(" + typ + ")",
		}, web3.ModeAuto)
		if err != nil {
			t.Fatalf("returns=(%s): %v", typ, err)
		}
		body, err := p.Decode(answer)
		if err != nil {
			t.Fatalf("returns=(%s): decode: %v", typ, err)
		}
		want := `["0x` + hex.EncodeToString(word[:n]) + `"]`
		if string(body) != want {
			t.Errorf("returns=(%s):\n got %s\nwant %s", typ, body, want)
		}
	}
}

// TestBoolInferenceCoversBothLiteralsAndNeitherNeighbour pins the whole of
// ERC-6860's boolean inference: the two literals infer bool, and anything
// else that merely looks like one stays a string, so the selector names the
// overload the URL asked for.
func TestBoolInferenceCoversBothLiteralsAndNeitherNeighbour(t *testing.T) {
	for _, tc := range []struct{ arg, signature string }{
		{"true", "render(bool)"},
		{"false", "render(bool)"},
		{"True", "render(string)"},
		{"FALSE", "render(string)"},
		{"1", "render(uint256)"},
		{"0", "render(uint256)"},
		{"yes", "render(string)"},
	} {
		p, err := web3.NewPlan(web3.Request{
			Contract: reviewContract, ChainID: 1,
			Path: "render/" + tc.arg,
		}, web3.ModeAuto)
		if err != nil {
			t.Errorf("/render/%s: %v", tc.arg, err)
			continue
		}
		want := hex.EncodeToString(web3.SelectorForTest(tc.signature))
		if got := hex.EncodeToString(p.Calldata); !strings.HasPrefix(got, want) {
			t.Errorf("/render/%s called a different overload: calldata %s, want the selector of %s (%s)",
				tc.arg, got[:8], tc.signature, want)
		}
	}
}

// TestEveryIntegerAliasHashesAsItsCanonicalType walks the aliases against the
// widths they stand for: a selector built from `uint` must equal the one
// built from `uint256`, or the call names a function the contract does not
// have.
func TestEveryIntegerAliasHashesAsItsCanonicalType(t *testing.T) {
	for alias, canonical := range map[string]string{"uint": "uint256", "int": "int256"} {
		aliasPlan, err := planOneArg(t, alias, "1")
		if err != nil {
			t.Fatalf("%s!1: %v", alias, err)
		}
		canonicalPlan, err := planOneArg(t, canonical, "1")
		if err != nil {
			t.Fatalf("%s!1: %v", canonical, err)
		}
		if !strings.EqualFold(hex.EncodeToString(aliasPlan.Calldata), hex.EncodeToString(canonicalPlan.Calldata)) {
			t.Errorf("%s!1 and %s!1 build different calls:\n %s\n %s", alias, canonical,
				hex.EncodeToString(aliasPlan.Calldata), hex.EncodeToString(canonicalPlan.Calldata))
		}
	}
}
