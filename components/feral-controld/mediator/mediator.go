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
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// OOMRecoveryCallback is called after the displayDefaultPlaylist command
// is successfully sent to the webapp following an OOM kill event.
type OOMRecoveryCallback func(oomKillCount int)

//go:generate mockgen -source=mediator.go -destination=../mocks/mediator.go -package=mocks -mock_names=Mediator=MockMediator

type Mediator interface {
	Start()
	Stop()
	InitializeMDNS(advertiser mdns.Advertiser, info mdns.DeviceInfo, internetConnected bool)
	SetPendingOOMRecovery(oomKillCount int, onRecovered OOMRecoveryCallback)
}

type mediator struct {
	relayer      relayer.Relayer
	dbus         dbus.DBus
	cdp          cdp.CDP
	cmdHandler   commandrouter.Handler
	executor     devicectl.Executor
	statusPoller status.Poller
	logger       *zap.Logger
	refresher    playlist_refresher.Refresher
	json         wrapper.JSON

	mdnsMu         sync.Mutex
	mdnsAdvertiser mdns.Advertiser
	mdnsDeviceInfo mdns.DeviceInfo

	oomMu              sync.Mutex
	oomKillCount       int
	oomRecoveryPending bool
	oomOnRecovered     OOMRecoveryCallback
	oomRetryCount      int
}

const MaxOOMRecoveryRetries = 60

func New(
	relayer relayer.Relayer,
	dbus dbus.DBus,
	cdp cdp.CDP,
	cmdHandler commandrouter.Handler,
	executor devicectl.Executor,
	refresher playlist_refresher.Refresher,
	statusPoller status.Poller,
	json wrapper.JSON,
	l *zap.Logger,
) Mediator {
	return &mediator{
		relayer:      relayer,
		dbus:         dbus,
		cdp:          cdp,
		cmdHandler:   cmdHandler,
		executor:     executor,
		statusPoller: statusPoller,
		json:         json,
		logger:       l,
		refresher:    refresher,
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

func (m *mediator) SetPendingOOMRecovery(oomKillCount int, onRecovered OOMRecoveryCallback) {
	m.oomMu.Lock()
	defer m.oomMu.Unlock()
	m.oomRecoveryPending = true
	m.oomKillCount = oomKillCount
	m.oomRetryCount = 0
	m.oomOnRecovered = onRecovered

	// Suppress player status notifications so the stale playlist state
	// is never persisted on the relayer while recovery is in progress.
	m.statusPoller.SuppressPlayerNotifications(true)

	m.logger.Warn("OOM recovery armed, player notifications suppressed until recovery completes",
		zap.Int("chromium_oom_kill_count", oomKillCount))
}

// tryOOMRecovery waits for the webapp to become responsive (non-nil
// player status), then sends displayDefaultPlaylist and verifies.
// Called on each dbus sysmetrics signal; the recovery stays armed
// (and player notifications stay suppressed) until the player
// confirms the default playlist or the retry limit is reached.
func (m *mediator) tryOOMRecovery(ctx context.Context) {
	m.oomMu.Lock()
	if !m.oomRecoveryPending {
		m.oomMu.Unlock()
		return
	}
	count := m.oomKillCount
	retries := m.oomRetryCount
	m.oomRetryCount++
	m.oomMu.Unlock()

	if retries >= MaxOOMRecoveryRetries {
		m.logger.Warn("OOM recovery: max retries reached, giving up",
			zap.Int("chromium_oom_kill_count", count),
			zap.Int("retries", retries))
		m.finishOOMRecovery(count)
		return
	}

	// Wait until the player is actually responsive before sending.
	playerStatus, err := m.statusPoller.FetchPlayerStatus(ctx)
	if err != nil || playerStatus == nil {
		m.logger.Debug("OOM recovery: player not ready yet, will retry on next signal",
			zap.Int("retry", retries),
			zap.Error(err))
		return
	}

	// Player is responsive but showing something else — override it.
	cmd := commands.Command{
		Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
		Arguments: map[string]any{},
	}

	if _, err := m.cmdHandler.Process(ctx, cmd); err != nil {
		m.logger.Warn("OOM recovery: failed to send displayDefaultPlaylist, will retry",
			zap.Error(err),
			zap.Int("retry", retries))
		return
	}

	m.finishOOMRecovery(count)

	m.logger.Info("OOM recovery: sent displayDefaultPlaylist, will verify on next cycle",
		zap.Int("chromium_oom_kill_count", count),
		zap.Int("retry", retries))
}

func (m *mediator) finishOOMRecovery(count int) {
	m.oomMu.Lock()
	m.oomRecoveryPending = false
	m.oomRetryCount = 0
	cb := m.oomOnRecovered
	m.oomMu.Unlock()

	m.statusPoller.SuppressPlayerNotifications(false)

	m.logger.Info("OOM recovery complete, player notifications resumed",
		zap.Int("chromium_oom_kill_count", count))

	if cb != nil {
		cb(count)
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

		m.tryOOMRecovery(ctx)

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
	m.logger.Info("handle received relayer message", zap.ByteString("payload", logPayload))

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

		return m.relayer.Send(ctx, resp)
	}

	return nil
}
