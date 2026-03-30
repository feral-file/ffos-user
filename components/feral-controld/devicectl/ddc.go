package devicectl

import (
	"context"
	"encoding/json"
	"fmt"
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
	Monitor    *string           `json:"monitor,omitempty"`
	Errors     map[string]string `json:"errors,omitempty"`
}

type ddcBriefKind int

const (
	ddcBriefContinuous ddcBriefKind = iota
	ddcBriefSNC
	ddcBriefCNC
)

type ddcBriefParsed struct {
	Kind    ddcBriefKind
	Current int
	Max     int
	SL      int
}

func normalizeDdcVcpCode(code string) string {
	c := strings.ToUpper(strings.TrimSpace(code))
	c = strings.TrimPrefix(c, "0X")
	return c
}

func parseDdcutilGetVcpBriefLine(line string) (string, ddcBriefParsed, error) {
	fields := strings.Fields(strings.TrimSpace(line))
	// Example:
	// - VCP 10 C 50 100
	// - VCP 62 CNC x00 x64 x00 x32
	// - VCP 8D SNC x01
	// - VCP 84 ERR
	if len(fields) < 3 || !strings.EqualFold(fields[0], "VCP") {
		return "", ddcBriefParsed{}, fmt.Errorf("not a VCP brief line: %q", line)
	}

	code := normalizeDdcVcpCode(fields[1])
	kind := strings.ToUpper(fields[2])

	switch kind {
	case "C":
		if len(fields) != 5 {
			return "", ddcBriefParsed{}, fmt.Errorf("unexpected continuous VCP line: %q", line)
		}
		cur, err1 := strconv.Atoi(fields[3])
		max, err2 := strconv.Atoi(fields[4])
		if err1 != nil || err2 != nil {
			return "", ddcBriefParsed{}, fmt.Errorf("parse continuous VCP values: %v %v", err1, err2)
		}
		return code, ddcBriefParsed{Kind: ddcBriefContinuous, Current: cur, Max: max}, nil
	case "SNC":
		if len(fields) != 4 {
			return "", ddcBriefParsed{}, fmt.Errorf("unexpected SNC VCP line: %q", line)
		}
		sl, err := parseDdcHexByte(fields[3])
		if err != nil {
			return "", ddcBriefParsed{}, fmt.Errorf("parse SNC value: %w", err)
		}
		return code, ddcBriefParsed{Kind: ddcBriefSNC, SL: sl}, nil
	case "CNC", "CND":
		if len(fields) != 7 {
			return "", ddcBriefParsed{}, fmt.Errorf("unexpected CNC VCP line: %q", line)
		}
		// Brief CNC order is mh ml sh sl (16-bit max, then 16-bit current).
		mh, err1 := parseDdcHexByte(fields[3])
		ml, err2 := parseDdcHexByte(fields[4])
		sh, err3 := parseDdcHexByte(fields[5])
		sl, err4 := parseDdcHexByte(fields[6])
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil {
			return "", ddcBriefParsed{}, fmt.Errorf("parse CNC bytes: %v %v %v %v", err1, err2, err3, err4)
		}
		max := (mh << 8) | ml
		cur := (sh << 8) | sl
		return code, ddcBriefParsed{Kind: ddcBriefCNC, Current: cur, Max: max, SL: sl}, nil
	case "ERR":
		return code, ddcBriefParsed{}, fmt.Errorf("VCP reported ERR")
	default:
		return "", ddcBriefParsed{}, fmt.Errorf("unrecognized brief getvcp line kind: %q (%q)", fields[2], line)
	}
}

func parseDdcutilGetVcpBriefBatch(output string) (map[string]ddcBriefParsed, map[string]string) {
	parsed := map[string]ddcBriefParsed{}
	errs := map[string]string{}

	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(line), "vcp") {
			continue
		}

		code, p, err := parseDdcutilGetVcpBriefLine(line)
		if err != nil {
			// If we can at least parse the VCP code, attribute the error to it.
			// Otherwise, fall through with an un-attributed parse error.
			if code != "" {
				errs[code] = err.Error()
				continue
			}
			continue
		}
		parsed[code] = p
	}

	return parsed, errs
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
	case 0x00:
		// FF1 reports SNC x00 for normal operation (same effective state as 0x01 "on").
		return string(DdcPowerOn), true
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

