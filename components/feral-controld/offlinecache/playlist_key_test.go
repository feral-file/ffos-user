package offlinecache_test

import (
	"testing"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

func TestPlaylistRecordKey(t *testing.T) {
	a := []dp1playlist.PlaylistItem{{Source: "https://example.com/a"}}
	b := []dp1playlist.PlaylistItem{{Source: "https://example.com/b"}}
	key := offlinecache.PlaylistRecordKey

	// An id wins, so records already on disk keep their names.
	assert.Equal(t, "abc", key("abc", "https://example.com/p", a))
	// By URL: stable while the document behind it changes.
	assert.Equal(t, key("", "https://example.com/p", a), key("", "https://example.com/p", b))
	assert.NotEqual(t, key("", "https://example.com/p", a), key("", "https://example.com/q", a))
	// Inline: by the ordered sources.
	assert.Equal(t, key("", "", a), key("", "", a))
	assert.NotEqual(t, key("", "", a), key("", "", b))
	assert.Regexp(t, `^noid-[0-9a-f]{64}$`, key("", "", nil))
}
