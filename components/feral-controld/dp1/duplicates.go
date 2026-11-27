package dp1

import (
	"bytes"
	"encoding/json"

	"go.uber.org/zap"
)

// A document that names one member twice in an object has no single reading:
// the schema validator and the player take the last value, while the typed
// decode merges repeated objects, so a value the schema never saw would be
// played. Such a document is refused, at any depth.
//
// The check must run on the bytes as received: decoding into a map and
// re-encoding, as the inline exemption does, silently drops the repeat.

// refuseRepeatedMembers is the gate's refusal of such a document.
func refuseRepeatedMembers(logger *zap.Logger, source string, raw []byte) error {
	if !hasRepeatedMember(raw) {
		return nil
	}
	if logger != nil {
		logger.Warn("DP-1 document repeats a member name", zap.String("source", source))
	}
	return refuse(logger, source, []string{"/"})
}

// hasRepeatedMember reports whether any object in raw names a member twice,
// comparing the names after unescaping. Malformed JSON reports false: the
// decode that follows refuses it.
func hasRepeatedMember(raw []byte) bool {
	type level struct {
		names     map[string]bool // nil for an array
		awaitName bool
	}
	var stack []level
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	// valueDone marks the enclosing object ready for its next name.
	valueDone := func() {
		if n := len(stack); n > 0 && stack[n-1].names != nil {
			stack[n-1].awaitName = true
		}
	}
	for {
		token, err := dec.Token()
		if err != nil {
			return false
		}
		switch t := token.(type) {
		case json.Delim:
			switch t {
			case '{':
				stack = append(stack, level{names: map[string]bool{}, awaitName: true})
			case '[':
				stack = append(stack, level{})
			default:
				stack = stack[:len(stack)-1]
				valueDone()
			}
		default:
			if n := len(stack); n > 0 && stack[n-1].awaitName {
				name, _ := t.(string)
				if stack[n-1].names[name] {
					return true
				}
				stack[n-1].names[name] = true
				stack[n-1].awaitName = false
				continue
			}
			valueDone()
		}
	}
}
