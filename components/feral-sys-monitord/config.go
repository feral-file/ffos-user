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
)

var (
	config *Config
)

type SysMonitordConfig struct {
	SentryConfig *logger.SentryConfig `json:"sentry"`
}

// Config holds the entire configuration for the application.
type Config struct {
	SysMonitordConfig SysMonitordConfig `json:"sys_monitord_config"`
}

// LoadConfig reads and parses the JSON configuration file from the given path.
func LoadConfig() error {
	// Try to read the app monitord config file
	data, err := os.ReadFile(sysMonitordConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read app monitord config file: %w", err)
	}

	if err := json.Unmarshal(data, &config.SysMonitordConfig); err != nil {
		return fmt.Errorf("failed to parse app monitord config file: %w", err)
	}

	return nil
}
