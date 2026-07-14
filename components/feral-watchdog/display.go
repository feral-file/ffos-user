package main

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultDRMSysfsRoot is the sysfs directory that exposes DRM connector status
// files. It is the same source display-restore.sh consults
// (/sys/class/drm/card*-*/status), so the watchdog's notion of "is a display
// attached" stays consistent with the kiosk's display-bring-up path.
const defaultDRMSysfsRoot = "/sys/class/drm"

// isDisplayConnected reports whether at least one DRM connector reads
// "connected", using the same signal as display-restore.sh. The sysfs root is a
// parameter so tests can point it at a fixture directory.
//
// FAIL OPEN is the load-bearing invariant here: suppressing Chromium hang
// escalation is only safe when every connector positively reads
// "disconnected". Anything else — no connectors exposed, an unreadable status
// file, or a readable value the kernel could not resolve (DRM reports
// "unknown" for unprobeable connectors) — leaves the overall state not
// positively known, so we return true ("display connected") and preserve all
// existing failure detection. A hardware/CI environment whose sysfs layout or
// status vocabulary differs must never silently disable the watchdog.
func isDisplayConnected(sysfsRoot string) bool {
	matches, err := filepath.Glob(filepath.Join(sysfsRoot, "card*-*", "status"))
	if err != nil || len(matches) == 0 {
		// Unknown state (bad glob or no connectors exposed) -> fail open.
		return true
	}

	allKnownDisconnected := true
	for _, statusFile := range matches {
		data, err := os.ReadFile(statusFile) // #nosec G304 -- path comes from a fixed sysfs glob.
		if err != nil {
			// This connector's state is unknown; a later connector may still read
			// "connected", so keep scanning instead of returning immediately.
			allKnownDisconnected = false
			continue
		}
		switch strings.TrimSpace(string(data)) {
		case "connected":
			return true
		case "disconnected":
			// Positively known-absent; keep scanning the remaining connectors.
		default:
			// Readable but not a positive reading (e.g. DRM "unknown") -> the
			// overall state cannot be declared known-disconnected.
			allKnownDisconnected = false
		}
	}

	return !allKnownDisconnected
}
