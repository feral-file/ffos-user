package config

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"

	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// macRegex validates MAC address format (XX:XX:XX:XX:XX:XX where X is hex digit)
var macRegex = regexp.MustCompile(`^([0-9a-fA-F]{2}:){5}[0-9a-fA-F]{2}$`)

type CDPConfig struct {
	Endpoint string `json:"endpoint"`
}

type RelayerConfig struct {
	Endpoint string `json:"endpoint"`
	APIKey   string `json:"apiKey"`
}

type MintPairingConfig struct {
	Enabled                bool   `json:"enabled"`
	BrokerBaseURL          string `json:"brokerBaseURL"`
	IdleTTLSeconds         int    `json:"idleTTLSeconds"`
	PollIntervalMillis     int    `json:"pollIntervalMillis"`
	ApprovalTimeoutSeconds int    `json:"approvalTimeoutSeconds"`
}

// CommandStormConfig tunes device-side command-storm protection in the command
// router. All fields are optional; an absent section keeps the built-in
// defaults, which are safe with zero configuration.
type CommandStormConfig struct {
	// Disabled turns the storm gate off entirely. Default (false) keeps it on.
	Disabled bool `json:"disabled"`
	// MaxConcurrent overrides the global in-flight command budget when > 0.
	MaxConcurrent int64 `json:"maxConcurrent"`
}

// OfflineCacheConfig tunes the offlinecache package (see
// components/feral-controld/offlinecache and docs/offline-artwork-capture.md).
// All fields besides Enabled are optional; zero/empty values fall back to
// offlinecache.OptionsFromConfig's built-in defaults, which are safe with
// zero configuration beyond Enabled=true. The feature defaults OFF: it
// spawns a second Chromium process and opens a second CDP connection to
// the kiosk (see offlinecache.KioskReplay's doc on that risk), so it must
// be explicitly opted into per the plan's "Open risks".
type OfflineCacheConfig struct {
	Enabled bool `json:"enabled"`
	// RootDir is the on-disk store root (blobs/items/playlists).
	RootDir string `json:"rootDir,omitempty"`
	// MaxDiskBytes bounds total store size. Unlike most fields here,
	// <=0 does NOT mean "unlimited" — offlinecache.OptionsFromConfig
	// deliberately treats an unset/non-positive value as "use
	// offlinecache.DefaultMaxDiskBytes" instead, since this feature
	// caches potentially gigabyte-scale software-artwork assets and a
	// config that merely omits this field must not silently let the
	// cache grow unbounded on a disk-constrained device.
	MaxDiskBytes int64 `json:"maxDiskBytes,omitempty"`
	// CaptureWindowMs bounds how long one capture observes network
	// activity before finalizing; <=0 uses the capturer's own default.
	CaptureWindowMs int `json:"captureWindowMs,omitempty"`
	// HeadlessBinaryPath/HeadlessUserDataDir/HeadlessDebugPort configure
	// the separate headless Chromium capture uses (distinct from the
	// kiosk at CDPConfig.Endpoint).
	HeadlessBinaryPath          string `json:"headlessBinaryPath,omitempty"`
	HeadlessUserDataDir         string `json:"headlessUserDataDir,omitempty"`
	HeadlessDebugPort           int    `json:"headlessDebugPort,omitempty"`
	HeadlessIdleTeardownSeconds int    `json:"headlessIdleTeardownSeconds,omitempty"`
	// StaticServerAddr is the loopback host:port serving large (>200MB)
	// cached assets that exceed the CDP Fetch.fulfillRequest body ceiling.
	StaticServerAddr string `json:"staticServerAddr,omitempty"`
	// MissPolicy is "fail_closed" (default) or "pass_through"; see
	// offlinecache.MissPolicy's doc.
	MissPolicy string `json:"missPolicy,omitempty"`
	// ResourceGate tunes the resource-aware admission gate in front of the
	// capture worker (offlinecache/admission.go). An absent section keeps
	// the gate ON with built-in defaults — it exists to protect the kiosk
	// Chromium from capture-induced memory/thermal pressure, so like the
	// storm gate it must not require configuration to be effective.
	ResourceGate *OfflineCacheResourceGateConfig `json:"resourceGate,omitempty"`
	// HeadlessLimits caps the headless capture Chromium's CPU and memory
	// via a transient systemd scope (offlinecache/downloader.go's
	// HeadlessLimits). An absent section keeps the cap ON with built-in
	// defaults, for the same protect-by-default reason as ResourceGate.
	HeadlessLimits *OfflineCacheHeadlessLimitsConfig `json:"headlessLimits,omitempty"`
}

