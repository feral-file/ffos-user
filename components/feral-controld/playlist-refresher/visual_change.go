package refresher

import (
	"strconv"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

func noteFingerprint(n *playlists.Note) string {
	if n == nil {
		return ""
	}
	s := n.Text
	if n.Duration != nil {
		s += "\x1e" + strconv.FormatFloat(*n.Duration, 'g', -1, 64)
	}
	return s
}

func itemStableID(it dp1playlist.PlaylistItem, idx int) string {
	if it.ID != "" {
		return it.ID
	}
	return "__idx__" + strconv.Itoa(idx)
}

// visiblePlaylistChanged compares two fully resolved playlists for differences that affect what
// the viewer sees as the cast surface: playlist-level and per-item intermission notes (playlists
// extension), and the identity set of items (add/remove/replace by stable id). Non-visual edits
// (e.g. duration or media URL tweaks without note/title-card changes) return false so the caller
// can apply a soft refresh instead of restarting playback.
func visiblePlaylistChanged(prev, next *dp1.Playlist) bool {
	if prev == nil || next == nil {
		return true
	}
	if noteFingerprint(prev.Note) != noteFingerprint(next.Note) {
		return true
	}
	prevByKey := make(map[string]dp1playlist.PlaylistItem)
	for i, it := range prev.Items {
		prevByKey[itemStableID(it, i)] = it
	}
	nextByKey := make(map[string]dp1playlist.PlaylistItem)
	for i, it := range next.Items {
		nextByKey[itemStableID(it, i)] = it
	}
	if len(prevByKey) != len(nextByKey) {
		return true
	}
	for k := range prevByKey {
		if _, ok := nextByKey[k]; !ok {
			return true
		}
	}
	for k, pIt := range prevByKey {
		nIt := nextByKey[k]
		if noteFingerprint(pIt.Note) != noteFingerprint(nIt.Note) {
			return true
		}
	}
	return false
}
