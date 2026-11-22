package control

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestCheckJSON(t *testing.T) {
	cases := []struct {
		in string
		ok bool
	}{
		{`{}`, true},
		{`{"a":1,"b":[{"a":1},{"a":2}]}`, true},
		{`{"a":1,"a":2}`, false},
		{`{"a":{"b":1,"b":1}}`, false},
		{`[{"x":1,"x":1}]`, false},
		{`{"a":1,"a":2}`, false},
		{`{"a":1} {}`, false},
		{`{"a":1`, false},
		{"{\"a\":\"\xff\"}", false},
		{`"str"`, true},
		{strings.Repeat("[", maxJSONDepth+2) + strings.Repeat("]", maxJSONDepth+2), false},
	}
	for _, tc := range cases {
		if err := checkJSON([]byte(tc.in)); (err == nil) != tc.ok {
			t.Errorf("checkJSON(%q) = %v, want ok=%v", tc.in, err, tc.ok)
		}
	}
}

func TestStrictObject(t *testing.T) {
	if _, err := strictObject([]byte(`{"a":1,"b":2}`), []string{"a"}, []string{"b"}); err != nil {
		t.Fatal(err)
	}
	for _, in := range []string{`{"a":1,"c":2}`, `{"b":2}`, `[]`, `null`, ` 1`} {
		if _, err := strictObject([]byte(in), []string{"a"}, []string{"b"}); err == nil {
			t.Errorf("accepted %s", in)
		}
	}
}

func TestB64uDecode(t *testing.T) {
	for in, ok := range map[string]bool{
		"":        true,
		"AQID":    true,
		"AQI":     true,
		"AQI=":    false,
		"AQ\nID":  false,
		"AQ\rID":  false,
		"AQ+D":    false,
		"AR":      false, // non-zero trailing bits
		"AQ/D":    false,
		"_-_-":    true,
		"AQID AQ": false,
	} {
		if _, err := b64uDecode(in); (err == nil) != ok {
			t.Errorf("b64uDecode(%q) err=%v, want ok=%v", in, err, ok)
		}
	}
}

func TestUintMember(t *testing.T) {
	for in, want := range map[string]int64{
		`0`: 0, `1`: 1, `9007199254740991`: maxSafeInt,
		`9007199254740992`: -1, `-1`: -1, `1.0`: -1, `1e3`: -1, `01`: -1, `"1"`: -1, `true`: -1, `null`: -1,
	} {
		v, err := uintMember(map[string]json.RawMessage{"k": json.RawMessage(in)}, "k")
		if want < 0 {
			if err == nil {
				t.Errorf("%s accepted as %d", in, v)
			}
			continue
		}
		if err != nil || v != uint64(want) {
			t.Errorf("%s = %d, %v", in, v, err)
		}
	}
}

func TestMarshalJSONNoEscape(t *testing.T) {
	b, _ := marshalJSON(map[string]string{"a": "<&>"})
	if string(b) != `{"a":"<&>"}` {
		t.Fatal(string(b))
	}
}
