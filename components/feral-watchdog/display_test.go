package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsDisplayConnected(t *testing.T) {
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
			// "unknown" must count as no-display or headless devices keep the
			// full escalation path live and restart-storm (field regression of
			// the earlier fail-open-on-unknown behavior from the PR #218
			// review). Hotplug flips the connector to "connected" via HPD, so
			// no real display is masked by this.
			name:     "readable unknown counts as no display",
			statuses: map[string]string{"card0-HDMI-A-1": "unknown"},
			want:     false,
		},
		{
			name: "unknown alongside disconnected counts as no display",
			statuses: map[string]string{
				"card0-HDMI-A-1": "disconnected",
				"card1-DP-1":     "unknown",
			},
			want: false,
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
			if got := isDisplayConnected(root); got != tc.want {
				t.Fatalf("isDisplayConnected = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsDisplayConnectedFailsOpen pins the remaining fail-open cases: only a
// sysfs layout with NO readable connector status at all (no connectors, a
// missing root, or every status unreadable) is treated as "display connected"
// so escalation is never silently disabled on an unrecognized environment.
func TestIsDisplayConnectedFailsOpen(t *testing.T) {
	t.Run("empty root - no connectors", func(t *testing.T) {
		if !isDisplayConnected(t.TempDir()) {
			t.Fatal("empty sysfs root must fail open to connected")
		}
	})

	t.Run("missing root", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist")
		if !isDisplayConnected(missing) {
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
		if !isDisplayConnected(root) {
			t.Fatal("unreadable connector status must fail open to connected")
		}
	})

	// One unreadable connector next to a readable "disconnected" one no longer
	// fails open: readable statuses exist and none reads "connected", which is
	// the same headless shape as the FF1 "unknown" readings. Fail-open here
	// would reopen the headless restart storm on any box with a single flaky
	// sysfs read.
	t.Run("unreadable connector alongside readable disconnected", func(t *testing.T) {
		root := drmRootWithStatuses(t, map[string]string{"card0-HDMI-A-1": "disconnected"})
		if err := os.MkdirAll(filepath.Join(root, "card1-DP-1", "status"), 0o750); err != nil {
			t.Fatalf("failed to set up unreadable status: %v", err)
		}
		if isDisplayConnected(root) {
			t.Fatal("unreadable connector alongside readable disconnected must count as no display")
		}
	})
}
