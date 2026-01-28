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

	// MAC addresses (fetched at startup, not from config file)
	EthernetMAC string `json:"-"`
	WifiMAC     string `json:"-"`
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

	// Fetch MAC addresses at startup
	c.EthernetMAC = m.getMACAddress("enp1s0", logger)
	c.WifiMAC = m.getMACAddress("wlp2s0", logger)

	logger.Info("MAC addresses loaded",
		zap.String("ethernetMAC", c.EthernetMAC),
		zap.String("wifiMAC", c.WifiMAC))

	m.config = &c
	return m.config, nil
}

// getMACAddress fetches the MAC address for a given network interface
func (m *defaultConfigManager) getMACAddress(interfaceName string, logger *zap.Logger) string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Run: ip link show dev <interface> | grep -o -E 'link/ether ([0-9a-fA-F:]{17})' | awk '{print $2}'
	// We use sh -c to run the piped command
	cmd := m.exec.CommandContext(ctx, "sh", "-c",
		fmt.Sprintf("ip link show dev %s | grep -o -E 'link/ether ([0-9a-fA-F:]{17})' | awk '{print $2}'", interfaceName))

	output, err := cmd.Output()
	if err != nil {
		logger.Warn("Failed to get MAC address",
			zap.String("interface", interfaceName),
			zap.Error(err))
		return ""
	}

	mac := strings.TrimSpace(string(output))
	return mac
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
