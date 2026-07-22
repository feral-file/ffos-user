package commandrouter

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

//go:generate mockgen -source=handler.go -destination=../mocks/command.go -package=mocks -mock_names=Handler=MockCommandHandler
type Handler interface {
	Process(ctx context.Context, command commands.Command) (interface{}, error)
}

type handler struct {
	executor     devicectl.Executor
	cdp          cdp.CDP
	dp1          dp1.DP1
	json         wrapper.JSON
	statusPoller status.Poller
	mintPairing  mintpairing.Service
	// offlineCache may be nil (feature disabled / not yet wired), mirroring
	// the mintPairing nil-guard pattern above.
	offlineCache offlinecache.Service
	// kioskReplay may be nil for the same reason as offlineCache above.
	// Kept as a separate nilable dependency (rather than folded into
	// offlineCache) because it is specifically about the kiosk's live CDP
	// Fetch-interception scope, not the download/store side of caching.
	kioskReplay offlinecache.KioskReplay
	logger      *zap.Logger
}

func New(
	executor devicectl.Executor,
	cdp cdp.CDP,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	mintPairing mintpairing.Service,
	offlineCache offlinecache.Service,
	kioskReplay offlinecache.KioskReplay,
	json wrapper.JSON,
	logger *zap.Logger,
) Handler {
	return &handler{
		executor:     executor,
		cdp:          cdp,
		dp1:          dp1,
		statusPoller: statusPoller,
		mintPairing:  mintPairing,
		offlineCache: offlineCache,
		kioskReplay:  kioskReplay,
		json:         json,
		logger:       logger,
	}
}

