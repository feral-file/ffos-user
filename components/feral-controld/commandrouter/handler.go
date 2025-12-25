package commandrouter

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/metric"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

//go:generate mockgen -source=handler.go -destination=../mocks/command.go -package=mocks -mock_names=Handler=MockCommandHandler
type Handler interface {
	Process(ctx context.Context, command commands.Command) (interface{}, error)
}

type handler struct {
	executor      devicectl.Executor
	cdp           cdp.CDP
	dp1           dp1.DP1
	json          wrapper.JSON
	statusPoller  status.Poller
	metricTracker metric.Tracker
	logger        *zap.Logger
}

func New(
	executor devicectl.Executor,
	cdp cdp.CDP,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	metricTracker metric.Tracker,
	json wrapper.JSON,
	logger *zap.Logger,
) Handler {
	return &handler{
		executor:      executor,
		cdp:           cdp,
		dp1:           dp1,
		statusPoller:  statusPoller,
		metricTracker: metricTracker,
		json:          json,
		logger:        logger,
	}
}

// Process processes the command and returns the result
func (h *handler) Process(ctx context.Context, command commands.Command) (interface{}, error) {
	commandType := command.Type
	if commandType == "" {
		h.logger.Warn("Received command with no type", zap.Any("command", command))
		return nil, nil
	}

	if commandType.DeviceCtlCommand() {
		// Handle device control command
		result, err := h.executor.Execute(ctx,
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
		var playlistURL string
		if commandType == commands.CMD_DISPLAY_PLAYLIST {
			var err error
			switch {
			case command.Arguments["playlistUrl"] != nil:
				url, ok := command.Arguments["playlistUrl"].(string)
				if !ok || url == "" {
					return nil, fmt.Errorf("playlistUrl is not a string or empty")
				}

				playlistURL = url
				playlist, err = h.dp1.ProcessPlaylistURL(ctx, url, true)
				if err != nil {
					return nil, err
				}

			case command.Arguments["dp1_call"] != nil:
				playlistMap, ok := command.Arguments["dp1_call"].(map[string]interface{})
				if !ok {
					return nil, fmt.Errorf("playlist is not a map")
				}

				playlistBytes, err := h.json.Marshal(playlistMap)
				if err != nil {
					return nil, fmt.Errorf("failed to marshal playlist: %w", err)
				}

				if err := h.json.Unmarshal(playlistBytes, &playlist); err != nil {
					return nil, fmt.Errorf("failed to unmarshal playlist: %w", err)
				}

				if len(playlist.DynamicQueries) > 0 {
					playlist, err = h.dp1.ProcessDynamicPlaylist(ctx, *playlist, true, false)
					if err != nil {
						h.logger.Error("Failed to process dynamic playlist", zap.Error(err))
						return nil, err
					}
				}

			default:
				return nil, fmt.Errorf("unknown payload type")
			}

			command.Arguments["dp1_call"] = playlist

		}

		// Forward to CDP (final, full data)
		result, err := h.sendCDPRequest(command)
		if err != nil {
			return nil, err
		}

		// Track playlist view metric after sending to CDP
		if commandType == commands.CMD_DISPLAY_PLAYLIST && playlist != nil && h.metricTracker != nil {
			if err := h.metricTracker.TrackPlaylistView(ctx, playlist, playlistURL); err != nil {
				h.logger.Error("Failed to track playlist view metric", zap.Error(err))
				// Don't fail the command if metric tracking fails
			}
		}

		// Force refresh status poller
		if h.statusPoller != nil {
			h.statusPoller.ForceRefresh()
		}

		return result, nil
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
