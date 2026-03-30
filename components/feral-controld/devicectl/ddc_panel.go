package devicectl

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DdcPanelAction selects which VCP is driven for ddcPanelControl requests.
type DdcPanelAction string

const (
	DdcPanelActionBrightness DdcPanelAction = "brightness"
	DdcPanelActionContrast   DdcPanelAction = "contrast"
	DdcPanelActionVolume     DdcPanelAction = "volume"
	DdcPanelActionMute       DdcPanelAction = "mute"
	DdcPanelActionPower      DdcPanelAction = "power"
)

var knownDdcPanelActions = map[string]DdcPanelAction{
	string(DdcPanelActionBrightness): DdcPanelActionBrightness,
	string(DdcPanelActionContrast):   DdcPanelActionContrast,
	string(DdcPanelActionVolume):     DdcPanelActionVolume,
	string(DdcPanelActionMute):       DdcPanelActionMute,
	string(DdcPanelActionPower):      DdcPanelActionPower,
}

// ParseDdcPanelAction normalizes and validates the wire-level action string.
func ParseDdcPanelAction(s string) (DdcPanelAction, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	a, ok := knownDdcPanelActions[key]
	if !ok {
		return "", fmt.Errorf("invalid ddcPanelControl action %q", s)
	}
	return a, nil
}

// DdcMuteSetting is the discrete mute VCP value for DdcPanelActionMute.
type DdcMuteSetting string

const (
	DdcMuteOn  DdcMuteSetting = "on"
	DdcMuteOff DdcMuteSetting = "off"
)

var knownDdcMuteSettings = map[string]DdcMuteSetting{
	string(DdcMuteOn):  DdcMuteOn,
	string(DdcMuteOff): DdcMuteOff,
}

// ParseDdcMuteSetting validates mute VCP payload values.
func ParseDdcMuteSetting(s string) (DdcMuteSetting, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	m, ok := knownDdcMuteSettings[key]
	if !ok {
		return "", fmt.Errorf("invalid mute value %q: want %q or %q", s, DdcMuteOn, DdcMuteOff)
	}
	return m, nil
}

// DdcPowerSetting is the discrete power VCP value for DdcPanelActionPower.
type DdcPowerSetting string

const (
	DdcPowerStandby DdcPowerSetting = "standby"
	DdcPowerOff     DdcPowerSetting = "off"
	DdcPowerOn      DdcPowerSetting = "on"
)

var knownDdcPowerSettings = map[string]DdcPowerSetting{
	string(DdcPowerStandby): DdcPowerStandby,
	string(DdcPowerOff):     DdcPowerOff,
	string(DdcPowerOn):      DdcPowerOn,
}

// ParseDdcPowerSetting validates power VCP payload values.
func ParseDdcPowerSetting(s string) (DdcPowerSetting, error) {
	key := strings.ToLower(strings.TrimSpace(s))
	p, ok := knownDdcPowerSettings[key]
	if !ok {
		return "", fmt.Errorf("invalid power value %q: want %q, %q, or %q",
			s, DdcPowerStandby, DdcPowerOff, DdcPowerOn)
	}
	return p, nil
}

const (
	ddcVCPBrightnessCode = "10"
	ddcVCPContrastCode   = "12"
	ddcVCPVolumeCode     = "62"
	ddcVCPMuteCode       = "8D"
	ddcVCPPowerCode      = "D6"
)

func resolveDdcSetVCP(action DdcPanelAction, raw json.RawMessage) (vcpCode, vcpVal string, err error) {
	switch action {
	case DdcPanelActionBrightness:
		n, err := ddcParsePercent(raw)
		if err != nil {
			return "", "", err
		}
		return ddcVCPBrightnessCode, fmt.Sprintf("%d", n), nil
	case DdcPanelActionContrast:
		n, err := ddcParsePercent(raw)
		if err != nil {
			return "", "", err
		}
		return ddcVCPContrastCode, fmt.Sprintf("%d", n), nil
	case DdcPanelActionVolume:
		n, err := ddcParsePercent(raw)
		if err != nil {
			return "", "", err
		}
		return ddcVCPVolumeCode, fmt.Sprintf("%d", n), nil
	case DdcPanelActionMute:
		s, err := ddcParseStringValue(raw)
		if err != nil {
			return "", "", err
		}
		m, err := ParseDdcMuteSetting(s)
		if err != nil {
			return "", "", err
		}
		switch m {
		case DdcMuteOn:
			return ddcVCPMuteCode, "1", nil
		case DdcMuteOff:
			return ddcVCPMuteCode, "2", nil
		default:
			return "", "", fmt.Errorf("internal: unhandled mute setting %q", m)
		}
	case DdcPanelActionPower:
		s, err := ddcParseStringValue(raw)
		if err != nil {
			return "", "", err
		}
		p, err := ParseDdcPowerSetting(s)
		if err != nil {
			return "", "", err
		}
		switch p {
		case DdcPowerStandby:
			return ddcVCPPowerCode, "04", nil
		case DdcPowerOff:
			return ddcVCPPowerCode, "05", nil
		case DdcPowerOn:
			return ddcVCPPowerCode, "01", nil
		default:
			return "", "", fmt.Errorf("internal: unhandled power setting %q", p)
		}
	default:
		return "", "", fmt.Errorf("internal: unhandled ddcPanelAction %q", action)
	}
}

func ddcParsePercent(raw json.RawMessage) (int, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("value must be an integer 0-100: %w", err)
	}
	if n < 0 || n > 100 {
		return 0, fmt.Errorf("value must be between 0 and 100, got %d", n)
	}
	return n, nil
}

func ddcParseStringValue(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("value must be a string: %w", err)
	}
	return strings.TrimSpace(s), nil
}
