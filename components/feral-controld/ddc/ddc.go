package ddc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/drm"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// -----------------------------------------------------------------------------
// PanelDDC interface – shared by the status poller and the command executor.
// -----------------------------------------------------------------------------

// PanelDDC's gating contract: CollectStatus and ApplyControl are ALWAYS real
// attempts — an explicit request (cloud command, sleep transition) is real
// intent and keeps its pre-tracker semantics (CollectStatus always returns a
// status object, with per-field Errors on failure). Only the periodic status
// poller is expected to consult ShouldPoll first, so a display that does not
// speak DDC/CI stops costing ddcutil subprocesses every 5s without changing
// any command-facing behavior.
//
//go:generate mockgen -source=ddc.go -destination=../mocks/ddc.go -package=mocks -mock_names=PanelDDC=MockPanelDDC
type PanelDDC interface {
	CollectStatus(ctx context.Context) (*DdcPanelStatus, error)
	ApplyControl(ctx context.Context, action DdcPanelAction, value json.RawMessage) error
	// ShouldPoll reports whether a background status poll is currently worth
	// running. False while the availability tracker judges the display
	// DDC/CI-incapable and the next reprobe window has not arrived. Also
	// refreshes the display fingerprint, so a plugged/swapped display
	// re-opens polling on the very next tick.
	ShouldPoll() bool
	// Generation identifies the attached-display generation; it increments
	// whenever the DRM fingerprint changes (plug, unplug, swap) AND whenever
	// the panel recovers from an "unavailable" verdict without a fingerprint
	// change (a monitor power-toggled while its HPD/EDID stayed up). Consumers
	// that cache their own per-display give-up state (e.g. the sleep panel
	// leg's retry cap) compare generations so either kind of display comeback
	// re-arms them. Calling it also refreshes the fingerprint.
	Generation() uint64
}

// New returns a PanelDDC that drives the default ddcutil display.
// Requires RW /dev/i2c-* (udev/i2c group).
func New(exec wrapper.Exec, clock wrapper.Clock, logger *zap.Logger) PanelDDC {
	return &panelDdc{
		exec:          exec,
		clock:         clock,
		logger:        logger,
		fingerprintFn: func() string { return drm.Fingerprint(drm.DefaultSysfsRoot) },
	}
}

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

// DdcPanelStatus is returned by CollectStatus. Omitted fields mean that feature
// could not be read. Errors maps field name to message.
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
			return "", ddcBriefParsed{}, fmt.Errorf("parse continuous VCP values: %w", errors.Join(err1, err2))
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
			return "", ddcBriefParsed{}, fmt.Errorf("parse CNC bytes: %w", errors.Join(err1, err2, err3, err4))
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

// ddcProbeFailThreshold is how many hard "no DDC/CI display" failures
// (without an intervening success — generic errors neither count nor reset)
// it takes to mark the panel unsupported. Three attempts spaced by
// the 5s status poll (~15s window) absorb the common case of DDC coming
// alive a few seconds after hotplug/power-on, so a slow-waking panel is not
// misjudged.
const ddcProbeFailThreshold = 3

// ddcRecoveryFutileThreshold stops the recovery-and-retry path for the
// current display generation after this many CONSECUTIVE poll rounds where
// recovery was attempted and did NOT rescue the read. The recovery exists for
// transient bus/display conditions; a display that ALWAYS needs it and never
// benefits (e.g. partial DDC support that returns non-zero exit with usable
// VCP lines) would otherwise cost 3 ddcutil subprocesses plus an Info log
// every 5s round, forever. A clean initial read, a rescuing recovery, or a
// display change re-enables recovery. Suppression only applies while failed
// reads still yield usable VCP lines (see getVCPBriefBatch): a totally-failed
// read is a different situation than the one the verdict was learned from and
// always gets the rescue attempt.
const ddcRecoveryFutileThreshold = 3

// ddcReprobeInterval bounds how long an "unsupported" verdict is trusted
// without re-checking, for a display generation that has NEVER answered a
// DDC read. A display's DDC/CI can start working with NO hotplug event at
// all (the user toggles DDC/CI in the monitor's OSD menu), so a purely
// event-driven reset would miss it; a slow periodic probe is the backstop.
// Display changes (plug/unplug/swap) reset immediately via the DRM
// fingerprint instead.
const ddcReprobeInterval = 10 * time.Minute

