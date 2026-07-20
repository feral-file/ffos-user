package commandrouter

import (
	"context"
	"errors"
	"fmt"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// offlineCacheCommands is deliberately separate from deviceCtlCommands: these
// are controld-owned, pre-CDP commands (same precedent as mint pairing in
// handler.go) and must never be forwarded to window.handleCDPRequest.
var offlineCacheCommands = map[commands.Type]bool{
	commands.CMD_DOWNLOAD_PLAYLIST_ITEM:    true,
	commands.CMD_DOWNLOAD_PLAYLIST:         true,
	commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE: true,
	commands.CMD_CLEAR_PLAYLIST_CACHE:      true,
	commands.CMD_GET_OFFLINE_CACHE_STATUS:  true,
}

func isOfflineCacheCommand(t commands.Type) bool {
	return offlineCacheCommands[t]
}

// handleOfflineCacheCommand dispatches one of the 5 offline-cache commands.
// Every branch returns (result, nil) with an explicit ok/error body rather
// than a Go error — matching the mint-pairing precedent in handler.go and
// the explicit RPC ok/error shape docs/controld-inbound-controller-messages.md
// mandates for new commands (ok, error.code, retryable, echoed by the
// mediator via messageID).
func (h *handler) handleOfflineCacheCommand(ctx context.Context, commandType commands.Type, args map[string]any) (interface{}, error) {
	if h.offlineCache == nil {
		return disabledResponse("offline caching is not enabled"), nil
	}

	switch commandType {
	case commands.CMD_DOWNLOAD_PLAYLIST_ITEM:
		return h.handleDownloadPlaylistItem(ctx, args)
	case commands.CMD_DOWNLOAD_PLAYLIST:
		return h.handleDownloadPlaylist(ctx, args)
	case commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE:
		return h.handleClearPlaylistItemCache(ctx, args)
	case commands.CMD_CLEAR_PLAYLIST_CACHE:
		return h.handleClearPlaylistCache(ctx, args)
	case commands.CMD_GET_OFFLINE_CACHE_STATUS:
		return h.handleGetOfflineCacheStatus(args)
	default:
		// Unreachable: isOfflineCacheCommand only returns true for the
		// cases above.
		return nil, fmt.Errorf("offline cache: unhandled command %s", commandType)
	}
}

func (h *handler) handleDownloadPlaylistItem(ctx context.Context, args map[string]any) (interface{}, error) {
	itemID, ok := stringArg(args["itemId"])
	if !ok {
		return errorResponse("invalid_request", "itemId is required", false), nil
	}

	playlist, err := h.resolveOfflineCachePlaylist(ctx, args)
	if err != nil {
		return errorResponse("resolve_failed", err.Error(), true), nil
	}

	item, found := findPlaylistItem(playlist, itemID)
	if !found {
		return errorResponse("not_found", "itemId not found in playlist", false), nil
	}

	if err := h.offlineCache.DownloadItem(ctx, item); err != nil {
		return offlineCacheErrorResponse(err), nil
	}
	return map[string]any{"ok": true, "status": "queued", "itemId": itemID}, nil
}

func (h *handler) handleDownloadPlaylist(ctx context.Context, args map[string]any) (interface{}, error) {
	playlist, err := h.resolveOfflineCachePlaylist(ctx, args)
	if err != nil {
		return errorResponse("resolve_failed", err.Error(), true), nil
	}

	raw, err := h.json.Marshal(playlist)
	if err != nil {
		h.logger.Error("offline cache: failed to marshal resolved playlist", zap.Error(err))
		return errorResponse("internal", "failed to encode resolved playlist", true), nil
	}

	// sourceURL, when the download was requested by playlistUrl rather
	// than an inline dp1_call, lets Service index this playlist so a
	// later displayPlaylist-by-URL for the same URL can still find and
	// replay it from cache if live DP-1 resolution fails (e.g. no
	// network) — see handler.go's displayPlaylist branch and
	// Service.CachedPlaylistForURL. "" (inline download) means no such
	// fallback is possible for this playlist, same as before this field
	// existed.
	sourceURL, _ := stringArg(args["playlistUrl"])

	queued, total, err := h.offlineCache.DownloadPlaylist(ctx, raw, sourceURL)
	if err != nil {
		return offlineCacheErrorResponse(err), nil
	}
	return map[string]any{"ok": true, "status": "queued", "total": total, "softwareCount": queued}, nil
}

func (h *handler) handleClearPlaylistItemCache(ctx context.Context, args map[string]any) (interface{}, error) {
	itemID, ok := stringArg(args["itemId"])
	if !ok {
		return errorResponse("invalid_request", "itemId is required", false), nil
	}
	if err := h.offlineCache.ClearItem(itemID); err != nil {
		return offlineCacheErrorResponse(err), nil
	}
	h.resyncKioskReplayScopeAfterClear(ctx)
	return map[string]any{"ok": true, "itemId": itemID}, nil
}

func (h *handler) handleClearPlaylistCache(ctx context.Context, args map[string]any) (interface{}, error) {
	playlistID, ok := stringArg(args["playlistId"])
	if !ok {
		return errorResponse("invalid_request", "playlistId is required", false), nil
	}
	if err := h.offlineCache.ClearPlaylist(playlistID); err != nil {
		return offlineCacheErrorResponse(err), nil
	}
	h.resyncKioskReplayScopeAfterClear(ctx)
	return map[string]any{"ok": true, "playlistId": playlistID}, nil
}

// resyncKioskReplayScopeAfterClear re-syncs replay's live Fetch-
// interception scope with whichever items are still cached, immediately
// after a successful clear. Without this, ClearItem/ClearPlaylist only
// touch the store (see their docs) — if the item just cleared is the one
// currently displayed, replayer's in-memory resources map keeps stale
// entries pointing at now-deleted blobs until the next displayPlaylist
// command or playlist-refresher's periodic pass (up to
// refresher.PLAYLIST_REFRESH_INTERVAL later), during which every request
// for it is a miss instead of either serving correctly or falling back to
// the network cleanly.
//
// Best-effort and fire-and-forget: a resync failure here must never turn a
// successful clear into a reported error, since the clear itself already
// fully succeeded against the store. Mirrors the same
// FetchPlayerStatus->resolve->SyncPlaylist shape Process's
// CMD_DISPLAY_PLAYLIST branch and playlist-refresher's own
// processPlayingPlaylist independently implement for their own inputs
// (RPC command arguments vs. polled player status respectively) —
// commandrouter and playlist-refresher intentionally do not depend on each
// other (see AGENTS.md's service-boundary guidance), so this is kept as
// its own small instance of that shape rather than reaching across the
// package boundary for it.
func (h *handler) resyncKioskReplayScopeAfterClear(ctx context.Context) {
	if h.kioskReplay == nil || h.statusPoller == nil {
		return
	}
	playerStatus, err := h.statusPoller.FetchPlayerStatus(ctx)
	if err != nil {
		h.logger.Warn("offline cache: failed to fetch player status to resync replay scope after clear", zap.Error(err))
		return
	}
	if playerStatus == nil || playerStatus.Command != string(commands.CMD_DISPLAY_PLAYLIST) {
		return
	}

	playlist, err := h.resolveDisplayedPlaylist(ctx, playerStatus)
	if err != nil {
		h.logger.Warn("offline cache: failed to resolve currently displayed playlist to resync replay scope after clear", zap.Error(err))
		return
	}
	if playlist == nil {
		return
	}

	itemIDs := make([]string, 0, len(playlist.Items))
	for _, item := range playlist.Items {
		itemIDs = append(itemIDs, item.ID)
	}
	if syncErr := h.kioskReplay.SyncPlaylist(ctx, itemIDs); syncErr != nil {
		h.logger.Warn("offline cache: failed to sync kiosk replay scope after clear", zap.Error(syncErr))
	}
}

// resolveDisplayedPlaylist mirrors playlist-refresher's own
// playerStatus.PlaylistURL/Playlist resolution (see
// refresher.processPlayingPlaylist). A nil, nil return means the player
// status carried neither a URL nor an inline playlist (as opposed to an
// error resolving one it did carry), which the caller treats as "nothing
// to resync."
func (h *handler) resolveDisplayedPlaylist(ctx context.Context, playerStatus *status.PlayerStatus) (*dp1.Playlist, error) {
	switch {
	case playerStatus.PlaylistURL != nil:
		return h.dp1.ProcessPlaylistURL(ctx, *playerStatus.PlaylistURL, false)
	case playerStatus.Playlist != nil:
		if !playerStatus.Playlist.HasDynamicContent() {
			return playerStatus.Playlist, nil
		}
		return h.dp1.ProcessDynamicPlaylist(ctx, *playerStatus.Playlist, false)
	default:
		return nil, nil
	}
}

func (h *handler) handleGetOfflineCacheStatus(args map[string]any) (interface{}, error) {
	itemIDs := stringSliceArg(args["itemIds"])
	snapshot, err := h.offlineCache.Status(itemIDs)
	if err != nil {
		return offlineCacheErrorResponse(err), nil
	}
	return map[string]any{
		"ok":       true,
		"items":    snapshot.Items,
		"totals":   snapshot.Totals,
		"diskUsed": snapshot.DiskUsedBytes,
	}, nil
}

// resolveOfflineCachePlaylist mirrors the playlistUrl/dp1_call resolution
// Process already does for CMD_DISPLAY_PLAYLIST, but with minimal=false: a
// download request wants the whole playlist (up to dp1.MAX_PLAYLIST_ITEMS_LIMIT
// items), not the minimal first batch a live display uses. Kept as its own
// helper (rather than sharing displayPlaylist's inline switch) so the
// display-integration todo can evolve that path independently.
func (h *handler) resolveOfflineCachePlaylist(ctx context.Context, args map[string]any) (*dp1.Playlist, error) {
	switch {
	case args["playlistUrl"] != nil:
		url, ok := args["playlistUrl"].(string)
		if !ok || url == "" {
			return nil, fmt.Errorf("playlistUrl is not a string or empty")
		}
		return h.dp1.ProcessPlaylistURL(ctx, url, false)

	case args["dp1_call"] != nil:
		playlistMap, ok := args["dp1_call"].(map[string]interface{})
		if !ok {
			return nil, fmt.Errorf("playlist is not a map")
		}

		playlistBytes, err := h.json.Marshal(playlistMap)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal playlist: %w", err)
		}

		var playlist *dp1.Playlist
		if err := h.json.Unmarshal(playlistBytes, &playlist); err != nil {
			return nil, fmt.Errorf("failed to unmarshal playlist: %w", err)
		}

		if playlist.HasDynamicContent() {
			return h.dp1.ProcessDynamicPlaylist(ctx, *playlist, false)
		}
		return playlist, nil

	default:
		return nil, fmt.Errorf("request must include playlistUrl or dp1_call")
	}
}

