package main

import (
	"context"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"
)

const (
	DBUS_INTERFACE godbus.Interface = "com.feralfile.appmonitord"
	DBUS_PATH      godbus.Path      = "/com/feralfile/appmonitord"
	DBUS_NAME      string           = "com.feralfile.appmonitord"

	MONITORD_DBUS_INTERFACE godbus.Interface = "com.feralfile.sysmonitord"
	MONITORD_DBUS_PATH      godbus.Path      = "/com/feralfile/sysmonitord"
	MONITORD_DBUS_NAME      string           = "com.feralfile.sysmonitord"

	SETUPD_DBUS_INTERFACE godbus.Interface = "com.feralfile.setupd.general"
	SETUPD_DBUS_PATH      godbus.Path      = "/com/feralfile/setupd"
	SETUPD_DBUS_NAME      string           = "com.feralfile.setupd"

	MONITORD_METHOD_GET_CONNECTIVITY_STATUS godbus.Member = "GetConnectivityStatus"
	MONITORD_METHOD_GET_SYSMETRICS          godbus.Member = "GetSysMetrics"

	SETUPD_METHOD_GET_PAGE_STATE godbus.Member = "GetPageState"
)

//go:generate mockgen -source=dbus.go -destination=../mocks/mock_dbus.go -package=mocks -mock_names=ClientInterface=MockDBusClient
type ClientInterface interface {
	Start() error
	Stop() error
	Export(obj interface{}, path godbus.Path, iface godbus.Interface) error
	Call(ctx context.Context, name string, path godbus.Path, iface godbus.Interface, method godbus.Member, args ...any) ([]any, error)
	RetryableSend(ctx context.Context, payload godbus.DBusPayload) error
	OnBusSignal(handler godbus.BusSignalHandler)
	RemoveBusSignal(handler godbus.BusSignalHandler)
}

type AppMonitordDBus struct {
	logger *zap.Logger
}

func NewAppMonitordDBus(logger *zap.Logger) *AppMonitordDBus {
	return &AppMonitordDBus{
		logger: logger,
	}
}
