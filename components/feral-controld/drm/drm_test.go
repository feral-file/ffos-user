package drm

import (
	"os"
	"path/filepath"
	"testing"
)

func drmRootWithStatuses(t *testing.T, statuses map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for connector, status := range statuses {
		dir := filepath.Join(root, connector)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("failed to create connector dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status+"\n"), 0o600); err != nil {
			t.Fatalf("failed to write status: %v", err)
		}
	}
	return root
}

func TestDisplayConnected(t *testing.T) {
	tests := []struct {
		name     string
		statuses map[string]string
		want     bool
	}{
		{
			name:     "single connected",
			statuses: map[string]string{"card0-HDMI-A-1": "connected"},
			want:     true,
		},
		{
			name:     "single disconnected is known-disconnected",
			statuses: map[string]string{"card0-HDMI-A-1": "disconnected"},
			want:     false,
		},
		{
			name: "any connected wins",
			statuses: map[string]string{
				"card0-HDMI-A-1": "disconnected",
				"card1-DP-1":     "connected",
			},
			want: true,
		},
		{
			name: "all disconnected",
			statuses: map[string]string{
				"card0-HDMI-A-1": "disconnected",
				"card1-DP-1":     "disconnected",
			},
			want: false,
		},
		{
			// FF1's amdgpu persistently reads "unknown" on empty connectors, so
			// "unknown" must count as no-display — same semantics as
			// feral-watchdog's isDisplayConnected.
			name:     "readable unknown counts as no display",
			statuses: map[string]string{"card0-HDMI-A-1": "unknown"},
			want:     false,
		},
		{
			name: "connected wins even after unknown",
			statuses: map[string]string{
				"card0-HDMI-A-1": "unknown",
				"card1-DP-1":     "connected",
			},
			want: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			root := drmRootWithStatuses(t, tc.statuses)
			if got := DisplayConnected(root); got != tc.want {
				t.Fatalf("DisplayConnected = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestDisplayConnectedFailsOpen pins the fail-open cases: only a sysfs layout
// with NO readable connector status at all (no connectors, a missing root, or
// every status unreadable) is treated as "display connected" so an
// unrecognized environment keeps pre-gate behavior.
func TestDisplayConnectedFailsOpen(t *testing.T) {
	t.Run("empty root - no connectors", func(t *testing.T) {
		if !DisplayConnected(t.TempDir()) {
			t.Fatal("empty sysfs root must fail open to connected")
		}
	})

	t.Run("missing root", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if !DisplayConnected(missing) {
			t.Fatal("missing sysfs root must fail open to connected")
		}
	})

	t.Run("connector present but status unreadable", func(t *testing.T) {
		root := t.TempDir()
		// Create the status path as a directory so ReadFile fails: with zero
		// readable statuses the environment is unrecognized -> fail open.
		if err := os.MkdirAll(filepath.Join(root, "card0-HDMI-A-1", "status"), 0o750); err != nil {
			t.Fatalf("failed to set up unreadable status: %v", err)
		}
		if !DisplayConnected(root) {
			t.Fatal("unreadable connector status must fail open to connected")
		}
	})

	t.Run("unreadable connector alongside readable disconnected", func(t *testing.T) {
		root := drmRootWithStatuses(t, map[string]string{"card0-HDMI-A-1": "disconnected"})
		if err := os.MkdirAll(filepath.Join(root, "card1-DP-1", "status"), 0o750); err != nil {
			t.Fatalf("failed to set up unreadable status: %v", err)
		}
		if DisplayConnected(root) {
			t.Fatal("unreadable connector alongside readable disconnected must count as no display")
		}
	})
}
