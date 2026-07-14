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
			// PR #218 review: DRM reports "unknown" for connectors it cannot
			// probe. That is not a positive "no display" reading, so it must
			// fail open instead of suppressing Chromium escalation.
			name:     "readable unknown fails open",
			statuses: map[string]string{"card0-HDMI-A-1": "unknown"},
			want:     true,
		},
		{
			name: "unknown alongside disconnected fails open",
			statuses: map[string]string{
				"card0-HDMI-A-1": "disconnected",
				"card1-DP-1":     "unknown",
			},
			want: true,
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

// TestIsDisplayConnectedFailsOpen pins the fail-open invariant for every
// unknown-state case: no connectors, a missing root, and an unreadable status
// file must all be treated as "display connected" so escalation is preserved.
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
		// Create the status path as a directory so ReadFile fails: the only
		// connector is unreadable, so the state is unknown -> fail open.
		if err := os.MkdirAll(filepath.Join(root, "card0-HDMI-A-1", "status"), 0o750); err != nil {
			t.Fatalf("failed to set up unreadable status: %v", err)
		}
		if !isDisplayConnected(root) {
			t.Fatal("unreadable connector status must fail open to connected")
		}
	})

	// PR #218 review: one readable "disconnected" connector next to an
	// unreadable one is NOT a positively known overall state — the unreadable
	// connector could be the one with the display.
	t.Run("unreadable connector alongside readable disconnected", func(t *testing.T) {
		root := drmRootWithStatuses(t, map[string]string{"card0-HDMI-A-1": "disconnected"})
		if err := os.MkdirAll(filepath.Join(root, "card1-DP-1", "status"), 0o750); err != nil {
			t.Fatalf("failed to set up unreadable status: %v", err)
		}
		if !isDisplayConnected(root) {
			t.Fatal("mixed unreadable + disconnected connectors must fail open to connected")
		}
	})
}
