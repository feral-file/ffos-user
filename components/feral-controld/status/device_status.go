package status

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/devicename"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"golang.org/x/sync/errgroup"
)

//go:generate mockgen -source=device_status.go -destination=../mocks/device_status.go -package=mocks -mock_names=DeviceStatus=MockDeviceStatus
type DeviceStatus interface {
	GetStatus(ctx context.Context) (*DeviceStatusResponse, error)
}

// versionCache stores the cached latest version information
type versionCache struct {
	version   string
	timestamp time.Time
	mu        sync.RWMutex
}

type deviceStatus struct {
	json       wrapper.JSON
	os         wrapper.OS
	exec       wrapper.Exec
	httpClient wrapper.HTTPClient
	io         wrapper.IO
	cdp        cdp.CDP
	cache      *versionCache

	// lastOutage, when wired (SetLastOutageSource), serves the netlog
	// recorder's last closed outage summary. It lives on THIS collector —
	// not the devicectl executor — because both consumers of the summary
	// flow through GetStatus: the pulled getDeviceStatus command reply AND
	// the poller's pushed device_status change feed (stage 2b's whole point
	// is that the backend sees the diagnosis WITHOUT polling). Probe-free by
	// contract: it reads the recorder's in-memory summary. Set once at
	// wiring time before any GetStatus caller runs (type-asserted seam,
	// deliberately not on the DeviceStatus interface, so mocks stay
	// untouched).
	lastOutage func() *LastOutage
}

// SetLastOutageSource wires the netlog outage-summary source (see the
// lastOutage field). Call before Start/first use.
func (d *deviceStatus) SetLastOutageSource(fn func() *LastOutage) {
	d.lastOutage = fn
}

func NewDeviceStatus(
	json wrapper.JSON,
	os wrapper.OS,
	exec wrapper.Exec,
	httpClient wrapper.HTTPClient,
	io wrapper.IO,
	cdpClient cdp.CDP,
) DeviceStatus {
	return &deviceStatus{
		json:       json,
		os:         os,
		exec:       exec,
		httpClient: httpClient,
		io:         io,
		cdp:        cdpClient,
		cache:      &versionCache{},
	}
}

// DeviceStatusContract is the API contract version this firmware speaks,
// reported on every getDeviceStatus reply (relayer and LAN cast alike) so the
// app can identify a v2 frame when mDNS is unavailable — multicast-filtering
// APs, cross-VLAN (docs/app-triggered-wifi-setup.md §4.2; added in the v2
// release itself because adding it later costs a full OTA convergence cycle
// before the app can rely on it). MUST stay equal to hub.StatusContractV2; a
// hub-side test pins the two constants together, the same technique as the
// mdns `api` TXT key.
const DeviceStatusContract = "2"

// NetworkHealth is the additive `network` object on the hub status routes and
// the relayer/cast getDeviceStatus reply (docs/network-recovery-ux.md §4.7):
// the same diagnosis the on-screen narration shows, so the app can surface it
// and offer "Change Wi-Fi" whenever the device is reachable. All values come
// from the provisioning machine's mu-guarded snapshot plus the cached
// monitord internet verdict — a status poll never runs a probe.
type NetworkHealth struct {
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
	SSID   string `json:"ssid,omitempty"`
	// Link is "wifi" | "ethernet" | "none" | "unknown" — the machine's last
	// real probe evidence, never a fresh read.
	Link     string `json:"link"`
	Internet bool   `json:"internet"`
	// Deferred tells the app its OWN presence (control-plane contact) is what
	// is holding a pending setup-mode raise down, so it can surface the
	// pairing/startWifiSetup action instead of silently resetting the clock.
	Deferred bool `json:"deferred,omitempty"`
}

