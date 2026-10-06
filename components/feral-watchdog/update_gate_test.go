package main

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// lockKey renders lockPath the way /proc/locks names it, for fixtures.
func lockKey(t *testing.T, lockPath string) string {
	t.Helper()
	key, err := procLocksKey(lockPath)
	if err != nil {
		t.Fatalf("stat lock fixture: %v", err)
	}
	return key
}

// writeLocksTable writes a /proc/locks fixture and returns its path.
func writeLocksTable(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "locks")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("write locks fixture: %v", err)
	}
	return path
}

// newLockFile creates an empty file standing in for /run/feral-updater.lock.
func newLockFile(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "feral-updater.lock")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create lock fixture: %v", err)
	}
	return path
}

// simulateUpdate makes updateInProgress report true for the rest of the
// test, by pointing the gate at a lock file and a /proc/locks fixture in
// which that file is flock-held. It returns a function that ends the update.
func simulateUpdate(t *testing.T) (end func()) {
	t.Helper()
	lock := newLockFile(t)
	prevLock, prevLocks := updaterLockFile, procLocksFile
	updaterLockFile = lock
	setUpdaterHolder(t, "4242")
	t.Cleanup(func() { updaterLockFile, procLocksFile = prevLock, prevLocks })
	return func() { procLocksFile = writeLocksTable(t) }
}

// setUpdaterHolder rewrites the /proc/locks fixture so the lock simulateUpdate
// installed is held by pid (a new updater run).
func setUpdaterHolder(t *testing.T, pid string) {
	t.Helper()
	procLocksFile = writeLocksTable(t, "1: FLOCK  ADVISORY  WRITE "+pid+" "+lockKey(t, updaterLockFile)+" 0 EOF")
}

func TestFlockLineHolder(t *testing.T) {
	const key = "00:1a:5678"
	tests := []struct {
		name    string
		line    string
		wantPID string
		wantOK  bool
	}{
		{"held write flock", "1: FLOCK  ADVISORY  WRITE 1234 00:1a:5678 0 EOF", "1234", true},
		{"held read flock", "3: FLOCK  ADVISORY  READ  1234 00:1a:5678 0 EOF", "1234", true},
		{"blocked waiter is not a holder", "1: -> FLOCK  ADVISORY  WRITE 4321 00:1a:5678 0 EOF", "", false},
		{"posix lock on same file", "2: POSIX  ADVISORY  WRITE 1234 00:1a:5678 0 EOF", "", false},
		{"ofd lock on same file", "2: OFDLCK ADVISORY  WRITE -1 00:1a:5678 0 EOF", "", false},
		{"other inode", "1: FLOCK  ADVISORY  WRITE 1234 00:1a:56789 0 EOF", "", false},
		{"other device", "1: FLOCK  ADVISORY  WRITE 1234 08:01:5678 0 EOF", "", false},
		{"short line", "1: FLOCK", "", false},
		{"empty", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			pid, ok := flockLineHolder(tc.line, key)
			if pid != tc.wantPID || ok != tc.wantOK {
				t.Fatalf("flockLineHolder(%q) = %q, %v, want %q, %v", tc.line, pid, ok, tc.wantPID, tc.wantOK)
			}
		})
	}
}

func TestFlockHolder(t *testing.T) {
	lock := newLockFile(t)
	key := lockKey(t, lock)

	t.Run("missing lock file is not an update", func(t *testing.T) {
		_, held, err := flockHolder(filepath.Join(t.TempDir(), "absent.lock"), writeLocksTable(t))
		if err != nil || held {
			t.Fatalf("got held=%v err=%v, want false, nil", held, err)
		}
	})
	t.Run("held among other locks", func(t *testing.T) {
		locks := writeLocksTable(t,
			"1: POSIX  ADVISORY  WRITE 99 00:05:1 0 EOF",
			"2: FLOCK  ADVISORY  WRITE 4242 "+key+" 0 EOF")
		pid, held, err := flockHolder(lock, locks)
		if err != nil || !held || pid != "4242" {
			t.Fatalf("got pid=%q held=%v err=%v, want 4242, true, nil", pid, held, err)
		}
	})
	t.Run("only a waiter", func(t *testing.T) {
		_, held, err := flockHolder(lock, writeLocksTable(t, "1: -> FLOCK  ADVISORY  WRITE 4242 "+key+" 0 EOF"))
		if err != nil || held {
			t.Fatalf("got held=%v err=%v, want false, nil", held, err)
		}
	})
	t.Run("unreadable locks table is an error", func(t *testing.T) {
		if _, _, err := flockHolder(lock, filepath.Join(t.TempDir(), "absent")); err == nil {
			t.Fatal("want error for unreadable locks table")
		}
	})
}

