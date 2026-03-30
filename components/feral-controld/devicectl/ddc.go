package devicectl

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// -----------------------------------------------------------------------------
// Panel control: wire types and setvcp resolution (ddcPanelControl)
// -----------------------------------------------------------------------------

// DdcPanelControlRequest is the JSON body for command ddcPanelControl.
type DdcPanelControlRequest struct {
	Action string          `json:"action"`
	Value  json.RawMessage `json:"value"`
}

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
		n, err := parseDdcJSONPercent(raw)
		if err != nil {
			return "", "", err
		}
		return ddcVCPBrightnessCode, fmt.Sprintf("%d", n), nil
	case DdcPanelActionContrast:
		n, err := parseDdcJSONPercent(raw)
		if err != nil {
			return "", "", err
		}
		return ddcVCPContrastCode, fmt.Sprintf("%d", n), nil
	case DdcPanelActionVolume:
		n, err := parseDdcJSONPercent(raw)
		if err != nil {
			return "", "", err
		}
		return ddcVCPVolumeCode, fmt.Sprintf("%d", n), nil
	case DdcPanelActionMute:
		s, err := parseDdcJSONString(raw)
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
		s, err := parseDdcJSONString(raw)
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

func parseDdcJSONPercent(raw json.RawMessage) (int, error) {
	var n int
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, fmt.Errorf("value must be an integer 0-100: %w", err)
	}
	if n < 0 || n > 100 {
		return 0, fmt.Errorf("value must be between 0 and 100, got %d", n)
	}
	return n, nil
}

func parseDdcJSONString(raw json.RawMessage) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", fmt.Errorf("value must be a string: %w", err)
	}
	return strings.TrimSpace(s), nil
}

// -----------------------------------------------------------------------------
// Panel status: getvcp --brief parsing (ddcPanelStatus)
// -----------------------------------------------------------------------------

// DdcPanelStatus is returned by ddcPanelStatus. Omitted fields mean that feature could not be read.
// Errors maps field name to message.
type DdcPanelStatus struct {
	Brightness *int              `json:"brightness,omitempty"`
	Contrast   *int              `json:"contrast,omitempty"`
	Volume     *int              `json:"volume,omitempty"`
	Mute       *string           `json:"mute,omitempty"`
	Power      *string           `json:"power,omitempty"`
	Errors     map[string]string `json:"errors,omitempty"`
}

var (
	reDdcBriefContinuous = regexp.MustCompile(`(?i)^VCP\s+\S+\s+C\s+(\d+)\s+(\d+)\s*$`)
	reDdcBriefERR        = regexp.MustCompile(`(?i)^VCP\s+\S+\s+ERR\s*$`)
	reDdcBriefSNC        = regexp.MustCompile(`(?i)^VCP\s+\S+\s+SNC\s+(\S+)\s*$`)
	// Docs say "CNC"; some builds use "CND".
	reDdcBriefCNC = regexp.MustCompile(`(?i)^VCP\s+\S+\s+CN[CD]\s+(\S+)\s+(\S+)\s+(\S+)\s+(\S+)\s*$`)
)

type ddcBriefKind int

const (
	ddcBriefInvalid ddcBriefKind = iota
	ddcBriefContinuous
	ddcBriefSNC
	ddcBriefCNC
)

type ddcBriefParsed struct {
	Kind    ddcBriefKind
	Current int
	Max     int
	SL      int
}

