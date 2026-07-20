package otagate

import "testing"

func TestParseSemver_Invalid(t *testing.T) {
	for _, s := range []string{"", "1", "1.2", "1.2.x", "a.b.c", "1.2.3.4", "1.2.-3"} {
		if _, err := parseSemver(s); err == nil {
			t.Errorf("parseSemver(%q) expected error, got nil", s)
		}
	}
}

func TestSemverCompare(t *testing.T) {
	cases := []struct {
		a, b string
		want int // sign of a.compare(b)
	}{
		{"1.0.0", "1.0.0", 0},
		{"1.0.0", "1.0.1", -1},
		{"1.2.0", "1.1.9", 1},
		{"2.0.0", "1.9.9", 1},
		{"1.0.0", "2.0.0", -1},
		// Pre-release has lower precedence than the same core release.
		{"1.0.0-alpha", "1.0.0", -1},
		{"1.0.0", "1.0.0-alpha", 1},
		// Pre-release ordering (SemVer 2.0.0 spec example).
		{"1.0.0-alpha", "1.0.0-alpha.1", -1},
		{"1.0.0-alpha.1", "1.0.0-alpha.beta", -1},
		{"1.0.0-alpha.beta", "1.0.0-beta", -1},
		{"1.0.0-beta", "1.0.0-beta.2", -1},
		{"1.0.0-beta.2", "1.0.0-beta.11", -1},
		{"1.0.0-beta.11", "1.0.0-rc.1", -1},
		{"1.0.0-rc.1", "1.0.0", -1},
		// Build metadata is ignored for precedence.
		{"1.0.0+build.1", "1.0.0+build.99", 0},
	}
	for _, tc := range cases {
		a, err := parseSemver(tc.a)
		if err != nil {
			t.Fatalf("parseSemver(%q): %v", tc.a, err)
		}
		b, err := parseSemver(tc.b)
		if err != nil {
			t.Fatalf("parseSemver(%q): %v", tc.b, err)
		}
		got := sign(a.compare(b))
		if got != tc.want {
			t.Errorf("compare(%q, %q) sign = %d, want %d", tc.a, tc.b, got, tc.want)
		}
		// less() must agree with compare().
		if want := tc.want < 0; a.less(b) != want {
			t.Errorf("less(%q, %q) = %v, want %v", tc.a, tc.b, a.less(b), want)
		}
	}
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}
