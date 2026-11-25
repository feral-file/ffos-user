package commandrouter

import (
	"context"
	"errors"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// displayedSelectorArg is downloadPlaylistItem's source-free way to name an
// item (feral-file/feral-file#3633): "the item at this index of what you
// are displaying", for a controller that never held the document. The
// player_status every controller receives blanks item sources
// (status.lightweightPlayerStatus), so such a controller can name an
// on-screen item only by its position and id.
const displayedSelectorArg = "displayed"

// displayedSelector is the parsed `displayed` argument.
//
// index is the item's position in player_status `items`, which is the
// player's admitted list (filtered, narrowed, shuffled) and the same list,
// index for index, that FetchPlayerStatus returns here: the notification
// only blanks sources on that same slice.
//
// id, when given, must equal that item's DP-1 id. It is what makes a stale
// index safe: if the display changed between the caller's status and this
// command, the id no longer matches and nothing is queued. DP-1 makes ids
// optional, so it cannot be required here; callers that want the guard
// (ff-app, ff-cli) refuse to send the selector for an item without one.
type displayedSelector struct {
	index int
	id    string
}

// parseDisplayedSelector returns the selector, or a ready invalid_request
// body. A malformed selector and one combined with any other way of naming
// the item are both client bugs: answering them would mean guessing which
// of two names the caller meant.
func parseDisplayedSelector(args map[string]any) (displayedSelector, map[string]any) {
	for _, other := range []string{"source", "playlistUrl", "dp1_call"} {
		if _, present := args[other]; present {
			return displayedSelector{}, errorResponse("invalid_request",
				"displayed cannot be combined with "+other, false)
		}
	}
	raw, ok := args[displayedSelectorArg].(map[string]any)
	if !ok {
		return displayedSelector{}, errorResponse("invalid_request", "displayed must be an object", false)
	}
	index, ok := intArg(raw["index"])
	if !ok {
		return displayedSelector{}, errorResponse("invalid_request", "displayed.index must be an integer", false)
	}
	sel := displayedSelector{index: index}
	if rawID, present := raw["id"]; present {
		id, ok := rawID.(string)
		if !ok || id == "" {
			return displayedSelector{}, errorResponse("invalid_request", "displayed.id must be a non-empty string", false)
		}
		sel.id = id
	}
	return sel, nil
}

// handleDownloadDisplayedItem keeps the item the player is displaying at
// sel.index. It adds no persistence of its own; the two cast shapes map
// onto what already exists:
//
//   - A URL cast (the default playlist included) is handed to
//     handleDownloadPlaylistItem as {playlistUrl, source}, exactly the
//     request a controller holding the URL would send. The playlist
//     record, its series sidecar and the URL index are written by that
//     path from a fresh resolve, never from the status list.
//
//   - An inline or dynamic cast queues the item and writes NO playlist
//     record. The status list is already narrowed and filtered, so a record
//     rebuilt from it would replace a fuller record with the same id (a
//     whole-playlist download) and orphan the members it drops, and a
//     check-then-save would race DownloadPlaylist. Nothing needs the record
//     for a work kept this way: replay after an offline reboot is armed from
//     source keys (replay-scope.json), not playlist records; status and GC
//     are per source; an inline playlist has no URL any fallback could
//     reach its record by. The cost: clearPlaylistCache for that playlist id
//     does not include this work; clearPlaylistItemCache by source does.
//
// The reply carries the item's real source on purpose: it is how a
// controller that never held the document learns the cache identity every
// other offline-cache command keys on. Sources still never go onto the
// player_status broadcast. The reply widens nothing: a direct checkStatus
// reply already carries every on-screen source to the same tier. See the
// Security Notes in docs/controld-inbound-controller-messages.md.
func (h *handler) handleDownloadDisplayedItem(ctx context.Context, args map[string]any) (interface{}, error) {
	sel, invalid := parseDisplayedSelector(args)
	if invalid != nil {
		return invalid, nil
	}
	if h.statusPoller == nil {
		return errorResponse("internal", "player status is not available", false), nil
	}

	// Sampled before the status read for the same reason
	// handleDownloadPlaylistItem samples it before its resolve: a clear
	// that completes while we read must still win. The URL branch below
	// samples its own barrier inside the delegated handler, which is after
	// this read: a clear that completed during the read is then ordered
	// before the download, as if the caller had sent it after the clear.
	barrier := h.offlineCache.ClearBarrier()

	playerStatus, err := h.statusPoller.FetchPlayerStatus(ctx)
	if err != nil {
		return errorResponse("resolve_failed", "could not read what is on display: "+err.Error(), true), nil
	}
	item, found := displayedItem(playerStatus, sel)
	if !found {
		return errorResponse("not_found",
			"the item is not on display (nothing is playing, or the display changed)", false), nil
	}

	if playerStatus.PlaylistURL != nil && *playerStatus.PlaylistURL != "" {
		return h.handleDownloadPlaylistItem(ctx, map[string]any{
			"playlistUrl": *playerStatus.PlaylistURL,
			"source":      item.Source,
		})
	}

	playlistID := ""
	if playerStatus.Playlist != nil {
		playlistID = playerStatus.Playlist.ID
	}
	if h.offlineCache.ClearedSinceBarrier(playlistID, barrier, item.Source) {
		return errorResponse("busy",
			"a clear for this playlist or item landed while it was being read, so nothing was queued", true), nil
	}
	status := "queued"
	if err := h.offlineCache.DownloadItem(ctx, item); err != nil {
		if !errors.Is(err, offlinecache.ErrItemInlineNotQueued) {
			return offlineCacheErrorResponse(err), nil
		}
		// A data: source carries its own bytes, and the reply hands it
		// back, so there is nothing to download. Unlike the existing
		// inline outcome (handleDownloadPlaylistItem), there is also no
		// playlist body to persist: this path never writes a record, and
		// the bytes do not depend on one. Do not "align" the two paths
		// by adding a save here (see AGENTS.md, the displayed selector).
		status = "not_queued_inline"
	}
	return map[string]any{"ok": true, "status": status, "source": item.Source}, nil
}

// displayedItem is the item sel names in playerStatus, if it is on display.
// Both playlist cast commands count: the default playlist is a work no
// controller sent, which is exactly the case this selector exists for.
func displayedItem(playerStatus *status.PlayerStatus, sel displayedSelector) (dp1playlist.PlaylistItem, bool) {
	if playerStatus == nil || playerStatus.Items == nil {
		return dp1playlist.PlaylistItem{}, false
	}
	if playerStatus.Command != string(commands.CMD_DISPLAY_PLAYLIST) &&
		playerStatus.Command != string(commands.CMD_DISPLAY_DEFAULT_PLAYLIST) {
		return dp1playlist.PlaylistItem{}, false
	}
	items := *playerStatus.Items
	if sel.index < 0 || sel.index >= len(items) {
		return dp1playlist.PlaylistItem{}, false
	}
	item := items[sel.index]
	if sel.id != "" && item.ID != sel.id {
		return dp1playlist.PlaylistItem{}, false
	}
	if item.Source == "" {
		return dp1playlist.PlaylistItem{}, false
	}
	return item, true
}
