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
	"github.com/feral-file/ffos-user/components/feral-controld/metric"
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

// Configuration for all components
type Config struct {
	CDPConfig       *CDPConfig              `json:"cdp"`
	RelayerConfig   *RelayerConfig          `json:"relayer"`
	SentryConfig    *logger.SentryConfig    `json:"sentry"`
	OpenPanelConfig *metric.OpenPanelConfig `json:"openpanel"`
	EnableHub       bool                    `json:"enableHub"`

	// MACInfo is a JSON string containing MAC addresses for all network interfaces
	// e.g., {"enp1s0":"aa:bb:cc:dd:ee:ff","wlp2s0":"11:22:33:44:55:66"}
	MACInfo string `json:"-"`
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

	logger.Info("MAC info loaded", zap.String("macInfo", c.MACInfo))

	m.config = &c
	return m.config, nil
}

// getMACInfo fetches MAC addresses for all network interfaces and returns as JSON string
func (m *defaultConfigManager) getMACInfo(logger *zap.Logger) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Get list of network devices
	devices := m.getNetworkDevices(ctx, logger)
	if len(devices) == 0 {
		return "{}"
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

	// Convert to JSON
	jsonBytes, err := json.Marshal(macMap)
	if err != nil {
		logger.Warn("Failed to marshal MAC info", zap.Error(err))
		return "{}"
	}

	return string(jsonBytes)
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
			CDPConfig:       &CDPConfig{},
			RelayerConfig:   &RelayerConfig{},
			SentryConfig:    &logger.SentryConfig{},
			OpenPanelConfig: &metric.OpenPanelConfig{},
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