// loadCachedPlaylistForURL is handler.go's displayPlaylist-by-URL offline
// fallback: the raw body it returns was already fully resolved (dynamic
// content materialized) and signature-verified once, back when
// downloadPlaylist originally saved it — see Service.CachedPlaylistForURL
// and DownloadPlaylist's docs — so unmarshaling it here needs no further
// DP-1 processing. Returns an error whenever there is nothing to fall
// back to (offline caching disabled, url was never downloaded, the
// downloaded copy has since been cleared, or the saved body is
// unexpectedly malformed), so the caller can report the original live
// resolution error instead.
func (h *handler) loadCachedPlaylistForURL(url string) (*dp1.Playlist, error) {
	if h.offlineCache == nil {
		return nil, fmt.Errorf("offline cache: disabled, no cached fallback for %s", url)
	}
	raw, err := h.offlineCache.CachedPlaylistForURL(url)
	if err != nil {
		return nil, fmt.Errorf("offline cache: no cached playlist for %s: %w", url, err)
	}
	var playlist *dp1.Playlist
	if err := h.json.Unmarshal(raw, &playlist); err != nil {
		return nil, fmt.Errorf("offline cache: parse cached playlist for %s: %w", url, err)
	}
	return playlist, nil
}

func findPlaylistItem(playlist *dp1.Playlist, itemID string) (dp1playlist.PlaylistItem, bool) {
	if playlist == nil {
		return dp1playlist.PlaylistItem{}, false
	}
	for _, item := range playlist.Items {
		if item.ID == itemID {
			return item, true
		}
	}
	return dp1playlist.PlaylistItem{}, false
}

