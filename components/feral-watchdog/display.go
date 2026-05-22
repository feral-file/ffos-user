package main

import (
	"os"
	"path/filepath"
	"strings"
)

const defaultDRMStatusGlob = "/sys/class/drm/card*-*/status"

// DisplayState is the watchdog's view of physical monitor presence.
// Unknown is intentionally fail-open: if sysfs cannot be read, recovery policy
// should keep the existing Chromium checks rather than hide real startup faults.
type DisplayState struct {
	Connected bool
	Known     bool
}

type DisplayDetector interface {
	State() DisplayState
}

type sysfsDisplayDetector struct {
	statusGlob string
}

func NewSysfsDisplayDetector() DisplayDetector {
	return &sysfsDisplayDetector{statusGlob: defaultDRMStatusGlob}
}

func (d *sysfsDisplayDetector) State() DisplayState {
	matches, err := filepath.Glob(d.statusGlob)
	if err != nil || len(matches) == 0 {
		return DisplayState{Known: false}
	}

	readAny := false
	for _, path := range matches {
		// #nosec G304 -- watchdog intentionally reads kernel DRM status files from
		// a fixed glob; tests inject temp-dir paths through the unexported struct.
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		readAny = true
		switch strings.TrimSpace(string(b)) {
		case "connected":
			return DisplayState{Known: true, Connected: true}
		case "disconnected":
			continue
		default:
			return DisplayState{Known: false}
		}
	}

	if !readAny {
		return DisplayState{Known: false}
	}
	return DisplayState{Known: true, Connected: false}
}