// ddcReprobeIntervalProven is the much shorter lease for a display generation
// that HAS answered DDC reads before. A hard "no DDC/CI" from such a panel is
// almost never a capability change — the dominant field case is the monitor
// manually powered off while its DRM connector keeps reading "connected"
// (cached EDID, no fingerprint change). The verdict must therefore be
// re-checked on a human timescale so the panel is picked up within ~half a
// minute of being powered back on, not up to ten minutes later. Cost while
// the monitor stays off: one probe (a few ddcutil subprocesses) per interval.
const ddcReprobeIntervalProven = 30 * time.Second

type panelDdc struct {
	exec   wrapper.Exec
	clock  wrapper.Clock
	logger *zap.Logger

	// fingerprintFn snapshots the attached-display identity (DRM connector
	// statuses + EDID hashes). Injected so tests can drive display changes.
	fingerprintFn func() string

	// The availability tracker. It exists to stop the 5s status poll from
	// shelling out to ddcutil forever on displays that do not speak DDC/CI
	// (common on TVs, or monitors with DDC/CI disabled in the OSD), while
	// still recovering automatically when the display situation changes.
	//
	// Demotion to unsupported requires ddcProbeFailThreshold hard-signature
	// failures with no intervening success. Generic errors (I2C timeouts,
	// ERR VCPs, partial reads) NEVER demote — a display that keeps serving
	// even one readable VCP must keep its 5s status updates flowing, and a
	// display wedged in a generic-failure state (e.g. DDC dead in standby)
	// must not earn a verdict that would blank status after it wakes. The
	// recovery-futility mechanism below keeps such displays cheap instead.
	// Neither CollectStatus nor ApplyControl is gated — explicit requests
	// always get a real attempt; their outcomes feed the tracker, which is
	// also what makes a panel whose DDC dies in standby self-heal: the
	// wake-up setvcp that succeeds promotes the tracker back. Only ShouldPoll
	// consults the verdict, so suppression is strictly a background-poll
	// concern.
	//
	// mu is never held across a ddcutil subprocess: lock to decide, unlock
	// to run, lock to record. Concurrent probes are harmless.
	mu          sync.Mutex
	unsupported bool
	failStreak  int
	nextProbeAt time.Time
	fingerprint string
	generation  uint64
	// everSucceeded remembers whether THIS display generation has ever
	// answered a DDC read/write. It selects the reprobe lease when the
	// tracker demotes: a proven panel gets ddcReprobeIntervalProven (its
	// hard failures are almost always "monitor powered off", a temporary
	// state), an unproven one the slow ddcReprobeInterval (genuinely
	// DDC-less TVs must stay cheap).
	everSucceeded bool
	// Recovery-futility tracking (see ddcRecoveryFutileThreshold): once
	// recoveryFutile is set, poll-path getvcp runs single-shot until a clean
	// initial read or a display change re-enables recovery. setVCP (explicit
	// intent) always keeps its recovery path.
	recoveryFutileStreak int
	recoveryFutile       bool
}

// refreshFingerprintLocked resets the tracker when the attached-display
// identity changed (plug, unplug, swap). Requires p.mu held.
func (p *panelDdc) refreshFingerprintLocked(fp string) {
	if fp == p.fingerprint {
		return
	}
	if p.fingerprint != "" {
		p.logger.Info("Display fingerprint changed; resetting DDC availability",
			zap.String("fingerprint", fp))
	}
	p.fingerprint = fp
	p.generation++
	p.unsupported = false
	p.failStreak = 0
	p.nextProbeAt = time.Time{}
	p.everSucceeded = false
	p.recoveryFutileStreak = 0
	p.recoveryFutile = false
}

// syncFingerprint refreshes the display fingerprint outside any other lock
// hold. Every public entry point calls it so a display change is noticed no
// matter which path touches the panel first.
func (p *panelDdc) syncFingerprint() {
	fp := p.fingerprintFn()
	p.mu.Lock()
	p.refreshFingerprintLocked(fp)
	p.mu.Unlock()
}

