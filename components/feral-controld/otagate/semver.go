package otagate

import (
	"fmt"
	"strconv"
	"strings"
)

// semver is a minimal SemVer 2.0.0 value supporting the comparisons the OTA gate
// needs: current-vs-min-runtime, current-vs-latest, current-vs-min-upgradeable.
// feral-setupd used the Rust `semver` crate; controld has no semver dependency,
// so this ports just the parse + precedence rules those comparisons rely on.
// Build metadata (after '+') is ignored for precedence, per the spec.
type semver struct {
	major int
	minor int
	patch int
	pre   []string
}

func parseSemver(s string) (semver, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return semver{}, fmt.Errorf("empty version string")
	}

	// Strip build metadata; it does not affect precedence.
	if i := strings.IndexByte(s, '+'); i >= 0 {
		s = s[:i]
	}

	var pre []string
	if i := strings.IndexByte(s, '-'); i >= 0 {
		preStr := s[i+1:]
		s = s[:i]
		if preStr == "" {
			return semver{}, fmt.Errorf("empty pre-release in %q", s)
		}
		pre = strings.Split(preStr, ".")
	}

	parts := strings.Split(s, ".")
	if len(parts) != 3 {
		return semver{}, fmt.Errorf("expected MAJOR.MINOR.PATCH, got %q", s)
	}

	nums := make([]int, 3)
	for idx, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return semver{}, fmt.Errorf("invalid numeric identifier %q", p)
		}
		nums[idx] = n
	}

	return semver{major: nums[0], minor: nums[1], patch: nums[2], pre: pre}, nil
}

// compare returns -1 if v < o, 0 if equal, +1 if v > o, following SemVer 2.0.0
// precedence (numeric core first, then pre-release: a version with a pre-release
// has lower precedence than the same version without one).
func (v semver) compare(o semver) int {
	if c := cmpInt(v.major, o.major); c != 0 {
		return c
	}
	if c := cmpInt(v.minor, o.minor); c != 0 {
		return c
	}
	if c := cmpInt(v.patch, o.patch); c != 0 {
		return c
	}

	// A normal (no pre-release) version outranks a pre-release of the same core.
	switch {
	case len(v.pre) == 0 && len(o.pre) == 0:
		return 0
	case len(v.pre) == 0:
		return 1
	case len(o.pre) == 0:
		return -1
	}

	return comparePre(v.pre, o.pre)
}

func (v semver) less(o semver) bool { return v.compare(o) < 0 }

func comparePre(a, b []string) int {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if c := comparePreIdent(a[i], b[i]); c != 0 {
			return c
		}
	}
	// All shared identifiers equal: the longer set has higher precedence.
	return cmpInt(len(a), len(b))
}

func comparePreIdent(a, b string) int {
	aNum, aIsNum := toUint(a)
	bNum, bIsNum := toUint(b)
	switch {
	case aIsNum && bIsNum:
		return cmpInt(aNum, bNum)
	case aIsNum:
		// Numeric identifiers always have lower precedence than alphanumeric ones.
		return -1
	case bIsNum:
		return 1
	default:
		return strings.Compare(a, b)
	}
}

func toUint(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

func cmpInt(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	default:
		return 0
	}
}
