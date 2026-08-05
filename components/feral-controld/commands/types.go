package commands

import "encoding/json"

type Type string

func (c Type) Ptr() *Type {
	return &c
}

func (c Type) String() string {
	return string(c)
}

// Device control commands
var deviceCtlCommands = map[Type]bool{
	CMD_CONNECT:                    true,
	CMD_SHOW_PAIRING_QR_CODE:       true,
	CMD_PROFILE:                    true,
	CMD_KEYBOARD_EVENT:             true,
	CMD_MOUSE_DRAG_EVENT:           true,
	CMD_MOUSE_TAP_EVENT:            true,
	CMD_MOUSE_DOUBLE_TAP_EVENT:     true,
	CMD_MOUSE_LONG_PRESS_EVENT:     true,
	CMD_MOUSE_CLICK_AND_DRAG_EVENT: true,
	CMD_ZOOM_GESTURE:               true,
	CMD_SCREEN_ROTATION:            true,
	CMD_SHUTDOWN:                   true,
	CMD_REBOOT:                     true,
	CMD_ANALYTICS_TOGGLE:           true,
	CMD_BETA_FEATURES_TOGGLE:       true,
	CMD_DEVICE_STATUS:              true,
	CMD_UPDATE_TO_LATEST:           true,
	CMD_FACTORY_RESET:              true,
	CMD_UPLOAD_LOGS:                true,
	CMD_SET_VOLUME:                 true,
	CMD_TOGGLE_MUTE:                true,
	CMD_SSH_ACCESS:                 true,
	CMD_DDC_PANEL_CONTROL:          true,
	CMD_DDC_PANEL_STATUS:           true,
	CMD_SET_SLEEP_SCHEDULE:         true,
	CMD_SLEEP_NOW:                  true,
	CMD_WAKE_NOW:                   true,
	CMD_START_WIFI_SETUP:           true,
}

type Command struct {
	Type      Type           `json:"command,omitempty"` // FIXME: rename json key after decouple the player and relayer concepts
	Arguments map[string]any `json:"request,omitempty"` // FIXME: rename json key after decouple the player and relayer concepts
}

func (c Command) JSON() ([]byte, error) {
	return json.Marshal(c)
}

const (
	CMD_CONNECT                    Type = "connect"
	CMD_SHOW_PAIRING_QR_CODE       Type = "showPairingQRCode"
	CMD_PROFILE                    Type = "deviceMetrics"
	CMD_KEYBOARD_EVENT             Type = "sendKeyboardEvent"
	CMD_MOUSE_DRAG_EVENT           Type = "dragGesture"
	CMD_MOUSE_TAP_EVENT            Type = "tapGesture"
	CMD_MOUSE_DOUBLE_TAP_EVENT     Type = "doubleTapGesture"
	CMD_MOUSE_LONG_PRESS_EVENT     Type = "longPressGesture"
	CMD_MOUSE_CLICK_AND_DRAG_EVENT Type = "clickAndDragGesture"
	CMD_ZOOM_GESTURE               Type = "zoomGesture"
	CMD_SYS_METRICS                Type = "deviceMetrics"
	CMD_SCREEN_ROTATION            Type = "rotate"
	CMD_SHUTDOWN                   Type = "shutdown"
	CMD_REBOOT                     Type = "reboot"
	CMD_ANALYTICS_TOGGLE           Type = "analyticsToggle"
	CMD_BETA_FEATURES_TOGGLE       Type = "betaFeaturesToggle"
	CMD_DEVICE_STATUS              Type = "getDeviceStatus"
	CMD_UPDATE_TO_LATEST           Type = "updateToLatestVersion"
	CMD_DISPLAY_PLAYLIST           Type = "displayPlaylist"
	CMD_FACTORY_RESET              Type = "factoryReset"
	CMD_UPLOAD_LOGS                Type = "uploadLogs"
	CMD_SET_VOLUME                 Type = "setVolume"
	CMD_TOGGLE_MUTE                Type = "toggleMute"
	CMD_SSH_ACCESS                 Type = "sshAccess"
	CMD_DISPLAY_DEFAULT_PLAYLIST   Type = "displayDefaultPlaylist"
	CMD_REFRESH_ARTWORK            Type = "refreshArtwork"
	CMD_SET_SLEEP_SCHEDULE         Type = "setSleepSchedule"
	CMD_SLEEP_NOW                  Type = "sleepNow"
	CMD_WAKE_NOW                   Type = "wakeNow"
	CMD_SET_SLEEP_MODE             Type = "setSleepMode"
	// CMD_START_WIFI_SETUP puts the frame into its existing SoftAP setup mode
	// on the app's request (docs/app-triggered-wifi-setup.md). The reply is
	// produced BEFORE any radio work — raising the AP severs the link that
	// carries it — and everything after the raise is the unchanged
	// out-of-box flow. Ships with the initial v2 release, so the v2 LAN gate
	// (mDNS TXT api=2 + /api/v2/status contract "2") is the capability gate.
	CMD_START_WIFI_SETUP           Type = "startWifiSetup"
	CMD_START_MINT_PAIRING_SESSION Type = "startMintPairingSession"
	CMD_CLOSE_MINT_PAIRING_SESSION Type = "closeMintPairingSession"
	CMD_MINT_PAIRING_APPROVAL      Type = "mintPairingApprovalDecision"
	// CMD_DDC_PANEL_CONTROL drives the attached panel over DDC via ddcutil (brightness, contrast,
	// speaker volume, mute, and power). One JSON command type; request body selects the operation.
	CMD_DDC_PANEL_CONTROL Type = "ddcPanelControl"
	// CMD_DDC_PANEL_STATUS reads the same VCPs as ddcPanelControl via ddcutil getvcp --brief.
	CMD_DDC_PANEL_STATUS Type = "ddcPanelStatus"

	// Offline artwork caching commands (see components/feral-controld/offlinecache
	// and offline-artwork-capture.md). Routed as a pre-CDP special case in
	// commandrouter, same precedent as the mint-pairing commands above: these
	// are controld-owned and never forwarded to window.handleCDPRequest.
	CMD_DOWNLOAD_PLAYLIST_ITEM    Type = "downloadPlaylistItem"
	CMD_DOWNLOAD_PLAYLIST         Type = "downloadPlaylist"
	CMD_CLEAR_PLAYLIST_ITEM_CACHE Type = "clearPlaylistItemCache"
	CMD_CLEAR_PLAYLIST_CACHE      Type = "clearPlaylistCache"
	CMD_GET_OFFLINE_CACHE_STATUS  Type = "getOfflineCacheStatus"
)

func (c Type) DeviceCtlCommand() bool {
	return deviceCtlCommands[c]
}
