package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Constants for configuration paths.
const (
	homeDir            = "/home/feralfile"
	configDirName      = ".config"
	configFileBasename = "x1-config.json"
	privateKeyFilename = "device.pem"
	publicKeyFilename  = "device.pub"
)

var (
	configDir      = filepath.Join(homeDir, configDirName)
	configFile     = filepath.Join(homeDir, configFileBasename)
	privateKeyFile = filepath.Join(configDir, privateKeyFilename)
	publicKeyFile  = filepath.Join(configDir, publicKeyFilename)
	config         *Config
)

// Config holds the configuration loaded from the x1-config.json file.
type Config struct {
	Branch            string `json:"branch"`
	Version           string `json:"version"`
	HeartbeatEndpoint string `json:"heartbeat_endpoint"`
	Pubkey            string `json:"pubkey"`
}

// LoadConfig reads and parses the JSON configuration file from the given path.
func LoadConfig() error {
	// #nosec G304 -- path is constructed from constants
	file, err := os.Open(configFile)
	if err != nil {
		return fmt.Errorf("failed to open config file %s: %w", configFile, err)
	}
	defer closeFile(file)

	bytes, err := io.ReadAll(file)
	if err != nil {
		return fmt.Errorf("failed to read config file: %w", err)
	}

	if err := json.Unmarshal(bytes, &config); err != nil {
		return fmt.Errorf("failed to parse config JSON: %w", err)
	}

	config.Pubkey, err = CleanPublicKeyBase64()
	if err != nil {
		return fmt.Errorf("failed to get public key: %w", err)
	}

	return nil
}