// Process processes the command and returns the result
func (h *handler) Process(ctx context.Context, command commands.Command) (interface{}, error) {
	commandType := command.Type
	if commandType == "" {
		h.logger.Warn("Received command with no type", zap.Any("command", command))
		return nil, nil
	}

	var result interface{}
	var err error

	if commandType == commands.CMD_START_MINT_PAIRING_SESSION {
		if h.mintPairing == nil {
			return map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":      "disabled",
					"message":   "mint pairing is not enabled",
					"retryable": false,
				},
			}, nil
		}
		return h.mintPairing.HandleStartPairingSession(ctx, command.Arguments)
	}

	if commandType == commands.CMD_CLOSE_MINT_PAIRING_SESSION {
		if h.mintPairing == nil {
			return map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":      "disabled",
					"message":   "mint pairing is not enabled",
					"retryable": false,
				},
			}, nil
		}
		return h.mintPairing.HandleClosePairingSession(ctx, command.Arguments)
	}

	if commandType == commands.CMD_MINT_PAIRING_APPROVAL {
		if h.mintPairing == nil {
			return map[string]any{
				"ok": false,
				"error": map[string]any{
					"code":      "not_found",
					"message":   "mint pairing is not enabled",
					"retryable": false,
				},
			}, nil
		}
		return h.mintPairing.HandleApprovalDecision(ctx, command.Arguments)
	}

	if isOfflineCacheCommand(commandType) {
		return h.handleOfflineCacheCommand(ctx, commandType, command.Arguments)
	}

	if commandType.DeviceCtlCommand() {
		// Handle device control command
		result, err = h.executor.Execute(ctx,
			commands.Command{
				Type:      commandType,
				Arguments: command.Arguments,
			})
		if err != nil {
			h.logger.Error("Failed to execute command", zap.Error(err))
			return nil, err
		}

		return result, nil
	} else {
		var playlist *dp1.Playlist
		if commandType == commands.CMD_DISPLAY_PLAYLIST {
			status.RecordPlaybackAttempt()
			defer func() {
				if err != nil {
					status.RecordPlaybackFailure()
					// SyncPlaylist below already switched replay's live
					// Fetch-interception scope to the NEW playlist before
					// this CDP send ran (see that call site's doc for why
					// that ordering is required). Since the send itself
					// failed, the kiosk never actually switched — it is
					// still showing whatever it displayed before — so
					// leaving scope pointed at the new playlist would
					// misclassify the still-on-screen old playlist's own
					// requests as misses. Re-syncing to the player's
					// actual current status reverts that.
					h.resyncKioskReplayScopeToCurrentDisplay(ctx)
					return
				}
				h.logger.Info("result from CDP", zap.Any("result", result))
				if !isPlayerResponseOk(result) {
					h.logger.Warn("Playback verification failed: player did not respond with ok")
					status.RecordPlaybackFailure()
					// Same rationale as the err != nil branch above: the
					// send succeeded at the transport level but the
					// player itself rejected the command, so it is still
					// displaying whatever it had before.
					h.resyncKioskReplayScopeToCurrentDisplay(ctx)
				}
			}()
			// heldPlaybackLock records whether this path acquired the
			// playback coordinator (only when it actually syncs replay
			// scope below). Releasing it is deferred so the lock spans
			// BOTH the scope sync and the CDP send at the end of this
			// branch, making that pair atomic against concurrent
			// displayPlaylist commands and playlist-refresher passes (see
			// KioskReplay.LockPlayback's doc). Registered AFTER the
			// failure/rejection resync defer above so it runs BEFORE it
			// (defers are LIFO): that resync re-acquires this same
			// non-reentrant lock, so this path must have released it
			// first.
			var heldPlaybackLock bool
			defer func() {
				if heldPlaybackLock {
					h.kioskReplay.UnlockPlayback()
				}
			}()
			switch {
			case command.Arguments["playlistUrl"] != nil:
				url, ok := command.Arguments["playlistUrl"].(string)
				if !ok || url == "" {
					return nil, fmt.Errorf("playlistUrl is not a string or empty")
				}

				playlist, err = h.dp1.ProcessPlaylistURL(ctx, url, true)
				if err != nil {
					// Live DP-1 resolution failed — most commonly, the
					// device has no network right now. Fall back to the
					// exact playlist body last saved by downloadPlaylist
					// for this same URL, if any, rather than hard-failing
					// a playlist that is actually fully cached and
					// replayable offline (see docs/offline-artwork-
					// capture.md §6 and Service.CachedPlaylistForURL's
					// doc). This is a "last known good" copy, not a live
					// re-resolution: it will not reflect anything
					// published at url after it was downloaded, and (by
					// construction, since it can only exist if it was
					// downloaded successfully before) was already
					// signature-verified once at that time.
					cachedPlaylist, cacheErr := h.loadCachedPlaylistForURL(url)
					if cacheErr != nil {
						return nil, err
					}
					h.logger.Warn("offline cache: displayPlaylist falling back to cached copy after live DP-1 resolution failure",
						zap.String("playlist_url", url), zap.Error(err))
					playlist = cachedPlaylist
					err = nil
				}

			case command.Arguments["dp1_call"] != nil:
				playlistMap, ok := command.Arguments["dp1_call"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("playlist is not a map")
				}

				var playlistBytes []byte
				playlistBytes, err = h.json.Marshal(playlistMap)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal playlist: %w", err)
				}

				if err = h.json.Unmarshal(playlistBytes, &playlist); err != nil {
					return nil, fmt.Errorf("failed to unmarshal playlist: %w", err)
				}

				if playlist.HasDynamicContent() {
					playlist, err = h.dp1.ProcessDynamicPlaylist(ctx, *playlist, true)
					if err != nil {
						h.logger.Error("Failed to process dynamic playlist", zap.Error(err))
						return nil, err
					}
				}

			default:
				return nil, fmt.Errorf("unknown payload type")
			}

			command.Arguments["dp1_call"] = playlist

			// Sync replay's Fetch-interception scope to this playlist's
			// currently-cached items before forwarding to CDP below.
			// feral-player advances through a multi-item playlist
			// client-side without telling controld which item is on
			// screen at any instant, so scope covers every cached item
			// in the playlist rather than a single "current" one (see
			// Replayer.EnableForPlaylist's doc). Best-effort: a sync
			// failure must never block the actual display command, since
			// offline replay is a strict enhancement over the live path.
			if h.kioskReplay != nil && playlist != nil {
				// Acquire the playback coordinator here and hold it (via
				// the deferred unlock above) across the CDP send below,
				// so scope-sync + navigation cannot interleave with
				// another display/refresh's own sync+send. Deliberately
				// acquired AFTER the (possibly slow, network-bound) DP-1
				// resolution above, which does not touch scope.
				h.kioskReplay.LockPlayback()
				heldPlaybackLock = true
				itemIDs := make([]string, 0, len(playlist.Items))
				for _, item := range playlist.Items {
					itemIDs = append(itemIDs, item.ID)
				}
				if syncErr := h.kioskReplay.SyncPlaylist(ctx, itemIDs); syncErr != nil {
					h.logger.Warn("offline cache: failed to sync kiosk replay scope for playlist", zap.Error(syncErr))
				}
			}
		}

		if commandType == commands.CMD_REFRESH_ARTWORK {
			_, err = h.cdp.Send("Network.clearBrowserCache", map[string]interface{}{})
			if err != nil {
				h.logger.Warn("Failed to clear Chromium browser cache before artwork refresh", zap.Error(err))
			}
		}

		// Forward to CDP (final, full data)
		result, err = h.sendCDPRequest(command)
		if err != nil {
			return nil, err
		}

		// Force refresh status poller
		if h.statusPoller != nil {
			h.statusPoller.ForceRefresh()
		}

		return result, nil
	}
}

// isPlayerResponseOk checks whether the CDP result from the player
// contains { "message": { "ok": true } }.
func isPlayerResponseOk(result interface{}) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	msg, ok := m["message"].(map[string]interface{})
	if !ok {
		return false
	}
	okVal, _ := msg["ok"].(bool)
	return okVal
}

// sendCDPRequest marshals payload and sends to CDP
func (h *handler) sendCDPRequest(command commands.Command) (interface{}, error) {
	p, err := command.JSON()
	if err != nil {
		h.logger.Error("Failed to marshal payload", zap.Error(err))
		return nil, err
	}

	result, err := h.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(p)),
	})
	if err != nil {
		h.logger.Error("Failed to send CDP request", zap.Error(err))
		return nil, err
	}

	return result, nil
}
