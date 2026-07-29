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
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
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
	scheduler   playlistschedule.Scheduler
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
	scheduler playlistschedule.Scheduler,
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
		scheduler:    scheduler,
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
		var schedulerSnapshot playlistschedule.Snapshot
		var schedulerSource playlistschedule.Source
		schedulerMutated := false
		schedulerRestored := false
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
				if !playerresponse.OK(result) {
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

				playlist, err = h.dp1.ProcessPlaylistURLForCast(ctx, url)
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
				schedulerSource = playlistschedule.Source{PlaylistURL: url}

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
					schedulerSource = playlistschedule.Source{DynamicPlaylist: playlist}
					playlist, err = h.dp1.ProcessDynamicPlaylistForCast(ctx, *playlist)
					if err != nil {
						h.logger.Error("Failed to process dynamic playlist", zap.Error(err))
						return nil, err
					}
				}

			default:
				return nil, fmt.Errorf("unknown payload type")
			}

			// Player CanvasService rejects displayPlaylist without a known
			// intent.action ("Unknown DP1 action: undefined" → ok:false).
			// Controller casts are force-display, same contract as
			// playlistschedule push (now_display, never soft refresh).
			ensureDisplayPlaylistIntent(command.Arguments)

			// Acquire the playback coordinator here and hold it (via the
			// deferred unlock above) across both the replay scope sync
			// (syncReplayScope below) and the CDP send, so scope-sync +
			// navigation cannot interleave with another display/refresh's
			// own sync+send. Deliberately acquired AFTER the (possibly
			// slow, network-bound) DP-1 resolution above, which does not
			// touch scope.
			if h.kioskReplay != nil && playlist != nil {
				h.kioskReplay.LockPlayback()
				heldPlaybackLock = true
			}
		}

		// syncReplayScope points replay's Fetch-interception scope at p's
		// currently-cached items. feral-player advances through a
		// multi-item playlist client-side without telling controld which
		// item is on screen at any instant, so scope covers every cached
		// item in the playlist rather than a single "current" one (see
		// Replayer.EnableForPlaylist's doc). Best-effort: a sync failure
		// must never block the actual display command, since offline
		// replay is a strict enhancement over the live path.
		//
		// Callers invoke this ONLY once a CDP send for p is actually
		// going to happen, immediately before it — never for a cast the
		// scheduler defers or rejects. Scope must keep tracking what is
		// genuinely on screen: a deferred (future-only) cast leaves the
		// previous playlist displaying, and switching interception to the
		// new playlist anyway would — under a fail_closed scope — start
		// blocking the on-screen playlist's own requests even with live
		// network, until a later corrective pass noticed
		// (feral-file/ffos-user#229 review finding).
		syncReplayScope := func(p *dp1.Playlist) {
			if h.kioskReplay == nil || p == nil {
				return
			}
			itemIDs := make([]string, 0, len(p.Items))
			for _, item := range p.Items {
				itemIDs = append(itemIDs, item.ID)
			}
			if syncErr := h.kioskReplay.SyncPlaylist(ctx, itemIDs); syncErr != nil {
				h.logger.Warn("offline cache: failed to sync kiosk replay scope for playlist", zap.Error(syncErr))
			}
			// Announce this authoritative scope change (under the
			// lock) so a concurrent corrective resync that sampled
			// the generation earlier will defer to it instead of
			// clobbering it with a stale playlist's scope — see
			// KioskReplay.PlaybackGeneration's doc. Bumped even if the
			// sync above logged an error: this path is authoritative
			// for what SHOULD be on screen, and the resync must not
			// override that intent with an older snapshot.
			h.kioskReplay.MarkPlaybackChanged()
		}

		if commandType == commands.CMD_REFRESH_ARTWORK {
			_, err = h.cdp.Send("Network.clearBrowserCache", map[string]interface{}{})
			if err != nil {
				h.logger.Warn("Failed to clear Chromium browser cache before artwork refresh", zap.Error(err))
			}
		}

		// Forward to CDP. displayPlaylist and displayDefaultPlaylist share the
		// scheduler push lock with RecomputeNow so a stale timed push cannot land
		// after a newer cast or OOM-recovery fallback.
		// displayDefaultPlaylist is player-owned fallback today; it may no-op
		// successfully, so this path must not clear scheduler authority until
		// controld can prove that default playback replaced the current playlist.
		switch {
		case commandType == commands.CMD_DISPLAY_PLAYLIST && h.scheduler != nil:
			h.scheduler.WithPlayerPush(func() {
				schedulerSnapshot = h.scheduler.Snapshot()
				// Filter displayAt playlists to the active set before the player
				// sees them. The scheduler keeps the full list for timer/wake updates.
				fullPlaylist := playlist
				playlist = h.scheduler.PrepareWithSource(playlist, schedulerSource)
				schedulerMutated = true
				if playlist == nil {
					err = fmt.Errorf("playlist has invalid displayAt")
					h.scheduler.Restore(schedulerSnapshot)
					schedulerRestored = true
					return
				}
				if len(playlist.Items) == 0 {
					// The scheduler retained and armed the future schedule; the
					// player rejects an empty displayPlaylist, so leave its current
					// artwork in place until a cohort becomes eligible. Replay
					// scope is deliberately NOT synced on this path (see
					// syncReplayScope's caller contract): nothing new reaches
					// the screen, so interception must stay pointed at the
					// playlist that keeps displaying.
					h.scheduler.Commit()
					// A relayer RPC and hub request both need an explicit acceptance
					// response even though no CDP write was valid. This also prevents
					// playback metrics from treating the deferred schedule as a failure.
					result = map[string]interface{}{
						"message": map[string]interface{}{"ok": true, "deferred": true},
					}
					return
				}
				// Scope the FULL playlist's items, not just the filtered
				// active cohort: the scheduler's own timer/wake/retry
				// cutovers push later cohorts of this same playlist
				// directly (playlistschedule's push), with no replay-scope
				// hook of their own, so the scope installed here must
				// already cover every cohort a cutover can display. The
				// cost is only precision, in the safe direction: uncached
				// future items keep the scope "mixed", whose miss policy
				// is pass-through rather than fail_closed (see
				// Replayer.EnableForPlaylist), and the playlist-refresher's
				// periodic pass re-syncs as downloads complete.
				syncReplayScope(fullPlaylist)
				command.Arguments["dp1_call"] = playlist
				result, err = h.sendCDPRequest(command)
				if err != nil || !playerresponse.OK(result) {
					h.scheduler.Restore(schedulerSnapshot)
					schedulerRestored = true
				} else {
					h.scheduler.Commit()
				}
			})
		case commandType == commands.CMD_DISPLAY_DEFAULT_PLAYLIST && h.scheduler != nil:
			h.scheduler.WithPlayerPush(func() {
				result, err = h.sendCDPRequest(command)
			})
		default:
			if commandType == commands.CMD_DISPLAY_PLAYLIST {
				// No scheduler configured: every cast reaches the player
				// unfiltered, so scope-sync immediately precedes the send.
				syncReplayScope(playlist)
				command.Arguments["dp1_call"] = playlist
			}
			result, err = h.sendCDPRequest(command)
		}
		if err != nil {
			if schedulerMutated && !schedulerRestored && h.scheduler != nil {
				h.scheduler.Restore(schedulerSnapshot)
			}
			// refreshArtwork's evaluate needs a live player page
			// (window.handleCDPRequest), but a refresh is most needed exactly
			// when the page is broken — e.g. Chromium serving stale cached
			// chunks after a player bundle swap (#234), where the app never
			// boots. The cache was already cleared above; a browser-level
			// Page.reload needs no page JS and completes the recovery. Only
			// when the reload itself fails is the command truly dead.
			if commandType != commands.CMD_REFRESH_ARTWORK {
				return nil, err
			}
			if _, reloadErr := h.cdp.Send("Page.reload", map[string]interface{}{"ignoreCache": true}); reloadErr != nil {
				return nil, err
			}
			h.logger.Warn("refreshArtwork: player page unresponsive; recovered with cache clear + Page.reload", zap.Error(err))
			err = nil
			result = map[string]interface{}{
				"message": map[string]interface{}{"ok": true, "recovered": "reload"},
			}
		}

		// Force refresh status poller
		if h.statusPoller != nil {
			h.statusPoller.ForceRefresh()
		}

		return result, nil
	}
}

// ensureDisplayPlaylistIntent sets intent.action=now_display when the cast
// request has no action yet. Controllers historically send only playlistUrl /
// dp1_call; the player still requires a known DP1 action. Soft refresh keeps
// its own path (playlist-refresher sets refresh:true and does not use this
// helper). An explicit controller intent is preserved.
func ensureDisplayPlaylistIntent(args map[string]interface{}) {
	if args == nil {
		return
	}
	if intent, ok := args["intent"].(map[string]interface{}); ok {
		if action, _ := intent["action"].(string); action != "" {
			return
		}
		intent["action"] = "now_display"
		return
	}
	args["intent"] = map[string]interface{}{
		"action": "now_display",
	}
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
