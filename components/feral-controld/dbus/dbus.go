package dbus

import (
	"context"

	"github.com/feral-file/godbus"
)

const (
	NAME string = "com.feralfile.controld"

	// INTERFACE and PATH are controld's own identity for signals it EMITS
	// (as opposed to the MONITORD_* constants below, which name a signal
	// controld and feral-watchdog both CONSUME from feral-sys-monitord).
	// feral-watchdog defines its own copies of these two string values
	// (components/feral-watchdog is a separate Go module and cannot import
	// this package) — keep them in sync; see EVENT_CDP_STUCK.
	INTERFACE godbus.Interface = godbus.Interface(NAME)
	PATH      godbus.Path      = "/com/feralfile/controld"

	// EVENT_CDP_STUCK (ffos-user#356): fired when the cdp package's connect
	// supervisor has failed to (re)establish a CDP page-target connection to
	// Chromium for cdphealth.StuckThreshold, and again (with Body[0]=false)
	// the moment it reconnects. feral-controld only REPORTS this — it must
	// never restart chromium-kiosk.service or decide to reboot itself; that
	// decision belongs to feral-watchdog's ChromiumMonitor (see both
	// components' AGENTS.md). Body is a single bool.
	EVENT_CDP_STUCK godbus.Member = "cdp_stuck"

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
	// Send emits a signal on the session bus under controld's own identity
	// (INTERFACE/PATH above) — the outbound counterpart to Call/OnBusSignal,
	// which are both inbound. Added for EVENT_CDP_STUCK (ffos-user#356);
	// mirrors feral-sys-monitord's existing *godbus.DBusClient.Send usage.
	Send(payload godbus.DBusPayload) error
	OnBusSignal(handler godbus.BusSignalHandler)
	RemoveBusSignal(handler godbus.BusSignalHandler)
}
