package metric

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

var amdSclkMHzPattern = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*Mhz`)

// gpuFrequencyPercent exposes current/max clock as a percentage for clients that
// still want clock-ratio semantics. FF app should prefer gpu_busy when available.
func gpuFrequencyPercent(currentMHz, maxMHz float64) float64 {
	if maxMHz <= 0 {
		return 0
	}
	return currentMHz / maxMHz * 100
}

// discoverGPUDevicePath returns /sys/class/drm/cardN/device for the primary GPU.
func discoverGPUDevicePath() (string, error) {
	entries, err := os.ReadDir("/sys/class/drm")
	if err != nil {
		return "", err
	}

	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasPrefix(name, "card") || strings.Contains(name, "-") {
			continue
		}

		devicePath := filepath.Join("/sys/class/drm", name, "device")
		info, err := os.Stat(devicePath)
		if err != nil || !info.IsDir() {
			continue
		}

		// Prefer the card that exposes GPU utilization or Intel GT frequency caps.
		for _, marker := range []string{"gpu_busy_percent", "gt_busy_percent", "pp_dpm_sclk", "gt_max_freq_mhz"} {
			if _, err := os.Stat(filepath.Join(devicePath, marker)); err == nil {
				return devicePath, nil
			}
		}
	}

	return "", fmt.Errorf("no GPU drm device path found")
}

func readSysfsPercent(path string) (float64, error) {
	// Paths are built only from /sys/class/drm discovery plus fixed filenames.
	//nolint:gosec // G304: sysfs path is not caller-controlled.
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	value, err := strconv.ParseFloat(strings.TrimSpace(string(data)), 64)
	if err != nil {
		return 0, fmt.Errorf("parse %s: %w", path, err)
	}
	return value, nil
}

// readGPUBusyPercent reads shader/engine busy % from amdgpu or i915 sysfs.
func readGPUBusyPercent(devicePath string) (float64, error) {
	for _, name := range []string{"gpu_busy_percent", "gt_busy_percent"} {
		path := filepath.Join(devicePath, name)
		if _, err := os.Stat(path); err != nil {
			continue
		}
		return readSysfsPercent(path)
	}
	return 0, fmt.Errorf("no gpu busy sysfs file under %s", devicePath)
}

// parseAMDMaxSclkMHz returns the highest P-state MHz line from pp_dpm_sclk.
func parseAMDMaxSclkMHz(ppDPM string) (float64, error) {
	var maxMHz float64
	found := false

	for _, line := range strings.Split(ppDPM, "\n") {
		match := amdSclkMHzPattern.FindStringSubmatch(line)
		if len(match) < 2 {
			continue
		}
		mhz, err := strconv.ParseFloat(match[1], 64)
		if err != nil {
			continue
		}
		if mhz > maxMHz {
			maxMHz = mhz
			found = true
		}
	}

	if !found {
		return 0, fmt.Errorf("no sclk P-state lines in pp_dpm_sclk")
	}
	return maxMHz, nil
}

func readAMDMaxSclkMHz(devicePath string) (float64, error) {
	//nolint:gosec // G304: devicePath comes from discoverGPUDevicePath().
	data, err := os.ReadFile(filepath.Join(devicePath, "pp_dpm_sclk"))
	if err != nil {
		return 0, err
	}
	return parseAMDMaxSclkMHz(string(data))
}

func maxEngineBusyPercent(engines map[string]struct {
	Busy float64 `json:"busy"`
}) float64 {
	var maxBusy float64
	for _, engine := range engines {
		if engine.Busy > maxBusy {
			maxBusy = engine.Busy
		}
	}
	return maxBusy
}

// resolveGPUBusy prefers intel_gpu_top engine busy, then falls back to sysfs.
func resolveGPUBusy(engineBusy float64, devicePath string) (float64, error) {
	if engineBusy > 0 {
		return engineBusy, nil
	}
	if devicePath == "" {
		return 0, errBestEffortMetricUnavailable
	}
	return readGPUBusyPercent(devicePath)
}

func (p *SysResMonitor) applyGPUDerivedPercentages() {
	p.lastMetrics.GPU.FrequencyPercent = gpuFrequencyPercent(
		p.lastMetrics.GPU.CurrentFrequency,
		p.lastMetrics.GPU.MaxFrequency,
	)
}
