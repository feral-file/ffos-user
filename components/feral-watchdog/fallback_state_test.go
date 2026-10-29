package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestMain points the persisted fallback-reboot counter (and the updater
// lock, see update_gate_test.go) at a path whose directory does not exist,
// for every test that does not opt in with useFallbackStateFile. Loads then read "no file" (zero) and stores fail, so
// tests that predate #254 see exactly the old reboot behavior and can never
// leak a counter into one another (or into /home/feralfile on a dev box).
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "feral-watchdog-test-")
	if err != nil {
		panic(err)
	}
	chromiumFallbackStateFile = filepath.Join(dir, "absent", "watchdog-chromium-fallback.json")
	// Same isolation for the update gate: no lock file means "no update", so
	// every test that does not call simulateUpdate sees the ungated ladder
	// regardless of what the CI host's /run and /proc/locks contain.
	updaterLockFile = filepath.Join(dir, "absent", "feral-updater.lock")
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

// TestChromiumMonitorPlainRestartHoldsStaleCountUntilConfirmed pins
// ffos-user#356's review history on this exact gate across two
// feralfile-bot passes: a stale persisted count from a prior, unrelated
// boot must survive a plain cold boot/restart (no fallback hold, no
// cdpStuck report yet) until controld has ACTUALLY confirmed CDP health —
// never merely because some local time elapsed with nothing reported.
// Elapsed-time alone was the bot's second-pass finding: cdphealth's own
// StuckThreshold timer is anchored to when CONTROLD first observes a
// disconnect, a clock with no shared epoch against this process's own
// monitorStart, so a local timeout can expire before controld's own report
// would ever have arrived. The fix waits for an explicit received signal
// (cdpReportReceived) instead — including cdphealth's own new
// first-ever-healthy confirmation for a device that was never unhealthy at
// all, which is exactly what this test simulates with SetCDPStuck(false).
func TestChromiumMonitorPlainRestartHoldsStaleCountUntilConfirmed(t *testing.T) {
	path := useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	// No fallbackSince, no cdpStuck report yet — a plain cold boot/restart,
	// not a recovery from the fallback screen and not a cdpStuck escalation.

	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != CHROMIUM_MAX_FALLBACK_REBOOTS {
		t.Fatalf("expected the stale persisted count to survive a success with no report received yet, got %d, %v", n, err)
	}

	// Still no report: even many more successful ticks must not forgive
	// the budget on elapsed time alone.
	for i := 0; i < 20; i++ {
		if err := monitor.check(context.Background()); err != nil {
			t.Fatalf("tick %d: expected success against ok endpoint, got %v", i, err)
		}
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != CHROMIUM_MAX_FALLBACK_REBOOTS {
		t.Fatalf("expected the stale persisted count to still survive after many ticks with no report received, got %d, %v", n, err)
	}

	// controld's cdphealth now sends its first-ever confirmation (a device
	// that was healthy the whole time, per cdphealth.go's everConfirmed).
	monitor.SetCDPStuck(false)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected the persisted file to be removed once an actual report confirmed health, stat err=%v", err)
	}
}

// TestChromiumMonitorNoTimeBasedEscapeForRebootCapEvenAfterLongDelay pins
// feralfile-bot's finding against an earlier version of this gate that DID
// have a time-based escape hatch (CHROMIUM_CDP_REPORT_FALLBACK_WINDOW,
// 10xCHROMIUM_STARTUP_GRACE): the bot reproduced that cdphealth's own emit()
// retries a failed Send indefinitely, so a transient D-Bus outage can delay
// controld's FIRST successful report arbitrarily far past any fixed window —
// indistinguishable, from this process's side, from cdphealth never
// existing at all. Any timeout long enough to avoid that false positive is
// also long enough to re-erase the cap during a genuinely sustained CDP
// failure that outlasts it. The fallback window was removed rather than
// re-tuned: there is no signal available to feral-watchdog that
// distinguishes "no report is coming" from "a report is coming, slowly".
// This test drives far more ticks than the old window would have tolerated
// and asserts the cap still survives every one of them, with the only exit
// an explicit SetCDPStuck call — no amount of elapsed silence forgives it.
func TestChromiumMonitorNoTimeBasedEscapeForRebootCapEvenAfterLongDelay(t *testing.T) {
	path := useFallbackStateFile(t, CHROMIUM_MAX_FALLBACK_REBOOTS)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	// No SetCDPStuck call at all, ever — simulating a feral-controld whose
	// cdphealth is either genuinely absent, or present but unable to get a
	// single Send through yet (a sustained D-Bus outage), both of which
	// look identical from here: pure silence.

	for i := 0; i < 50; i++ {
		if err := monitor.check(context.Background()); err != nil {
			t.Fatalf("tick %d: expected success against ok endpoint, got %v", i, err)
		}
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != CHROMIUM_MAX_FALLBACK_REBOOTS {
		t.Fatalf("expected the stale persisted count to survive 50 ticks of silence with no report ever received, got %d, %v", n, err)
	}

	// Simulate real elapsed wall-clock time far past what any plausible
	// fixed window would have tolerated (the removed fallback window was
	// 10xCHROMIUM_STARTUP_GRACE = 15 minutes; this is 10x that again) —
	// without mutating the clock directly, 50 fast ticks alone would never
	// exercise a time-based regression, since they execute in milliseconds.
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-100 * CHROMIUM_STARTUP_GRACE)
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != CHROMIUM_MAX_FALLBACK_REBOOTS {
		t.Fatalf("expected the stale persisted count to survive even after a huge simulated elapsed time with no report ever received, got %d, %v", n, err)
	}

	// The delayed report finally gets through: only now may the cap clear.
	monitor.SetCDPStuck(false)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected the persisted file to be removed once an actual report finally confirmed health, stat err=%v", err)
	}
}

