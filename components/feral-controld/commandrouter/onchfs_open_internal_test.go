package commandrouter

import (
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

func TestOnchfsOpening(t *testing.T) {
	onchfs := dp1.OnchfsSource("onchfs://1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a/?fxhash=0x06ec")
	ipfs := "https://ipfs.example/ipfs/bafy/index.html"
	other := "https://origin.example/work.mp4"

	cases := []struct {
		name      string
		items     []dp1playlist.PlaylistItem
		startItem interface{}
		wantFirst string // the first item Ensure sees; the list is reordered only when it waits
		wantWait  time.Duration
	}{
		{"onchfs opens the display request: wait for it", []dp1playlist.PlaylistItem{{ID: "a", Source: onchfs}, {ID: "b", Source: ipfs}}, nil, onchfs, onchfsCastWait},
		{"ordinary work opens, onchfs later: no wait", []dp1playlist.PlaylistItem{{ID: "a", Source: ipfs}, {ID: "b", Source: other}, {ID: "c", Source: onchfs}}, nil, ipfs, 0},
		{"startItem by id names the onchfs work: it goes first", []dp1playlist.PlaylistItem{{ID: "a", Source: ipfs}, {ID: "c", Source: onchfs}}, map[string]interface{}{"id": "c"}, onchfs, onchfsCastWait},
		{"startItem by source", []dp1playlist.PlaylistItem{{ID: "a", Source: ipfs}, {ID: "c", Source: onchfs}}, map[string]interface{}{"source": onchfs}, onchfs, onchfsCastWait},
		{"id wins over source", []dp1playlist.PlaylistItem{{ID: "a", Source: onchfs}, {ID: "b", Source: ipfs}}, map[string]interface{}{"id": "b", "source": onchfs}, onchfs, 0},
		{"startItem naming an ordinary work: no wait", []dp1playlist.PlaylistItem{{ID: "a", Source: onchfs}, {ID: "b", Source: ipfs}}, map[string]interface{}{"id": "b"}, onchfs, 0},
		{"unknown startItem falls back to item 0", []dp1playlist.PlaylistItem{{ID: "a", Source: onchfs}, {ID: "b", Source: ipfs}}, map[string]interface{}{"id": "zzz"}, onchfs, onchfsCastWait},
		{"malformed startItem falls back to item 0", []dp1playlist.PlaylistItem{{ID: "a", Source: ipfs}, {ID: "b", Source: onchfs}}, "b", ipfs, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, wait := onchfsOpening(tc.items, tc.startItem)
			assert.Equal(t, tc.wantWait, wait)
			assert.Equal(t, tc.wantFirst, got[0].Source)
			assert.Len(t, got, len(tc.items), "the reorder keeps every work")
		})
	}

	t.Run("the caller's list is not reordered", func(t *testing.T) {
		items := []dp1playlist.PlaylistItem{{ID: "a", Source: ipfs}, {ID: "c", Source: onchfs}}
		_, _ = onchfsOpening(items, map[string]interface{}{"id": "c"})
		assert.Equal(t, ipfs, items[0].Source)
	})
	t.Run("empty list", func(t *testing.T) {
		got, wait := onchfsOpening(nil, nil)
		assert.Empty(t, got)
		assert.Zero(t, wait)
	})
}
