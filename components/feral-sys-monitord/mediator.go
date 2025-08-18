package main

import (
	"context"
	"encoding/json"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"
)

type Mediator struct {
	dbus          *godbus.DBusClient
	sysResMonitor *metric.SysResMonitor
	connectivity  *Connectivity
	eventWatcher  *SysEventWatcher
	logger        *zap.Logger
}

func NewMediator(
	dbus *godbus.DBusClient,
	monitor *metric.SysResMonitor,
	connectivity *Connectivity,
	eventWatcher *SysEventWatcher,
	logger *zap.Logger) *Mediator {
	return &Mediator{
		dbus:          dbus,
		sysResMonitor: monitor,
		connectivity:  connectivity,
		eventWatcher:  eventWatcher,
		logger:        logger,
	}
}

func (p *Mediator) Start() {
	p.sysResMonitor.OnMonitor(p.handleSysMetrics)
	p.connectivity.OnConnectivityChange(p.handleConnectivityChange)
	p.eventWatcher.OnEvent(p.handleSysEvent)
}

func (p *Mediator) Stop() {
	p.eventWatcher.RemoveEventHandler(p.handleSysEvent)
	p.connectivity.RemoveConnectivityChange(p.handleConnectivityChange)
	p.sysResMonitor.RemoveMonitorHandler(p.handleSysMetrics)
}

func (p *Mediator) handleSysMetrics(metrics *metric.SysMetrics) {
	p.logger.Debug("Received metrics", zap.Any("metrics", metrics))

	// Marshal the metrics to a byte slice
	metricsBytes, err := json.Marshal(metrics)
	if err != nil {
		p.logger.Error("Failed to marshal metrics", zap.Error(err))
		return
	}

	// Send a DBus signal
	err = p.dbus.Send(godbus.DBusPayload{
		Interface: DBUS_INTERFACE,
		Path:      DBUS_PATH,
		Member:    DBUS_EVENT_SYSMETRICS,
		Body:      []interface{}{metricsBytes},
	})
	if err != nil {
		p.logger.Error("Failed to send DBus signal", zap.Error(err))
	}
}

func (p *Mediator) handleConnectivityChange(ctx context.Context, connected bool) {
	p.logger.Debug("Received connectivity change", zap.Bool("connected", connected))

	// Send a DBus signal
	err := p.dbus.Send(godbus.DBusPayload{
		Interface: DBUS_INTERFACE,
		Path:      DBUS_PATH,
		Member:    DBUS_EVENT_CONNECTIVITY_CHANGE,
		Body:      []interface{}{connected},
	})
	if err != nil {
		p.logger.Error("Failed to send DBus signal", zap.Error(err))
	}
}

func (p *Mediator) handleSysEvent(event Event) {
	p.logger.Debug("Received sys event", zap.String("event", string(event)))

	// Send a DBus signal
	err := p.dbus.Send(godbus.DBusPayload{
		Interface: DBUS_INTERFACE,
		Path:      DBUS_PATH,
		Member:    DBUS_EVENT_SYSEVENT,
		Body:      []interface{}{event},
	})
	if err != nil {
		p.logger.Error("Failed to send DBus signal", zap.Error(err))
	}
}
