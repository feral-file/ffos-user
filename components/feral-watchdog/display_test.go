package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSysfsDisplayDetectorConnected(t *testing.T) {
	detector := newTestDisplayDetector(t, map[string]string{
		"card0-HDMI-A-1/status": "disconnected\n",
		"card0-DP-1/status":     "connected\n",
	})

	state := detector.State()
	if !state.Known || !state.Connected {
		t.Fatalf("expected known connected display, got %+v", state)
	}
}

func TestSysfsDisplayDetectorDisconnected(t *testing.T) {
	detector := newTestDisplayDetector(t, map[string]string{
		"card0-HDMI-A-1/status": "disconnected\n",
		"card0-DP-1/status":     "disconnected\n",
	})

	state := detector.State()
	if !state.Known || state.Connected {
		t.Fatalf("expected known disconnected display, got %+v", state)
	}
}

func TestSysfsDisplayDetectorUnknownStatusFailsOpen(t *testing.T) {
	detector := newTestDisplayDetector(t, map[string]string{
		"card0-HDMI-A-1/status": "unknown\n",
	})

	state := detector.State()
	if state.Known {
		t.Fatalf("expected literal unknown DRM status to fail open, got %+v", state)
	}
}

func TestSysfsDisplayDetectorUnknownWithoutReadableStatus(t *testing.T) {
	dir := t.TempDir()
	detector := &sysfsDisplayDetector{statusGlob: filepath.Join(dir, "card*-*", "status")}

	state := detector.State()
	if state.Known {
		t.Fatalf("expected unknown display state with no status files, got %+v", state)
	}
}

func newTestDisplayDetector(t *testing.T, files map[string]string) *sysfsDisplayDetector {
	t.Helper()
	dir := t.TempDir()
	for rel, contents := range files {
		path := filepath.Join(dir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatalf("failed to create status dir: %v", err)
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			t.Fatalf("failed to write status file: %v", err)
		}
	}
	return &sysfsDisplayDetector{statusGlob: filepath.Join(dir, "card*-*", "status")}
}

type staticDisplayDetector struct {
	state DisplayState
}

func (s staticDisplayDetector) State() DisplayState {
	return s.state
}
