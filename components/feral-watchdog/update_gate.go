package main

import (
	"bufio"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

// updaterLockFile is the flock that ffos's feral-updater.sh holds (fd 9,
// exclusive) for the whole of every update it runs: the nightly timer
// (feral-updater@.service) and the boot OTA gate (controld's
// feral-updater-run@.service) both go through it, and it stays held across
// both children — feral-system-update.sh (full image) and
// feral-service-update.sh (pacman) — until each one has requested its
// reboot. That makes it the one signal that covers every update path.
// (`systemctl reboot` returns at once, so the lock is released while shutdown
// is still running; a gated reboot attempted in that gap is redundant, and
// the Chromium monitor's re-anchored grace outlasts the shutdown.)
//
// It is a package variable (not a const) so tests can point it at a fixture;
// TestMain points it at a path that does not exist.
//
// Cross-repo contract (ffos#124): the path and the "held for the whole
// update" property live in ffos (archiso-ff1/airootfs/root/scripts/
// feral-updater.sh). If that script moves the lock or stops holding it
// across its children, this gate goes silently inert — recovery would just
// behave as it did before the gate existed, never worse.
var updaterLockFile = "/run/feral-updater.lock"

// procLocksFile is the kernel's table of held file locks; a variable for the
// same test reason as updaterLockFile.
var procLocksFile = "/proc/locks"

// UPDATE_GATE_MAX_HOLD caps how long one update may suppress recovery. The
// lock is released only when the updater exits, and the updater can stay
// alive for a very long time without making progress: its API curl has no
// timeout, the final download attempt runs "however slow" (just over the
// 1 KB/s stall floor, a 1 GB ISO takes days), and an rsync or mount can hang
// in D-state. Without a cap a wedged update would leave a crashed kiosk on a
// black screen, a full disk unrebooted and a GPU hang ignored indefinitely.
// 8 h covers the slowest download the updater documents (~400 min for a
// 2-4 GB image at 1 Mbps) plus the install; past it the gate fails open with
// one Error (Sentry) so stuck updates are visible across the fleet.
const UPDATE_GATE_MAX_HOLD = 8 * time.Hour

// updateGate reports whether an OTA or package update currently holds
// updaterLockFile, so recovery actions can wait for it.
//
// Why it exists: an update is the one time this daemon's recovery actions do
// harm. The rsync/mkinitcpio IO storm can starve Chromium's /json/version
// probe long enough to walk the restart ladder, and a large ISO download can
// push the disk over DISK_CRITICAL_THRESHOLD; a reboot from either kills the
// update (a wasted multi-GB download, a latched OTA-gate failure, and a
// kiosk restart narrated over the update screen). The update ends in its own
// reboot anyway, which is the recovery we would have asked for.
//
// Why /proc/locks and not flock(LOCK_NB): probing by taking the lock, even
// for microseconds, can make the updater's own `flock -n` fail, and the
// updater treats that as "another instance is running" and skips the update.
// Reading /proc/locks never touches the lock. It is also exact: the kernel
// drops the entry when the updater exits or is killed, so a crashed update
// can never leave recovery suppressed.
//
// A hold is identified by the holder's PID, so the UPDATE_GATE_MAX_HOLD
// clock restarts for the next update (a new updater process) but not for a
// wedged one.
//
// Fail direction: any error (no lock file, unreadable /proc/locks) and an
// expired hold both read as "no update", which keeps the pre-gate recovery
// behavior.
type updateGate struct {
	logger *zap.Logger
	now    func() time.Time // injectable for the ceiling test

	mu      sync.Mutex
	holder  string    // PID of the hold being timed; "" when none is held
	since   time.Time // when holder was first seen
	expired bool      // the ceiling was hit for holder (logged once)
}

func newUpdateGate(logger *zap.Logger) *updateGate {
	return &updateGate{logger: logger, now: time.Now}
}

// active reports whether recovery should currently wait for an update.
func (g *updateGate) active() bool {
	pid, held, err := flockHolder(updaterLockFile, procLocksFile)

	g.mu.Lock()
	defer g.mu.Unlock()
	if err != nil || !held {
		g.holder, g.since, g.expired = "", time.Time{}, false
		return false
	}
	now := g.now()
	if pid != g.holder {
		g.holder, g.since, g.expired = pid, now, false
	}
	heldFor := now.Sub(g.since)
	if heldFor < UPDATE_GATE_MAX_HOLD {
		return true
	}
	if !g.expired {
		g.expired = true
		g.logger.Error("Update gate: updater has held its lock past the ceiling; resuming normal recovery",
			zap.String("updater_pid", pid),
			zap.Duration("held", heldFor),
			zap.Duration("ceiling", UPDATE_GATE_MAX_HOLD))
	}
	return false
}

// flockHolder reports whether some process holds a flock(2) lock on lockPath,
// and which PID, according to the /proc/locks-format table at locksPath. A missing lockPath
// is not an error: the updater creates it on first run, so its absence just
// means no update has run since boot (/run is tmpfs).
func flockHolder(lockPath, locksPath string) (pid string, held bool, err error) {
	key, err := procLocksKey(lockPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("stat %s: %w", lockPath, err)
	}

	f, err := os.Open(locksPath) // #nosec G304 -- fixed /proc path, test-injectable.
	if err != nil {
		return "", false, fmt.Errorf("open %s: %w", locksPath, err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		if pid, ok := flockLineHolder(scanner.Text(), key); ok {
			return pid, true, nil
		}
	}
	if err := scanner.Err(); err != nil {
		return "", false, fmt.Errorf("read %s: %w", locksPath, err)
	}
	return "", false, nil
}

// flockLineHolder reports whether one /proc/locks line is a HELD flock on
// the file identified by key, and the holder's PID (field 5). Lines look like
//
//	1: FLOCK  ADVISORY  WRITE 1234 00:1a:5678 0 EOF
//	1: -> FLOCK  ADVISORY  WRITE 4321 00:1a:5678 0 EOF
//
// The "->" form is a process BLOCKED waiting for the lock, not holding it,
// so it must not count. POSIX/OFD locks on the same file are ignored: the
// updater uses flock. (feral-recovery-update.sh also flocks the file, for an
// instant, as its own "is an update running" probe; seen here that is a
// momentary update, which defers nothing that matters.)
func flockLineHolder(line, key string) (pid string, ok bool) {
	fields := strings.Fields(line)
	if len(fields) < 6 || fields[1] == "->" {
		return "", false
	}
	if fields[1] != "FLOCK" || fields[5] != key {
		return "", false
	}
	return fields[4], true
}
