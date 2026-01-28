package config

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"

	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/metric"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

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

	// Run the command to get MAC addresses for all network interfaces as JSON
	//nolint:lll
	cmd := m.exec.CommandContext(ctx, "sh", "-c",
		`echo "{"$(nmcli -t -f DEVICE,TYPE device | grep -E ':(ethernet|wifi|gsm|cdma)' | cut -d: -f1 | while read d; do mac=$(ethtool -P $d 2>/dev/null | awk '/Permanent address:/ {print $NF}'); [ -z "$mac" ] && mac=$(cat /sys/class/net/$d/address 2>/dev/null); echo "\"$d\":\"$mac\""; done | paste -sd, -)"}"`)

	output, err := cmd.Output()
	if err != nil {
		logger.Warn("Failed to get MAC info", zap.Error(err))
		return "{}"
	}

	macInfo := strings.TrimSpace(string(output))
	return macInfo
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
