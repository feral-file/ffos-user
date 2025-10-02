package command

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/operation"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

//go:generate mockgen -source=handler.go -destination=../mocks/command.go -package=mocks -mock_names=Handler=MockCommandHandler
type Handler interface {
	Process(ctx context.Context, payload relayer.Payload) (interface{}, error)
	SetStatusPoller(statusPoller status.Poller)
}

type handler struct {
	executor     operation.Executor
	cdp          cdp.CDP
	dp1          dp1.DP1
	json         wrapper.JSON
	statusPoller status.Poller
	logger       *zap.Logger
}

func New(
	executor operation.Executor,
	cdp cdp.CDP,
	dp1 dp1.DP1,
	json wrapper.JSON,
	logger *zap.Logger,
) Handler {
	return &handler{
		executor: executor,
		cdp:      cdp,
		dp1:      dp1,
		json:     json,
		logger:   logger,
	}
}

func (h *handler) SetStatusPoller(statusPoller status.Poller) {
	h.statusPoller = statusPoller
}

// Process processes the command and returns the result
func (h *handler) Process(ctx context.Context, payload relayer.Payload) (interface{}, error) {
	cmd := payload.Message.Command
	if cmd == nil {
		h.logger.Warn("Received relayer message with no command", zap.Any("payload", payload))
		return nil, nil
	}

	if cmd.ControldCmds() {
		// Handle command directly
		result, err := h.executor.Execute(ctx,
			operation.Command{
				Command:   *cmd,
				Arguments: payload.Message.Args,
			})
		if err != nil {
			h.logger.Error("Failed to execute command", zap.Error(err))
			return nil, err
		}

		return map[string]interface{}{
			"type":      "RPC",
			"messageID": payload.MessageID,
			"message":   result,
		}, nil

	} else {
		if *cmd == relayer.CMD_DISPLAY_PLAYLIST {
			var playlist *dp1.Playlist
			var err error
			switch {
			case payload.Message.Args["playlistUrl"] != nil:
				url, ok := payload.Message.Args["playlistUrl"].(string)
				if !ok || url == "" {
					return nil, fmt.Errorf("playlistUrl is not a string or empty")
				}

				playlist, err = h.dp1.ProcessPlaylistURL(ctx, url, true)
				if err != nil {
					return nil, err
				}

			case payload.Message.Args["dp1_call"] != nil:
				playlistMap, ok := payload.Message.Args["dp1_call"].(map[string]interface{})
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
					playlist, err = h.dp1.ProcessDynamicPlaylist(ctx, *playlist, true)
					if err != nil {
						h.logger.Error("Failed to process dynamic playlist", zap.Error(err))
						return nil, err
					}
				}

			default:
				return nil, fmt.Errorf("unknown payload type")
			}

			payload.Message.Args["dp1_call"] = playlist

		}

		// Forward to CDP (final, full data)
		result, err := h.sendCDPRequest(payload)
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

// sendCDPRequest marshals payload and sends to CDP with tracing
func (h *handler) sendCDPRequest(payload relayer.Payload) (interface{}, error) {
	p, err := payload.JSON()
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