// LastOutage is the additive outage summary on getDeviceStatus / the
// device_status feed (docs/wan-outage-observability.md stage 2b): the netlog
// recorder's last closed episode, so the backend sees the diagnosis without
// pulling logs. Values come from the recorder's in-memory summary — a status
// poll never reads the ring, let alone probes. Not persisted: a daemon
// restart clears it (the ring still holds the full history for uploadLogs).
type LastOutage struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
	// Class is the netlog taxonomy value (link-down, wan-down, dns-broken,
	// captive-portal, backend-only-down, ... or unknown-*).
	Class string `json:"class"`
	// Count24h is how many outages ENDED in the 24h before this reply.
	Count24h int `json:"count24h"`
}

// DeviceStatusResponse represents the structure of device status information
type DeviceStatusResponse struct {
	// Contract is DeviceStatusContract on every reply — deliberately no
	// omitempty: its PRESENCE is the capability signal (an old firmware's
	// reply simply lacks the key, and the app fails closed to the legacy
	// path).
	Contract string `json:"contract"`
	// Network is attached by the devicectl executor from the provisioning
	// machine's snapshot (see NetworkHealth); nil (omitted) only when that
	// seam is unwired.
	Network *NetworkHealth `json:"network,omitempty"`
	// LastOutage is attached by the devicectl executor from the netlog
	// recorder (see LastOutage); nil (omitted) when the recorder is disabled
	// or no outage has closed since process start.
	LastOutage          *LastOutage       `json:"lastOutage,omitempty"`
	ScreenRotation      string            `json:"screenRotation,omitempty"`
	ConnectedWifi       string            `json:"connectedWifi,omitempty"`
	InstalledVersion    string            `json:"installedVersion,omitempty"`
	LatestVersion       string            `json:"latestVersion,omitempty"`
	AnalyticsDisabled   bool              `json:"analyticsDisabled,omitempty"`
	BetaFeaturesEnabled bool              `json:"betaFeaturesEnabled,omitempty"`
	MACInfo             map[string]string `json:"macInfo,omitempty"`
	Volume              *int              `json:"volume,omitempty"`
	IsMuted             *bool             `json:"isMuted,omitempty"`
	DisplayURL          *string           `json:"displayURL,omitempty"` // Chrome UI URL from CDP; omitted if unavailable.
	// DeviceName is the owner's label for this unit, empty when nobody has set
	// one. Deliberately no omitempty, for the same reason as Contract above:
	// its PRESENCE is the capability signal a controller gates its rename
	// affordance on, and an unnamed device is the ordinary case — omitting the
	// key there would report "this firmware cannot be renamed" on every frame
	// that simply has not been.
	DeviceName string `json:"deviceName"`
	// SleepSchedule is derived from persisted schedule + wall clock (see sleepschedule).
	// It reflects intended FF1 sleep mode; FFP panel DDC power may lag after transitions
	// because controld aligns panel power asynchronously (best-effort, eventual vs. ddcPanelStatus).
	SleepSchedule *sleepschedule.Status `json:"sleepSchedule,omitempty"`
}

