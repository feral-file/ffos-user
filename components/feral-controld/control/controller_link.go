package control

// The app-link envelope around a controller QR (docs/ff1-control-v1.md
// §5.6.1.1): `https://link.feralfile.com/controller#[d=<deviceId>&]c=<pct>`.
// Like the rest of registration.go, the device never runs this: it is the
// reference the shared controller-link fixtures are generated from, so an
// owner's app builds and parses exactly the same strings.

import (
	"errors"
	"regexp"
	"strings"
	"unicode/utf8"
)

// ControllerLinkPrefix is the fixed part of every controller link. The path
// names nothing; everything else is in the fragment, which a browser never
// sends.
const ControllerLinkPrefix = "https://link.feralfile.com/controller"

// linkDeviceIDPattern is the §2 hostname shape a link's `d` must have.
var linkDeviceIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

// ControllerLink renders q as a controller link, naming deviceID when it has
// the hostname shape. A missing or malformed id is left out rather than
// written, so it costs the owner a pick instead of the whole link.
func ControllerLink(q ControllerQR, deviceID string) string {
	target := ""
	if linkDeviceIDPattern.MatchString(deviceID) {
		target = "d=" + EncodeURIComponent(deviceID) + "&"
	}
	return ControllerLinkPrefix + "#" + target + "c=" + EncodeURIComponent(q.String())
}

// ParseControllerLink parses a controller link into its QR and the device id
// it names ("" when none). Anything §5.6.1.1 does not allow is an error: a
// repeated or unknown key, a missing `c`, a malformed escape or invalid
// UTF-8, a `d` outside the hostname shape, or a `c` that is not a valid QR.
// A query before the fragment, and anything after a raw `?` inside it, are
// ignored.
func ParseControllerLink(s string) (ControllerQR, string, error) {
	var q ControllerQR
	rest, ok := strings.CutPrefix(s, ControllerLinkPrefix)
	if !ok || (rest != "" && !strings.ContainsAny(rest[:1], "/?#")) {
		return q, "", errors.New("control: not a controller link")
	}
	_, fragment, ok := strings.Cut(rest, "#")
	if !ok {
		return q, "", errors.New("control: controller link has no fragment")
	}
	fragment, _, _ = strings.Cut(fragment, "?")
	values := map[string]string{}
	for _, pair := range strings.Split(fragment, "&") {
		key, raw, ok := strings.Cut(pair, "=")
		if !ok || key == "" {
			return q, "", errors.New("control: controller link has a malformed member")
		}
		if _, dup := values[key]; dup {
			return q, "", errors.New("control: controller link repeats " + key)
		}
		v, err := decodeURIComponent(raw)
		if err != nil {
			return q, "", err
		}
		values[key] = v
	}
	code, hasCode := values["c"]
	deviceID, hasDevice := values["d"]
	delete(values, "c")
	delete(values, "d")
	if len(values) != 0 || !hasCode {
		return q, "", errors.New("control: controller link members must be c and an optional d")
	}
	if hasDevice && !linkDeviceIDPattern.MatchString(deviceID) {
		return q, "", errors.New("control: controller link d is not a device id")
	}
	q, err := ParseControllerQR(code)
	if err != nil {
		return q, "", err
	}
	return q, deviceID, nil
}

// EncodeURIComponent is ECMAScript's encodeURIComponent (Dart's
// Uri.encodeComponent), exported for the fixture generator's hand-built
// links: every UTF-8 byte outside A-Z a-z 0-9 - _ . ! ~ * '
// ( ) becomes an upper-case %XX. Go's url.PathEscape and QueryEscape keep or
// rewrite other characters (`&`, `=`, `+`, space), so the links would not be
// byte-identical.
func EncodeURIComponent(s string) string {
	const hex = "0123456789ABCDEF"
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if 'A' <= c && c <= 'Z' || 'a' <= c && c <= 'z' || '0' <= c && c <= '9' || strings.IndexByte("-_.!~*'()", c) >= 0 {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('%')
		b.WriteByte(hex[c>>4])
		b.WriteByte(hex[c&15])
	}
	return b.String()
}

// decodeURIComponent undoes EncodeURIComponent: every %XX becomes its byte,
// `+` stays `+`, and the result must be valid UTF-8 (Dart's
// Uri.decodeComponent refuses the same inputs).
func decodeURIComponent(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			b.WriteByte(s[i])
			continue
		}
		if i+2 >= len(s) || !isHex(s[i+1]) || !isHex(s[i+2]) {
			return "", errors.New("control: controller link has a malformed escape")
		}
		b.WriteByte(unhex(s[i+1])<<4 | unhex(s[i+2]))
		i += 2
	}
	if !utf8.ValidString(b.String()) {
		return "", errors.New("control: controller link value is not UTF-8")
	}
	return b.String(), nil
}

func isHex(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c <= '9':
		return c - '0'
	case c <= 'F':
		return c - 'A' + 10
	default:
		return c - 'a' + 10
	}
}