// ShouldPoll implements the poller's gate; see the interface contract.
func (p *panelDdc) ShouldPoll() bool {
	fp := p.fingerprintFn()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshFingerprintLocked(fp)

	return !p.unsupported || !p.clock.Now().Before(p.nextProbeAt)
}

// Generation implements the display-generation probe; see the interface
// contract.
func (p *panelDdc) Generation() uint64 {
	fp := p.fingerprintFn()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.refreshFingerprintLocked(fp)
	return p.generation
}

// recordOutcome feeds one ddcutil result back into the tracker. hardFail is
// true only for the definitive "no DDC/CI capable display" signature.
func (p *panelDdc) recordOutcome(success, hardFail bool) {
	p.mu.Lock()
	defer p.mu.Unlock()

	switch {
	case success:
		if p.unsupported {
			p.logger.Info("DDC panel is responding again; resuming status polls")
			// Coming back from an "unavailable" verdict is a display comeback
			// even when the DRM fingerprint never moved (monitor power-toggled
			// with HPD/EDID held up — the fingerprint cannot see it). Bump the
			// generation so Generation()-keyed give-up state (the sleep panel
			// leg's retry cap) re-arms and the scheduled power state is
			// re-driven onto the returned panel; tracker state itself is NOT
			// reset here (everSucceeded must survive so the next off period
			// keeps the short proven-panel reprobe lease).
			p.generation++
		}
		p.unsupported = false
		p.failStreak = 0
		p.nextProbeAt = time.Time{}
		p.everSucceeded = true
	case p.unsupported:
		// Any failed attempt while unsupported — hard OR generic — arms the
		// next reprobe window. Without this, a reprobe that failed with a
		// non-hard error would leave nextProbeAt in the past and reopen
		// continuous polling, resurrecting the exact flood the verdict
		// suppresses. The verdict itself only ever lifts on a success.
		p.nextProbeAt = p.clock.Now().Add(p.reprobeDelayLocked())
	case hardFail:
		p.failStreak++
		p.logger.Info("DDC probe found no DDC/CI capable display",
			zap.Int("consecutive_failures", p.failStreak),
			zap.Int("threshold", ddcProbeFailThreshold))
		if p.failStreak >= ddcProbeFailThreshold {
			p.unsupported = true
			p.nextProbeAt = p.clock.Now().Add(p.reprobeDelayLocked())
			p.logger.Info("Marking DDC panel unavailable; suspending status polls",
				zap.Duration("reprobe_interval", p.reprobeDelayLocked()),
				zap.Bool("panel_proven_before", p.everSucceeded))
		}
	}
	// Generic failures while not (yet) unsupported: leave the state alone.
	// They are transient I2C/panel conditions and must neither demote nor
	// break the consecutive-hard-failure accounting into false resets.
	// Persistently-generic displays are handled by recovery futility, not
	// by demotion (their readable VCPs must keep updating every poll).
}

// reprobeDelayLocked selects the "unsupported" reprobe lease for the current
// display generation: short for a panel that has already proven it speaks
// DDC/CI (its hard failures are near-certainly a monitor power-off, which
// must be picked back up quickly), slow for one that never has (genuinely
// DDC-less displays must stay cheap). Requires p.mu held.
func (p *panelDdc) reprobeDelayLocked() time.Duration {
	if p.everSucceeded {
		return ddcReprobeIntervalProven
	}
	return ddcReprobeInterval
}

// ApplyControl runs setvcp for the resolved action/value pair.
//
// Deliberately NOT gated by the availability tracker: a control request is
// explicit intent (user slider, sleep transition) and always deserves a real
// attempt — and an ungated ApplyControl is the escape hatch that recovers a
// tracker wrongly demoted while the panel's DDC was asleep. Its outcome
// still feeds the tracker both ways.
func (p *panelDdc) ApplyControl(ctx context.Context, action DdcPanelAction, value json.RawMessage) error {
	code, val, err := resolveDdcSetVCP(action, value)
	if err != nil {
		return err
	}

	p.syncFingerprint()

	p.logger.Info("ddcutil setvcp",
		zap.String("action", string(action)),
		zap.String("vcp", code),
		zap.String("value", val))
	if err := p.setVCP(ctx, code, val); err != nil {
		// setVCP embeds ddcutil's combined output in the error text, so the
		// hard-signature check works on the error alone.
		p.recordOutcome(false, ddcutilOutputImpliesNoDdcDisplay(nil, err))
		p.logger.Error("ddcutil setvcp failed", zap.Error(err))
		return err
	}
	p.recordOutcome(true, false)
	return nil
}

