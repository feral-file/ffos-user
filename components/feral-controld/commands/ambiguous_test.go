package commands

import (
	"encoding/json"
	"testing"
)

// Review F3: a duplicate or case-variant top-level member makes the typed
// decode (case-insensitive, merging) disagree with RawArguments (exact key).
func TestAmbiguousTopLevel(t *testing.T) {
	for in, want := range map[string]bool{
		`{"command":"x","request":{}}`:                        false,
		`{"command":"x","request":{"request":1,"Request":2}}`: false, // nested is the policy's job
		`{"command":"x","request":{},"request":{}}`:           true,
		`{"command":"x","request":{},"Request":{}}`:           true,
		`{"command":"x","COMMAND":"y"}`:                       true,
		`[]`:                                                  false,
		`{`:                                                   false,
	} {
		if got := AmbiguousTopLevel([]byte(in)); got != want {
			t.Errorf("%s: %v", in, got)
		}
	}
	var c Command
	if err := json.Unmarshal([]byte(`{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{}},"Request":{"evil":1}}`), &c); err != nil || !c.Ambiguous {
		t.Fatalf("ambiguous message not flagged: %v %+v", err, c)
	}
}

// Review N2: the folding must match encoding/json's, which this test pins by
// asking encoding/json itself which key it binds.
func TestAmbiguousFoldingMatchesEncodingJSON(t *testing.T) {
	for _, msg := range []string{
		"{\"command\":\"x\",\"request\":{\"a\":1},\"reque\u017ft\":{\"b\":2}}",
		"{\"command\":\"x\",\"request\":{\"a\":1},\"REQUE\u017fT\":{\"b\":2}}",
	} {
		var w struct {
			Request map[string]any `json:"request"`
		}
		if err := json.Unmarshal([]byte(msg), &w); err != nil {
			t.Fatal(err)
		}
		if w.Request["b"] == nil {
			t.Fatalf("premise: encoding/json did not fold %s", msg)
		}
		if !AmbiguousTopLevel([]byte(msg)) {
			t.Fatalf("%s not flagged although encoding/json folds it", msg)
		}
	}
}
