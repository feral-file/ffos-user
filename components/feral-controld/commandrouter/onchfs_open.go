package commandrouter

import (
	"maps"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpath"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// onchfsStartItem returns startItem with an onchfs source re-spelled as the
// item it names, so the player's exact source match finds it
// (feral-file/feral-file#3636).
//
// Why: every onchfs item reaches the player under its device-local source
// (dp1/onchfs.go), but a controller names the work the way its rows hold it:
// the raw `onchfs://` URI (app 1.9.28, the indexer's form since
// ff-indexer-v2#180) or the dead fxhash gateway URL (app main, ff-app#1299).
// Neither is the string the player holds, and the player compares sources
// exactly (ff-player src/utils/startItem.ts), so a display request of a
// wallet collection, whose item ids the device regenerates, opened on item 0.
//
// What must stay true:
//   - Only the spelling changes: the match is dp1.SameOnchfsWork (root,
//     path, query, fragment, whatever chain label either side carries), so two fxhash
//     editions of one project, which differ only in their query, stay
//     distinct. The chain label is ignored because an item's label comes
//     from its provenance, which the controller's spelling cannot carry.
//   - The value the item holds is copied verbatim, so the player's own
//     comparison is the one that decides.
//   - A source some item already holds exactly is left alone: the player
//     would match that item first, and re-spelling could move the opening.
//   - Anything else (absent, malformed, not onchfs, no such work in items)
//     is returned unchanged, which is the player's start-at-the-beginning.
//   - The caller's map is never written: a copy carries the new source.
//
// The id is not consulted: the player tries it first, and a source re-spelled
// beside an id that matches is never read.
func onchfsStartItem(items []dp1playlist.PlaylistItem, startItem interface{}) interface{} {
	m, ok := startItem.(map[string]interface{})
	if !ok {
		return startItem
	}
	source, _ := m["source"].(string)
	if source == "" {
		return startItem
	}
	for _, item := range items {
		if item.Source == source {
			return startItem
		}
	}
	for _, item := range items {
		if dp1.SameOnchfsWork(source, item.Source) {
			out := maps.Clone(m)
			out["source"] = item.Source
			return out
		}
	}
	return startItem
}

// onchfsOpening returns the display request's items reordered so the work the display request
// opens on comes first, and how long the display request should wait for it to be read
// from chain: onchfsCastWait when that work is onchfs, zero otherwise.
//
// Why: OnchfsMaterializer.Ensure waits for the FIRST onchfs work in the list
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
// startItem.source, else item 0. The caller passes the startItem
// onchfsStartItem already re-spelled, so a source naming the work by its
// onchfs:// URI or the dead gateway finds it here exactly as it does in the
// player.
func onchfsOpening(items []dp1playlist.PlaylistItem, startItem interface{}) ([]dp1playlist.PlaylistItem, time.Duration) {
	if len(items) == 0 {
		return items, 0
	}
	open := startItemIndex(items, startItem)
	if !contentpath.IsOnchfsSourceURL(items[open].Source) {
		return items, 0
	}
	if open == 0 {
		return items, onchfsCastWait
	}
	ordered := make([]dp1playlist.PlaylistItem, 0, len(items))
	ordered = append(ordered, items[open])
	ordered = append(ordered, items[:open]...)
	ordered = append(ordered, items[open+1:]...)
	return ordered, onchfsCastWait
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
