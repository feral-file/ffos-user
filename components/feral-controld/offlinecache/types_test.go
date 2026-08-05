package offlinecache_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

func TestResource_IsRedirect(t *testing.T) {
	tests := []struct {
		name     string
		resource offlinecache.Resource
		want     bool
	}{
		{
			name:     "302 with redirect target",
			resource: offlinecache.Resource{URL: "https://host/a.js", Status: 302, RedirectTo: "https://host/b.js"},
			want:     true,
		},
		{
			name:     "301 with redirect target",
			resource: offlinecache.Resource{URL: "https://host/a.js", Status: 301, RedirectTo: "https://host/b.js"},
			want:     true,
		},
		{
			name:     "3xx without redirect target is not a redirect",
			resource: offlinecache.Resource{URL: "https://host/a.js", Status: 304},
			want:     false,
		},
		{
			name:     "200 with body is not a redirect",
			resource: offlinecache.Resource{URL: "https://host/a.js", Status: 200, SHA256: "abc"},
			want:     false,
		},
		{
			name:     "4xx is not a redirect",
			resource: offlinecache.Resource{URL: "https://host/a.js", Status: 404},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.resource.IsRedirect())
		})
	}
}

func TestItemRecord_ResourceByURL(t *testing.T) {
	record := offlinecache.ItemRecord{
		Resources: []offlinecache.Resource{
			{URL: "https://host/index.html", Status: 200, SHA256: "aaa"},
			{URL: "https://host/app.js", Status: 200, SHA256: "bbb"},
		},
	}

	t.Run("known url returns its resource", func(t *testing.T) {
		res, ok := record.ResourceByURL("https://host/app.js")
		assert.True(t, ok)
		assert.Equal(t, "bbb", res.SHA256)
	})

	t.Run("unknown url returns zero value and false", func(t *testing.T) {
		res, ok := record.ResourceByURL("https://host/missing.js")
		assert.False(t, ok)
		assert.Equal(t, offlinecache.Resource{}, res)
	})

	t.Run("empty resources returns false", func(t *testing.T) {
		empty := offlinecache.ItemRecord{}
		_, ok := empty.ResourceByURL("https://host/index.html")
		assert.False(t, ok)
	})
}

// TestSourceKey_GoldenValue pins SourceKey's output format as an on-disk
// and wire contract: record filenames and status paging cursors are both
// built from it, so any change to the hash function, encoding, or casing
// is a silent cache-invalidation/format break this test makes loud.
func TestSourceKey_GoldenValue(t *testing.T) {
	// Independently computed hex(sha256("https://example.com/art")).
	assert.Equal(t,
		"c4192ef8bbedd92f87c9f156e01e9f0ffdfa0954f2df3ed82be333b858aa01af",
		offlinecache.SourceKey("https://example.com/art"))
	// Byte-exact: trivially different spellings are distinct identities.
	assert.NotEqual(t,
		offlinecache.SourceKey("https://example.com/art"),
		offlinecache.SourceKey("https://example.com/art/"))
}