// ddcVolumePercentFromParsed maps getvcp --brief output to a 0–100 volume level.
// Continuous VCPs already report current in device units; CNC encodes 16-bit max/current.
func ddcVolumePercentFromParsed(p ddcBriefParsed) (int, error) {
	switch p.Kind {
	case ddcBriefContinuous:
		return p.Current, nil
	case ddcBriefCNC:
		// For FF1 VCP `62` (volume), the "current" word already maps to the
		// effective 0-100 volume level. Some panels/firmware versions also
		// report a `max` word with a byte order we can't rely on, so we avoid
		// scaling by `Max` here.
		if p.Current < 0 {
			return 0, nil
		}
		if p.Current > 100 {
			return 100, nil
		}
		return p.Current, nil
	default:
		return 0, fmt.Errorf("expected continuous or CNC VCP for volume, got kind %v", p.Kind)
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

func parseDdcutilDetectBriefMonitorModel(output string) (string, error) {
	// Example (conceptual):
	// Monitor:   Vendor: Model
	// We want: "<Vendor>:<Model>".
	for _, raw := range strings.Split(output, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" {
			continue
		}
		if !strings.Contains(strings.ToLower(line), "monitor:") {
			continue
		}

		fields := strings.Split(line, ":")
		// fields[0] = "Monitor"
		// fields[1] = Vendor (awk $2)
		// fields[2] = Model  (awk $3)
		if len(fields) < 3 {
			return "", fmt.Errorf("unexpected monitor line format: %q", line)
		}
		vendor := strings.TrimSpace(fields[1])
		model := strings.TrimSpace(fields[2])
		if vendor == "" || model == "" {
			return "", fmt.Errorf("empty vendor/model in monitor line: %q", line)
		}
		return vendor + ":" + model, nil
	}
	return "", fmt.Errorf("monitor line not found")
}

func (p *panelDdc) detectMonitorModel(ctx context.Context) (string, error) {
	out, err := p.exec.CommandContext(ctx, "ddcutil", "detect", "--brief").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ddcutil detect: %w", err)
	}
	return parseDdcutilDetectBriefMonitorModel(string(out))
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

	monitorModel, err := p.detectMonitorModel(ctx)
	if err != nil {
		// Model discovery is auxiliary; we don't block panel VCP status.
		errs["monitor"] = err.Error()
	} else if monitorModel != "" {
		status.Monitor = ddcStringPtr(monitorModel)
	}

	codes := make([]string, 0, len(ddcPanelStatusQueries))
	for _, q := range ddcPanelStatusQueries {
		codes = append(codes, normalizeDdcVcpCode(q.code))
	}

	out, err := p.getVCPBriefBatch(ctx, codes)
	if err != nil {
		// If ddcutil fails as a whole, attribute the same failure to each field.
		for _, q := range ddcPanelStatusQueries {
			errs[q.field] = err.Error()
		}
		status.Errors = errs
		return status, nil
	}

	parsedByCode, parseErrsByCode := parseDdcutilGetVcpBriefBatch(string(out))
	for _, q := range ddcPanelStatusQueries {
		code := normalizeDdcVcpCode(q.code)
		if msg, ok := parseErrsByCode[code]; ok {
			errs[q.field] = msg
			continue
		}

		parsed, ok := parsedByCode[code]
		if !ok {
			errs[q.field] = fmt.Sprintf("missing VCP %s", q.code)
			continue
		}

		switch q.field {
		case "brightness", "contrast":
			if parsed.Kind != ddcBriefContinuous {
				errs[q.field] = fmt.Sprintf("expected continuous VCP, got kind %v", parsed.Kind)
				continue
			}
			switch q.field {
			case "brightness":
				status.Brightness = ddcIntPtr(parsed.Current)
			case "contrast":
				status.Contrast = ddcIntPtr(parsed.Current)
			}
		case "volume":
			vol, err := ddcVolumePercentFromParsed(parsed)
			if err != nil {
				errs[q.field] = err.Error()
				continue
			}
			status.Volume = ddcIntPtr(vol)
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

func (p *panelDdc) getVCPBriefBatch(ctx context.Context, vcpCodes []string) ([]byte, error) {
	args := make([]string, 0, 4+len(vcpCodes))
	args = append(args, "--noverify", "getvcp", "--brief")
	args = append(args, vcpCodes...)
	out, err := p.execDdcutilWithDisplayRecovery(ctx, append([]string{"ddcutil"}, args...)...)
	if err != nil {
		return out, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

func (p *panelDdc) setVCP(ctx context.Context, vcpCode, value string) error {
	out, err := p.execDdcutilWithDisplayRecovery(ctx, "ddcutil", "--noverify", "setvcp", vcpCode, value)
	if err != nil {
		return fmt.Errorf("ddcutil setvcp %s %s: %s: %w", vcpCode, value, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// execDdcutilWithDisplayRecovery runs ddcutil; on any error (or missing VCP lines
// for `getvcp --brief`), runs getvcp 60 once and retries.
func (p *panelDdc) execDdcutilWithDisplayRecovery(ctx context.Context, argv ...string) ([]byte, error) {
	if len(argv) < 1 {
		return nil, fmt.Errorf("ddcutil: missing command name")
	}
	run := func() ([]byte, error) {
		return p.exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	}

	out, err := run()
	expectsVcpBriefOutput := ddcutilCommandExpectsVcpBriefOutput(argv)
	hasVcpBriefLine := ddcutilOutputHasVcpBriefLine(out)

	// Retry if:
	// - ddcutil returned an error (err != nil), or
	// - output implies display-not-found, or
	// - for getvcp --brief: output contains no leading `VCP ...` line (often means
	//   transient display/bus issues where ddcutil returned success).
	if err == nil && !ddcutilOutputImpliesDisplayNotFound(out, err) && !(expectsVcpBriefOutput && !hasVcpBriefLine) {
		return out, err
	}

	p.logger.Info("ddcutil reported error or missing VCP output; running recovery and retrying once",
		zap.Strings("ddcutil_argv", argv))

	recoverOut, recoverErr := p.exec.CommandContext(
		ctx,
		"ddcutil",
		"--noverify",
		"getvcp",
		"60",
		"--brief",
	).CombinedOutput()
	if recoverErr != nil {
		p.logger.Warn("ddcutil getvcp 60 as recovery after ddcutil error",
			zap.Error(recoverErr),
			zap.String("output", strings.TrimSpace(string(recoverOut))))
	}

	return run()
}

func ddcutilCommandExpectsVcpBriefOutput(argv []string) bool {
	// argv includes the "ddcutil" binary itself.
	sawGetvcp := false
	sawBrief := false
	for _, a := range argv {
		if strings.EqualFold(a, "getvcp") {
			sawGetvcp = true
		}
		if a == "--brief" {
			sawBrief = true
		}
	}
	return sawGetvcp && sawBrief
}

func ddcutilOutputHasVcpBriefLine(out []byte) bool {
	for _, raw := range strings.Split(string(out), "\n") {
		line := strings.TrimSpace(raw)
		if strings.HasPrefix(strings.ToLower(line), "vcp ") {
			return true
		}
		// Some `ddcutil` versions may format as `VCP\t...`
		if strings.HasPrefix(strings.ToLower(line), "vcp\t") {
			return true
		}
	}
	return false
}

func ddcutilOutputImpliesDisplayNotFound(out []byte, err error) bool {
	s := strings.ToLower(string(out))
	if err != nil {
		s += " " + strings.ToLower(err.Error())
	}
	return strings.Contains(s, "display not found")
}