// OfflineCacheHeadlessLimitsConfig tunes the resource cap applied to the
// headless capture Chromium. Non-positive/absent values fall back to the
// offlinecache.DefaultHeadless* constants; Disabled removes the cap
// entirely (plain spawn). The cap degrades to a plain spawn on its own
// when transient systemd scopes are unavailable, so misconfiguration can
// slow captures but never block them.
type OfflineCacheHeadlessLimitsConfig struct {
	Disabled bool `json:"disabled"`
	// CPUQuotaPercent caps total CPU cycles (systemd CPUQuota; 100 = one
	// full CPU).
	CPUQuotaPercent int `json:"cpuQuotaPercent,omitempty"`
	// AllowedCPUs pins the capture Chromium to a CPU subset (systemd
	// AllowedCPUs syntax, e.g. "0-3"), bounding worst-case package power
	// draw. Default: the first quarter of the machine's logical CPUs.
	AllowedCPUs string `json:"allowedCpus,omitempty"`
	// MemoryMaxBytes is the cgroup memory ceiling; exceeding it OOM-kills
	// the capture Chromium (the job fails cleanly), not the kiosk.
	MemoryMaxBytes int64 `json:"memoryMaxBytes,omitempty"`
}

// OfflineCacheResourceGateConfig tunes when the offline cache defers
// starting downloads because the device is under resource pressure. All
// thresholds are optional; non-positive/absent values fall back to the
// offlinecache.Default* admission constants (which are anchored below the
// watchdog's and firmware's own protection thresholds — see
// offlinecache/admission.go). Follows CommandStormConfig's optional-
// pointer-section + Disabled opt-out convention.
type OfflineCacheResourceGateConfig struct {
	// Disabled turns the admission gate off entirely (downloads start
	// unconditionally, the pre-gate behavior). Default (false) keeps it on.
	Disabled bool `json:"disabled"`
	// Software* gate items captured via the headless Chromium; Media*
	// gate browser-free direct downloads. Values are used-memory percent
	// and CPU °C above which new downloads of that class defer.
	SoftwareMaxMemoryPercent float64 `json:"softwareMaxMemoryPercent,omitempty"`
	SoftwareMaxCPUTempC      float64 `json:"softwareMaxCpuTempC,omitempty"`
	MediaMaxMemoryPercent    float64 `json:"mediaMaxMemoryPercent,omitempty"`
	MediaMaxCPUTempC         float64 `json:"mediaMaxCpuTempC,omitempty"`
	// MemorySafetyCeilingPercent is the projected-usage line a software
	// capture's worst case must stay under. The effective software memory
	// threshold is the stricter of SoftwareMaxMemoryPercent and
	// (this ceiling - headlessLimits.memoryMaxBytes as a percentage of
	// this device's RAM), which is what keeps the gate and the headless
	// cap aligned on any RAM size — see offlinecache's
	// AdmissionPolicy.SoftwareReserveBytes.
	MemorySafetyCeilingPercent float64 `json:"memorySafetyCeilingPercent,omitempty"`
	// MetricsStaleAfterSeconds bounds how old the last sysmetrics sample
	// may be before the gate fails open (admits unconditionally).
	MetricsStaleAfterSeconds int `json:"metricsStaleAfterSeconds,omitempty"`
	// MaxDeferSeconds is ACCEPTED BUT INERT. Deferral no longer has a
	// deadline for any class: a download waits for the device to recover
	// instead of failing on a timer, and leaves the queue only by being
	// processed or by an explicit clear (see dequeueAdmitted in
	// offlinecache/service.go). Setting it logs a warning rather than
	// being silently ignored; the field is kept so an existing config
	// still parses and so that warning has something to name.
	MaxDeferSeconds int `json:"maxDeferSeconds,omitempty"`
}