// CollectStatus queries brightness, contrast, volume, mute, and power VCPs.
//
// Never gated: it always runs the pre-tracker collection flow (detect, then
// the VCP batch with the recovery retry) and always returns a status object,
// with per-field Errors on failure — the wire contract cloud commands have
// always seen. The availability tracker only observes the outcome; background
// suppression happens in the poller via ShouldPoll.
func (p *panelDdc) CollectStatus(ctx context.Context) (*DdcPanelStatus, error) {
	p.syncFingerprint()

	panelStatus := &DdcPanelStatus{}
	errs := map[string]string{}

	monitorModel, err := p.detectMonitorModel(ctx)
	if err != nil {
		// Model discovery is auxiliary; we don't block panel VCP status.
		errs["monitor"] = err.Error()
	} else if monitorModel != "" {
		panelStatus.Monitor = ddcStringPtr(monitorModel)
	}

	codes := make([]string, 0, len(ddcPanelStatusQueries))
	for _, q := range ddcPanelStatusQueries {
		codes = append(codes, normalizeDdcVcpCode(q.code))
	}

	out, err := p.getVCPBriefBatch(ctx, codes)
	if err != nil {
		p.recordOutcome(false, ddcutilOutputImpliesNoDdcDisplay(out, err))
		// If ddcutil fails as a whole, attribute the same failure to each field.
		for _, q := range ddcPanelStatusQueries {
			errs[q.field] = err.Error()
		}
		panelStatus.Errors = errs
		return panelStatus, nil
	}

	parsedByCode, parseErrsByCode := parseDdcutilGetVcpBriefBatch(string(out))
	// At least one readable VCP proves the display speaks DDC/CI; a line-less
	// success is a transient condition and must not demote (generic outcome).
	p.recordOutcome(len(parsedByCode) > 0, false)
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
				panelStatus.Brightness = ddcIntPtr(parsed.Current)
			case "contrast":
				panelStatus.Contrast = ddcIntPtr(parsed.Current)
			}
		case "volume":
			vol, err := ddcVolumePercentFromParsed(parsed)
			if err != nil {
				errs[q.field] = err.Error()
				continue
			}
			panelStatus.Volume = ddcIntPtr(vol)
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
			panelStatus.Mute = ddcStringPtr(s)
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
			panelStatus.Power = ddcStringPtr(s)
		}
	}

	if len(errs) > 0 {
		panelStatus.Errors = errs
	}
	return panelStatus, nil
}

// ddcutilGetvcpNeedsRescue reports whether a getvcp --brief outcome is the
// kind the recovery path retries: an error, a display-not-found hint, or
// output without a single VCP line.
func ddcutilGetvcpNeedsRescue(out []byte, err error) bool {
	return err != nil ||
		ddcutilOutputImpliesDisplayNotFound(out, err) ||
		!ddcutilOutputHasVcpBriefLine(out)
}

