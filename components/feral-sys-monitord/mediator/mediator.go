package mediator

import (
	"context"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/connectivity"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/dbus"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/event"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/wrapper"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"
)

//go:generate mockgen -source=mediator.go -destination=../mocks/mediator.go -package=mocks -mock_names=Mediator=MockMediator
type Mediator interface {
	// Start starts the mediator
	Start()

	// Stop stops the mediator
	Stop()
}

type mediator struct {
	dbus                *godbus.DBusClient
	systemMonitor       metric.Monitor
	connectivityHandler connectivity.Handler
	eventWatcher        event.Watcher
	logger              *zap.Logger

	// Dependencies
	json wrapper.JSON
}

func New(
	dbus *godbus.DBusClient,
	systemMonitor metric.Monitor,
	connectivityHandler connectivity.Handler,
	eventWatcher event.Watcher,
	logger *zap.Logger,
	json wrapper.JSON,
) *mediator {
	return &mediator{
		dbus:                dbus,
		systemMonitor:       systemMonitor,
		connectivityHandler: connectivityHandler,
		eventWatcher:        eventWatcher,
		logger:              logger,
		json:                json,
	}
}

func (m *mediator) Start() {
	m.systemMonitor.AddHandler(m.handleSystemMetrics)
	m.connectivityHandler.AddHandler(m.handleConnectivityChange)
	m.eventWatcher.AddHandler(m.handleSystemEvent)
}

func (m *mediator) Stop() {
	m.eventWatcher.RemoveHandler(m.handleSystemEvent)
	m.connectivityHandler.RemoveHandler(m.handleConnectivityChange)
	m.systemMonitor.RemoveHandler(m.handleSystemMetrics)
}

func (m *mediator) handleSystemMetrics(metrics *metric.SystemMetrics) {
	m.logger.Debug("Received metrics", zap.Any("metrics", metrics))

	// Marshal the metrics to a byte slice
	metricsBytes, err := m.json.Marshal(metrics)
	if err != nil {
		m.logger.Error("Failed to marshal metrics", zap.Error(err))
		return
	}

	// Send a DBus signal
	err = m.dbus.Send(godbus.DBusPayload{
		Interface: dbus.INTERFACE,
		Path:      dbus.PATH,
		Member:    dbus.EVENT_SYSMETRICS,
		Body:      []interface{}{metricsBytes},
	})
	if err != nil {
		m.logger.Error("Failed to send DBus signal", zap.Error(err))
	}
}

func (m *mediator) handleConnectivityChange(ctx context.Context, connected bool) {
	m.logger.Debug("Received connectivity change", zap.Bool("connected", connected))

	// Send a DBus signal
	err := m.dbus.Send(godbus.DBusPayload{
		Interface: dbus.INTERFACE,
		Path:      dbus.PATH,
		Member:    dbus.EVENT_CONNECTIVITY_CHANGE,
		Body:      []interface{}{connected},
	})
	if err != nil {
		m.logger.Error("Failed to send DBus signal", zap.Error(err))
	}
}

func (m *mediator) handleSystemEvent(event event.Event) {
	m.logger.Debug("Received sys event", zap.String("event", string(event)))

	// Send a DBus signal
	err := m.dbus.Send(godbus.DBusPayload{
		Interface: dbus.INTERFACE,
		Path:      dbus.PATH,
		Member:    dbus.EVENT_SYSEVENT,
		Body:      []interface{}{event},
	})
	if err != nil {
		m.logger.Error("Failed to send DBus signal", zap.Error(err))
	}
}
