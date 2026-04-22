package refresher

import (
	"strconv"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// noteFingerprint is visibility-only: intermission note text. Note Duration is ignored (timing-only),
// consistent with ignoring item Duration.
func noteFingerprint(n *playlists.Note) string {
	if n == nil {
		return ""
	}
	return n.Text
}

func itemStableID(it dp1playlist.PlaylistItem, idx int) string {
	if it.ID != "" {
		return it.ID
	}
	return "__idx__" + strconv.Itoa(idx)
}

// visiblePlaylistChanged compares two fully resolved playlists for differences that affect what
// the viewer sees as the cast surface: playlist-level and per-item intermission note **text**
// (playlists extension; note Duration ignored), per-slot item identity (including playback order
// when items carry stable ids), per-item media URL (Source), and add/remove/replace. Item Duration
// tweaks alone are ignored (timing-only); other non-address metadata (title, license, etc.) is
// also ignored so the caller can soft-refresh unless the address or ordered identity changed.
//
// Items without id use a per-index fallback key only; reordering two such items without changing
// notes or Source is still treated as unchanged (same limitation as before).
func visiblePlaylistChanged(prev, next *dp1.Playlist) bool {
	if prev == nil || next == nil {
		return true
	}
	if noteFingerprint(prev.Note) != noteFingerprint(next.Note) {
		return true
	}
	if len(prev.Items) != len(next.Items) {
		return true
	}
	for i := range prev.Items {
		pIt, nIt := prev.Items[i], next.Items[i]
		if itemStableID(pIt, i) != itemStableID(nIt, i) {
			return true
		}
		if pIt.Source != nIt.Source {
			return true
		}
		if noteFingerprint(pIt.Note) != noteFingerprint(nIt.Note) {
			return true
		}
	}
	return false
}
