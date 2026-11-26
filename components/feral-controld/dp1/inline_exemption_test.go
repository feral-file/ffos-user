package dp1

import (
	"testing"

	"go.uber.org/zap"
)

// The exemption is exactly the owner's list: inline accepts it, a fetched
// document does not, and nothing else rides along.
func TestCheckInlineCast(t *testing.T) {
	const src = `"source":"https://example.com/a"`
	doc := func(rest string) []byte { return []byte(`{"dpVersion":"1.0.0",` + rest + `}`) }
	one := `"title":"Works","items":[{` + src + `}]`

	tests := []struct {
		name   string
		doc    []byte
		inline bool // accepted inline
	}{
		{"playlist id", doc(one + `,"id":"favorite"`), true},
		{"item id", doc(`"title":"t","items":[{"id":"eip155:1:erc721:0xabc:1",` + src + `}]`), true},
		{"created without offset", doc(one + `,"created":"2026-11-26T16:00:00.123456"`), true},
		{"empty items with dynamicQueries and app id", doc(`"title":"t","id":"addr:ethereum:0xabc","items":[],"dynamicQueries":[{"endpoint":"https://example.com/q"}]`), true},
		{"title over 200", doc(`"title":"` + string(make200()) + `x","items":[{` + src + `}]`), false},
		{"id of another type", doc(one + `,"id":7`), false},
		{"created that is not a time", doc(one + `,"created":"yesterday"`), false},
		{"bad item slug", doc(`"title":"t","items":[{"slug":"Not A Slug",` + src + `}]`), false},
		{"bad source", doc(`"title":"t","items":[{"id":"x","source":"nope"}]`), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := CheckInlineCast(zap.NewNop(), "test", tt.doc) == nil; got != tt.inline {
				t.Errorf("inline accepted = %v, want %v", got, tt.inline)
			}
			// Empty legacy lists aside, none of these passes the strict check.
			if CheckPlayable(zap.NewNop(), "test", tt.doc) == nil {
				t.Error("strict check accepted it")
			}
		})
	}
}

func make200() []byte {
	b := make([]byte, 200)
	for i := range b {
		b[i] = 'a'
	}
	return b
}
