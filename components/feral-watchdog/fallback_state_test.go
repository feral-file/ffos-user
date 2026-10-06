package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestMain points the persisted fallback-reboot counter at a path whose
// directory does not exist, for every test that does not opt in with
// useFallbackStateFile. Loads then read "no file" (zero) and stores fail, so
// tests that predate #254 see exactly the old reboot behavior and can never
// leak a counter into one another (or into /home/feralfile on a dev box).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "feral-watchdog-test-")
	if err != nil {
		panic(err)
	}
	chromiumFallbackStateFile = filepath.Join(dir, "absent", "watchdog-chromium-fallback.json")
	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

// useFallbackStateFile gives one test a real, writable counter path, seeded
// with n when n > 0. Must be called before NewChromiumMonitor, which loads it.
func useFallbackStateFile(t *testing.T, n int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "watchdog-chromium-fallback.json")
	if n > 0 {
		if err := storeChromiumFallbackReboots(path, n); err != nil {
			t.Fatalf("seed fallback state: %v", err)
		}
	}
	prev := chromiumFallbackStateFile
	chromiumFallbackStateFile = path
	t.Cleanup(func() { chromiumFallbackStateFile = prev })
	return path
}

func TestChromiumFallbackStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")

	if n, err := loadChromiumFallbackReboots(path); err != nil || n != 0 {
		t.Fatalf("missing file: want 0, nil; got %d, %v", n, err)
	}
	if err := storeChromiumFallbackReboots(path, 2); err != nil {
		t.Fatalf("store: %v", err)
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != 2 {
		t.Fatalf("after store: want 2, nil; got %d, %v", n, err)
	}
	if err := clearChromiumFallbackReboots(path); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("clear must remove the file, stat err=%v", err)
	}
	if err := clearChromiumFallbackReboots(path); err != nil {
		t.Fatalf("clearing an absent file must succeed: %v", err)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("store must not leave temp files behind, found %d entries", len(entries))
	}
}

