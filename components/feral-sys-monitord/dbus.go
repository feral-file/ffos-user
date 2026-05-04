package main

import (
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"

	"github.com/feral-file/godbus"
	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

const (
	DBUS_INTERFACE godbus.Interface = "com.feralfile.sysmonitord"
	DBUS_NAME      string           = "com.feralfile.sysmonitord"
	DBUS_PATH      godbus.Path      = "/com/feralfile/sysmonitord"

	DBUS_EVENT_SYSMETRICS          godbus.Member = "sysmetrics"
	DBUS_EVENT_CONNECTIVITY_CHANGE godbus.Member = "connectivity_change"
	DBUS_EVENT_SYSEVENT            godbus.Member = "sysevent"
)

type SysMonitordDBus struct {
	connectivity  *Connectivity
	sysResMonitor *metric.SysResMonitor
	logger        *zap.Logger
}

func NewSysMonitordDBus(connectivity *Connectivity, sysResMonitor *metric.SysResMonitor, logger *zap.Logger) *SysMonitordDBus {
	return &SysMonitordDBus{
		connectivity:  connectivity,
		sysResMonitor: sysResMonitor,
		logger:        logger,
	}
}

func (s *SysMonitordDBus) GetConnectivityStatus(refresh bool) (bool, *dbus.Error) {
	s.logger.Info("DBus RPC called: GetConnectivityStatus", zap.Bool("refresh", refresh))
	if refresh {
		connected, err := s.connectivity.CheckConnectivity(PING_TIMEOUT)
		if err != nil {
			// We accept not being able to check connectivity and push the error to the caller
			return false, dbus.NewError(err.Error(), []interface{}{})
		}
		return connected, nil
	} else {
		return s.connectivity.GetLastConnected(), nil
	}
}

func (s *SysMonitordDBus) GetSysMetrics() (*metric.SysDBusMetrics, *dbus.Error) {
	s.logger.Info("DBus RPC called: GetSysMetrics")
	return s.sysResMonitor.LastMetrics().DBus(), nil
}
