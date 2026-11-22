package control

import (
	"strings"
	"testing"
)

// The v1 label set is frozen (§8): the store re-checks labels on load, so a
// change here would boot-lock devices holding labels that were valid when
// enrolled. This pins every member and a few neighbors that must stay
// allowed. Change it only with a new protocol version.
func TestLabelCharacterSetFrozen(t *testing.T) {
	var rejected []rune
	for r := rune(0x00); r <= 0x1f; r++ {
		rejected = append(rejected, r)
	}
	for r := rune(0x7f); r <= 0x9f; r++ {
		rejected = append(rejected, r)
	}
	rejected = append(rejected, 0x061c, 0x200b, 0x200c, 0x200d, 0x200e, 0x200f,
		0x202a, 0x202b, 0x202c, 0x202d, 0x202e, 0x2060, 0x2066, 0x2067, 0x2068, 0x2069,
		0x2028, 0x2029, 0xfeff)
	for _, r := range rejected {
		if validLabel("a" + string(r) + "b") {
			t.Errorf("U+%04X accepted", r)
		}
	}
	for _, r := range []rune{' ', '~', 0xa0, 0x061b, 0x200a, 0x2010, 0x2027, 0x202f, 0x2061, 0x2065, 0x206a, 0xfefe, 0xff01, '中', '😀'} {
		if !validLabel("a" + string(r) + "b") {
			t.Errorf("U+%04X rejected", r)
		}
	}
	if !validLabel(strings.Repeat("é", maxLabelRunes)) || validLabel(strings.Repeat("é", maxLabelRunes+1)) {
		t.Error("length bound changed")
	}
}
