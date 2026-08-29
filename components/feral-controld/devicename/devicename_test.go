package devicename_test

import (
	"strings"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/devicename"
)

func TestSanitize(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "ordinary name is unchanged", in: "Living Room", want: "Living Room"},
		{name: "trims and collapses whitespace", in: "  Living   Room  ", want: "Living Room"},
		{name: "strips control characters", in: "Living\nRoom\x00", want: "Living Room"},
		{
			// A name set over the LAN command surface can carry these
			// invisibly; they would let one unit's record render as another's.
			name: "strips bidi overrides and zero-width",
			in:   "Living\u202eRoom\u200b",
			want: "LivingRoom",
		},
		{
			// Empty is a value, not a rejection: it is how an owner undoes a
			// rename, and the unit falls back to its serial.
			name: "input that cleans away to nothing yields empty",
			in:   "  \n\x00 ",
			want: "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := devicename.Sanitize(tc.in); got != tc.want {
				t.Fatalf("Sanitize(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestSanitizeTruncatesHard(t *testing.T) {
	// No ellipsis: the stored string is read back into the field the owner
	// edits next, so a decoration would become part of the name on the
	// following save.
	got := devicename.Sanitize(strings.Repeat("A", 100))

	if want := strings.Repeat("A", devicename.MaxLength); got != want {
		t.Fatalf("Sanitize(long) = %q, want %q", got, want)
	}
	if strings.Contains(got, "…") {
		t.Fatalf("Sanitize(long) = %q, want no ellipsis", got)
	}
}

func TestSanitizeCountsRunesNotBytes(t *testing.T) {
	// The limit exists for display slots, which are measured in characters.
	// Slicing bytes would also cut a multi-byte rune in half and emit
	// invalid UTF-8 into the mDNS record.
	in := strings.Repeat("é", 40)

	got := devicename.Sanitize(in)

	if n := len([]rune(got)); n != devicename.MaxLength {
		t.Fatalf("Sanitize(accented) kept %d runes, want %d", n, devicename.MaxLength)
	}
	if got != strings.Repeat("é", devicename.MaxLength) {
		t.Fatalf("Sanitize(accented) = %q, want %d accented runes", got, devicename.MaxLength)
	}
}

func TestSanitizeDropsTrailingSpaceFromTruncation(t *testing.T) {
	in := strings.Repeat("A", devicename.MaxLength) + " tail"

	if got, want := devicename.Sanitize(in), strings.Repeat("A", devicename.MaxLength); got != want {
		t.Fatalf("Sanitize(boundary) = %q, want %q", got, want)
	}
}
