package dbus

import (
	"context"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/connectivity"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"

	"github.com/feral-file/godbus"
	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

const (
	INTERFACE godbus.Interface = "com.feralfile.sysmonitord"
	NAME      string           = "com.feralfile.sysmonitord"
	PATH      godbus.Path      = "/com/feralfile/sysmonitord"

	EVENT_SYSMETRICS          godbus.Member = "sysmetrics"
	EVENT_CONNECTIVITY_CHANGE godbus.Member = "connectivity_change"
	EVENT_SYSEVENT            godbus.Member = "sysevent"
)

//go:generate mockgen -source=handler.go -destination=../mocks/dbus.go -package=mocks -mock_names=DBus=MockDBus
type DBus interface {
	Start() error
	Stop() error
	Export(obj interface{}, path godbus.Path, iface godbus.Interface) error
	Call(ctx context.Context, name string, path godbus.Path, iface godbus.Interface, method godbus.Member, args ...any) ([]any, error)
	RetryableSend(ctx context.Context, payload godbus.DBusPayload) error
	OnBusSignal(handler godbus.BusSignalHandler)
	RemoveBusSignal(handler godbus.BusSignalHandler)
}

//go:generate mockgen -source=handler.go -destination=../mocks/dbus.go -package=mocks -mock_names=Handler=MockDBusHandler
type Handler interface {
	// GetConnectivityStatus gets the connectivity status
	GetConnectivityStatus(refresh bool) (bool, *dbus.Error)

	// GetSysMetrics gets the system metrics
	GetSysMetrics() (*metric.SystemDBusMetrics, *dbus.Error)
}

type handler struct {
	connectivityHandler connectivity.Handler
	systemMonitor       metric.Monitor
	logger              *zap.Logger
}

func NewHandler(connectivityHandler connectivity.Handler, systemMonitor metric.Monitor, logger *zap.Logger) Handler {
	return &handler{
		connectivityHandler: connectivityHandler,
		systemMonitor:       systemMonitor,
		logger:              logger,
	}
}

func (h *handler) GetConnectivityStatus(refresh bool) (bool, *dbus.Error) {
	h.logger.Info("DBus RPC called: GetConnectivityStatus", zap.Bool("refresh", refresh))
	if refresh {
		connected, err := h.connectivityHandler.CheckConnectivity(connectivity.RPC_PING_TIMEOUT)
		if err != nil {
			// We accept not being able to check connectivity and push the error to the caller
			return false, dbus.NewError(err.Error(), []interface{}{})
		}
		return connected, nil
	} else {
		return h.connectivityHandler.GetLastConnected(), nil
	}
}

func (h *handler) GetSysMetrics() (*metric.SystemDBusMetrics, *dbus.Error) {
	h.logger.Info("DBus RPC called: GetSysMetrics")
	return h.systemMonitor.LastMetrics().DBus(), nil
}
