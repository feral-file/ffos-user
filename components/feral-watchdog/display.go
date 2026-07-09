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
// FAIL OPEN is the load-bearing invariant here: if the glob matches nothing or
// every status file is unreadable, the connector state is unknown and we return
// true ("display connected"). Only a display we can positively read as
// disconnected suppresses Chromium hang escalation; an unknown state must
// preserve all existing failure detection, so a hardware/CI environment whose
// sysfs layout differs never silently disables the watchdog.
func isDisplayConnected(sysfsRoot string) bool {
	matches, err := filepath.Glob(filepath.Join(sysfsRoot, "card*-*", "status"))
	if err != nil || len(matches) == 0 {
		// Unknown state (bad glob or no connectors exposed) -> fail open.
		return true
	}

	sawReadable := false
	for _, statusFile := range matches {
		data, err := os.ReadFile(statusFile) // #nosec G304 -- path comes from a fixed sysfs glob.
		if err != nil {
			continue
		}
		sawReadable = true
		if strings.TrimSpace(string(data)) == "connected" {
			return true
		}
	}

	// If we could not read any status file the state is unknown -> fail open.
	// Only when every connector was readable and none reported "connected" do we
	// declare the display KNOWN-disconnected.
	if !sawReadable {
		return true
	}
	return false
}
