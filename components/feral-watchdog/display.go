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
// A display counts as connected ONLY on a positive "connected" reading. An
// earlier revision failed open on any "unknown" reading, but FF1's amdgpu
// persistently reports "unknown" on connectors with nothing attached, so on
// real headless hardware the headless suppression never engaged: the watchdog
// kept escalating "Chromium never came up" through kiosk restarts into the
// 3-restarts-in-5-minutes reboot budget, compounding the kiosk-side restart
// storm (start-kiosk.sh's display wait failed open the same way). Treating
// "unknown" as no-display is safe because attaching a real monitor raises HPD
// and flips its connector to "connected" before Chromium could possibly be
// expected up.
//
// FAIL OPEN remains only for the case where no connector status is readable at
// all (no DRM sysfs, or every status file unreadable): there we return true so
// a hardware/CI environment whose sysfs layout differs never silently disables
// the watchdog. This must stay in lockstep with wait_for_display in
// users/feralfile/scripts/start-kiosk.sh AND with DisplayConnected in
// components/feral-controld/drm/drm.go (controld is a separate Go module, so
// it carries its own copy) — if the gates diverge, one side suppresses while
// another escalates and headless devices restart-loop or flood ddcutil.
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
			// Skip, but only fail open if NOTHING ends up readable: a later
			// connector may still give a positive reading either way.
			continue
		}
		sawReadable = true
		if strings.TrimSpace(string(data)) == "connected" {
			return true
		}
	}

	// Readable statuses existed and none read "connected" -> headless
	// ("disconnected" and "unknown" alike). No readable status at all -> the
	// environment is unknown, fail open.
	return !sawReadable
}
