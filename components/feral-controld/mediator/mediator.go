package mediator

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/feral-file/godbus"
	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/command"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

//go:generate mockgen -source=mediator.go -destination=../mocks/mediator.go -package=mocks -mock_names=Mediator=MockMediator

type Mediator interface {
	Start()
	Stop()
	SetStatusPoller(statusPoller status.Poller)
}

type mediator struct {
	relayer      relayer.Relayer
	dbus         dbus.DBus
	cdp          cdp.CDP
	cmd          command.CommandHandler
	statusPoller status.Poller
	clock        wrapper.Clock
	logger       *zap.Logger
	tracer       *logger.RelayerMessageTracer
	json         wrapper.JSON
	refresher    refresher.Refresher
}

func New(
	relayer relayer.Relayer,
	dbus dbus.DBus,
	cdp cdp.CDP,
	cmd command.CommandHandler,
	clock wrapper.Clock,
	json wrapper.JSON,
	refresher refresher.Refresher,
	l *zap.Logger,
) Mediator {
	return &mediator{
		relayer:   relayer,
		dbus:      dbus,
		cdp:       cdp,
		cmd:       cmd,
		clock:     clock,
		logger:    l,
		tracer:    logger.NewRelayerMessageTracer(l),
		json:      json,
		refresher: refresher,
	}
}

func (m *mediator) Start() {
	m.dbus.OnBusSignal(m.handleDBusSignal)
	m.relayer.OnRelayerMessage(m.handleRelayerMessage)
}

func (m *mediator) Stop() {
	m.relayer.RemoveRelayerMessage(m.handleRelayerMessage)
	m.dbus.RemoveBusSignal(m.handleDBusSignal)
}

func (m *mediator) handleDBusSignal(
	ctx context.Context,
	payload godbus.DBusPayload) ([]interface{}, error) {
	if payload.Member.IsACK() {
		return nil, nil
	}

	if payload.Member != dbus.MONITORD_EVENT_SYSMETRICS {
		m.logger.Info("handle received DBus signal", zap.String("name", payload.Name()), zap.String("path", payload.Path.String()))
	}

	switch payload.Member {
	case dbus.MONITORD_EVENT_SYSMETRICS:
		if len(payload.Body) != 1 {
			m.logger.Error("Invalid number of arguments", zap.Int("expected", 1), zap.Int("actual", len(payload.Body)))
			return nil, fmt.Errorf("invalid number of arguments")
		}

		body, ok := payload.Body[0].([]byte)
		if !ok {
			m.logger.Error("Invalid body type", zap.String("expected", "[]byte"), zap.String("actual", reflect.TypeOf(payload.Body[0]).String()))
			return nil, fmt.Errorf("invalid body type")
		}

		m.logger.Debug("Received sysmetrics", zap.String("metrics", string(body)))
		m.cmd.SaveLastSysMetrics(body)

	case dbus.MONITORD_EVENT_CONNECTIVITY_CHANGE:
		if len(payload.Body) != 1 {
			m.logger.Error("Invalid number of arguments", zap.Int("expected", 1), zap.Int("actual", len(payload.Body)))
			return nil, fmt.Errorf("invalid number of arguments")
		}

		connected, ok := payload.Body[0].(bool)
		if !ok {
			m.logger.Error("Invalid body type", zap.String("expected", "bool"), zap.String("actual", reflect.TypeOf(payload.Body[0]).String()))
			return nil, fmt.Errorf("invalid body type")
		}

		m.logger.Info("Received connectivity change event", zap.Bool("connected", connected), zap.Bool("relayer_connected", m.relayer.IsConnected()))

		// Send the connectivity change to web app
		_, err := m.cdp.Send(
			cdp.METHOD_EVALUATE,
			map[string]interface{}{
				"expression": fmt.Sprintf("window.handleConnectivityChange(%t)", connected),
			})
		if err != nil {
			m.logger.Error("Failed to send CDP request", zap.Error(err))
		}

		// Reconnect the relayer if it's not already connected
		if connected && !m.relayer.IsConnected() {
			err := m.relayer.RetryableConnect(ctx)
			if err != nil {
				m.logger.Error("Failed to reconnect to relayer", zap.Error(err))
			}
		}

	default:
		m.logger.Warn("Unknown signal", zap.String("member", payload.Member.String()))
	}

	return nil, nil
}

