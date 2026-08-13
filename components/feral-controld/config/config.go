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
	// Dir overrides the ring location (default netlog.DefaultDir).
	Dir string `json:"dir,omitempty"`
	// MaxTotalBytes overrides the ring's total size cap (default 8 MiB).
	// The cap must stay far below uploadLogs' 128 MB bundle budget.
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
