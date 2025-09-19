package status

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"golang.org/x/sync/errgroup"
)

//go:generate mockgen -source=device_status.go -destination=../mocks/device_status.go -package=mocks -mock_names=DeviceStatus=MockDeviceStatus
type DeviceStatus interface {
	GetStatus(ctx context.Context) (*DeviceStatusResponse, error)
}

type deviceStatus struct {
	json wrapper.JSON
	os   wrapper.OS
	exec wrapper.Exec
	http wrapper.HTTP
	io   wrapper.IO
}

func NewDeviceStatus(
	json wrapper.JSON,
	os wrapper.OS,
	exec wrapper.Exec,
	http wrapper.HTTP,
	io wrapper.IO,
) DeviceStatus {
	return &deviceStatus{
		json: json,
		os:   os,
		exec: exec,
		http: http,
		io:   io,
	}
}

// DeviceStatusResponse represents the structure of device status information
type DeviceStatusResponse struct {
	ScreenRotation   string `json:"screenRotation,omitempty"`
	ConnectedWifi    string `json:"connectedWifi,omitempty"`
	InstalledVersion string `json:"installedVersion,omitempty"`
	LatestVersion    string `json:"latestVersion,omitempty"`
}

// GetStatus retrieves comprehensive device status information
// This function can be used by both command handlers and status polling
func (d deviceStatus) GetStatus(ctx context.Context) (*DeviceStatusResponse, error) {
	response := &DeviceStatusResponse{}

	// Use errgroup for parallel execution
	g, ctx := errgroup.WithContext(ctx)

	// Variables to collect results safely
	var screenRotation, connectedWifi, installedVersion, latestVersion string

	// Get screen rotation
	g.Go(func() error {
		// Default to landscape
		screenRotation = "landscape"

		configPath := "/home/feralfile/.config/screen-orientation"
		configData, err := d.os.ReadFile(configPath)
		if err != nil {
			return nil // Don't fail if config file doesn't exist
		}

		if len(configData) > 0 {
			savedRotation := strings.TrimSpace(string(configData))
			orientationMap := map[string]string{
				"normal": "landscape",
				"90":     "portrait",
				"180":    "landscapeReverse",
				"270":    "portraitReverse",
			}
			if orientation, ok := orientationMap[savedRotation]; ok {
				screenRotation = orientation
			}
		}
		return nil
	})

	// Get WiFi information
	g.Go(func() error {
		cmd := d.exec.CommandContext(ctx, "nmcli", "-t", "-f", "NAME,DEVICE,STATE", "connection", "show", "--active")
		output, err := cmd.Output()
		if err != nil {
			return nil // Don't fail if nmcli command fails
		}

		lines := strings.Split(string(output), "\n")
		for _, line := range lines {
			parts := strings.Split(line, ":")
			if len(parts) >= 3 && parts[2] == "activated" {
				// Check if device name starts with 'wl' (wireless) or contains 'wifi'
				deviceName := parts[1]
				if strings.HasPrefix(deviceName, "wl") || strings.Contains(deviceName, "wifi") {
					connectedWifi = parts[0] // Network name
					break
				}
			}
		}
		return nil
	})

	// Get installed version and latest version
	g.Go(func() error {
		configFile := "/home/feralfile/ff1-config.json"
		configBytes, err := d.os.ReadFile(configFile)
		if err != nil {
			return fmt.Errorf("failed to read config file: %w", err)
		}

		var config struct {
			Version  string `json:"version"`
			Branch   string `json:"branch"`
			Endpoint string `json:"endpoint"`
		}

		if err := d.json.Unmarshal(configBytes, &config); err != nil {
			return fmt.Errorf("failed to parse config file: %w", err)
		}

		installedVersion = config.Version

		// Get latest version from API if credentials are available
		if config.Branch != "" && config.Endpoint != "" {
			version, err := d.fetchLatestVersion(ctx, config.Endpoint, config.Branch)
			if err != nil {
				return fmt.Errorf("failed to fetch latest version: %w", err)
			}
			latestVersion = version
		}

		return nil
	})

	// Wait for all goroutines to complete
	if err := g.Wait(); err != nil {
		return nil, err
	}

	// Safely assign results after all goroutines complete
	response.ScreenRotation = screenRotation
	response.ConnectedWifi = connectedWifi
	response.InstalledVersion = installedVersion
	response.LatestVersion = latestVersion

	return response, nil
}

// fetchLatestVersion retrieves the latest version from the distribution API
func (d deviceStatus) fetchLatestVersion(ctx context.Context, endpoint, branch string) (string, error) {
	apiURL := fmt.Sprintf("%s/api/latest/%s", endpoint, branch)

	// Create HTTP client with 2-second timeout
	client := &http.Client{
		Timeout: 2 * time.Second,
	}

	req, err := http.NewRequestWithContext(ctx, "GET", apiURL, nil)
	if err != nil {
		return "", err
	}

	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	body, err := d.io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}

	var apiResponse struct {
		LatestVersion string `json:"latest_version"`
	}

	if err := d.json.Unmarshal(body, &apiResponse); err != nil {
		return "", err
	}

	return apiResponse.LatestVersion, nil
}
