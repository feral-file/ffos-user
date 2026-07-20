package metric

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseAMDMaxSclkMHz(t *testing.T) {
	t.Parallel()

	const sample = `0: 200Mhz
1: 700Mhz
2: 2200Mhz *
`

	maxMHz, err := parseAMDMaxSclkMHz(sample)
	if err != nil {
		t.Fatalf("parseAMDMaxSclkMHz() error = %v", err)
	}
	if maxMHz != 2200 {
		t.Fatalf("parseAMDMaxSclkMHz() = %v, want 2200", maxMHz)
	}
}

func TestPickGPUDevicePathPrefersBootVGA(t *testing.T) {
	t.Parallel()

	path, ok := pickGPUDevicePath([]gpuDeviceCandidate{
		{devicePath: "/sys/class/drm/card1/device", bootVGA: false},
		{devicePath: "/sys/class/drm/card0/device", bootVGA: true},
	})
	if !ok {
		t.Fatal("expected a GPU device path to be selected")
	}
	if path != "/sys/class/drm/card0/device" {
		t.Fatalf("pickGPUDevicePath() = %q, want boot VGA adapter", path)
	}
}

func TestPickGPUDevicePathFallsBackToFirstCandidate(t *testing.T) {
	t.Parallel()

	path, ok := pickGPUDevicePath([]gpuDeviceCandidate{
		{devicePath: "/sys/class/drm/card2/device"},
		{devicePath: "/sys/class/drm/card3/device"},
	})
	if !ok {
		t.Fatal("expected a GPU device path to be selected")
	}
	if path != "/sys/class/drm/card2/device" {
		t.Fatalf("pickGPUDevicePath() = %q, want first candidate", path)
	}
}

func TestDiscoverGPUDevicePathAcceptsAMDGPUBusyPercent(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	cardPath := filepath.Join(root, "card0")
	devicePath := filepath.Join(cardPath, "device")

	if err := os.MkdirAll(devicePath, 0o700); err != nil {
		t.Fatalf("os.MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(devicePath, "gpu_busy_percent"), []byte("42\n"), 0o600); err != nil {
		t.Fatalf("os.WriteFile() error = %v", err)
	}

	path, err := discoverGPUDevicePathFrom(root)
	if err != nil {
		t.Fatalf("discoverGPUDevicePathFrom() error = %v", err)
	}
	if path != devicePath {
		t.Fatalf("discoverGPUDevicePathFrom() = %q, want %q", path, devicePath)
	}

	busy, err := readGPUBusyPercent(devicePath)
	if err != nil {
		t.Fatalf("readGPUBusyPercent() error = %v", err)
	}
	if busy != 42 {
		t.Fatalf("readGPUBusyPercent() = %v, want 42", busy)
	}
}
