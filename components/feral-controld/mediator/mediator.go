package mediator

import (
	"context"
	"fmt"
	"reflect"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/command"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/operation"
	playlist_refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
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
	cmd          command.Handler
	executor     operation.Executor
	statusPoller status.Poller
	logger       *zap.Logger
	refresher    playlist_refresher.Refresher
}

func New(
	relayer relayer.Relayer,
	dbus dbus.DBus,
	cdp cdp.CDP,
	cmd command.Handler,
	executor operation.Executor,
	refresher playlist_refresher.Refresher,
	l *zap.Logger,
) Mediator {
	return &mediator{
		relayer:   relayer,
		dbus:      dbus,
		cdp:       cdp,
		cmd:       cmd,
		executor:  executor,
		logger:    l,
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
		m.executor.SaveLastSysMetrics(body)

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

	switch payload.MessageID {
	case relayer.MESSAGE_ID_SYSTEM:
		topicID := payload.Message.TopicID
		if topicID == nil {
			err := fmt.Errorf("payload doesn't contain topicID")
			m.logger.Error("Payload doesn't contain topicID", zap.Any("payload", payload))
			return err
		}

		// Save state
		s := state.GetState()
		s.Relayer.TopicID = *topicID
		err := s.Save()
		if err != nil {
			m.logger.Error("Failed to persist state", zap.Error(err))
			return err
		}

	default:
		result, err := m.cmd.Process(ctx, payload)
		if err != nil {
			m.logger.Error("Failed to process command", zap.Error(err))
			return err
		}
		if result == nil {
			m.logger.Warn("Processed command returned no result", zap.Any("payload", payload))
			return nil
		}

		return m.relayer.Send(ctx, result)
	}

	return nil
}

// SetStatusPoller sets the StatusPoller reference after initialization
func (m *mediator) SetStatusPoller(statusPoller status.Poller) {
	m.statusPoller = statusPoller
	m.cmd.SetStatusPoller(statusPoller)
}
