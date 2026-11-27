package offlinecache

import (
	"crypto/sha256"
	"encoding/hex"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
)

// derivedPlaylistKeyPrefix marks a record key controld derived itself. `id`
// is format:uuid where present, so no conforming document can collide with it.
const derivedPlaylistKeyPrefix = "noid-"

// PlaylistRecordKey is the key a playlist's offline record, clear generation
// and URL index are filed under.
//
// DP-1 makes `id` optional, so a document without one is cached under a key
// derived from what stays the same between two downloads of it: the URL it was
// requested by, or, for an inline document, its ordered item sources. A
// document that carries an id keeps it, so every record already on disk keeps
// its name.
//
// Every caller that samples or checks a clear generation for a playlist must
// use this, with the same sourceURL the download passes, or a clear racing the
// download is checked against a different key than the one saved.
func PlaylistRecordKey(id, sourceURL string, items []dp1playlist.PlaylistItem) string {
	if id != "" {
		return id
	}
	h := sha256.New()
	if sourceURL != "" {
		h.Write([]byte("url\x00" + sourceURL))
	} else {
		h.Write([]byte("sources"))
		for _, item := range items {
			h.Write([]byte("\x00" + item.Source))
		}
	}
	return derivedPlaylistKeyPrefix + hex.EncodeToString(h.Sum(nil))
}
