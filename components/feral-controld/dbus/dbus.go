package dbus

import (
	"context"

	"github.com/feral-file/godbus"
)

const (
	NAME string = "com.feralfile.controld"

	MONITORD_INTERFACE                      godbus.Interface = "com.feralfile.sysmonitord"
	MONITORD_PATH                           godbus.Path      = "/com/feralfile/sysmonitord"
	MONITORD_NAME                           string           = "com.feralfile.sysmonitord"
	MONITORD_METHOD_GET_CONNECTIVITY_STATUS godbus.Member    = "GetConnectivityStatus"
	MONITORD_EVENT_SYSMETRICS               godbus.Member    = "sysmetrics"
	MONITORD_EVENT_CONNECTIVITY_CHANGE      godbus.Member    = "connectivity_change"
)

//go:generate mockgen -source=dbus.go -destination=../mocks/dbus.go -package=mocks -mock_names=DBus=MockDBus
type DBus interface {
	Start() error
	Stop() error
	Export(obj interface{}, path godbus.Path, iface godbus.Interface) error
	Call(ctx context.Context, name string, path godbus.Path, iface godbus.Interface, method godbus.Member, args ...any) ([]any, error)
	OnBusSignal(handler godbus.BusSignalHandler)
	RemoveBusSignal(handler godbus.BusSignalHandler)
}
