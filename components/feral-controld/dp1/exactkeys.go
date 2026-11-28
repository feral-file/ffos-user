package dp1

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
)

// stripCaseVariants removes every object member that encoding/json would
// assign to a field of t although its name differs in case from that field's
// ("Title" for title, "SOURCE" for source). To DP-1 such a member is an
// unknown one; left in place it overrides the member the schema checked and
// the signer covered. A document with none is returned as is, byte for byte.
//
// Structs and lists of the typed decode are visited, including those with an
// UnmarshalJSON of their own: dp1-go's guard only their extension members, not
// the core ones. A json.RawMessage, a map or a scalar is left alone. Top-level variants are not
// lost, because unknownPlaylistMembers reads them from the original bytes.
func stripCaseVariants(data []byte, t reflect.Type) []byte {
	out, changed := stripValue(data, t)
	if !changed {
		return data
	}
	return out
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

func stripValue(data []byte, t reflect.Type) ([]byte, bool) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == rawMessageType {
		return data, false
	}
	switch t.Kind() {
	case reflect.Struct:
		return stripObject(data, jsonFields(t))
	case reflect.Slice, reflect.Array:
		var elems []json.RawMessage
		if json.Unmarshal(data, &elems) != nil {
			return data, false
		}
		changed := false
		for i := range elems {
			if out, c := stripValue(elems[i], t.Elem()); c {
				elems[i], changed = out, true
			}
		}
		return remarshal(data, elems, changed)
	default:
		return data, false
	}
}

func stripObject(data []byte, fields map[string]reflect.Type) ([]byte, bool) {
	var members map[string]json.RawMessage
	if json.Unmarshal(data, &members) != nil {
		return data, false
	}
	changed := false
	for name, value := range members {
		if ft, exact := fields[name]; exact {
			if out, c := stripValue(value, ft); c {
				members[name], changed = out, true
			}
			continue
		}
		for field := range fields {
			if strings.EqualFold(name, field) {
				delete(members, name)
				changed = true
				break
			}
		}
	}
	return remarshal(data, members, changed)
}

func remarshal(original []byte, v any, changed bool) ([]byte, bool) {
	if !changed {
		return original, false
	}
	out, err := marshalNoEscape(v)
	if err != nil {
		return original, false
	}
	return bytes.TrimSpace(out), true
}

// jsonFields maps a struct's wire member names to their types, following
// embedded structs the way encoding/json promotes them.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	fields := map[string]reflect.Type{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if f.Anonymous && name == "" {
			et := f.Type
			for et.Kind() == reflect.Pointer {
				et = et.Elem()
			}
			if et.Kind() == reflect.Struct {
				for k, v := range jsonFields(et) {
					if _, taken := fields[k]; !taken {
						fields[k] = v
					}
				}
				continue
			}
		}
		if !f.IsExported() || name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		fields[name] = f.Type
	}
	return fields
}
