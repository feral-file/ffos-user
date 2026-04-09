package mediator

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mdns"
	playlist_refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

//go:generate mockgen -source=mediator.go -destination=../mocks/mediator.go -package=mocks -mock_names=Mediator=MockMediator

type Mediator interface {
	Start()
	Stop()
	InitializeMDNS(advertiser mdns.Advertiser, info mdns.DeviceInfo, internetConnected bool)
}

type mediator struct {
	relayer    relayer.Relayer
	dbus       dbus.DBus
	cdp        cdp.CDP
	cmdHandler commandrouter.Handler
	executor   devicectl.Executor
	logger     *zap.Logger
	refresher  playlist_refresher.Refresher
	json       wrapper.JSON

	mdnsMu         sync.Mutex
	mdnsAdvertiser mdns.Advertiser
	mdnsDeviceInfo mdns.DeviceInfo
}

func New(
	relayer relayer.Relayer,
	dbus dbus.DBus,
	cdp cdp.CDP,
	cmdHandler commandrouter.Handler,
	executor devicectl.Executor,
	refresher playlist_refresher.Refresher,
	json wrapper.JSON,
	l *zap.Logger,
) Mediator {
	return &mediator{
		relayer:    relayer,
		dbus:       dbus,
		cdp:        cdp,
		cmdHandler: cmdHandler,
		executor:   executor,
		json:       json,
		logger:     l,
		refresher:  refresher,
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

func (m *mediator) InitializeMDNS(advertiser mdns.Advertiser, info mdns.DeviceInfo, internetConnected bool) {
	m.mdnsMu.Lock()
	defer m.mdnsMu.Unlock()

	m.mdnsAdvertiser = advertiser
	m.mdnsDeviceInfo = info

	if internetConnected {
		if err := m.mdnsAdvertiser.Start(info); err != nil {
			m.logger.Warn("Failed to start mDNS advertiser", zap.Error(err))
		}
	}
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
		m.logger.Info("Processing connectivity transition",
			zap.Bool("connected", connected),
			zap.Bool("relayer_connected", m.relayer.IsConnected()),
			zap.Bool("mdns_active", m.mdnsAdvertiser != nil),
		)

		// Send the connectivity change to web app
		m.logger.Debug("Forwarding connectivity change to web app", zap.Bool("connected", connected))
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
			m.logger.Info("Connectivity restored, reconnecting relayer")
			err := m.relayer.RetryableConnect(ctx)
			if err != nil {
				m.logger.Error("Failed to reconnect to relayer", zap.Error(err))
			} else {
				m.logger.Info("Relayer reconnected after connectivity change")
			}
		}

		// Re-register mDNS on connectivity changes: stop on network loss
		// (sockets become invalid) and re-register on restore (bind fresh interfaces).
		m.mdnsMu.Lock()
		if m.mdnsAdvertiser != nil {
			m.mdnsAdvertiser.Stop()
			if connected {
				if err := m.mdnsAdvertiser.Start(m.mdnsDeviceInfo); err != nil {
					m.logger.Warn("Failed to restart mDNS advertiser", zap.Error(err))
				}
			}
		}
		m.mdnsMu.Unlock()

	default:
		m.logger.Warn("Unknown signal", zap.String("member", payload.Member.String()))
	}

	return nil, nil
}

func (m *mediator) handleRelayerMessage(ctx context.Context, payload relayer.Payload) error {
	payloadJSON, _ := m.json.Marshal(payload)
	logPayload := helper.TruncateBytes(payloadJSON, logger.MAX_FIELD_LENGTH)
	m.logger.Info("handle received relayer message",
		zap.ByteString("payload", logPayload),
		zap.String("messageID", payload.MessageID),
		zap.String("command", func() string {
			if payload.Message.Command == nil {
				return ""
			}
			return *payload.Message.Command
		}()),
	)

	switch payload.MessageID {
	case relayer.MESSAGE_ID_SYSTEM:
		topicID := payload.Message.TopicID
		if topicID == nil {
			err := fmt.Errorf("payload doesn't contain topicID")
			m.logger.Error("Payload doesn't contain topicID", zap.ByteString("payload", logPayload))
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
		var commandType commands.Type
		if payload.Message.Command != nil {
			commandType = commands.Type(*payload.Message.Command)
		}
		command := commands.Command{
			Type:      commandType,
			Arguments: payload.Message.Request,
		}
		result, err := m.cmdHandler.Process(ctx, command)
		if err != nil {
			m.logger.Error("Failed to process command", zap.Error(err))
			return err
		}
		if result == nil {
			m.logger.Warn("Processed command returned no result", zap.ByteString("payload", logPayload))
			return nil
		}

		resp := relayer.Response{
			Type:      "RPC",
			MessageID: payload.MessageID,
			Message:   result,
		}

		m.logger.Debug("Sending relayer RPC response", zap.String("messageID", payload.MessageID))
		return m.relayer.Send(ctx, resp)
	}

	return nil
}