// NetlogConfig tunes the WAN-outage flight recorder
// (components/feral-controld/netlog, docs/wan-outage-observability.md).
// Follows the optional-pointer-section + Disabled convention: an absent
// section keeps the recorder ON with built-in defaults — it exists to
// diagnose devices nobody can reach, so it must not require configuration
// to be effective. The one exception is SelfUploadAPIKey: the stage-2a
// automatic upload stays OFF until an operator provisions the support-logs
// API key, because the device ships with no credential for that API (the
// uploadLogs command receives its key from the controller per call).
type NetlogConfig struct {
	// Disabled turns the recorder (ring + ladder + lastOutage) off entirely.
	Disabled bool `json:"disabled"`
	// Dir overrides the ring location (default netlog.DefaultDir). A
	// relocated ring still rides uploadLogs bundles: the wiring hands the
	// effective directory to the log uploader (SetNetlogRingDir).
	Dir string `json:"dir,omitempty"`
	// MaxTotalBytes overrides the ring's hard total size cap (default
	// 8 MiB; values below netlog.MinTotalBytes are raised to it with a warn —
	// segments must hold the largest record). The cap must stay far below
	// uploadLogs' 128 MB bundle budget.
	MaxTotalBytes int64 `json:"maxTotalBytes,omitempty"`
	// SelfUploadAPIKey is the support-logs API key the reconnect-stability
	// self-upload authenticates with. Empty = self-upload disabled (the
	// recorder still records; uploadLogs still bundles the ring on demand).
	SelfUploadAPIKey string `json:"selfUploadApiKey,omitempty"`
}

// Configuration for all components
type Config struct {
	CDPConfig         *CDPConfig           `json:"cdp"`
	RelayerConfig     *RelayerConfig       `json:"relayer"`
	MintPairingConfig *MintPairingConfig   `json:"mintPairing"`
	SentryConfig      *logger.SentryConfig `json:"sentry"`
	// EnableHub gates the LAN hub. It is a pointer so an absent key can default
	// ON: the hub is the BLE-replacement recovery channel, so it must run
	// unless an operator explicitly sets "enableHub": false. Read via
	// HubEnabled(), never directly.
	EnableHub    *bool               `json:"enableHub"`
	CommandStorm *CommandStormConfig `json:"commandStorm,omitempty"`
	OfflineCache *OfflineCacheConfig `json:"offlineCache,omitempty"`
	Netlog       *NetlogConfig       `json:"netlog,omitempty"`
	// GatewayUserAgent scopes the kiosk User-Agent rewrite (see the
	// uarewrite package), carried as RAW bytes and decoded permissively by
	// GatewayUserAgentTuning() — same treatment, and the same reason, as
	// Provisioning below.
	//
	// Absent means ON with built-in defaults, unlike OfflineCache above:
	// this is a fix for artworks that otherwise never render, not an opt-in
	// feature, so a device that was never reconfigured must still get it.
	//
	// RawMessage is load-bearing, not a style choice. As a typed struct,
	// valid JSON carrying one wrong-typed value — `{"hosts": "ipfs.io"}`
	// instead of `["ipfs.io"]` — failed the top-level unmarshal, which made
	// config.Load fail, which is FATAL. Under Restart=always that is an
	// unbounded crash loop (the unit sets StartLimitIntervalSec=0 precisely
	// so it never latches dead), and it takes the LAN hub, captive portal,
	// provisioning, and claiming down with it — every remote recovery
	// surface at once, over a typo in an optional block whose whole purpose
	// is to be hand-edited when a new hostile gateway appears.
	GatewayUserAgent json.RawMessage `json:"gatewayUserAgent,omitempty"`

	// Provisioning carries the escape-policy tuning block
	// (docs/network-recovery-ux.md §4.1/§4.2) as RAW bytes, decoded
	// permissively by ProvisioningTuning(). RawMessage on purpose: config.Load
	// failing is FATAL under Restart=always, so a typo'd provisioning block
	// must never crash-loop the daemon and take down every recovery surface at
	// once — a syntactically valid but wrong-typed block logs and falls back
	// to defaults, while a SYNTAX error anywhere in the file still fails the
	// whole parse exactly as today (operator guidance: validate the JSON
	// before restarting). Existing keys keep their existing strict behavior.
	Provisioning json.RawMessage `json:"provisioning,omitempty"`

	// MACInfo contains MAC addresses for all network interfaces
	// e.g., map[string]string{"enp1s0":"aa:bb:cc:dd:ee:ff","wlp2s0":"11:22:33:44:55:66"}
	MACInfo map[string]string `json:"-"`
}

