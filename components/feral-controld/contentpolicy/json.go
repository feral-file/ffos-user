package contentpolicy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

func jsonMarshal(v any) ([]byte, error) { return json.Marshal(v) }

// jsonUnmarshalStrict rejects trailing data but IGNORES unknown fields, like
// every other state-file reader in this component (state, sleepschedule). A
// newer build that adds a field must not make an older one reject the file and
// discard the owner's saved policy; the completeness of the KNOWN v1 fields is
// what is validated, by wirePolicy.
func jsonUnmarshalStrict(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	if err := d.Decode(v); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); err != io.EOF {
		if err == nil {
			return errors.New("multiple JSON values")
		}
		return err
	}
	return nil
}
