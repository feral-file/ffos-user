package main

import (
	"encoding/json"
	"fmt"
	"os"

	"github.com/feral-file/ffos-user/components/feral-app-monitord/logger"

	"go.uber.org/zap"
)

// Constants for configuration paths.
const (
	ff1ConfigFile         = "/home/feralfile/ff1-config.json"
	appMonitordConfigFile = "/home/feralfile/.config/app-monitord.json"
)

var (
	config *Config
)

type FF1Config struct {
	Branch            string `json:"branch"`
	Version           string `json:"version"`
	HeartbeatEndpoint string `json:"heartbeat_endpoint"`
}

type AppMonitordConfig struct {
	SentryConfig *logger.SentryConfig `json:"sentry"`
}

// Config holds the configuration loaded from the ff1-config.json file.
type Config struct {
	FF1Config         FF1Config         `json:"ff1_config"`
	AppMonitordConfig AppMonitordConfig `json:"app_monitord_config"`
	Pubkey            string            `json:"pubkey"`
}

// LoadConfig reads and parses the JSON configuration file from the given path.
func LoadConfig() error {
	config = &Config{}
	data, err := os.ReadFile(ff1ConfigFile)
	if err != nil {
		return fmt.Errorf("failed to open config file %s: %w", ff1ConfigFile, err)
	}

	if err := json.Unmarshal(data, &config.FF1Config); err != nil {
		return fmt.Errorf("failed to parse config JSON: %w", err)
	}

	// ignore the error here, as the pubkey might not exist yet for QEMU
	config.Pubkey, err = CleanPublicKeyBase64()
	if err != nil {
		log.Error("Failed to read public key.", zap.Error(err))
		return nil
	}

	// Try to read the app monitord config file
	data, err = os.ReadFile(appMonitordConfigFile)
	if err != nil {
		return fmt.Errorf("failed to read app monitord config file: %w", err)
	}

	if err := json.Unmarshal(data, &config.AppMonitordConfig); err != nil {
		return fmt.Errorf("failed to parse app monitord config file: %w", err)
	}

	return nil
}