// GetStatus retrieves comprehensive device status information
// This function can be used by both command handlers and status polling
func (d deviceStatus) GetStatus(ctx context.Context) (*DeviceStatusResponse, error) {
	response := &DeviceStatusResponse{Contract: DeviceStatusContract}

	// Use errgroup for parallel execution
	g, ctx := errgroup.WithContext(ctx)

	// Variables to collect results safely
	var screenRotation, connectedWifi, installedVersion, latestVersion string
	var analyticsDisabled, betaFeaturesEnabled bool
	var volume *int
	var isMuted *bool
	var displayURL *string
	var sleepScheduleStatus *sleepschedule.Status
	var deviceName string

	// Chrome UI document URL. This is the same target listing the poller uses, but
	// keeping it here preserves the response shape for direct CMD_DEVICE_STATUS calls.
	g.Go(func() error {
		if d.cdp == nil {
			return nil
		}
		u, err := d.cdp.PageNavigationURL(ctx)
		if err != nil {
			// Non-fatal: device status remains useful without this field.
			return nil
		}
		displayURL = &u
		return nil
	})

	// Owner-set device name. A read failure or a corrupt record yields the
	// empty name rather than an error: the field is cosmetic, every consumer
	// falls back to the serial, and failing the whole status collection over a
	// label would take the frame's health readout down with it.
	g.Go(func() error {
		record, err := devicename.Load(d.os, d.json)
		if err != nil {
			return nil
		}
		deviceName = record.Name
		return nil
	})

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
			// don't care about the error - allow other status info to be returned even without network
			latestVersion, _ = d.getLatestVersion(ctx, config.Endpoint, config.Branch)
		}

		return nil
	})

	g.Go(func() error {
		record, err := sleepschedule.Load(d.os, d.json)
		if err != nil {
			if d.os.IsNotExist(err) {
				return nil
			}
			return nil
		}
		sleepScheduleStatus, _ = sleepschedule.EffectiveStatus(time.Now().In(sleepschedule.LocalTimezone()), record)
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

	// Get volume and mute status
	g.Go(func() error {
		// Get mute status
		muteCmd := d.exec.CommandContext(ctx, "pamixer", "--get-mute")
		muteOutput, err := muteCmd.Output()
		if err == nil {
			muteStr := strings.TrimSpace(string(muteOutput))
			// Parse "true" or "false"
			switch muteStr {
			case "true":
				muted := true
				isMuted = &muted
			case "false":
				muted := false
				isMuted = &muted
			}
		}
		// Don't fail if mute status fetch fails

		// Get volume status
		volumeCmd := d.exec.CommandContext(ctx, "pamixer", "--get-volume")
		volumeOutput, err := volumeCmd.Output()
		if err == nil {
			volumeStr := strings.TrimSpace(string(volumeOutput))
			// Parse the pactl volume value
			var pactlVolume int
			if _, err := fmt.Sscanf(volumeStr, "%d", &pactlVolume); err == nil {
				// Convert back to user volume using inverse formula
				// pactl_percent = 25 + (user_percent * 0.75)
				// user_percent = (pactl_percent - 25) / 0.75
				var userVolume int
				if pactlVolume <= 25 {
					userVolume = 0
				} else {
					userVolume = (pactlVolume - 25) * 100 / 75
				}
				volume = &userVolume
			}
		}
		// Don't fail if volume fetch fails

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
	response.Volume = volume
	response.IsMuted = isMuted
	response.DisplayURL = displayURL
	response.DeviceName = deviceName
	response.SleepSchedule = sleepScheduleStatus

	// Get MAC info from config (fetched once at startup)
	cfg := config.Get()
	response.MACInfo = cfg.MACInfo

	// Attach the netlog outage summary (stage 2b) — additive, probe-free,
	// omitted when unwired or empty. Attached here (not in the executor) so
	// the poller's pushed device_status feed carries it too; the summary only
	// changes when an outage closes, so the poller's MD5 dedupe still
	// suppresses no-change ticks.
	if d.lastOutage != nil {
		response.LastOutage = d.lastOutage()
	}

	return response, nil
}

// getLatestVersion returns the cached version if it's still valid (less than 1 hour old),
// otherwise fetches a new version from the API and updates the cache
func (d *deviceStatus) getLatestVersion(ctx context.Context, endpoint, branch string) (string, error) {
	const cacheDuration = time.Hour

	// 1. Quick Check (Read Lock)
	d.cache.mu.RLock()
	isFresh := d.cache.version != "" && time.Since(d.cache.timestamp) < cacheDuration
	cachedVersion := d.cache.version
	d.cache.mu.RUnlock()

	if isFresh {
		return cachedVersion, nil
	}

	// 2. Network Fetch (No Lock)
	version, err := d.fetchLatestVersion(ctx, endpoint, branch)
	if err != nil {
		log.Printf("Failed to fetch latest version: %v", err)
		if cachedVersion != "" {
			return cachedVersion, nil // Return stale cache without error
		}
		return "", err // No cache available, return error
	}

	// 3. Update (Write Lock)
	d.cache.mu.Lock()
	d.cache.version = version
	d.cache.timestamp = time.Now()
	d.cache.mu.Unlock()

	return version, nil
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