// HubEnabled reports whether the LAN hub should run. It defaults ON: only an
// explicit "enableHub": false disables it.
func (c *Config) HubEnabled() bool {
	return c.EnableHub == nil || *c.EnableHub
}

// GatewayUserAgentConfig scopes the kiosk User-Agent rewrite that keeps
// artworks on bot-challenging origins renderable (feral-file/ffos-user#296,
// and the uarewrite package's own doc for the mechanism).
//
// Hosts is the whole safety story of this feature. The rewrite is NOT
// applied globally on purpose: an origin that rejects unrecognized agents
// would start failing artworks that render today, and that regression would
// be far harder to attribute than the bug being fixed. Keeping the scope in
// config means a newly hostile gateway is an operator edit rather than a
// daemon release.
type GatewayUserAgentConfig struct {
	// Enabled gates the rewrite. Pointer so an absent key defaults ON —
	// only an explicit "enabled": false turns it off. Read via
	// IsEnabled(), never directly: it is nil-safe on both the section and
	// the field, which is what lets an absent block resolve to ON.
	Enabled *bool `json:"enabled"`
	// UserAgent replaces Chromium's own on matching requests. Empty uses
	// uarewrite.DefaultUserAgent. Do NOT set this to a browser UA: a
	// browser UA is precisely what the mitigation layer challenges.
	UserAgent string `json:"userAgent,omitempty"`
	// Hosts are the bare origins to rewrite for (scheme and port are
	// tolerated and stripped). Empty uses uarewrite.DefaultHosts.
	// Matching is exact per host — subdomains must be listed explicitly.
	//
	// An unusable entry (a wildcard such as "*.ipfs.io" being the likely
	// spelling) is DROPPED and named in an Error log; the rest of the list
	// still applies, so appending a typo'd gateway cannot revoke the hosts
	// that were already working. Only when NO entry survives does this fall
	// back to the defaults — the same landing point an unreadable block
	// gets. See uarewrite.NewFromOperatorHosts.
	Hosts []string `json:"hosts,omitempty"`
}

// IsEnabled reports whether the rewrite should run. It defaults ON: this is
// a fix, not an opt-in feature, so a device whose controld.json predates the
// key must still receive it, and only an explicit "enabled": false disables
// it.
//
// Defined on the SECTION with a nil-receiver check rather than on Config,
// because initializeApp is handed individual config sections rather than the
// whole Config. A second Config-level accessor existed briefly and was
// removed: it was a pure alias with no production caller, and two spellings
// of one default is how the two drift apart.
func (g *GatewayUserAgentConfig) IsEnabled() bool {
	if g == nil || g.Enabled == nil {
		return true
	}
	return *g.Enabled
}