func stringArg(v any) (string, bool) {
	s, ok := v.(string)
	if !ok || s == "" {
		return "", false
	}
	return s, true
}

func stringSliceArg(v any) []string {
	raw, ok := v.([]interface{})
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		if s, ok := item.(string); ok && s != "" {
			out = append(out, s)
		}
	}
	return out
}

func disabledResponse(message string) map[string]any {
	return errorResponse("disabled", message, false)
}

func errorResponse(code, message string, retryable bool) map[string]any {
	return map[string]any{
		"ok": false,
		"error": map[string]any{
			"code":      code,
			"message":   message,
			"retryable": retryable,
		},
	}
}

// offlineCacheErrorResponse classifies a Service error into an RPC error
// body. ErrUnsupportedMediaClass is the one case a mobile-app client should
// treat as non-retryable (the artwork will never become software-based);
// everything else (disk/store/network) is surfaced as retryable.
func offlineCacheErrorResponse(err error) map[string]any {
	if errors.Is(err, offlinecache.ErrServiceNotStarted) {
		// Same "disabled" shape h.offlineCache == nil already returns
		// above: a client cannot resolve this by retrying the command,
		// only by the daemon restarting (or the underlying startup
		// failure — e.g. an unreadable store root — being fixed).
		return disabledResponse(err.Error())
	}
	if errors.Is(err, offlinecache.ErrUnsupportedMediaClass) {
		return errorResponse("unsupported_media", err.Error(), false)
	}
	if errors.Is(err, offlinecache.ErrItemNotFound) || errors.Is(err, offlinecache.ErrPlaylistNotFound) {
		return errorResponse("not_found", err.Error(), false)
	}
	return errorResponse("offline_cache_error", err.Error(), true)
}
