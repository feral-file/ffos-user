package status

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"golang.org/x/sync/errgroup"
)

//go:generate mockgen -source=device_status.go -destination=../mocks/device_status.go -package=mocks -mock_names=DeviceStatus=MockDeviceStatus
type DeviceStatus interface {
	GetStatus(ctx context.Context) (*DeviceStatusResponse, error)
}

type deviceStatus struct {
	json       wrapper.JSON
	os         wrapper.OS
	exec       wrapper.Exec
	httpClient wrapper.HTTPClient
	io         wrapper.IO
}

func NewDeviceStatus(
	json wrapper.JSON,
	os wrapper.OS,
	exec wrapper.Exec,
	httpClient wrapper.HTTPClient,
	io wrapper.IO,
) DeviceStatus {
	return &deviceStatus{
		json:       json,
		os:         os,
		exec:       exec,
		httpClient: httpClient,
		io:         io,
	}
}

// DeviceStatusResponse represents the structure of device status information
type DeviceStatusResponse struct {
	ScreenRotation      string            `json:"screenRotation,omitempty"`
	ConnectedWifi       string            `json:"connectedWifi,omitempty"`
	InstalledVersion    string            `json:"installedVersion,omitempty"`
	LatestVersion       string            `json:"latestVersion,omitempty"`
	AnalyticsDisabled   bool              `json:"analyticsDisabled,omitempty"`
	BetaFeaturesEnabled bool              `json:"betaFeaturesEnabled,omitempty"`
	MACInfo             map[string]string `json:"macInfo,omitempty"`
	Timezone            string            `json:"timezone,omitempty"`
	CurrentTime         string            `json:"currentTime,omitempty"`
}

// GetStatus retrieves comprehensive device status information
// This function can be used by both command handlers and status polling
func (d deviceStatus) GetStatus(ctx context.Context) (*DeviceStatusResponse, error) {
	response := &DeviceStatusResponse{}

	// Use errgroup for parallel execution
	g, ctx := errgroup.WithContext(ctx)

	// Variables to collect results safely
	var screenRotation, connectedWifi, installedVersion, latestVersion string
	var timezone, currentTime string
	var analyticsDisabled, betaFeaturesEnabled bool

	// Get screen rotation
	g.Go(func() error {
		// Default to landscape
		screenRotation = "landscape"

		configData, err := d.os.ReadFile(constants.SCREEN_ORIENTATION_FILE)
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
		configBytes, err := d.os.ReadFile(constants.FF1_CONFIG_FILE)
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
		// Note: Network errors are non-fatal - latestVersion will remain empty if fetch fails
		if config.Branch != "" && config.Endpoint != "" {
			version, err := d.fetchLatestVersion(ctx, config.Endpoint, config.Branch)
			if err == nil {
				latestVersion = version
			}
			// Don't return error - allow other status info to be returned even without network
		}

		return nil
	})

	// Get analytics toggle (disabled when file exists)
	g.Go(func() error {
		const analyticsTogglePath = "/home/feralfile/.state/analytics-toggle-off"
		_, err := d.os.ReadFile(analyticsTogglePath)
		if err == nil {
			analyticsDisabled = true
			return nil
		}
		if d.os.IsNotExist(err) {
			analyticsDisabled = false
			return nil
		}
		return nil
	})

	// Get beta features toggle (enabled when file exists)
	g.Go(func() error {
		const betaTogglePath = "/home/feralfile/.state/beta-features-toggle-on"
		_, err := d.os.ReadFile(betaTogglePath)
		if err == nil {
			betaFeaturesEnabled = true
			return nil
		}
		if d.os.IsNotExist(err) {
			betaFeaturesEnabled = false
			return nil
		}
		return nil
	})

	// Get timezone and current time using timedatectl
	g.Go(func() error {
		// Get timezone
		tzCmd := d.exec.CommandContext(ctx, "timedatectl", "show", "--property=Timezone", "--value")
		tzOutput, err := tzCmd.Output()
		if err == nil {
			timezone = strings.TrimSpace(string(tzOutput))
		}
		// Don't fail if timezone fetch fails

		// Get current time (local time)
		dateCmd := d.exec.CommandContext(ctx, "date", "+%Y-%m-%d %H:%M:%S")
		dateOutput, err := dateCmd.Output()
		if err == nil {
			currentTime = strings.TrimSpace(string(dateOutput))
		}
		// Don't fail if time fetch fails

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
	response.AnalyticsDisabled = analyticsDisabled
	response.BetaFeaturesEnabled = betaFeaturesEnabled
	response.Timezone = timezone
	response.CurrentTime = currentTime

	// Get MAC info from config (fetched once at startup)
	cfg := config.Get()
	response.MACInfo = cfg.MACInfo

	return response, nil
}

// fetchLatestVersion retrieves the latest version from the distribution API
func (d deviceStatus) fetchLatestVersion(ctx context.Context, endpoint, branch string) (string, error) {
	apiURL := fmt.Sprintf("%s/api/latest/%s", endpoint, branch)

	// Create HTTP client with 30-second timeout
	client := &http.Client{
		Timeout: 30 * time.Second,
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