// GatewayUserAgentTuning decodes the raw gatewayUserAgent block.
//
// A malformed or wrong-typed block is NOT an error here: it logs and returns
// nil, which every caller reads as "absent" and therefore as the built-in
// defaults. That is deliberate, and it is the whole point of holding the
// block as RawMessage — see the field's doc for what failing instead used to
// cost.
//
// Falling back to defaults rather than to disabled is also deliberate. The
// realistic edit to this block is an operator ADDING a newly hostile gateway,
// so a typo that disabled the rewrite would break artworks that render today
// — a regression whose symptom (unrelated artworks failing) points nowhere
// near its cause (a config edit about a different host). Falling back leaves
// the device in exactly the state an unconfigured device is in.
//
// There is ONE exception, and it is not a nuance: an explicit
// "enabled": false is recovered on its own even when the rest of the block
// is unreadable. Defaults are ON, so without that carve-out a type error in
// a sibling field would silently re-arm the rewrite on a device whose
// operator had turned it off — the one direction where "the state an
// unconfigured device is in" is NOT what was asked for.
//
// The log is Error, not the Warn its Provisioning sibling uses, because the
// consequence differs: there, defaults replace some tuning knobs; here the
// operator's ENTIRE stated intent is discarded. It carries the raw block, not
// the effective host list: by definition the block did not parse, so there is
// no operator-supplied scope to name — the scope actually in force is logged
// by uarewrite's "kiosk User-Agent rewrite armed" line when the interceptor
// arms, which is the line to grep for the running scope.
func (c *Config) GatewayUserAgentTuning(logger *zap.Logger) *GatewayUserAgentConfig {
	if len(c.GatewayUserAgent) == 0 {
		return nil
	}
	var g GatewayUserAgentConfig
	if err := json.Unmarshal(c.GatewayUserAgent, &g); err != nil {
		// Falling back to defaults is right for a typo in the SCOPE, but
		// wrong for the switch: "enabled": false is an operator saying
		// "off", and defaults are ON. Discarding the whole block would arm
		// a rewrite on a device that explicitly asked for none, and the
		// only trace would be this log line. So recover the switch on its
		// own — encoding/json ignores unknown fields, meaning this narrow
		// decode succeeds precisely when the failure was in a SIBLING
		// field. A malformed "enabled" itself still falls through to
		// defaults, which is the case the paragraph above reasons about.
		if disabled := decodeDisableSwitch(c.GatewayUserAgent); disabled != nil {
			if logger != nil {
				logger.Error("gatewayUserAgent config block malformed; honoring only its explicit \"enabled\": false",
					zap.Error(err), zap.String("raw", string(c.GatewayUserAgent)))
			}
			return &GatewayUserAgentConfig{Enabled: disabled}
		}
		if logger != nil {
			logger.Error("gatewayUserAgent config block malformed; using built-in defaults",
				zap.Error(err), zap.String("raw", string(c.GatewayUserAgent)))
		}
		return nil
	}
	return &g
}

// decodeDisableSwitch pulls ONLY an explicit "enabled": false out of a block
// that failed to decode as a whole, returning nil for every other outcome
// (unreadable, absent, or true). Returning a pointer rather than a bool keeps
// "operator said false" distinct from "nothing to honor" — the caller must
// not turn the latter into a disable.
func decodeDisableSwitch(raw json.RawMessage) *bool {
	var sw struct {
		Enabled *bool `json:"enabled"`
	}
	if err := json.Unmarshal(raw, &sw); err != nil {
		return nil
	}
	if sw.Enabled == nil || *sw.Enabled {
		return nil
	}
	return sw.Enabled
}

