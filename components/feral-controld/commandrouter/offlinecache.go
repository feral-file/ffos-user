package commandrouter

import (
	"context"
	"errors"
	"fmt"
	"math"

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

	// Only a playlistUrl-resolved download has a URL to index for the
	// offline fallback; an inline dp1_call has none (same as
	// DownloadPlaylist). When there IS one, sample the playlist's
	// clear-generation BEFORE the download so a ClearPlaylist racing this
	// whole download+index sequence is honored (the index write is
	// skipped) rather than silently resurrecting the cleared playlist
	// fallback record — see Service.CurrentPlaylistClearGeneration's doc.
	// Sampled here (just after resolve, before DownloadItem) for the same
	// reason DownloadPlaylist samples right after it has the playlist
	// body: it is the earliest point the playlist ID is known.
	sourceURL, indexByURL := stringArg(args["playlistUrl"])
	var clearGen uint64
	if indexByURL {
		clearGen = h.offlineCache.CurrentPlaylistClearGeneration(playlist.ID)
	}

	if err := h.offlineCache.DownloadItem(ctx, item); err != nil {
		return offlineCacheErrorResponse(err), nil
	}

	// Best-effort, and deliberately after the queue above already
	// succeeded: index the resolved playlist by its playlistUrl for the
	// SAME displayPlaylist-by-URL offline fallback DownloadPlaylist's
	// sourceURL already provides — see
	// Service.IndexPlaylistForOfflineDisplay's doc for why a single-item
	// download would otherwise leave that URL with no offline fallback at
	// all, even though this one item is now genuinely cached. A failure
	// here must never turn an already-successful item queue into an error
	// response, since the item itself is queued/cached either way.
	if indexByURL {
		h.indexResolvedPlaylistForOfflineDisplay(playlist, sourceURL, clearGen)
	}

	return map[string]any{"ok": true, "status": "queued", "itemId": itemID}, nil
}

func (h *handler) indexResolvedPlaylistForOfflineDisplay(playlist *dp1.Playlist, sourceURL string, clearGen uint64) {
	raw, err := h.json.Marshal(playlist)
	if err != nil {
		h.logger.Warn("offline cache: failed to marshal resolved playlist for URL indexing",
			zap.String("playlist_url", sourceURL), zap.Error(err))
		return
	}
	if err := h.offlineCache.IndexPlaylistForOfflineDisplay(raw, sourceURL, clearGen); err != nil {
		h.logger.Warn("offline cache: failed to index playlist for offline display fallback",
			zap.String("playlist_url", sourceURL), zap.Error(err))
	}
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
	return map[string]any{"ok": true, "status": "queued", "total": total, "queuedCount": queued}, nil
}

func (h *handler) handleClearPlaylistItemCache(ctx context.Context, args map[string]any) (interface{}, error) {
	itemID, ok := stringArg(args["itemId"])
	if !ok {
		return errorResponse("invalid_request", "itemId is required", false), nil
	}
	if err := h.offlineCache.ClearItem(itemID); err != nil {
		return offlineCacheErrorResponse(err), nil
	}
	h.resyncKioskReplayScopeToCurrentDisplay(ctx)
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
	h.resyncKioskReplayScopeToCurrentDisplay(ctx)
	return map[string]any{"ok": true, "playlistId": playlistID}, nil
}

