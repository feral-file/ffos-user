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
	// CMD_DDC_PANEL_CONTROL drives the attached panel over DDC via ddcutil (brightness, contrast,
	// speaker volume, mute, and power). One JSON command type; request body selects the operation.
	CMD_DDC_PANEL_CONTROL Type = "ddcPanelControl"
	// CMD_DDC_PANEL_STATUS reads the same VCPs as ddcPanelControl via ddcutil getvcp --brief.
	CMD_DDC_PANEL_STATUS Type = "ddcPanelStatus"
)

func (c Type) DeviceCtlCommand() bool {
	return deviceCtlCommands[c]
}