// TestChromiumFallbackStateUntrustedReadsZero pins the fail direction: a
// record the watchdog cannot trust reads as zero (keep rebooting, the pre-#254
// self-heal) and surfaces an error for the log, rather than parking the device.
func TestChromiumFallbackStateUntrustedReadsZero(t *testing.T) {
	for name, content := range map[string]string{
		"corrupt":  "{not json",
		"negative": `{"consecutive_reboots":-1}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "state.json")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			n, err := loadChromiumFallbackReboots(path)
			if n != 0 || err == nil {
				t.Fatalf("want 0 and an error, got %d, %v", n, err)
			}
		})
	}
}

// expiredHoldMonitor builds a monitor on tty1 with a display attached whose
// fallback hold has already run its full CHROMIUM_FALLBACK_HOLD.
func expiredHoldMonitor(t *testing.T, endpoint string) *ChromiumMonitor {
	t.Helper()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-(CHROMIUM_FALLBACK_HOLD + time.Second))
	monitor.monitorStart = time.Now()
	monitor.mu.Unlock()
	return monitor
}

// TestChromiumMonitorFirstFallbackRebootIsPersisted pins the first half of
// #254: the hold that ends in a reboot records it on disk before rebooting,
// so the next boot knows a fresh boot has already been tried.
func TestChromiumMonitorFirstFallbackRebootIsPersisted(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	path := useFallbackStateFile(t, 0)
	monitor := expiredHoldMonitor(t, closedLocalHTTPEndpoint(t))

	_ = monitor.check(context.Background())

	if got := readRestartCount(t, rebootFile); got != "1" {
		t.Fatalf("first expired hold must reboot once, got %s", got)
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != 1 {
		t.Fatalf("reboot must be persisted as 1, got %d, %v", n, err)
	}
}

// TestChromiumMonitorParksAfterRebootCap pins the terminal state #254 asks
// for: when the previous boot already ended in a fallback reboot, an expired
// hold must NOT reboot again, must keep the hold (and the fallback screen)
// armed, and must stay that way on every later tick.
func TestChromiumMonitorParksAfterRebootCap(t *testing.T) {
	restartFile, _, fallbackFile, rebootFile := installFallbackStubs(t)
	path := useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	monitor := expiredHoldMonitor(t, closedLocalHTTPEndpoint(t))
	monitor.commandHandler.fallbackShown = true

	for i := 0; i < 5; i++ {
		if err := monitor.check(context.Background()); err == nil {
			t.Fatalf("check %d: expected failure against closed endpoint", i)
		}
	}

	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("parked monitor must never reboot, got %s", got)
	}
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("parked monitor must not restart the kiosk, got %s", got)
	}
	if got := readRestartCount(t, fallbackFile); got != "0" {
		t.Fatalf("parked monitor must not re-show the fallback, got %s", got)
	}
	monitor.mu.Lock()
	inHold, parked := !monitor.fallbackSince.IsZero(), monitor.fallbackParked
	monitor.mu.Unlock()
	if !inHold || !parked {
		t.Fatalf("want hold armed and parked, got inHold=%v parked=%v", inHold, parked)
	}
	if !monitor.commandHandler.isFallbackShown() {
		t.Fatal("fallback screen must stay owned so RAM/GPU restarts cannot erase it")
	}
	if n, _ := loadChromiumFallbackReboots(path); n != CHROMIUM_MAX_FALLBACK_REBOOTS {
		t.Fatalf("parking must not change the persisted count, got %d", n)
	}
}

// TestChromiumMonitorHealthyCheckClearsPersistedCount pins the way out: one
// successful check (OTA fixed it, operator restarted the kiosk) removes the
// record, so a later unrelated fault gets the reboot self-heal again.
func TestChromiumMonitorHealthyCheckClearsPersistedCount(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	path := useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := expiredHoldMonitor(t, endpoint)
	monitor.mu.Lock()
	monitor.fallbackParked = true
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("healthy check must remove the persisted count, stat err=%v", err)
	}
	monitor.mu.Lock()
	reboots, parked, inHold := monitor.fallbackReboots, monitor.fallbackParked, !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if reboots != 0 || parked || inHold {
		t.Fatalf("want counter 0, not parked, no hold; got %d, %v, %v", reboots, parked, inHold)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("recovery must not reboot, got %s", got)
	}
}

// TestChromiumMonitorPersistFailureStillReboots pins the fail direction of the
// write: a device that cannot record the reboot keeps the old self-heal
// rather than silently losing it.
func TestChromiumMonitorPersistFailureStillReboots(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	// Default TestMain path: its directory does not exist, so the store fails.
	monitor := expiredHoldMonitor(t, closedLocalHTTPEndpoint(t))

	_ = monitor.check(context.Background())

	if got := readRestartCount(t, rebootFile); got != "1" {
		t.Fatalf("a failed persist must still reboot, got %s", got)
	}
}

// TestChromiumMonitorParkSurvivesDisplayCycle pins the park across an unplug:
// a display removed while parked abandons the hold (headless never reboots),
// and the hold that follows a replug parks again instead of rebooting,
// because the persisted count is untouched by the unplug.
func TestChromiumMonitorParkSurvivesDisplayCycle(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	path := useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	monitor := expiredHoldMonitor(t, closedLocalHTTPEndpoint(t))
	monitor.commandHandler.fallbackShown = true
	connected := monitor.drmSysfsRoot

	_ = monitor.check(context.Background()) // parks

	monitor.drmSysfsRoot = disconnectedDRMRoot(t)
	_ = monitor.check(context.Background())
	monitor.mu.Lock()
	inHold, parked := !monitor.fallbackSince.IsZero(), monitor.fallbackParked
	monitor.mu.Unlock()
	if inHold || parked {
		t.Fatalf("unplug must abandon the hold and the park latch, got inHold=%v parked=%v", inHold, parked)
	}

	// Replug, then stand in for the ladder + a full hold having run again.
	monitor.drmSysfsRoot = connected
	monitor.mu.Lock()
	monitor.headless = false
	monitor.fallbackSince = time.Now().Add(-(CHROMIUM_FALLBACK_HOLD + time.Second))
	monitor.mu.Unlock()
	_ = monitor.check(context.Background())

	monitor.mu.Lock()
	parked = monitor.fallbackParked
	monitor.mu.Unlock()
	if !parked {
		t.Fatal("the hold after a replug must park again")
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("no reboot anywhere in an unplug/replug while parked, got %s", got)
	}
	if n, _ := loadChromiumFallbackReboots(path); n != CHROMIUM_MAX_FALLBACK_REBOOTS {
		t.Fatalf("display cycle must not change the persisted count, got %d", n)
	}
}

// TestChromiumMonitorParkWithDevConsoleNeverReboots pins the dev-console gate
// while parked: tty2 re-anchors the hold, and the return to tty1 followed by a
// full hold parks again rather than rebooting.
func TestChromiumMonitorParkWithDevConsoleNeverReboots(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	monitor := expiredHoldMonitor(t, closedLocalHTTPEndpoint(t))
	monitor.commandHandler.fallbackShown = true
	tty1 := monitor.ttyActiveFile

	_ = monitor.check(context.Background()) // parks

	monitor.ttyActiveFile = ttyActiveFixture(t, "tty2")
	_ = monitor.check(context.Background())
	monitor.mu.Lock()
	since := monitor.fallbackSince
	monitor.mu.Unlock()
	if since.IsZero() || time.Since(since) > time.Minute {
		t.Fatalf("dev console must keep the hold armed and re-anchored, fallbackSince=%v", since)
	}

	monitor.ttyActiveFile = tty1
	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-(CHROMIUM_FALLBACK_HOLD + time.Second))
	monitor.mu.Unlock()
	_ = monitor.check(context.Background())

	monitor.mu.Lock()
	parked := monitor.fallbackParked
	monitor.mu.Unlock()
	if !parked {
		t.Fatal("expected to stay parked after returning to tty1")
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("parked monitor must not reboot around a dev console, got %s", got)
	}
}
