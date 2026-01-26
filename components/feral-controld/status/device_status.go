package status

import (
	"context"
	"fmt"
	"strings"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	controldbus "github.com/feral-file/ffos-user/components/feral-controld/dbus"
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
	dbus controldbus.DBus
}

func NewDeviceStatus(
	json wrapper.JSON,
	os wrapper.OS,
	exec wrapper.Exec,
	dbus controldbus.DBus,
) DeviceStatus {
	return &deviceStatus{
		json: json,
		os:   os,
		exec: exec,
		dbus: dbus,
	}
}

// DeviceStatusResponse represents the structure of device status information
type DeviceStatusResponse struct {
	ScreenRotation      string `json:"screenRotation,omitempty"`
	ConnectedWifi       string `json:"connectedWifi,omitempty"`
	InstalledVersion    string `json:"installedVersion,omitempty"`
	LatestVersion       string `json:"latestVersion,omitempty"`
	AnalyticsDisabled   bool   `json:"analyticsDisabled,omitempty"`
	BetaFeaturesEnabled bool   `json:"betaFeaturesEnabled,omitempty"`
}

// GetStatus retrieves comprehensive device status information
// This function can be used by both command handlers and status polling
func (d deviceStatus) GetStatus(ctx context.Context) (*DeviceStatusResponse, error) {
	response := &DeviceStatusResponse{}

	// Use errgroup for parallel execution
	g, ctx := errgroup.WithContext(ctx)

	// Variables to collect results safely
	var screenRotation, connectedWifi, installedVersion, latestVersion string
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

	// Get installed version
	g.Go(func() error {
		configBytes, err := d.os.ReadFile(constants.FF1_CONFIG_FILE)
		if err != nil {
			return fmt.Errorf("failed to read config file: %w", err)
		}

		var config struct {
			Version string `json:"version"`
		}

		if err := d.json.Unmarshal(configBytes, &config); err != nil {
			return fmt.Errorf("failed to parse config file: %w", err)
		}

		installedVersion = config.Version
		return nil
	})

	// Get latest version from sys-monitord D-Bus
	g.Go(func() error {
		// Call sys-monitord D-Bus method GetLatestVersion with refresh=false (use cache)
		result, err := d.dbus.Call(
			ctx,
			controldbus.MONITORD_NAME,
			controldbus.MONITORD_PATH,
			controldbus.MONITORD_INTERFACE,
			controldbus.MONITORD_METHOD_GET_LATEST_VERSION,
			false, // don't force refresh, use cached value
		)
		if err != nil {
			// Don't fail - allow other status info to be returned even without latest version
			return nil
		}

		// D-Bus returns VersionDBusResponse as a struct, which godbus returns
		// as a single element containing the struct fields as []interface{}
		// We only need LatestVersion (first field) for device status
		if len(result) >= 1 {
			// result[0] is the struct, which is represented as []interface{} with 4 string fields
			if structFields, ok := result[0].([]interface{}); ok && len(structFields) >= 1 {
				if version, ok := structFields[0].(string); ok {
					latestVersion = version
				}
			}
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

	return response, nil
}