// resyncKioskReplayScopeToCurrentDisplay re-syncs replay's live Fetch-
// interception scope with whatever the player actually reports itself as
// currently displaying (via a live FetchPlayerStatus call, not any
// locally-held state). It has two call sites, each reacting to a
// different way replay's scope can drift from reality:
//
//   - handleClearPlaylistItemCache/handleClearPlaylistCache, immediately
//     after a successful clear. Without this, ClearItem/ClearPlaylist only
//     touch the store (see their docs) — if the item just cleared is the
//     one currently displayed, replayer's in-memory resources map keeps
//     stale entries pointing at now-deleted blobs until the next
//     displayPlaylist command or playlist-refresher's periodic pass (up to
//     refresher.PLAYLIST_REFRESH_INTERVAL later), during which every
//     request for it is a miss instead of either serving correctly or
//     falling back to the network cleanly.
//   - Process's CMD_DISPLAY_PLAYLIST branch, when the display command
//     itself fails (CDP send error) or is rejected (player replies
//     ok:false). That branch calls SyncPlaylist for the NEW playlist
//     before asking CDP to display it (see that call site's own doc for
//     why), so a failure there leaves replay's scope pointed at a
//     playlist the player never actually switched to while the kiosk is
//     still genuinely showing the old one. Re-querying and re-syncing to
//     whatever the player reports right now is what reverts that
//     mistakenly-applied new scope back to the truth.
//
// Both are best-effort and fire-and-forget: a resync failure here must
// never turn an already-decided outcome (a successful clear, or an
// already-failed/rejected display command) into something reported
// differently to the caller, and a failure here just leaves scope stale
// until the next periodic playlist-refresher pass — the same bounded-
// staleness trade-off already accepted for the clear-time case. Mirrors
// the same FetchPlayerStatus->resolve->SyncPlaylist shape playlist-
// refresher's own processPlayingPlaylist independently implements for its
// own input (polled player status) — commandrouter and playlist-refresher
// intentionally do not depend on each other (see AGENTS.md's
// service-boundary guidance), so this is kept as its own small instance
// of that shape rather than reaching across the package boundary for it.
func (h *handler) resyncKioskReplayScopeToCurrentDisplay(ctx context.Context) {
	if h.kioskReplay == nil || h.statusPoller == nil {
		return
	}
	// Sample the authoritative-display generation BEFORE reading the
	// kiosk's current playlist below: that read (FetchPlayerStatus +
	// resolveDisplayedPlaylist) is network-bound and runs OUTSIDE the
	// playback lock, so a concurrent displayPlaylist can switch the kiosk
	// to a different playlist while it is in flight. After acquiring the
	// lock we re-check this generation and bail if it advanced, so this
	// corrective resync can never install a stale playlist's scope over a
	// newer authoritative one — see KioskReplay.PlaybackGeneration's doc.
	//
	// The sample itself is taken UNDER the playback lock, and that
	// matters as much as the ordering. A displayPlaylist whose sync+send
	// critical section is in flight right now did its store reads
	// (SyncPlaylist -> scopeFor/EnableForPlaylist) before this clear
	// deleted the records and blobs it references, and it publishes its
	// generation bump only inside that same section. Sampling without the
	// lock could read the pre-bump generation, then observe the bump at
	// the re-check below and defer to that scope — a scope built from
	// PRE-clear store reads, leaving replay pointed at just-deleted blobs
	// until the next refresher pass. Waiting for the lock serializes this
	// sample after every install whose reads could predate the clear
	// (every installer holds the lock across its store reads — see
	// LockPlayback's caller contract), so a generation advance observed
	// at the re-check can only come from an install whose store reads
	// happened after the clear; deferring to that one is correct.
	h.kioskReplay.LockPlayback()
	genBeforeResolve := h.kioskReplay.PlaybackGeneration()
	h.kioskReplay.UnlockPlayback()

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
	// Serialize this scope resync against any concurrent displayPlaylist
	// sync+send (see KioskReplay.LockPlayback's doc). Acquired only around
	// the SyncPlaylist call, not the FetchPlayerStatus/resolve above,
	// which is network-bound and does not touch scope. This never nests
	// inside the displayPlaylist send-path lock: the display-failure
	// caller releases that lock (its deferred unlock runs first, LIFO)
	// before this deferred resync runs.
	h.kioskReplay.LockPlayback()
	defer h.kioskReplay.UnlockPlayback()
	// An authoritative displayPlaylist/refresher pass committed a new
	// scope between our pre-resolve sample and here: its scope is fresher
	// and matches what is actually on screen now, so applying our stale
	// snapshot would be the exact "install A's scope over B" regression
	// this guard exists to prevent (see KioskReplay.PlaybackGeneration).
	if h.kioskReplay.PlaybackGeneration() != genBeforeResolve {
		return
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
//
// The PlaylistURL branch falls back to loadCachedPlaylistForURL on live
// DP-1 failure, the same way Process's CMD_DISPLAY_PLAYLIST branch does
// (handler.go) — a device that is offline and already displaying a
// playlist via that same fallback would otherwise never resync replay
// scope after a clear, since resolving the currently-displayed playlist
// here would fail every time with no cache alternative.
func (h *handler) resolveDisplayedPlaylist(ctx context.Context, playerStatus *status.PlayerStatus) (*dp1.Playlist, error) {
	switch {
	case playerStatus.PlaylistURL != nil:
		url := *playerStatus.PlaylistURL
		playlist, err := h.dp1.ProcessPlaylistURL(ctx, url, false)
		if err != nil {
			cachedPlaylist, cacheErr := h.loadCachedPlaylistForURL(url)
			if cacheErr != nil {
				return nil, err
			}
			return cachedPlaylist, nil
		}
		return playlist, nil
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
	req, errResponse := parseStatusRequest(args)
	if errResponse != nil {
		return errResponse, nil
	}

	snapshot, err := h.offlineCache.Status(req)
	if err != nil {
		return offlineCacheErrorResponse(err), nil
	}

	// Built field by field rather than by marshaling the snapshot so the
	// optional parts stay absent instead of serializing as null: totals/
	// diskUsed are first-page-only (see StatusSnapshot's doc), and a
	// client on the last page must not see an empty nextCursor it might
	// follow.
	response := map[string]any{
		"ok":    true,
		"items": snapshot.Items,
	}
	if snapshot.Totals != nil {
		response["totals"] = snapshot.Totals
	}
	if snapshot.DiskUsedBytes != nil {
		response["diskUsed"] = *snapshot.DiskUsedBytes
	}
	if snapshot.NextCursor != "" {
		response["nextCursor"] = snapshot.NextCursor
		response["truncated"] = snapshot.Truncated
	}
	return response, nil
}

// parseStatusRequest validates the getOfflineCacheStatus arguments,
// returning either the parsed request or a ready-to-send invalid_request
// body (never both).
//
// This one command is validated strictly, unlike the lenient arg helpers
// the older commands share, because every one of its inputs decides how
// much work the daemon does and how large the response gets: a
// misspelled itemIds (say a bare string instead of an array) used to
// fall through to "report on EVERY item", turning a client-side typo
// into a full-store scan on a device that also has a kiosk browser to
// keep alive. Failing closed with a non-retryable error is the cheaper
// outcome for both sides.
func parseStatusRequest(args map[string]any) (offlinecache.StatusRequest, map[string]any) {
	var req offlinecache.StatusRequest

	if raw, present := args["itemIds"]; present && raw != nil {
		ids, ok := stringSliceArg(raw)
		if !ok {
			return req, errorResponse("invalid_request", "itemIds must be an array of non-empty strings", false)
		}
		if len(ids) > offlinecache.MaxStatusItemIDs {
			return req, errorResponse("invalid_request",
				fmt.Sprintf("itemIds accepts at most %d ids per request", offlinecache.MaxStatusItemIDs), false)
		}
		req.ItemIDs = ids
	}

	if raw, present := args["limit"]; present && raw != nil {
		limit, ok := intArg(raw)
		if !ok || limit < 0 {
			return req, errorResponse("invalid_request", "limit must be a non-negative integer", false)
		}
		// A limit above the cap is clamped rather than rejected: the
		// response still says what happened (nextCursor/truncated), and
		// a caller asking for "everything" should get the first page,
		// not an error.
		req.Limit = limit
	}

	if raw, present := args["cursor"]; present && raw != nil {
		cursor, ok := raw.(string)
		if !ok || cursor == "" {
			return req, errorResponse("invalid_request", "cursor must be a non-empty string", false)
		}
		req.Cursor = cursor
	}

	if raw, present := args["totalsOnly"]; present && raw != nil {
		totalsOnly, ok := raw.(bool)
		if !ok {
			return req, errorResponse("invalid_request", "totalsOnly must be a boolean", false)
		}
		req.TotalsOnly = totalsOnly
	}

	// totals describe the whole set; a cursor asks for part of it. Asking
	// for both is a client bug, and answering it either way (totals of
	// the whole set on a partial page, or a partial total) would hide
	// that bug behind a plausible-looking number.
	if req.TotalsOnly && req.Cursor != "" {
		return req, errorResponse("invalid_request", "totalsOnly cannot be combined with cursor", false)
	}

	return req, nil
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

// stringSliceArg reports ok=false for anything that is not a JSON array
// of non-empty strings, rather than silently yielding the entries it
// could make sense of — see parseStatusRequest's doc for why this one
// argument is worth failing closed on.
func stringSliceArg(v any) ([]string, bool) {
	raw, ok := v.([]interface{})
	if !ok {
		return nil, false
	}
	out := make([]string, 0, len(raw))
	for _, item := range raw {
		s, ok := item.(string)
		if !ok || s == "" {
			return nil, false
		}
		out = append(out, s)
	}
	return out, true
}

// maxIntArgValue is an overflow guard, not a policy limit: the callers'
// own bounds (MaxStatusItems, MaxStatusItemIDs) are far below it, and
// anything above it could not be converted to int meaningfully anyway.
const maxIntArgValue = 1 << 20

// intArg accepts the shapes an integer field can arrive in: float64
// (what encoding/json produces for every JSON number by default) and the
// native ints a Go caller passes directly. A non-integral or
// out-of-range number is rejected rather than truncated.
func intArg(v any) (int, bool) {
	switch n := v.(type) {
	case float64:
		if n != math.Trunc(n) || n < -maxIntArgValue || n > maxIntArgValue {
			return 0, false
		}
		return int(n), true
	case int:
		return n, true
	case int64:
		if n < -maxIntArgValue || n > maxIntArgValue {
			return 0, false
		}
		return int(n), true
	default:
		return 0, false
	}
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
// treat as non-retryable (live/HLS streaming sources cannot be cached offline);
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
	if errors.Is(err, offlinecache.ErrItemBusy) {
		// Retryable: the clear will succeed once the in-flight capture
		// finishes (a few seconds to captureWindowMs at most) — see
		// ErrItemBusy's doc.
		return errorResponse("busy", err.Error(), true)
	}
	if errors.Is(err, offlinecache.ErrClearedDuringDownload) {
		// Retryable, and the same "busy" shape as the two below: a
		// clear landed mid-request, so nothing was queued, but
		// re-issuing the download once that clear has settled queues
		// normally. Reported as an error rather than the flat
		// status:"queued" this branch used to fall through to — see
		// ErrClearedDuringDownload's doc for why claiming success here
		// stranded the client's progress UI on an item no worker would
		// ever pick up.
		return errorResponse("busy", err.Error(), true)
	}
	if errors.Is(err, offlinecache.ErrQueueFull) {
		// Retryable: the backlog drains as the single capture worker
		// works through it — see ErrQueueFull's doc. Same "busy" shape
		// as ErrItemBusy above (a condition that resolves on its own
		// given time, not one the caller can fix by changing the
		// request), just a different underlying cause.
		return errorResponse("busy", err.Error(), true)
	}
	return errorResponse("offline_cache_error", err.Error(), true)
}
