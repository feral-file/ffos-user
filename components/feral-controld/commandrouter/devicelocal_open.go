package commandrouter

import (
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
)

// deviceLocalOpening returns the display request's items reordered so the work the display request
// opens on comes first, and how long the display request should wait for it to be read
// from chain: deviceLocalOpenWait when that work is onchfs, zero otherwise.
//
// Why: DeviceLocalMaterializer.Ensure waits for the FIRST onchfs work in the list
// it is given. Handed the list as is, a wall that opens on an ordinary IPFS
// work but carries one onchfs work at position 40 waited up to 15 s on every
// display while the chain RPCs were unreachable (a captive or filtered network),
// for a work nobody would see for an hour. Only the opening work is worth a
// wait; the rest are read in the background and widen the replay scope when
// they land. The reorder only steers Ensure: the list sent to the player is
// untouched.
//
// The opening work is found the way the player finds it (ff-player
// src/utils/startItem.ts): startItem.id across the whole list first, then
// startItem.source, else item 0. A startItem whose source names the work's
// raw onchfs:// or web3:// URI rather than its device-local source falls back
// to item 0, which only costs the wait it would have cost before this rule
// existed.
func deviceLocalOpening(items []dp1playlist.PlaylistItem, startItem interface{}) ([]dp1playlist.PlaylistItem, time.Duration) {
	if len(items) == 0 {
		return items, 0
	}
	open := startItemIndex(items, startItem)
	if !contentpath.IsDeviceLocalSource(items[open].Source) {
		return items, 0
	}
	if open == 0 {
		return items, deviceLocalOpenWait
	}
	ordered := make([]dp1playlist.PlaylistItem, 0, len(items))
	ordered = append(ordered, items[open])
	ordered = append(ordered, items[:open]...)
	ordered = append(ordered, items[open+1:]...)
	return ordered, deviceLocalOpenWait
}

// startItemIndex mirrors the player's startItemIndex: 0 when startItem is
// absent, malformed or names no item in the list.
func startItemIndex(items []dp1playlist.PlaylistItem, startItem interface{}) int {
	m, ok := startItem.(map[string]interface{})
	if !ok {
		return 0
	}
	if id, _ := m["id"].(string); id != "" {
		for i, item := range items {
			if item.ID == id {
				return i
			}
		}
	}
	if source, _ := m["source"].(string); source != "" {
		for i, item := range items {
			if item.Source == source {
				return i
			}
		}
	}
	return 0
}