// TestFlockHeldAgainstKernel checks the parser against the real /proc/locks
// format with a real flock, so a kernel format drift fails here rather than
// silently disabling the gate on devices.
func TestFlockHeldAgainstKernel(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc/locks is Linux-only")
	}
	lock := newLockFile(t)
	f, err := os.Open(lock) // #nosec G304 -- path is inside t.TempDir.
	if err != nil {
		t.Fatalf("open lock: %v", err)
	}
	defer func() { _ = f.Close() }()

	if _, held, err := flockHolder(lock, "/proc/locks"); err != nil || held {
		t.Fatalf("before flock: held=%v err=%v", held, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX); err != nil {
		t.Fatalf("flock: %v", err)
	}
	if _, held, err := flockHolder(lock, "/proc/locks"); err != nil || !held {
		t.Fatalf("while held: held=%v err=%v", held, err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_UN); err != nil {
		t.Fatalf("unlock: %v", err)
	}
	if _, held, err := flockHolder(lock, "/proc/locks"); err != nil || held {
		t.Fatalf("after unlock: held=%v err=%v", held, err)
	}
}

// TestRebootDeferredDuringUpdate pins the choke point every reboot path goes
// through (RAM, GPU, disk, Chromium): no `systemctl reboot` while the updater
// lock is held, and the normal reboot once it is released.
func TestRebootDeferredDuringUpdate(t *testing.T) {
	_, rebootFile := installCountingSystemctlWithReboot(t)
	handler := NewCommandHandler(zap.NewNop(), nil)
	endUpdate := simulateUpdate(t)

	handler.rebootSystem(context.Background(), CrashReasonDiskFull)
	handler.rebootSystem(context.Background(), CrashReasonDiskFull)
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("reboot must be deferred during an update, got %s", got)
	}

	endUpdate()
	handler.rebootSystem(context.Background(), CrashReasonDiskFull)
	if got := readRestartCount(t, rebootFile); got != "1" {
		t.Fatalf("reboot must proceed once the update ends, got %s", got)
	}
}

// TestChromiumMonitorUpdateSuppressesEscalation pins the Chromium side of
// ffos#124: with the grace expired and the budget one restart from
// exhaustion, an update's starved probes cause no restart, no fallback and
// no reboot and accumulate no budget; when the update ends without a reboot
// the monitor grants a fresh grace instead of restarting on stale state.
func TestChromiumMonitorUpdateSuppressesEscalation(t *testing.T) {
	restartFile, stopFile, fallbackFile, rebootFile := installFallbackStubs(t)
	monitor := NewChromiumMonitor(closedLocalHTTPEndpoint(t), zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.restartHistory = []time.Time{time.Now(), time.Now()}
	monitor.mu.Unlock()
	endUpdate := simulateUpdate(t)

	for i := 0; i < 5; i++ {
		err := monitor.check(context.Background())
		if err == nil || !strings.Contains(err.Error(), errChromiumHeadless.Error()) {
			t.Fatalf("check %d: failure during an update must be tagged expected, got %v", i, err)
		}
	}
	for name, f := range map[string]string{"restart": restartFile, "stop": stopFile, "fallback": fallbackFile, "reboot": rebootFile} {
		if got := readRestartCount(t, f); got != "0" {
			t.Fatalf("an update must suppress %s, got %s", name, got)
		}
	}
	monitor.mu.Lock()
	updating, history := monitor.updating, len(monitor.restartHistory)
	monitor.mu.Unlock()
	if !updating || history != 2 {
		t.Fatalf("want updating latched and history untouched, got updating=%v history=%d", updating, history)
	}

	endUpdate()
	_ = monitor.check(context.Background())
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("the end of an update must grant a fresh grace, got %s restarts", got)
	}
	monitor.mu.Lock()
	updating, sinceStart := monitor.updating, time.Since(monitor.monitorStart)
	monitor.mu.Unlock()
	if updating || sinceStart > CHROMIUM_STARTUP_GRACE {
		t.Fatalf("want latch cleared and grace re-anchored, got updating=%v elapsed=%v", updating, sinceStart)
	}
}