// getVCPBriefBatch reads the poll VCPs with recovery-futility learning.
//
// Some displays ALWAYS need the recovery path and never benefit from it —
// field case: partial DDC support where every getvcp exits non-zero yet
// still emits usable VCP lines for the codes that do work. Pre-learning that
// cost 3 ddcutil subprocesses plus an Info log every 5s round, forever.
// After ddcRecoveryFutileThreshold consecutive futile recoveries the poll
// path goes single-shot at Debug; the readable VCPs keep flowing every round
// because the partial output still parses. A clean initial read, a rescuing
// recovery, or a display change (fingerprint reset) re-enables recovery for
// genuine transients, and suppression never applies to totally-failed reads
// (no VCP lines) — those always get the rescue attempt.
// setVCP keeps its recovery path unconditionally (explicit intent).
func (p *panelDdc) getVCPBriefBatch(ctx context.Context, vcpCodes []string) ([]byte, error) {
	args := make([]string, 0, 4+len(vcpCodes))
	args = append(args, "--noverify", "getvcp", "--brief")
	args = append(args, vcpCodes...)
	argv := append([]string{"ddcutil"}, args...)
	run := func() ([]byte, error) {
		return p.exec.CommandContext(ctx, argv[0], argv[1:]...).CombinedOutput()
	}

	out, err := run()
	if !ddcutilGetvcpNeedsRescue(out, err) {
		p.noteCleanInitialRead()
		return out, nil
	}

	// Futility suppression only holds while the failed initial read still
	// yields at least one usable VCP line — the partial-support shape the
	// verdict was learned from, where the read is already parseable and
	// recovery adds nothing. A read with NO VCP lines is a different
	// situation (field case: monitor power-cycled with a stable DRM
	// fingerprint, its DDC asleep until the wake poke), and skipping the
	// rescue there deadlocks status forever: the clean initial read that
	// would lift the latch may be impossible without the poke.
	if p.recoverySuppressed() && ddcutilOutputHasVcpBriefLine(out) {
		p.logger.Debug("ddcutil getvcp needs rescue; recovery suppressed as futile for this display",
			zap.Strings("ddcutil_argv", argv))
	} else {
		// Reprobes of a panel already judged unavailable fail here every lease
		// window for as long as the monitor stays powered off — expected, so
		// Debug; anything else is news and stays at Info.
		logAt := p.logger.Info
		if p.knownUnavailable() {
			logAt = p.logger.Debug
		}
		logAt("ddcutil reported error or missing VCP output; running recovery and retrying once",
			zap.Strings("ddcutil_argv", argv))
		p.runDdcRecoveryPoke(ctx)
		out, err = run()
		p.noteRecoveryAttempt(!ddcutilGetvcpNeedsRescue(out, err))
	}

	// Some panels return non-zero when one VCP reports ERR but still emit usable
	// VCP lines for others. In that case we want to keep parsing instead of
	// failing the whole batch, as long as we see at least one VCP brief line.
	if err != nil && !ddcutilOutputHasVcpBriefLine(out) {
		return out, fmt.Errorf("%s: %w", strings.TrimSpace(string(out)), err)
	}
	return out, nil
}

// recoverySuppressed reports whether the poll path should skip recovery.
func (p *panelDdc) recoverySuppressed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.recoveryFutile
}

// knownUnavailable reports whether the tracker currently holds the
// "unsupported" verdict. Used to pick log levels: a failing read against a
// panel already judged unavailable (the periodic reprobe of a powered-off
// monitor) is the expected state, not news, and must not emit Info/Warn lines
// every reprobe forever.
func (p *panelDdc) knownUnavailable() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.unsupported
}

// noteCleanInitialRead re-enables recovery: the display answered without
// help, so a future failure is transient-shaped again and worth a rescue.
func (p *panelDdc) noteCleanInitialRead() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.recoveryFutile {
		p.logger.Info("DDC reads are clean again; re-enabling the recovery path")
	}
	p.recoveryFutile = false
	p.recoveryFutileStreak = 0
}

// noteRecoveryAttempt records whether a recovery round actually rescued the
// read, and suppresses future recoveries once it has been consecutively
// futile ddcRecoveryFutileThreshold times. A rescue also lifts an existing
// futility verdict: recovery demonstrably helps this display again (e.g. the
// wake poke after a monitor power cycle), so the learned give-up no longer
// describes it.
func (p *panelDdc) noteRecoveryAttempt(rescued bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if rescued {
		if p.recoveryFutile {
			p.logger.Info("DDC recovery rescued a failing read; re-enabling the recovery path")
		}
		p.recoveryFutile = false
		p.recoveryFutileStreak = 0
		return
	}
	p.recoveryFutileStreak++
	if !p.recoveryFutile && p.recoveryFutileStreak >= ddcRecoveryFutileThreshold {
		p.recoveryFutile = true
		p.logger.Info("DDC recovery has not helped; switching to single-shot reads for this display",
			zap.Int("futile_recoveries", p.recoveryFutileStreak),
			zap.String("re_enabled_by", "a clean read, a rescuing recovery, or a display change"))
	}
}