func (m *mediator) handleRelayerMessage(ctx context.Context, payload relayer.Payload) error {
	m.logger.Info("handle received relayer message", zap.Any("payload", payload))

	// Start Sentry transaction for this relayer message
	transaction, tracedCtx := m.tracer.StartTransaction(ctx, payload)
	var finalErr error
	defer func() {
		// Always finish the transaction at the end
		m.tracer.FinishTransactionWithError(transaction, finalErr)
	}()

	// Create parsing span
	parseSpan := m.tracer.StartParsingSpan(tracedCtx)
	var parseErr error

	switch payload.MessageID {
	case relayer.MESSAGE_ID_SYSTEM:
		topicID := payload.Message.TopicID
		if topicID == nil {
			parseErr = fmt.Errorf("payload doesn't contain topicID")
			m.logger.Error("Payload doesn't contain topicID", zap.Any("payload", payload))
			m.tracer.FinishSpanWithError(parseSpan, parseErr)
			finalErr = parseErr
			return parseErr
		}

		// Parsing successful
		m.tracer.FinishSpanWithError(parseSpan, nil)

		// Create system handling span
		systemSpan := m.tracer.StartSpan(tracedCtx, "relayer.system")
		systemSpan.Description = "handle_system_message"
		systemSpan.SetData("stage", "system_handling")
		systemSpan.SetData("topic_id", *topicID)

		// Save state
		s := state.GetState()
		s.Relayer.TopicID = *topicID
		err := s.Save()
		if err != nil {
			m.logger.Error("Failed to persist state", zap.Error(err))
			m.tracer.FinishSpanWithError(systemSpan, err)
			finalErr = err
			return err
		}

		// Update global Sentry scope with new topic ID
		m.tracer.SetTopicIDGlobally(*topicID)

		m.tracer.FinishSpanWithError(systemSpan, nil)

	default:
		cmd := payload.Message.Command
		if cmd == nil {
			parseErr = fmt.Errorf("received relayer message with no command")
			m.logger.Warn("Received relayer message with no command", zap.Any("payload", payload))
			m.tracer.FinishSpanWithError(parseSpan, parseErr)
			// Not setting finalErr since this is not really an error, just no command
			return nil
		}

		// Parsing successful
		m.tracer.FinishSpanWithError(parseSpan, nil)

		if cmd.ControldCmds() {
			// Handle command directly
			execSpan := m.tracer.StartCommandExecutionSpan(tracedCtx, *cmd)

			result, err := m.cmd.Execute(tracedCtx,
				command.Command{
					Command:   *cmd,
					Arguments: payload.Message.Args,
				})
			if err != nil {
				m.logger.Error("Failed to execute command", zap.Error(err))
				m.tracer.FinishSpanWithError(execSpan, err)
				finalErr = err
				return err
			}

			m.tracer.FinishSpanWithError(execSpan, nil)

			// Send response
			responseSpan := m.tracer.StartResponseSpan(tracedCtx)
			responseSpan.SetData("response_type", "RPC")

			err = m.relayer.Send(tracedCtx,
				map[string]interface{}{
					"type":      "RPC",
					"messageID": payload.MessageID,
					"message":   result,
				})

			m.tracer.FinishSpanWithError(responseSpan, err)
			finalErr = err
			return err

		} else {
			if cmd.CastPlaylistCmd() {
				playlistURLRaw, hasPlaylistURL := payload.Message.Args["playlistUrl"]
				if hasPlaylistURL {
					if urlStr, ok := playlistURLRaw.(string); ok && urlStr != "" {
						m.logger.Info("CastPlaylist: starting interval to fetch playlist by URL")

						// Start periodic fetch by URL
						m.refresher.StartWithURL(tracedCtx, urlStr)

						// Fetch immediately for current request
						dp1Playlist, err := m.refresher.FetchPlaylistByURL(tracedCtx, urlStr)
						if err != nil {
							m.logger.Error("CastPlaylist: fetch playlist by URL failed", zap.Error(err))
							m.tracer.FinishSpanWithError(parseSpan, err)
							finalErr = err
							return err
						}

						// Process dynamicQuery inside fetched playlist (optional)
						if dp1Playlist.DynamicQueries != nil {
							// Convert single dynamicQuery to array format for consistency
							dp1Items, err := m.refresher.BuildPlaylistItems(tracedCtx, dp1Playlist, dp1Playlist.DynamicQueries)
							if err != nil {
								m.logger.Error("CastPlaylist: dynamic query failed", zap.Error(err))
								m.tracer.FinishSpanWithError(parseSpan, err)
								finalErr = err
								return err
							}

							// Update playlist items with dynamic query results
							dp1Playlist.Items = dp1Items
						}

						// validate items
						if err := m.ensurePlaylistHasItems(dp1Playlist, parseSpan, &finalErr); err != nil {
							return err
						}

						payload.Message.Args["dp1_call"] = dp1Playlist
					} else {
						m.logger.Error("CastPlaylist: playlistUrl is not a string or empty")
					}
				} else {
					// No playlistUrl, check provided playlist
					playlistRaw, ok := payload.Message.Args["dp1_call"]
					if !ok {
						parseErr = fmt.Errorf("payload doesn't contain playlist")
						m.logger.Error("CastPlaylist: missing playlist in args")
						m.tracer.FinishSpanWithError(parseSpan, parseErr)
						finalErr = parseErr
						return parseErr
					}

					// Convert map[string]interface{} to DP1Playlist struct
					playlistBytes, err := json.Marshal(playlistRaw)
					if err != nil {
						parseErr = fmt.Errorf("failed to marshal playlist: %w", err)
						m.logger.Error("CastPlaylist: failed to marshal playlist", zap.Error(parseErr))
						m.tracer.FinishSpanWithError(parseSpan, parseErr)
						finalErr = parseErr
						return parseErr
					}

					var playlist refresher.DP1Playlist
					err = json.Unmarshal(playlistBytes, &playlist)
					if err != nil {
						parseErr = fmt.Errorf("failed to unmarshal playlist: %w", err)
						m.logger.Error("CastPlaylist: failed to unmarshal playlist", zap.Error(parseErr))
						m.tracer.FinishSpanWithError(parseSpan, parseErr)
						finalErr = parseErr
						return parseErr
					}

					// Process dynamicQuery inside playlist (optional)
					if playlist.DynamicQueries != nil {
						m.logger.Info("CastPlaylist: starting interval for dynamic query")
						m.refresher.StartWithDynamicQueries(tracedCtx, playlist.DynamicQueries)

						dp1Items, err := m.refresher.BuildPlaylistItems(tracedCtx, &playlist, playlist.DynamicQueries)
						if err != nil {
							m.logger.Error("CastPlaylist: dynamic query failed", zap.Error(err))
							m.tracer.FinishSpanWithError(parseSpan, err)
							finalErr = err
							return err
						}

						playlist.Items = dp1Items
					}

					// Validate items
					if err := m.ensurePlaylistHasItems(&playlist, parseSpan, &finalErr); err != nil {
						return err
					}

					payload.Message.Args["dp1_call"] = playlist
				}
			}

			m.logger.Info("CastPlaylist: playlist", zap.Any("playlist", payload.Message.Args["dp1_call"]))

			// Forward to CDP
			cdpSpan := m.tracer.StartCDPRequestSpan(tracedCtx)

			p, err := payload.JSON()
			if err != nil {
				m.logger.Error("Failed to marshal payload", zap.Error(err))
				m.tracer.FinishSpanWithError(cdpSpan, err)
				finalErr = err
				return err
			}

			result, err := m.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
				"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(p)),
			})
			if err != nil {
				m.logger.Error("Failed to send CDP request", zap.Error(err))
				m.tracer.FinishSpanWithError(cdpSpan, err)
				finalErr = err
				return err
			}

			m.tracer.FinishSpanWithError(cdpSpan, nil)

			// Add brief pause as in original code
			m.clock.Sleep(500 * time.Millisecond)

			// Force refresh status poller
			if m.statusPoller != nil {
				m.statusPoller.ForceRefresh()
			}

			// Send response
			responseSpan := m.tracer.StartResponseSpan(tracedCtx)
			responseSpan.SetData("response_type", "CDP_RESULT")

			err = m.relayer.Send(tracedCtx, result)

			m.tracer.FinishSpanWithError(responseSpan, err)
			finalErr = err
			return err
		}
	}

	return nil
}

func (m *mediator) ensurePlaylistHasItems(playlist *refresher.DP1Playlist, parseSpan *sentry.Span, finalErr *error) error {
	if len(playlist.Items) > 0 {
		return nil
	}

	err := fmt.Errorf("empty playlist")
	m.logger.Error("CastPlaylist: playlist has no items", zap.Error(err))
	m.tracer.FinishSpanWithError(parseSpan, err)
	*finalErr = err
	return err
}

// SetStatusPoller sets the StatusPoller reference after initialization
func (m *mediator) SetStatusPoller(statusPoller status.Poller) {
	m.statusPoller = statusPoller
}