// TestChromiumMonitorUpdateDefersFallbackHoldReboot pins the hold branch: an
// update that starts while the fallback screen is up must not be killed by
// the hold's reboot; the hold restarts in full once the update ends.
func TestChromiumMonitorUpdateDefersFallbackHoldReboot(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	monitor := expiredHoldMonitor(t, closedLocalHTTPEndpoint(t))
	monitor.commandHandler.fallbackShown = true
	endUpdate := simulateUpdate(t)

	_ = monitor.check(context.Background())
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("the hold's reboot must wait for the update, got %s", got)
	}

	endUpdate()
	_ = monitor.check(context.Background())
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("a full hold must run again after the update, got %s reboots", got)
	}
	monitor.mu.Lock()
	held := time.Since(monitor.fallbackSince)
	monitor.mu.Unlock()
	if held >= CHROMIUM_FALLBACK_HOLD {
		t.Fatalf("want the hold re-anchored by the update, held %v", held)
	}
}

// TestUpdateGateCeiling pins C1 of the ffos#124 review: one updater process
// may suppress recovery for at most UPDATE_GATE_MAX_HOLD, after which the
// gate fails open with a single Error; a new updater process (new holder
// PID) gets a fresh allowance, and releasing the lock resets everything.
func TestUpdateGateCeiling(t *testing.T) {
	core, logs := observer.New(zapcore.ErrorLevel)
	gate := newUpdateGate(zap.New(core))
	now := time.Unix(1_700_000_000, 0)
	gate.now = func() time.Time { return now }
	end := simulateUpdate(t)

	steps := []struct {
		name    string
		advance time.Duration
		holder  string // "" keeps the current holder
		release bool
		want    bool
		errors  int
	}{
		{name: "fresh hold", want: true},
		{name: "just under the ceiling", advance: UPDATE_GATE_MAX_HOLD - time.Second, want: true},
		{name: "ceiling reached fails open", advance: time.Second, want: false, errors: 1},
		{name: "expired hold stays open, logged once", advance: time.Hour, want: false, errors: 1},
		{name: "new updater process restarts the clock", holder: "5151", want: true, errors: 1},
		{name: "release", release: true, want: false, errors: 1},
		{name: "same pid after release is a new hold", holder: "5151", want: true, errors: 1},
	}
	for _, st := range steps {
		now = now.Add(st.advance)
		if st.holder != "" {
			setUpdaterHolder(t, st.holder)
		}
		if st.release {
			end()
		}
		if got := gate.active(); got != st.want {
			t.Fatalf("%s: active() = %v, want %v", st.name, got, st.want)
		}
		if got := logs.Len(); got != st.errors {
			t.Fatalf("%s: %d Error logs, want %d", st.name, got, st.errors)
		}
	}
}

