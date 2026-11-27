package dp1

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRepeatedMemberNamesAreRefused(t *testing.T) {
	const head = `{"dpVersion":"1.1.0","title":"t",`
	const items = `"items":[{"source":"https://example.com/a"}]`
	for name, doc := range map[string]string{
		"merged object":    head + `"defaults":{"duration":-1},"defaults":{},` + items + `}`,
		"scalar":           head + `"title":"u",` + items + `}`,
		"escaped name":     head + `"titl\u0065":"u",` + items + `}`,
		"inside an item":   head + `"items":[{"source":"https://example.com/a","display":{"scaling":"fit"},"display":{}}]}`,
		"unknown member":   head + `"x":{"a":1,"a":2},` + items + `}`,
		"signature member": head + `"signatures":[],"signatures":[],` + items + `}`,
	} {
		t.Run(name, func(t *testing.T) {
			assert.ErrorIs(t, CheckPlayable(nil, "test", []byte(doc)), ErrPlaylistInvalid)
			// The exemption re-encodes the document; the repeat must be seen first.
			assert.ErrorIs(t, CheckInlineCast(nil, "test", []byte(doc)), ErrPlaylistInvalid)
		})
	}
	assert.Error(t, checkItem([]byte(`{"source":"https://example.com/a","source":"https://example.com/b"}`)))
}

// The same name in different objects, and a string value equal to a name, are
// not repeats.
func TestHasRepeatedMemberAcceptsDistinctMembers(t *testing.T) {
	for _, doc := range []string{
		`{"a":{"a":1},"b":[{"a":1},{"a":2}],"c":"a","d":["a","a"]}`,
		`{"a":[[],{}],"b":null,"c":1,"d":true}`,
		`[]`, `"a"`, ``, `{"a":`,
	} {
		assert.False(t, hasRepeatedMember([]byte(doc)), doc)
	}
}