// TestChromiumMonitorHeadlessExitDoesNotForgiveRebootCapWithoutConfirmation
// pins run-reviewer's own finding against the cdpReportReceived fix above
// (ffos-user#356): cdpStuck is cleared from three purely local sites, not
// only by an actual SetCDPStuck call — exiting a suppressed state in
// check()'s success path (wasSuppressed) is one of them. A stale
// cdpReportReceived=true plus that local clear used to satisfy
// canForgetRebootBudget too, forgiving the persisted cap off a display blip
// with no confirmation from controld at all. cdpConfirmedHealthy closes
// that: it is written only inside SetCDPStuck, so a local clear alone must
// not forgive the cap.
func TestChromiumMonitorHeadlessExitDoesNotForgiveRebootCapWithoutConfirmation(t *testing.T) {
	path := useFallbackStateFile(t, 1) // a prior boot already used its one fallback-hold reboot
	monitor := NewChromiumMonitor(closedLocalHTTPEndpoint(t), zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)

	// controld reports a genuine, still-unresolved CDP-stuck episode.
	monitor.SetCDPStuck(true)

	// Headless and the endpoint is unreachable: checkHangState's FAILURE
	// path latches m.headless, leaving cdpStuck untouched (still true,
	// still unresolved).
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against a closed endpoint")
	}

	// Display reconnects and /json/version answers before controld has had
	// any chance to re-affirm its report (cdphealth only re-affirms every
	// StuckThreshold=90s). check()'s success path force-clears the stale
	// cdpStuck latch via wasSuppressed — necessary for escalation, but not a
	// confirmation from controld. The persisted reboot cap must survive.
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor.cdpEndpoint = endpoint
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != 1 {
		t.Fatalf("expected the persisted reboot cap to survive a headless-exit's local cdpStuck clear with no controld confirmation, got %d, %v", n, err)
	}

	// Only an actual SetCDPStuck(false) from controld may forgive it.
	monitor.SetCDPStuck(false)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected the persisted file to be removed once an actual report confirmed health, stat err=%v", err)
	}
}

// TestChromiumMonitorCDPStuckRestartDoesNotForgiveRebootCapWithoutConfirmation
// pins run-reviewer's second reproduction against the same fix: the ordinary
// cdpStuck-triggered kiosk restart itself (restartChromium) also clears
// cdpStuck locally, not because controld confirmed anything. An immediate
// /json/version recovery right after that restart must not forgive the
// persisted cap either — the restart that just ran is exactly the kind of
// restart the real ffos-user#356 incident survived.
func TestChromiumMonitorCDPStuckRestartDoesNotForgiveRebootCapWithoutConfirmation(t *testing.T) {
	path := useFallbackStateFile(t, 1) // a prior boot already used its one fallback-hold reboot
	installCountingSystemctl(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	// controld reports CDP stuck; /json/version keeps answering 200
	// throughout (the real incident's exact blind spot), so escalateCDPStuck
	// — not checkHangState — restarts the kiosk. restartChromium clears
	// cdpStuck as part of acting on the escalation, not as a confirmation.
	monitor.SetCDPStuck(true)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}

	// The very next tick, /json/version still answers fine (the kiosk
	// restart "worked" at the process level) — but controld has not sent a
	// fresh report. The persisted cap must survive this tick too.
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if n, err := loadChromiumFallbackReboots(path); err != nil || n != 1 {
		t.Fatalf("expected the persisted reboot cap to survive the cdpStuck restart's own local cdpStuck clear with no controld confirmation, got %d, %v", n, err)
	}

	// Only an actual SetCDPStuck(false) from controld may forgive it.
	monitor.SetCDPStuck(false)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected the persisted file to be removed once an actual report confirmed health, stat err=%v", err)
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