func parseDdcutilGetVcpBrief(output string) (ddcBriefParsed, error) {
	line := ddcFirstVCPBriefLine(output)
	if line == "" {
		return ddcBriefParsed{}, fmt.Errorf("no VCP line in ddcutil output")
	}

	if reDdcBriefERR.MatchString(line) {
		return ddcBriefParsed{}, fmt.Errorf("VCP reported ERR")
	}
	if m := reDdcBriefContinuous.FindStringSubmatch(line); m != nil {
		cur, err1 := strconv.Atoi(m[1])
		max, err2 := strconv.Atoi(m[2])
		if err1 != nil || err2 != nil {
			return ddcBriefParsed{}, fmt.Errorf("parse continuous VCP values: %v %v", err1, err2)
		}
		return ddcBriefParsed{Kind: ddcBriefContinuous, Current: cur, Max: max}, nil
	}
	if m := reDdcBriefSNC.FindStringSubmatch(line); m != nil {
		sl, err := parseDdcHexByte(m[1])
		if err != nil {
			return ddcBriefParsed{}, fmt.Errorf("parse SNC value: %w", err)
		}
		return ddcBriefParsed{Kind: ddcBriefSNC, SL: sl}, nil
	}
	if m := reDdcBriefCNC.FindStringSubmatch(line); m != nil {
		sl, err := parseDdcHexByte(m[4])
		if err != nil {
			return ddcBriefParsed{}, fmt.Errorf("parse CNC SL byte: %w", err)
		}
		return ddcBriefParsed{Kind: ddcBriefCNC, SL: sl}, nil
	}

	return ddcBriefParsed{}, fmt.Errorf("unrecognized brief getvcp line: %s", line)
}

func ddcFirstVCPBriefLine(output string) string {
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(line), "vcp") {
			return line
		}
	}
	return ""
}

func parseDdcHexByte(s string) (int, error) {
	t := strings.TrimSpace(strings.ToLower(s))
	t = strings.TrimPrefix(t, "x")
	t = strings.TrimPrefix(t, "0x")
	v, err := strconv.ParseUint(t, 16, 8)
	if err != nil {
		return 0, err
	}
	return int(v), nil
}

func ddcMuteFromSL(sl int) (string, bool) {
	switch sl {
	case 0x01:
		return string(DdcMuteOn), true
	case 0x02:
		return string(DdcMuteOff), true
	default:
		return "", false
	}
}

func ddcPowerFromSL(sl int) (string, bool) {
	switch sl {
	case 0x01:
		return string(DdcPowerOn), true
	case 0x04:
		return string(DdcPowerStandby), true
	case 0x05:
		return string(DdcPowerOff), true
	default:
		return "", false
	}
}

var ddcPanelStatusQueries = []struct {
	field string
	code  string
}{
	{field: "brightness", code: ddcVCPBrightnessCode},
	{field: "contrast", code: ddcVCPContrastCode},
	{field: "volume", code: ddcVCPVolumeCode},
	{field: "mute", code: ddcVCPMuteCode},
	{field: "power", code: ddcVCPPowerCode},
}

func ddcIntPtr(n int) *int {
	v := n
	return &v
}

func ddcStringPtr(s string) *string {
	v := s
	return &v
}

// -----------------------------------------------------------------------------
// ddcutil runner: detect + retry, setvcp, getvcp, full status
// -----------------------------------------------------------------------------

// panelDdc runs ddcutil against the default display. Requires RW /dev/i2c-* (udev/i2c group).
type panelDdc struct {
	exec   wrapper.Exec
	logger *zap.Logger
}

func newPanelDdc(exec wrapper.Exec, logger *zap.Logger) *panelDdc {
	return &panelDdc{exec: exec, logger: logger}
}

// ApplyControl runs setvcp for the resolved action/value pair.
func (p *panelDdc) ApplyControl(ctx context.Context, action DdcPanelAction, value json.RawMessage) error {
	code, val, err := resolveDdcSetVCP(action, value)
	if err != nil {
		return err
	}
	p.logger.Info("ddcutil setvcp",
		zap.String("action", string(action)),
		zap.String("vcp", code),
		zap.String("value", val))
	if err := p.setVCP(ctx, code, val); err != nil {
		p.logger.Error("ddcutil setvcp failed", zap.Error(err))
		return err
	}
	return nil
}