// ProvisioningTuning carries the on-device knobs for the provisioning escape
// policy (docs/network-recovery-ux.md §4.1/§4.2). Every field follows the
// integer-with-unit-in-the-name convention (never time.Duration, which JSON
// would read as nanoseconds); zero means "use the built-in default" — the
// defaults live as constants in the provisioning package, next to the logic
// that runs on them, so config carries only overrides.
type ProvisioningTuning struct {
	// SetupIncompleteDisabled is the §4.1 kill-switch: disabling the
	// setup-incomplete fallback reverts unclaimed devices to
	// narration-plus-LAN-pairing only, with no other behavior change.
	SetupIncompleteDisabled bool `json:"setupIncompleteDisabled,omitempty"`

	EpisodeWindowSeconds         int `json:"episodeWindowSeconds,omitempty"`
	EpisodeApPhaseSeconds        int `json:"episodeApPhaseSeconds,omitempty"`
	EpisodeRaiseCycles           int `json:"episodeRaiseCycles,omitempty"`
	HubContactFreshSeconds       int `json:"hubContactFreshSeconds,omitempty"`
	DeferralCycleBudgetSeconds   int `json:"deferralCycleBudgetSeconds,omitempty"`
	DeferralEpisodeBudgetSeconds int `json:"deferralEpisodeBudgetSeconds,omitempty"`
	// EpisodeStationLadderSeconds overrides the escalating station-phase
	// ladder (default 300/600/1200).
	EpisodeStationLadderSeconds []int `json:"episodeStationLadderSeconds,omitempty"`

	RecheckApPhaseSeconds int `json:"recheckApPhaseSeconds,omitempty"`
	// RecheckApPhaseLadderSeconds overrides the escalating recheck AP-phase
	// ladder for the EARLY cycles (default 120/300/900); after the ladder,
	// every cycle uses recheckApPhaseSeconds.
	RecheckApPhaseLadderSeconds  []int `json:"recheckApPhaseLadderSeconds,omitempty"`
	RecheckBlinkCeilingSeconds   int   `json:"recheckBlinkCeilingSeconds,omitempty"`
	ActivationTimeoutSeconds     int   `json:"activationTimeoutSeconds,omitempty"`
	PortalActivityWindowSeconds  int   `json:"portalActivityWindowSeconds,omitempty"`
	PortalDeferralCeilingSeconds int   `json:"portalDeferralCeilingSeconds,omitempty"`
	UserRequestedSessionSeconds  int   `json:"userRequestedSessionSeconds,omitempty"`
	SessionAbsoluteCapSeconds    int   `json:"sessionAbsoluteCapSeconds,omitempty"`
}

// ProvisioningTuning decodes the raw provisioning block permissively: an
// absent block, or one that decodes to the wrong shape, yields the zero value
// (all defaults) — see the Provisioning field for why this must never fail
// the load.
func (c *Config) ProvisioningTuning(logger *zap.Logger) ProvisioningTuning {
	var t ProvisioningTuning
	if len(c.Provisioning) == 0 {
		return t
	}
	if err := json.Unmarshal(c.Provisioning, &t); err != nil {
		if logger != nil {
			logger.Warn("provisioning config block malformed; using built-in defaults", zap.Error(err))
		}
		return ProvisioningTuning{}
	}
	return t
}

//go:generate mockgen -source=config.go -destination=../mocks/config.go -package=mocks -mock_names=ConfigManager=MockConfigManager
type ConfigManager interface {
	Load(*zap.Logger) (*Config, error)
	Get() *Config
}

type defaultConfigManager struct {
	configLock sync.Mutex
	config     *Config
	os         wrapper.OS
	json       wrapper.JSON
	exec       wrapper.Exec
}

func NewConfigManager() ConfigManager {
	return &defaultConfigManager{
		os:   wrapper.NewOS(),
		json: wrapper.NewJSON(),
		exec: wrapper.NewExec(),
	}
}

// NewConfigManagerWithDeps creates a ConfigManager with custom dependencies (for testing)
func NewConfigManagerWithDeps(osWrapper wrapper.OS, jsonWrapper wrapper.JSON, execWrapper wrapper.Exec) ConfigManager {
	return &defaultConfigManager{
		os:   osWrapper,
		json: jsonWrapper,
		exec: execWrapper,
	}
}