func (p *panelDdc) setVCP(ctx context.Context, vcpCode, value string) error {
	out, err := p.execDdcutilWithDisplayRecovery(ctx, "ddcutil", "--noverify", "setvcp", "--sleep-multiplier", "1.5", vcpCode, value)
	if err != nil {
		return fmt.Errorf("ddcutil setvcp %s %s: %s: %w", vcpCode, value, strings.TrimSpace(string(out)), err)
	}
	return nil
}

// runDdcRecoveryPoke issues the `getvcp 60` read whose side effect wakes
// some panels' DDC out of a transient wedge; its own failure is only logged.
func (p *panelDdc) runDdcRecoveryPoke(ctx context.Context) {
	recoverOut, recoverErr := p.exec.CommandContext(
		ctx,
		"ddcutil",
		"--noverify",
		"getvcp",
		"60",
		"--brief",
	).CombinedOutput()
	if recoverErr != nil {
		// Same taxonomy as the recovery-attempt log above: a failing poke
		// against a panel already judged unavailable repeats every reprobe
		// while the monitor is off, so it drops to Debug.
		logAt := p.logger.Warn
		if p.knownUnavailable() {
			logAt = p.logger.Debug
		}
		logAt("ddcutil getvcp 60 as recovery after ddcutil error",
			zap.Error(recoverErr),
			zap.String("output", strings.TrimSpace(string(recoverOut))))
	}
}

// execDdcutilWithDisplayRecovery runs ddcutil; on any error (or missing VCP lines
// for `getvcp --brief`), runs getvcp 60 once and retries. Used by setVCP —
// explicit intent always keeps the recovery path (no futility suppression).
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
	if err == nil && !ddcutilOutputImpliesDisplayNotFound(out, err) && (!expectsVcpBriefOutput || hasVcpBriefLine) {
		return out, err
	}

	// Known-unavailable panels (monitor powered off) fail every attempt until
	// they come back; the intermediate recovery line is expected there and
	// drops to Debug. The caller still surfaces the final failure.
	logAt := p.logger.Info
	if p.knownUnavailable() {
		logAt = p.logger.Debug
	}
	logAt("ddcutil reported error or missing VCP output; running recovery and retrying once",
		zap.Strings("ddcutil_argv", argv))

	p.runDdcRecoveryPoke(ctx)

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

// ddcutilCombinedText lowercases ddcutil's output plus, when present, an
// error string that may embed it (e.g. setVCP's wrapped error), for
// signature matching.
func ddcutilCombinedText(out []byte, err error) string {
	s := strings.ToLower(string(out))
	if err != nil {
		s += " " + strings.ToLower(err.Error())
	}
	return s
}

func ddcutilOutputImpliesDisplayNotFound(out []byte, err error) bool {
	return strings.Contains(ddcutilCombinedText(out, err), "display not found")
}

// ddcutilOutputImpliesNoDdcDisplay reports whether ddcutil's output (or an
// error string that embeds it, e.g. from setVCP) carries the definitive
// "there is no DDC/CI capable display here" signature. Only this signature
// may demote the availability tracker; generic I2C errors and timeouts must
// not. Deliberately EXCLUDES "display not found": execDdcutilWithDisplayRecovery
// classifies that string as transient and worth a recovery retry (brief HPD
// blip, DP link retrain), and the tracker must not treat as fatal what the
// retry layer treats as recoverable.
//
// Signature provenance: observed verbatim from ddcutil on FF1 field hardware
// ("No displays implementing DDC/CI found", headless-device logs, 2026-07).
// If a ddcutil upgrade rewords it, the failure mode is fail-OPEN: demotion
// stops matching and polling continues (with its logging), rather than a
// healthy panel being wrongly suppressed.
func ddcutilOutputImpliesNoDdcDisplay(out []byte, err error) bool {
	return strings.Contains(ddcutilCombinedText(out, err), "no displays implementing ddc/ci")
}
