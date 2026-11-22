package control

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// The protocol's parsers are strict on purpose: two implementations that
// disagree about what a byte string means (which of two duplicate "kid"
// members wins, whether "\n" inside base64 is ignored) are a signature-
// confusion bug waiting to happen. Everything that crosses the trust boundary
// goes through the helpers below, never through a bare json.Unmarshal.

// maxJSONDepth bounds nesting so a hostile body cannot drive the duplicate-key
// walk arbitrarily deep. Real DP-1 playlists nest a handful of levels.
const maxJSONDepth = 128

var (
	errDuplicateKey = errors.New("duplicate JSON member")
	errNotObject    = errors.New("not a JSON object")
)

var b64 = base64.RawURLEncoding.Strict()

func b64u(b []byte) string { return b64.EncodeToString(b) }

// b64uDecode decodes unpadded base64url strictly. The stdlib decoder silently
// skips CR and LF, so those are rejected here first; padding and non-zero
// trailing bits are rejected by the Strict, Raw encoding.
func b64uDecode(s string) ([]byte, error) {
	if strings.ContainsAny(s, "\r\n=") {
		return nil, errors.New("base64url: invalid character")
	}
	return b64.DecodeString(s)
}

// checkJSON verifies raw is exactly one well-formed UTF-8 JSON value with no
// duplicate member names at any depth and nothing after it.
func checkJSON(raw []byte) error {
	if !utf8.Valid(raw) {
		return errors.New("JSON is not valid UTF-8")
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := walkJSON(dec, 0); err != nil {
		return err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return errors.New("trailing data after JSON value")
	}
	return nil
}

func walkJSON(dec *json.Decoder, depth int) error {
	if depth > maxJSONDepth {
		return errors.New("JSON nested too deeply")
	}
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	d, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	switch d {
	case '{':
		seen := map[string]struct{}{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, ok := kt.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			// Token returns the unescaped key, so "a" and "a" collide
			// here exactly as they would in any decoder that keeps one.
			if _, dup := seen[k]; dup {
				return fmt.Errorf("%w %q", errDuplicateKey, k)
			}
			seen[k] = struct{}{}
			if err := walkJSON(dec, depth+1); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := walkJSON(dec, depth+1); err != nil {
				return err
			}
		}
	default:
		return errors.New("unexpected JSON delimiter")
	}
	_, err = dec.Token() // the closing delimiter
	return err
}

// strictObject checks raw (see checkJSON), requires it to be an object whose
// member names are all in required∪optional with every required one present,
// and returns the members undecoded.
func strictObject(raw []byte, required, optional []string) (map[string]json.RawMessage, error) {
	if err := checkJSON(raw); err != nil {
		return nil, err
	}
	trimmed := bytes.TrimLeft(raw, " \t\r\n")
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return nil, errNotObject
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	for k := range m {
		if !slices.Contains(required, k) && !slices.Contains(optional, k) {
			return nil, fmt.Errorf("unexpected member %q", k)
		}
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("missing member %q", k)
		}
	}
	return m, nil
}

// isJSONObject reports whether raw (already checked) is an object.
func isJSONObject(raw json.RawMessage) bool {
	t := bytes.TrimLeft(raw, " \t\r\n")
	return len(t) > 0 && t[0] == '{'
}

func stringMember(m map[string]json.RawMessage, key string) (string, error) {
	raw, ok := m[key]
	if !ok {
		return "", fmt.Errorf("missing member %q", key)
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil || !bytes.HasPrefix(bytes.TrimSpace(raw), []byte(`"`)) {
		return "", fmt.Errorf("member %q is not a string", key)
	}
	return s, nil
}

func expectString(m map[string]json.RawMessage, key, want string) error {
	got, err := stringMember(m, key)
	if err != nil {
		return err
	}
	if got != want {
		return fmt.Errorf("member %q = %q, want %q", key, got, want)
	}
	return nil
}

// fixedB64Member decodes a base64url member that must be exactly len(dst) bytes.
func fixedB64Member(m map[string]json.RawMessage, key string, dst []byte) error {
	s, err := stringMember(m, key)
	if err != nil {
		return err
	}
	b, err := b64uDecode(s)
	if err != nil {
		return fmt.Errorf("member %q: %w", key, err)
	}
	if len(b) != len(dst) {
		return fmt.Errorf("member %q is %d bytes, want %d", key, len(b), len(dst))
	}
	copy(dst, b)
	return nil
}

// maxSafeInt is 2^53-1, the largest integer every JSON implementation in the
// protocol (Dart, JavaScript) represents exactly (§1).
const maxSafeInt = 1<<53 - 1

// uintMember decodes a non-negative integer claim in [0, 2^53-1]. "1.0",
// "1e3", "-0" and quoted numbers are rejected: the JS and Dart encoders never
// produce them, so seeing one means a hand-built or hostile body.
func uintMember(m map[string]json.RawMessage, key string) (uint64, error) {
	raw, ok := m[key]
	if !ok {
		return 0, fmt.Errorf("missing member %q", key)
	}
	s := string(bytes.TrimSpace(raw))
	if s == "" || strings.ContainsAny(s, ".eE+-\"") || (len(s) > 1 && s[0] == '0') {
		return 0, fmt.Errorf("member %q is not a canonical non-negative integer", key)
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil || v > maxSafeInt {
		return 0, fmt.Errorf("member %q out of range", key)
	}
	return v, nil
}

func boolMember(m map[string]json.RawMessage, key string) (bool, error) {
	switch string(bytes.TrimSpace(m[key])) {
	case "true":
		return true, nil
	case "false":
		return false, nil
	}
	return false, fmt.Errorf("member %q is not a boolean", key)
}

// marshalJSON encodes v compactly without HTML escaping and without the
// trailing newline json.Encoder adds. Every JSON this package signs or
// encrypts goes through it, so the bytes are stable and minimal.
func marshalJSON(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}
