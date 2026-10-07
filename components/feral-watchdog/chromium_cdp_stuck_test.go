package main

import (
	"context"
	"testing"

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
