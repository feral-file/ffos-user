package main

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestChromiumMonitorCDPStuckRestartsEvenWhenHealthy pins the ffos-user#356
// fix itself: /json/version answers fine (the exact blind spot the real
// incident exposed — controld's own CDP page-target dial was stuck for 7+
// hours while nothing else detected it), but feral-controld has reported
// SetCDPStuck(true). The monitor must still restart the kiosk.
func TestChromiumMonitorCDPStuckRestartsEvenWhenHealthy(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	// Arm post-connect mode first so the switch's !hasEverConnected branch
	// (which also tolerates cdpStuck) is not what makes this test pass.
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("a plain healthy check must never restart, got %s", got)
	}

	monitor.SetCDPStuck(true)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}

	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected one kiosk restart once controld reports CDP stuck, got %s", got)
	}
}

// TestChromiumMonitorCDPStuckSuppressedWhileHeadless pins that the new
// cdpStuck case does not bypass the existing headless suppression gate: a
// headless device legitimately has no Chromium to restart.
func TestChromiumMonitorCDPStuckSuppressedWhileHeadless(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)

	monitor.SetCDPStuck(true)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("a headless check reports success (no attempt to reach a kiosk that is not running): %v", err)
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("cdpStuck must not restart a headless device, got %s", got)
	}
}

// TestChromiumMonitorCDPStuckClearedAfterRestart pins the restart-storm
// guard: once cdpStuck has triggered one restart, it must not re-trigger a
// second one on the very next tick just because controld's episode (which
// only reports a FRESH disconnect, not a continuing one) never resent it.
func TestChromiumMonitorCDPStuckClearedAfterRestart(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	monitor.SetCDPStuck(true)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected exactly one restart, got %s", got)
	}

	monitor.mu.Lock()
	stillStuck := monitor.cdpStuck
	monitor.mu.Unlock()
	if stillStuck {
		t.Fatal("expected cdpStuck cleared once restartChromium acted on it")
	}

	// restartChromium drops back to pre-connect mode with a fresh grace
	// window; the very next check (still healthy) must not restart again.
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected no second restart from the stale cdpStuck latch, got %s", got)
	}
}

// TestChromiumMonitorCDPStuckClearedOnHeadlessReconnectViaSuccess pins
// ffos-user#356 review F2 (the success-path half): cdphealth.Monitor has no
// visibility into display/VT/update state, so it is guaranteed to latch
// cdpStuck=true on a headless device once a period runs past
// cdphealth.StuckThreshold — that latch must be treated as stale, not acted
// on, the moment the display reconnects and check()'s own success path
// grants a fresh startup grace.
func TestChromiumMonitorCDPStuckClearedOnHeadlessReconnectViaSuccess(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)

	monitor.SetCDPStuck(true)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("cdpStuck must not restart a headless device, got %s", got)
	}

	monitor.drmSysfsRoot = connectedDRMRoot(t)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected the fresh startup grace to hold on the very first tick after reconnect, got restart count %s", got)
	}
	monitor.mu.Lock()
	stillStuck := monitor.cdpStuck
	monitor.mu.Unlock()
	if stillStuck {
		t.Fatal("expected cdpStuck cleared once the headless period ended")
	}
}

// TestChromiumMonitorCDPStuckClearedOnHeadlessReconnectViaFailure pins the
// other half of F2: checkHangState's OWN `reconnected` reset block (reached
// from check()'s FAILURE path, when /json/version is still down the moment
// the display reconnects) must clear cdpStuck too, exactly like its three
// sibling latches — not only the success-path reset exercised above.
func TestChromiumMonitorCDPStuckClearedOnHeadlessReconnectViaFailure(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against a closed endpoint")
	}
	monitor.SetCDPStuck(true)

	// Display reconnects, but Chromium has not finished cold-starting yet —
	// /json/version is still unreachable on this exact tick, so this is a
	// FAILURE-path call to checkHangState, not the success path.
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against a closed endpoint")
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected the fresh startup grace to hold on the very first failed tick after reconnect, got restart count %s", got)
	}
	monitor.mu.Lock()
	stillStuck := monitor.cdpStuck
	monitor.mu.Unlock()
	if stillStuck {
		t.Fatal("expected cdpStuck cleared by checkHangState's own reconnected reset")
	}
}

// TestChromiumMonitorCDPStuckClearedOnFallbackRecovery pins the remaining
// F2 sibling the lead called out explicitly: a stale cdpStuck must not
// survive recovery from the fallback-hold screen either, or the device
// restarts again the instant it proves itself healthy.
func TestChromiumMonitorCDPStuckClearedOnFallbackRecovery(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	countFile := installCountingSystemctl(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-time.Minute)
	monitor.restartHistory = []time.Time{time.Now(), time.Now(), time.Now()}
	monitor.mu.Unlock()
	monitor.SetCDPStuck(true)

	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected recovery to resume with a fresh grace, not an immediate restart, got %s", got)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("recovery must not reboot, got %s", got)
	}
	monitor.mu.Lock()
	stillStuck := monitor.cdpStuck
	monitor.mu.Unlock()
	if stillStuck {
		t.Fatal("expected cdpStuck cleared on recovery from the fallback hold")
	}
}
