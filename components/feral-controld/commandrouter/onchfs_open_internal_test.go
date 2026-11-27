package commandrouter

import (
	"strings"
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

// The spellings a controller can name an onchfs work by (#3636): the raw URI
// app 1.9.28 sends, the dead gateway URL app main sends, and the device-local
// source a row built from a device status keeps. Each must come out as the
// exact source of the item it names, which here carries the Tezos label its
// provenance gave it, so the player's exact match finds it.
func TestOnchfsStartItem(t *testing.T) {
	const root = "1639e8c989cddd2c0f9e951b126a21b1964309854c0a8e0a9b1193a655c6712a"
	edition1 := dp1.OnchfsSource("onchfs://tezos/" + root + "/?fxhash=1")
	edition2 := dp1.OnchfsSource("onchfs://tezos/" + root + "/?fxhash=2")
	plain := "https://origin.example/work.mp4"
	items := []dp1playlist.PlaylistItem{{ID: "a", Source: plain}, {ID: "b", Source: edition2}, {ID: "c", Source: edition1}}

	renamed := []struct {
		name   string
		source string
	}{
		{"raw URI, no chain", "onchfs://" + root + "/?fxhash=1"},
		{"raw URI, no slash before the query", "onchfs://" + root + "?fxhash=1"},
		{"raw URI, upper-case root", "onchfs://" + strings.ToUpper(root) + "/?fxhash=1"},
		{"dead fxhash gateway", "https://onchfs.fxhash2.xyz/" + root + "/?fxhash=1"},
		{"device-local, no chain label", dp1.OnchfsSource("onchfs://" + root + "/?fxhash=1")},
	}
	for _, tc := range renamed {
		t.Run(tc.name, func(t *testing.T) {
			in := map[string]interface{}{"id": "regenerated", "source": tc.source, "extra": true}
			got := onchfsStartItem(items, in)
			assert.Equal(t, map[string]interface{}{"id": "regenerated", "source": edition1, "extra": true}, got)
			assert.Equal(t, tc.source, in["source"], "the caller's map is not written")
			assert.Equal(t, 2, startItemIndex(items, got), "the opening lands on the named edition")
		})
	}

	unchanged := []struct {
		name      string
		items     []dp1playlist.PlaylistItem
		startItem interface{}
	}{
		{"absent", items, nil},
		{"malformed", items, "c"},
		{"no source", items, map[string]interface{}{"id": "c"}},
		{"source not a string", items, map[string]interface{}{"source": 7}},
		{"ordinary source", items, map[string]interface{}{"source": plain}},
		{"exact source already", items, map[string]interface{}{"source": edition1}},
		{"an edition not in the list", items, map[string]interface{}{"source": "onchfs://" + root + "/?fxhash=3"}},
		{"not onchfs and not in the list", items, map[string]interface{}{"source": "https://elsewhere.example/x"}},
		{"empty list", nil, map[string]interface{}{"source": "onchfs://" + root + "/?fxhash=1"}},
	}
	for _, tc := range unchanged {
		t.Run("unchanged: "+tc.name, func(t *testing.T) {
			assert.Equal(t, tc.startItem, onchfsStartItem(tc.items, tc.startItem))
		})
	}

	t.Run("a work that repeats: the first item holding it", func(t *testing.T) {
		evm := dp1.OnchfsSource("onchfs://eip155:1/" + root + "/?fxhash=1")
		twice := []dp1playlist.PlaylistItem{{ID: "a", Source: plain}, {ID: "b", Source: evm}, {ID: "c", Source: edition1}}
		got := onchfsStartItem(twice, map[string]interface{}{"source": "onchfs://" + root + "/?fxhash=1"})
		assert.Equal(t, map[string]interface{}{"source": evm}, got)
	})
	t.Run("an exact holder later in the list wins over an earlier re-spelling", func(t *testing.T) {
		evm := dp1.OnchfsSource("onchfs://eip155:1/" + root + "/?fxhash=1")
		list := []dp1playlist.PlaylistItem{{ID: "a", Source: evm}, {ID: "b", Source: edition1}}
		in := map[string]interface{}{"source": edition1}
		assert.Equal(t, in, onchfsStartItem(list, in), "the player matches the exact holder; nothing to re-spell")
	})
}
