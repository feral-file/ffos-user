package web3_test

import (
	"encoding/hex"
	"errors"
	"strings"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/web3"
)

// A node's answer is hostile input: there is no content address to check it
// against, so the only thing standing between a lying node and the daemon's
// memory is the decoder's bounds. These cases are the ones the onchfs ABI
// decoder guards too (ffos-user#428 review F-series): a length larger than
// the answer, an offset past it, an offset that is not a word boundary, and a
// word that does not fit a length at all.
func TestDecodeRefusesAHostileAnswer(t *testing.T) {
	plan := func(t *testing.T) web3.Plan {
		t.Helper()
		p, err := web3.NewPlan(web3.Request{Contract: "0x4f32740cf412d647a622ba27c36fa4b76f71601f", ChainID: 1, Path: "artpiece"}, web3.ModeAuto)
		if err != nil {
			t.Fatalf("NewPlan: %v", err)
		}
		return p
	}
	const w32 = "0000000000000000000000000000000000000000000000000000000000000020"
	cases := map[string]string{
		"a length past the answer": w32 +
			"00000000000000000000000000000000000000000000000000000000000000ff" +
			strings.Repeat("41", 16) + strings.Repeat("00", 16),
		"an offset past the answer": "00000000000000000000000000000000000000000000000000000000000000c0" +
			strings.Repeat("00", 32) + strings.Repeat("00", 32),
		"an offset off a word boundary": "0000000000000000000000000000000000000000000000000000000000000021" +
			strings.Repeat("00", 32) + strings.Repeat("00", 32),
		"a word too large to be an offset": strings.Repeat("ff", 32) +
			strings.Repeat("00", 32) + strings.Repeat("00", 32),
		"a length word too large to be a length": w32 + strings.Repeat("ff", 32) +
			strings.Repeat("00", 32),
	}
	for name, answerHex := range cases {
		t.Run(name, func(t *testing.T) {
			answer, err := hex.DecodeString(answerHex)
			if err != nil {
				t.Fatalf("bad test input: %v", err)
			}
			if body, err := plan(t).Decode(answer); err == nil {
				t.Fatalf("Decode accepted a hostile answer and returned %d bytes", len(body))
			} else if !errors.Is(err, web3.ErrUnsupported) && !errors.Is(err, web3.ErrTooLarge) {
				t.Fatalf("Decode err = %v, want ErrUnsupported or ErrTooLarge", err)
			}
		})
	}
}

func TestDecodeRefusesNothing(t *testing.T) {
	p, err := web3.NewPlan(web3.Request{Contract: "0x4f32740cf412d647a622ba27c36fa4b76f71601f", ChainID: 1, Path: "artpiece"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	if _, err := p.Decode(nil); !errors.Is(err, web3.ErrNotFound) {
		t.Fatalf("Decode(nil) err = %v, want ErrNotFound", err)
	}
}

// A result too short to carry an offset and a length is taken as the raw
// bytes, as the reference implementation does, so a contract that returns a
// bare word still shows something rather than failing.
func TestDecodeShortResultIsTakenRaw(t *testing.T) {
	p, err := web3.NewPlan(web3.Request{Contract: "0x4f32740cf412d647a622ba27c36fa4b76f71601f", ChainID: 1, Path: "artpiece"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	raw, _ := hex.DecodeString(strings.Repeat("41", 32))
	body, err := p.Decode(raw)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(body) != 32 {
		t.Fatalf("Decode returned %d bytes, want the 32 raw ones", len(body))
	}
}

// The decoded body must be a copy, not a window into the node's buffer: the
// buffer is reused and a record that aliased it would serve whatever landed
// there next.
func TestDecodeCopiesOutOfTheAnswer(t *testing.T) {
	p, err := web3.NewPlan(web3.Request{Contract: "0x4f32740cf412d647a622ba27c36fa4b76f71601f", ChainID: 1, Path: "artpiece"}, web3.ModeAuto)
	if err != nil {
		t.Fatalf("NewPlan: %v", err)
	}
	answer, err := hex.DecodeString(
		"0000000000000000000000000000000000000000000000000000000000000020" +
			"0000000000000000000000000000000000000000000000000000000000000004" +
			"61626364" + strings.Repeat("00", 28))
	if err != nil {
		t.Fatal(err)
	}
	body, err := p.Decode(answer)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if string(body) != "abcd" {
		t.Fatalf("Decode = %q, want abcd", body)
	}
	for i := range answer {
		answer[i] = 0xff
	}
	if string(body) != "abcd" {
		t.Fatalf("the decoded body aliased the answer: now %q", body)
	}
}
