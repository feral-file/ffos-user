package dp1

import (
	"bytes"
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

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

func TestAuditDocumentReportsAndNeverRejects(t *testing.T) {
	tests := []struct {
		name     string
		doc      string
		wantWarn []string
	}{
		{"conformant", string(conformantFixture(t)), nil},
		{"short version", `{"dpVersion":"1.0","title":"t","items":[{"source":"https://example.com/a.html"}]}`, []string{"DP-1 document dpVersion is not a version"}},
		{"major mismatch", `{"dpVersion":"2.0.0","title":"t","items":[{"source":"https://example.com/a.html"}]}`, []string{"DP-1 document major version differs from this build; playing it anyway"}},
		{"missing version", `{"title":"t","items":[{"source":"https://example.com/a.html"}]}`, []string{"DP-1 document has no readable dpVersion"}},
		{"empty items", `{"dpVersion":"1.1.0","title":"t","items":[]}`, []string{"DP-1 document does not conform to the schema; playing it anyway"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			core, logs := observer.New(zap.WarnLevel)
			AuditDocument(zap.New(core), "test", []byte(tt.doc))
			var got []string
			for _, e := range logs.All() {
				got = append(got, e.Message)
			}
			for _, w := range tt.wantWarn {
				found := false
				for _, g := range got {
					found = found || g == w
				}
				if !found {
					t.Errorf("missing warning %q, got %v", w, got)
				}
			}
			if tt.wantWarn == nil && len(got) != 0 {
				t.Errorf("unexpected warnings %v", got)
			}
		})
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
	if _, dup := m["Title"]; dup {
		t.Errorf("case-folded known member must not be carried as an extra: %s", out)
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

func TestAuditDocumentAsyncRunsOneAtATimeAndReleasesTheSlot(t *testing.T) {
	core, logs := observer.New(zap.DebugLevel)
	logger := zap.New(core)

	auditSlot <- struct{}{} // another audit is "running"
	AuditDocumentAsync(logger, "busy", []byte(`{}`))
	<-auditSlot
	if logs.FilterMessage("DP-1 audit skipped: another audit is running").Len() != 1 {
		t.Fatalf("busy slot must skip the audit with a debug line: %v", logs.All())
	}

	AuditDocumentAsync(logger, "free", []byte(`{}`))
	deadline := time.Now().Add(5 * time.Second)
	for logs.FilterMessage("DP-1 document has no readable dpVersion").Len() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("audit did not run once the slot was free")
		}
		time.Sleep(5 * time.Millisecond)
	}
	for len(auditSlot) != 0 { // the goroutine releases the slot when it ends
		if time.Now().After(deadline) {
			t.Fatal("slot was not released")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
