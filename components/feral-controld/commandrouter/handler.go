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
	scheduler    playlistschedule.Scheduler
	logger       *zap.Logger
}

func New(
	executor devicectl.Executor,
	cdp cdp.CDP,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	mintPairing mintpairing.Service,
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
		if commandType == commands.CMD_DISPLAY_PLAYLIST {
			status.RecordPlaybackAttempt()
			defer func() {
				if err != nil {
					status.RecordPlaybackFailure()
					return
				}
				h.logger.Info("result from CDP", zap.Any("result", result))
				if !playerresponse.OK(result) {
					h.logger.Warn("Playback verification failed: player did not respond with ok")
					status.RecordPlaybackFailure()
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
					return nil, err
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
				playlist = h.scheduler.PrepareWithSource(playlist, schedulerSource)
				if playlist == nil {
					err = fmt.Errorf("playlist has invalid displayAt")
					h.scheduler.Restore(schedulerSnapshot)
					return
				}
				if len(playlist.Items) == 0 {
					// The scheduler retained and armed the future schedule; the
					// player rejects an empty displayPlaylist, so leave its current
					// artwork in place until a cohort becomes eligible.
					h.scheduler.Commit()
					// A relayer RPC and hub request both need an explicit acceptance
					// response even though no CDP write was valid. This also prevents
					// playback metrics from treating the deferred schedule as a failure.
					result = map[string]interface{}{
						"message": map[string]interface{}{"ok": true, "deferred": true},
					}
					return
				}
				command.Arguments["dp1_call"] = playlist
				result, err = h.sendCDPRequest(command)
				if err != nil || !playerresponse.OK(result) {
					h.scheduler.Restore(schedulerSnapshot)
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
				command.Arguments["dp1_call"] = playlist
			}
			result, err = h.sendCDPRequest(command)
		}
		if err != nil {
			// No restore-on-error here: every CMD_DISPLAY_PLAYLIST failure path
			// above already calls scheduler.Restore before returning from the
			// WithPlayerPush closure, so there is nothing left to undo by the
			// time control reaches this point. That is also load-bearing, not
			// just a redundancy removal — a restore here would run outside
			// pushMu, violating the scheduler's documented pushMu-before-mu lock
			// ordering (Restore must run under the same push lock as the failed
			// send it is undoing).
			//
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
