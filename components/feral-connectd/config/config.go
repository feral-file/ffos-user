package config

import (
	"fmt"
	"sync"

	"github.com/feral-file/ffos-user/components/feral-connectd/logger"
	"github.com/feral-file/ffos-user/components/feral-connectd/wrapper"
	"go.uber.org/zap"
)

const (
	CONFIG_FILE = "/home/feralfile/.config/connectd.json"
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
	CDPConfig     *CDPConfig           `json:"cdp"`
	RelayerConfig *RelayerConfig       `json:"relayer"`
	SentryConfig  *logger.SentryConfig `json:"sentry"`
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
}

func NewConfigManager() ConfigManager {
	return &defaultConfigManager{
		os:   wrapper.NewOS(),
		json: wrapper.NewJSON(),
	}
}

// NewConfigManagerWithDeps creates a ConfigManager with custom dependencies (for testing)
func NewConfigManagerWithDeps(osWrapper wrapper.OS, jsonWrapper wrapper.JSON) ConfigManager {
	return &defaultConfigManager{
		os:   osWrapper,
		json: jsonWrapper,
	}
}

func (m *defaultConfigManager) Load(logger *zap.Logger) (*Config, error) {
	logger.Info("Loading config", zap.String("file", CONFIG_FILE))

	// Lock during the entire load process to prevent concurrent access
	m.configLock.Lock()
	defer m.configLock.Unlock()

	// Return existing config if already loaded
	if m.config != nil {
		return m.config, nil
	}

	// Try to read the file
	data, err := m.os.ReadFile(CONFIG_FILE)
	if m.os.IsNotExist(err) {
		return nil, fmt.Errorf("config file not found: %w", err)
	} else if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	var c Config
	if err := m.json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	m.config = &c
	return m.config, nil
}

func (m *defaultConfigManager) Get() *Config {
	m.configLock.Lock()
	defer m.configLock.Unlock()

	if m.config == nil {
		m.config = &Config{
			CDPConfig:     &CDPConfig{},
			RelayerConfig: &RelayerConfig{},
			SentryConfig:  &logger.SentryConfig{},
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