func (m *defaultConfigManager) Load(logger *zap.Logger) (*Config, error) {
	logger.Info("Loading config", zap.String("file", constants.CONFIG_FILE))

	// Lock during the entire load process to prevent concurrent access
	m.configLock.Lock()
	defer m.configLock.Unlock()

	// Return existing config if already loaded
	if m.config != nil {
		return m.config, nil
	}

	// Try to read the file
	data, err := m.os.ReadFile(constants.CONFIG_FILE)
	if m.os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %w", err)
	} else if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var c Config
	if err := m.json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Fetch MAC info at startup
	c.MACInfo = m.getMACInfo(logger)

	logger.Info("MAC info loaded", zap.Any("macInfo", c.MACInfo))

	m.config = &c
	return m.config, nil
}

// getMACInfo fetches MAC addresses for all network interfaces and returns as a map
func (m *defaultConfigManager) getMACInfo(logger *zap.Logger) map[string]string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Get list of network devices
	devices := m.getNetworkDevices(ctx, logger)
	if len(devices) == 0 {
		return make(map[string]string)
	}

	// Get MAC addresses for each device
	macMap := make(map[string]string)
	for _, device := range devices {
		mac := m.getDeviceMAC(ctx, logger, device)
		if isValidMAC(mac) {
			macMap[device] = mac
		} else {
			logger.Debug("Invalid or missing MAC address, skipping device",
				zap.String("device", device),
				zap.String("mac", mac))
		}
	}

	return macMap
}

// getNetworkDevices returns a list of ethernet and wifi device names
func (m *defaultConfigManager) getNetworkDevices(ctx context.Context, logger *zap.Logger) []string {
	cmd := m.exec.CommandContext(ctx, "nmcli", "-t", "-f", "DEVICE,TYPE", "device")
	output, err := cmd.Output()
	if err != nil {
		logger.Warn("Failed to get network devices", zap.Error(err))
		return nil
	}

	var devices []string
	lines := strings.Split(strings.TrimSpace(string(output)), "\n")
	for _, line := range lines {
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		device, devType := parts[0], parts[1]
		if devType == "ethernet" || devType == "wifi" {
			devices = append(devices, device)
		}
	}

	return devices
}

// getDeviceMAC returns the MAC address for a given device
// It first tries ethtool for permanent address, then falls back to sysfs
func (m *defaultConfigManager) getDeviceMAC(ctx context.Context, logger *zap.Logger, device string) string {
	// Try ethtool first for permanent address
	cmd := m.exec.CommandContext(ctx, "ethtool", "-P", device)
	output, err := cmd.Output()
	if err == nil {
		// Parse "Permanent address: aa:bb:cc:dd:ee:ff"
		line := strings.TrimSpace(string(output))
		if strings.HasPrefix(line, "Permanent address:") {
			mac := strings.TrimSpace(strings.TrimPrefix(line, "Permanent address:"))
			if mac != "" && mac != "00:00:00:00:00:00" {
				return mac
			}
		}
	}
	return "" // Fallback to empty if ethtool fails or no valid MAC found
}

// isValidMAC checks if the given string is a valid MAC address
func isValidMAC(mac string) bool {
	if mac == "" {
		return false
	}
	return macRegex.MatchString(mac)
}

func (m *defaultConfigManager) Get() *Config {
	m.configLock.Lock()
	defer m.configLock.Unlock()

	if m.config == nil {
		m.config = &Config{
			CDPConfig:         &CDPConfig{},
			RelayerConfig:     &RelayerConfig{},
			MintPairingConfig: &MintPairingConfig{},
			SentryConfig:      &logger.SentryConfig{},
		}
	}
	return m.config
}

// Global instance for backward compatibility
var globalConfigManager ConfigManager = NewConfigManager()

// Backward compatible functions
func Load(logger *zap.Logger) (*Config, error) {
	return globalConfigManager.Load(logger)
}

func Get() *Config {
	return globalConfigManager.Get()
}

// For testing - inject a mock config manager
func InjectConfigManagerForTesting(cm ConfigManager) {
	globalConfigManager = cm
}

// Reset for testing
func ResetForTesting() {
	globalConfigManager = NewConfigManager()
}
