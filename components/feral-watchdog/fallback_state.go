package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// chromiumFallbackStateFile persists how many consecutive boots ended in a
// fallback-hold reboot (ffos-user#254). It lives with the other daemon state
// under /home/feralfile/.state (created by the image installer, owned by the
// feralfile user the watchdog runs as). A package var rather than a constant
// so tests redirect it in TestMain and never touch the host.
var chromiumFallbackStateFile = "/home/feralfile/.state/watchdog-chromium-fallback.json"

// chromiumFallbackState is the on-disk record. Only the counter drives policy;
// UpdatedAt is there for whoever reads the file on a bench device.
type chromiumFallbackState struct {
	ConsecutiveReboots int       `json:"consecutive_reboots"`
	UpdatedAt          time.Time `json:"updated_at"`
}

// loadChromiumFallbackReboots returns the persisted counter. A missing file is
// the normal state (no fallback reboot since Chromium was last healthy) and
// reads as zero. An unreadable or corrupt file also reads as zero, but with
// the error returned so the caller can log it: failing toward zero means the
// device keeps the pre-#254 self-heal reboot rather than parking on the error
// screen on the strength of a record it cannot trust.
func loadChromiumFallbackReboots(path string) (int, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- fixed daemon state path, test-overridable only.
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("read chromium fallback state %s: %w", path, err)
	}
	var s chromiumFallbackState
	if err := json.Unmarshal(data, &s); err != nil {
		return 0, fmt.Errorf("parse chromium fallback state %s: %w", path, err)
	}
	if s.ConsecutiveReboots < 0 {
		return 0, fmt.Errorf("chromium fallback state %s: negative counter %d", path, s.ConsecutiveReboots)
	}
	return s.ConsecutiveReboots, nil
}

// storeChromiumFallbackReboots writes the counter via tmp+fsync+rename. It is
// called immediately before `systemctl reboot`; a clean reboot syncs
// filesystems anyway, but the file and directory fsyncs keep the increment if
// the reboot turns into a hard reset, and the rename means a torn write can
// never leave a half-written record for the next boot to parse.
func storeChromiumFallbackReboots(path string, n int) error {
	data, err := json.Marshal(chromiumFallbackState{ConsecutiveReboots: n, UpdatedAt: time.Now().UTC()})
	if err != nil {
		return fmt.Errorf("encode chromium fallback state: %w", err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temp chromium fallback state: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write chromium fallback state: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync chromium fallback state: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close chromium fallback state: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename chromium fallback state into place: %w", err)
	}
	// fsync the directory so the rename itself survives a hard reset. A
	// failure here is logged by the caller but is on the safe side: a lost
	// rename reads back as "no file" (zero), i.e. one more reboot, not a park.
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open chromium fallback state dir: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync chromium fallback state dir: %w", err)
	}
	return nil
}

// clearChromiumFallbackReboots removes the record once Chromium has proved
// healthy. Removing rather than writing zero keeps "no file" as the one
// healthy state.
func clearChromiumFallbackReboots(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove chromium fallback state %s: %w", path, err)
	}
	return nil
}
