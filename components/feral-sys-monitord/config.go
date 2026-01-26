package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/logger"
)

// Constants for configuration paths.
const (
	sysMonitordConfigFile = "/home/feralfile/.config/sys-monitord.json"
	ff1ConfigFile         = "/home/feralfile/ff1-config.json"
)

var (
	config *Config
)

type SysMonitordConfig struct {
	SentryConfig *logger.SentryConfig `json:"sentry"`
}

// FF1Config represents the configuration from ff1-config.json
type FF1Config struct {
	Endpoint string `json:"endpoint"`
	Branch   string `json:"branch"`
	Version  string `json:"version"`
}

// Config holds the entire configuration for the application.
type Config struct {
	SysMonitordConfig SysMonitordConfig `json:"sys_monitord_config"`
	FF1Config         FF1Config         `json:"ff1_config"`
}

// LoadConfig reads and parses all configuration files.
func LoadConfig() error {
	config = &Config{}

	// Load sys-monitord config
	data, err := os.ReadFile(sysMonitordConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read sys-monitord config file: %w", err)
	}

	if err := json.Unmarshal(data, &config.SysMonitordConfig); err != nil {
		return fmt.Errorf("failed to parse sys-monitord config file: %w", err)
	}

	// Load FF1 config
	ff1Data, err := os.ReadFile(ff1ConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read FF1 config file: %w", err)
	}

	if err := json.Unmarshal(ff1Data, &config.FF1Config); err != nil {
		return fmt.Errorf("failed to parse FF1 config file: %w", err)
	}

	return nil
}