// CollectStatus queries brightness, contrast, volume, mute, and power VCPs.
func (p *panelDdc) CollectStatus(ctx context.Context) (*DdcPanelStatus, error) {
	status := &DdcPanelStatus{}
	errs := map[string]string{}

	for _, q := range ddcPanelStatusQueries {
		out, err := p.getVCPBrief(ctx, q.code)
		if err != nil {
			errs[q.field] = err.Error()
			continue
		}
		parsed, err := parseDdcutilGetVcpBrief(out)
		if err != nil {
			errs[q.field] = err.Error()
			continue
		}
		switch q.field {
		case "brightness", "contrast", "volume":
			if parsed.Kind != ddcBriefContinuous {
				errs[q.field] = fmt.Sprintf("expected continuous VCP, got kind %v", parsed.Kind)
				continue
			}
			switch q.field {
			case "brightness":
				status.Brightness = ddcIntPtr(parsed.Current)
			case "contrast":
				status.Contrast = ddcIntPtr(parsed.Current)
			case "volume":
				status.Volume = ddcIntPtr(parsed.Current)
			}
		case "mute":
			if parsed.Kind != ddcBriefSNC && parsed.Kind != ddcBriefCNC {
				errs[q.field] = fmt.Sprintf("expected SNC or CNC VCP for mute, got kind %v", parsed.Kind)
				continue
			}
			s, ok := ddcMuteFromSL(parsed.SL)
			if !ok {
				errs[q.field] = fmt.Sprintf("unmapped mute SL=0x%02x", parsed.SL)
				continue
			}
			status.Mute = ddcStringPtr(s)
		case "power":
			if parsed.Kind != ddcBriefSNC && parsed.Kind != ddcBriefCNC {
				errs[q.field] = fmt.Sprintf("expected SNC or CNC VCP for power, got kind %v", parsed.Kind)
				continue
			}
			s, ok := ddcPowerFromSL(parsed.SL)
			if !ok {
				errs[q.field] = fmt.Sprintf("unmapped power SL=0x%02x", parsed.SL)
				continue
			}
			status.Power = ddcStringPtr(s)
		}
	}

	if len(errs) > 0 {
		status.Errors = errs
	}
	return status, nil
}

func (p *panelDdc) getVCPBrief(ctx context.Context, vcpCode string) (string, error) {
	out, err := p.execDdcutilWithDisplayRecovery(ctx, "ddcutil", "--noverify", "getvcp", "--brief", vcpCode)
	if err != nil {
		return "", fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return string(out), nil
}

func (p *panelDdc) setVCP(ctx context.Context, vcpCode, value string) error {
	out, err := p.execDdcutilWithDisplayRecovery(ctx, "ddcutil", "--noverify", "setvcp", vcpCode, value)
	if err != nil {
		return fmt.Errorf("ddcutil setvcp %s %s: %s: %w", vcpCode, value, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// execDdcutilWithDisplayRecovery runs ddcutil; on "display not found", runs detect once and retries.
func (p *panelDdc) execDdcutilWithDisplayRecovery(ctx context.Context, argv ...string) ([]byte, error) {
	if len(argv) < 1 {
		return nil, fmt.Errorf("ddcutil: missing command name")
	}
	run := func() ([]byte, error) {
		return p.exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	}

	out, err := run()
	if !ddcutilOutputImpliesDisplayNotFound(out, err) {
		return out, err
	}

	p.logger.Info("ddcutil reported display not found; running detect and retrying once",
		zap.Strings("ddcutil_argv", argv))

	detectOut, detErr := p.exec.CommandContext(ctx, "ddcutil", "--noverify", "detect").CombinedOutput()
	if detErr != nil {
		p.logger.Warn("ddcutil detect after display-not-found",
			zap.Error(detErr),
			zap.String("output", strings.TrimSpace(string(detectOut))))
	}

	return run()
}

func ddcutilOutputImpliesDisplayNotFound(out []byte, err error) bool {
	s := strings.ToLower(string(out))
	if err != nil {
		s += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(s, "display not found")
}
