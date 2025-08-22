package mediator

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"github.com/feral-file/ffos-user/components/feral-connectd/cdp"
	"github.com/feral-file/ffos-user/components/feral-connectd/command"
	"github.com/feral-file/ffos-user/components/feral-connectd/dbus"
	"github.com/feral-file/ffos-user/components/feral-connectd/logger"
	"github.com/feral-file/ffos-user/components/feral-connectd/relayer"
	"github.com/feral-file/ffos-user/components/feral-connectd/state"
	"github.com/feral-file/ffos-user/components/feral-connectd/status"
	"github.com/feral-file/ffos-user/components/feral-connectd/wrapper"
	"github.com/feral-file/godbus"
	"go.uber.org/zap"
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
}

func New(
	relayer relayer.Relayer,
	dbus dbus.DBus,
	cdp cdp.CDP,
	cmd command.CommandHandler,
	clock wrapper.Clock,
	l *zap.Logger,
) Mediator {
	return &mediator{
		relayer: relayer,
		dbus:    dbus,
		cdp:     cdp,
		cmd:     cmd,
		clock:   clock,
		logger:  l,
		tracer:  logger.NewRelayerMessageTracer(l),
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

		if cmd.ConnectdCmd() {
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

// SetStatusPoller sets the StatusPoller reference after initialization
func (m *mediator) SetStatusPoller(statusPoller status.Poller) {
	m.statusPoller = statusPoller
}
