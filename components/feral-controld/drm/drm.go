// Package drm reports whether a physical display is attached, using DRM
// connector status from sysfs. controld uses it to keep periodic
// display-hardware work (ddcutil polling) quiet on headless devices.
package drm

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// DefaultSysfsRoot is the sysfs directory that exposes DRM connector status
// files. It is the same source feral-watchdog's display.go and the kiosk's
// wait_for_display (users/feralfile/scripts/start-kiosk.sh) consult, so
// controld's notion of "is a display attached" stays consistent with the
// display-bring-up and watchdog-suppression paths.
const DefaultSysfsRoot = "/sys/class/drm"

// DisplayConnected reports whether at least one DRM connector reads
// "connected". The sysfs root is a parameter so tests can point it at a
// fixture directory.
//
// A display counts as connected ONLY on a positive "connected" reading: FF1's
// amdgpu persistently reports "unknown" on connectors with nothing attached,
// so "unknown" must count as no-display or headless suppression never engages.
// Attaching a real monitor raises HPD and flips its connector to "connected".
//
// FAIL OPEN remains only for the case where no connector status is readable at
// all (no DRM sysfs, or every status file unreadable): there we return true so
// a hardware/CI environment whose sysfs layout differs behaves as before (the
// caller keeps doing its display work). This must stay in lockstep with
// feral-watchdog's isDisplayConnected and start-kiosk.sh's wait_for_display —
// if the gates diverge, one side suppresses while another escalates.
func DisplayConnected(sysfsRoot string) bool {
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

// Fingerprint returns a stable identity string for the currently attached
// display topology: every DRM connector's status plus a short hash of its
// EDID. It changes when a display is plugged, unplugged, or swapped for a
// different panel, and stays stable across DPMS/standby (connector status
// does not change there). Cheap enough to call on every poll tick — a
// handful of small sysfs reads.
//
// Callers compare successive values only; the string itself is opaque. An
// environment with no connectors (or an unreadable sysfs) yields "", which is
// simply another stable value — it still flips when connectors appear.
func Fingerprint(sysfsRoot string) string {
	matches, err := filepath.Glob(filepath.Join(sysfsRoot, "card*-*", "status"))
	if err != nil || len(matches) == 0 {
		return ""
	}
	sort.Strings(matches)

	var b strings.Builder
	for _, statusFile := range matches {
		dir := filepath.Dir(statusFile)

		status := "unreadable"
		if data, err := os.ReadFile(statusFile); err == nil { // #nosec G304 -- path comes from a fixed sysfs glob.
			status = strings.TrimSpace(string(data))
		}

		// EDID identifies the concrete panel, so swapping monitor A for
		// monitor B changes the fingerprint even if both read "connected".
		// Only read it for connected connectors: the others have no useful
		// EDID, and skipping them halves the per-tick sysfs reads. A swap
		// that never shows a non-connected sample still changes the EDID on
		// the connected connector, so detection is not weakened.
		edid := "-"
		if status == "connected" {
			if data, err := os.ReadFile(filepath.Join(dir, "edid")); err == nil && len(data) > 0 { // #nosec G304 -- fixed sysfs layout.
				sum := sha256.Sum256(data)
				edid = hex.EncodeToString(sum[:8])
			}
		}

		fmt.Fprintf(&b, "%s=%s:%s;", filepath.Base(dir), status, edid)
	}
	return b.String()
}