// TestDiskPolicySitsOutUpdate pins C2/A3: during an update a disk past the
// critical line after cleanup neither reboots, nor cleans the pacman cache
// (it can race a pacman update), nor logs an Error every tick; once the
// update ends the ladder resumes and reboots.
func TestDiskPolicySitsOutUpdate(t *testing.T) {
	_, rebootFile := installCountingSystemctlWithReboot(t)
	core, logs := observer.New(zapcore.ErrorLevel)
	handler := NewCommandHandler(zap.NewNop(), nil)
	disk := NewDiskHandler(zap.New(core), handler)
	disk.isCleaned = true
	full := &SysMetrics{Disk: DiskMetrics{TotalCapacity: 100, UsedCapacity: 97}}
	end := simulateUpdate(t)

	for i := 0; i < 5; i++ {
		disk.checkDiskUsage(context.Background(), full)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("disk reboot must wait for the update, got %s", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("disk policy must stay quiet during an update, got %d Error logs", logs.Len())
	}

	end()
	disk.checkDiskUsage(context.Background(), full)
	if got := readRestartCount(t, rebootFile); got != "1" {
		t.Fatalf("disk reboot must proceed after the update, got %s", got)
	}
}

// TestRAMRebootWaitsForUpdate pins the RAM side of C2: the reboot step
// (critical again within RAM_REBOOT_DURATION_THRESHOLD of a kiosk restart)
// waits for the update without the per-tick Error lines.
func TestRAMRebootWaitsForUpdate(t *testing.T) {
	_, rebootFile := installCountingSystemctlWithReboot(t)
	core, logs := observer.New(zapcore.ErrorLevel)
	ram := NewMemoryHandler(zap.New(core), NewCommandHandler(zap.NewNop(), nil))
	ram.highMemoryMonitoring = true
	ram.highMemStartTime = time.Now().Add(-RAM_MONITOR_DURATION_THRESHOLD - time.Second)
	ram.lastKioskRestart = time.Now()
	full := &SysMetrics{Memory: MemoryMetrics{MaxCapacity: 100, UsedCapacity: 99}}
	simulateUpdate(t)

	for i := 0; i < 3; i++ {
		ram.checkMemoryUsage(context.Background(), full)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("RAM reboot must wait for the update, got %s", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("RAM policy must not log Error per tick while waiting, got %d", logs.Len())
	}
}

// TestChromiumMonitorDevConsoleThenUpdate pins the hand-off between the two
// latches: the developer console wins while it is up, leaving it during an
// update hands over to the update latch (no restart), and only the end of
// the update re-anchors the grace.
func TestChromiumMonitorDevConsoleThenUpdate(t *testing.T) {
	restartFile, _, _, _ := installFallbackStubs(t)
	monitor := NewChromiumMonitor(closedLocalHTTPEndpoint(t), zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty2")
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.mu.Unlock()
	end := simulateUpdate(t)

	_ = monitor.check(context.Background())
	monitor.mu.Lock()
	dev, upd := monitor.devConsole, monitor.updating
	monitor.mu.Unlock()
	if !dev || upd {
		t.Fatalf("developer console must win: devConsole=%v updating=%v", dev, upd)
	}

	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	_ = monitor.check(context.Background())
	monitor.mu.Lock()
	upd = monitor.updating
	monitor.mu.Unlock()
	if !upd {
		t.Fatal("leaving the console mid-update must latch the update gate")
	}

	end()
	_ = monitor.check(context.Background())
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("no restart across the hand-off, got %s", got)
	}
	monitor.mu.Lock()
	dev, upd = monitor.devConsole, monitor.updating
	sinceStart := time.Since(monitor.monitorStart)
	monitor.mu.Unlock()
	if dev || upd || sinceStart > CHROMIUM_STARTUP_GRACE {
		t.Fatalf("want both latches cleared and a fresh grace, got dev=%v upd=%v elapsed=%v", dev, upd, sinceStart)
	}
}

// TestChromiumMonitorParkedHoldDuringUpdate pins that a device already parked
// on the fallback screen neither reboots nor leaves the park because an
// update ran.
func TestChromiumMonitorParkedHoldDuringUpdate(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	monitor := expiredHoldMonitor(t, closedLocalHTTPEndpoint(t))
	monitor.commandHandler.fallbackShown = true
	_ = monitor.check(context.Background()) // parks

	end := simulateUpdate(t)
	_ = monitor.check(context.Background())
	end()
	_ = monitor.check(context.Background())

	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("a parked device must not reboot around an update, got %s", got)
	}
	monitor.mu.Lock()
	holding := !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if !holding || !monitor.commandHandler.fallbackShown {
		t.Fatal("the fallback screen must stay up across the update")
	}
}
