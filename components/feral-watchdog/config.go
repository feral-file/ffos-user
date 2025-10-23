package main

import (
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/feral-file/ffos-user/components/feral-watchdog/logger"
	"go.uber.org/zap"
)

const (
	// Configuration file paths
	CONFIG_FILE = "/home/feralfile/.config/watchdog.json"
)

var (
	configLock sync.Mutex
	config     *Config
)

type CDPConfig struct {
	Endpoint string `json:"endpoint"`
}

// Config represents the configuration for the watchdog daemon
type Config struct {
	CDPConfig    *CDPConfig           `json:"cdp"`
	SentryConfig *logger.SentryConfig `json:"sentry"`
}

// LoadConfig loads the configuration from a JSON file
func LoadConfig(logger *zap.Logger) (*Config, error) {
	logger.Info("Loading config", zap.String("file", CONFIG_FILE))

	// Try to read the file
	data, err := os.ReadFile(CONFIG_FILE)
	if err != nil {
		return nil, fmt.Errorf("failed to read config file: %w", err)
	}

	// Lock during unmarshaling to prevent concurrent access
	configLock.Lock()
	defer configLock.Unlock()

	var c Config
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("failed to parse config file: %w", err)
	}

	// Set default endpoint if not provided
	if c.CDPConfig == nil || c.CDPConfig.Endpoint == "" {
		return nil, fmt.Errorf("cdp_endpoint is not provided")
	}

	config = &c
	return config, nil
}
