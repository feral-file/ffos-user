package metric

import (
	"bytes"
	"context"
	"fmt"
	"net/url"

	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	OPENPANEL_API_URL = "https://openpanel.feralfile.com/track"
)

// OpenPanelConfig contains the configuration for OpenPanel
type OpenPanelConfig struct {
	ClientID     string `json:"clientId"`
	ClientSecret string `json:"clientSecret"`
}

// IsEnabled checks if OpenPanel is enabled
func (c *OpenPanelConfig) IsEnabled() bool {
	return c != nil && c.ClientID != "" && c.ClientSecret != ""
}

// DeviceInfo contains information about the device
type DeviceInfo struct {
	DeviceID string
	Version  string
	Platform string
}

type openPanelTracker struct {
	config     *OpenPanelConfig
	deviceInfo *DeviceInfo
	os         wrapper.OS
	httpClient wrapper.HTTPClient
	json       wrapper.JSON
	logger     *zap.Logger
}

// NewOpenPanelTracker creates a new OpenPanel metric tracker
func NewOpenPanelTracker(
	config *OpenPanelConfig,
	os wrapper.OS,
	httpClient wrapper.HTTPClient,
	json wrapper.JSON,
	logger *zap.Logger,
) Tracker {
	return &openPanelTracker{
		config: config,
		deviceInfo: &DeviceInfo{
			DeviceID: "unknown",
			Version:  "unknown",
			Platform: "ff1",
		},
		os:         os,
		httpClient: httpClient,
		json:       json,
		logger:     logger,
	}
}

// Initialize loads device information from system files
func (t *openPanelTracker) Initialize() error {
	// Try to read device ID from /etc/hostname
	hostnameBytes, err := t.os.ReadFile(constants.HOSTNAME_FILE)
	if err == nil && len(hostnameBytes) > 0 {
		// Trim whitespace and newlines
		t.deviceInfo.DeviceID = string(bytes.TrimSpace(hostnameBytes))
	} else {
		return fmt.Errorf("failed to read device ID from /etc/hostname: %w", err)
	}

	// Try to read version from ff1-config.json
	configBytes, err := t.os.ReadFile(constants.FF1_CONFIG_FILE)
	if err == nil {
		var ff1Config struct {
			Version string `json:"version"`
		}
		if err := t.json.Unmarshal(configBytes, &ff1Config); err == nil {
			t.deviceInfo.Version = ff1Config.Version
		} else {
			return fmt.Errorf("failed to parse ff1-config.json: %w", err)
		}
	} else {
		return fmt.Errorf("failed to read ff1-config.json: %w", err)
	}

	t.logger.Info("Initialized metric tracker",
		zap.String("device_id", t.deviceInfo.DeviceID),
		zap.String("version", t.deviceInfo.Version),
		zap.Bool("enabled", t.config.IsEnabled()))

	return nil
}

// TrackPlaylistView tracks when a playlist is viewed/displayed
func (t *openPanelTracker) TrackPlaylistView(ctx context.Context, playlist *dp1.Playlist, playlistURL string) error {
	if !t.config.IsEnabled() {
		t.logger.Debug("OpenPanel tracking is disabled, skipping event")
		return nil
	}

	if playlist == nil {
		t.logger.Error("Playlist is nil, skipping event")
		return nil
	}

	// Extract playlist properties
	playlistKey := playlist.ID
	playlistScope := "generated"
	playlistFeedHost := ""

	// Parse URL to extract host if URL is provided
	if playlistURL != "" {
		// Parse URL to extract host
		if parsedURL, err := url.Parse(playlistURL); err == nil {
			playlistFeedHost = parsedURL.Host
		}

		// Set playlist scope to feed if URL is provided
		playlistScope = "feed"
	}

	// Build event properties
	properties := EventProperties{
		ActorType:        "device",
		ActorID:          t.deviceInfo.DeviceID,
		EnvApp:           "ff1",
		EnvAppVersion:    t.deviceInfo.Version,
		EnvPlatform:      t.deviceInfo.Platform,
		EnvOS:            "ffos",
		EnvOSVersion:     t.deviceInfo.Version,
		EnvBuildType:     "prod",
		PlaylistScope:    playlistScope,
		PlaylistKey:      playlistKey,
		PlaylistURL:      playlistURL,
		PlaylistFeedHost: playlistFeedHost,
	}

	// Build the request payload
	payload := map[string]interface{}{
		"type": "track",
		"payload": map[string]interface{}{
			"name":       "playlist_view",
			"properties": properties,
		},
	}

	// Marshal payload to JSON
	jsonData, err := t.json.Marshal(payload)
	if err != nil {
		t.logger.Error("Failed to marshal OpenPanel event payload", zap.Error(err))
		return fmt.Errorf("failed to marshal payload: %w", err)
	}

	// Send request in a goroutine to avoid blocking
	go func() {
		if err := t.sendRequest(jsonData, playlistKey); err != nil {
			t.logger.Error("Failed to send OpenPanel event", zap.Error(err))
		}
	}()

	return nil
}

// sendRequest sends the OpenPanel event request
func (t *openPanelTracker) sendRequest(jsonData []byte, playlistID string) error {
	req, err := t.httpClient.NewRequest("POST", OPENPANEL_API_URL, bytes.NewBuffer(jsonData))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("openpanel-client-id", t.config.ClientID)
	req.Header.Set("openpanel-client-secret", t.config.ClientSecret)

	// Send request
	resp, err := t.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Check response status
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		t.logger.Error("OpenPanel API returned error status",
			zap.Int("status_code", resp.StatusCode),
			zap.String("playlist_id", playlistID))
		return fmt.Errorf("OpenPanel API returned status %d", resp.StatusCode)
	}

	t.logger.Info("Successfully tracked playlist view event",
		zap.String("playlist_id", playlistID),
		zap.String("device_id", t.deviceInfo.DeviceID))

	return nil
}
