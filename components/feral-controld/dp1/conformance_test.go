package dp1

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
)

func conformantFixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("../sigverify/testdata/feed-dd875fbe.json")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestEmittedDPVersionMatchesSchemaPattern(t *testing.T) {
	if !regexp.MustCompile(`^\d+\.\d+\.\d+$`).MatchString(EmittedDPVersion) {
		t.Fatalf("EmittedDPVersion %q is not MAJOR.MINOR.PATCH", EmittedDPVersion)
	}
}

func TestCheckPlayable(t *testing.T) {
	const item = `{"source":"https://example.com/a.html"}`
	const head = `{"dpVersion":"1.1.0","title":"t",`
	tests := []struct {
		name    string
		doc     string
		pointer string // "" means playable
	}{
		{"signed fixture", string(conformantFixture(t)), ""},
		{"unsigned", head + `"items":[` + item + `]}`, ""},
		{"malformed signatures are not judged", head + `"items":[` + item + `],"signatures":[{"role":"nobody"}],"signature":7}`, ""},
		{"no id, unknown member", head + `"items":[` + item + `],"futureMember":1}`, ""},
		{"other major", `{"dpVersion":"2.0.0","title":"t","items":[` + item + `]}`, ""},
		{"dynamicQuery with empty items", head + `"items":[],"dynamicQuery":{"profile":"graphql-v1","endpoint":"https://example.com/q","query":"query { items { source } }","responseMapping":{"itemsPath":"data.items","itemSchema":"dp1/1.1"}}}`, ""},
		{"legacy dynamicQueries with empty items", head + `"items":[],"dynamicQueries":[{"endpoint":"https://example.com/q","params":{}}]}`, "/"},
		{"legacy dynamicQueries with static items", head + `"items":[` + item + `],"dynamicQueries":[{"endpoint":"https://example.com/q","params":{}}]}`, ""},
		// The schema reports it as the dynamicQuery that would have allowed it.
		{"empty items", head + `"items":[]}`, "/"},
		{"short version", `{"dpVersion":"1.0","title":"t","items":[` + item + `]}`, "/dpVersion"},
		{"missing version", `{"title":"t","items":[` + item + `]}`, "/"},
		{"case variant does not satisfy required", `{"dpVersion":"1.1.0","Title":"t","items":[` + item + `]}`, "/"},
		{"item without source", head + `"items":[{"title":"x"}]}`, "/items/0"},
		{"item id not a uuid", head + `"items":[{"id":"work-1","source":"https://example.com/a"}]}`, "/items/0/id"},
		{"not an object", `[]`, "/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := CheckPlayable(zap.NewNop(), "test", []byte(tt.doc))
			if tt.pointer == "" {
				if err != nil {
					t.Fatalf("refused: %v", err)
				}
				return
			}
			var schema *SchemaError
			if !errors.As(err, &schema) || !errors.Is(err, ErrPlaylistInvalid) {
				t.Fatalf("want a schema refusal, got %v", err)
			}
			if !slices.Contains(schema.Locations, tt.pointer) {
				t.Errorf("want pointer %s, got %v", tt.pointer, schema.Locations)
			}
		})
	}
}

// The refusal names locations, never what the sender wrote there.
func TestCheckPlayableNeverCarriesValues(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	err := CheckPlayable(zap.New(core), "test", []byte(`{"dpVersion":"PRIVATE_CANARY","title":"t","items":[{"source":"PRIVATE_CANARY"}]}`))
	if err == nil || strings.Contains(err.Error(), "PRIVATE_CANARY") {
		t.Fatalf("error leaks or is missing: %v", err)
	}
	for _, e := range logs.All() {
		if strings.Contains(fmt.Sprint(e.Message, e.ContextMap()), "PRIVATE_CANARY") {
			t.Errorf("log leaks value: %v", e)
		}
	}
}

// A digits-only dpVersion passes the pattern at any length (leading zeros), so
// the major-mismatch warning must log the parsed major, never the raw string.
func TestMajorMismatchLogsBoundedValue(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	doc := `{"dpVersion":"` + strings.Repeat("0", 100000) + `2.0.0","title":"t","items":[{"source":"https://example.com/a.html"}]}`
	if err := CheckPlayable(zap.New(core), "test", []byte(doc)); err != nil {
		t.Fatal(err)
	}
	mm := logs.FilterMessage("DP-1 document major version differs from this build; playing it anyway")
	if mm.Len() != 1 {
		t.Fatalf("want one major-mismatch warning, got %v", logs.All())
	}
	for _, f := range mm.All()[0].Context {
		if len(f.String) > 64 {
			t.Errorf("field %q carries %d bytes of sender content", f.Key, len(f.String))
		}
	}
}

func TestPlaylistKeepsUnknownTopLevelMembersOnReemit(t *testing.T) {
	doc := `{"dpVersion":"1.2.0","title":"t","items":[{"source":"https://example.com/a.html"}],"futureMember":{"a":[1,2]},"Title":"folded","verification":{"verified":true},"Satellites":[1]}`
	var p Playlist
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}
	if string(m["futureMember"]) != `{"a":[1,2]}` {
		t.Errorf("futureMember lost or changed: %s", out)
	}
	if p.Title != "t" || string(m["title"]) != `"t"` || string(m["Title"]) != `"folded"` {
		t.Errorf("case variant must not override title and must be preserved as itself: %s", out)
	}
	if string(m["Satellites"]) != `[1]` {
		t.Errorf("case-variant satellites must be preserved as an extra: %s", out)
	}
	if _, ok := m["verification"]; ok {
		t.Errorf("verdict must not ride the payload: %s", out)
	}
}

func TestBuildPlaylistItemInventsNoLicense(t *testing.T) {
	item := buildPlaylistItem(10, ffindexer.Token{})
	if item.License != "" {
		t.Errorf("license %q was invented for an indexer token", item.License)
	}
}

func TestMarshalLeavesHTMLEscapingToTheOuterEncoder(t *testing.T) {
	var p Playlist
	doc := `{"dpVersion":"1.1.0","title":"a&b","items":[{"source":"https://x/?a=1&b=2"}],"extra":"x&y"}`
	if err := json.Unmarshal([]byte(doc), &p); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(p); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), `\u0026`) {
		t.Errorf("outer SetEscapeHTML(false) was defeated: %s", buf.String())
	}
}

// A malformed dpVersion is sender-controlled content: the warning names the
// JSON pointer, never the value.
// A digits-only dpVersion passes the pattern at any length (leading zeros), so
// the major-mismatch warning must log the parsed major, never the raw string.
// A missing top-level member is reported by the validator at the document
// root (the empty-string location); the audit must keep that location, not log an empty list.
