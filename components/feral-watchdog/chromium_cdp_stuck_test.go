package main

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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

// TestChromiumMonitorCDPStuckActsOnFreshGateReadsNotStaleLatches pins
// ffos-user#356 review round 3's F1 fix directly: escalateCDPStuck (called
// from check()'s success path) must decide purely from a FRESH, local read
// of the suppression gates on every call, never from
// m.headless/m.devConsole/m.updating — those fields are checkHangState's
// own latch state, mutated only by the FAILURE path since round 3 (calling
// checkHangState from the success path, as this code used to, re-triggered
// its "entered" logging every tick during a live dev-console/update
// window; see check()'s own comment on escalateCDPStuck).
//
// Earlier (pre-round-3) this test asserted that reconnecting from headless
// granted the same kind of "fresh startup grace" checkHangState's own
// pre-connect branch grants. That was actually a symptom of the bug: it
// depended on the success path having latched m.headless=true via
// checkHangState in the first place. Once Chromium has ALREADY answered
// /json/version successfully (which is the only way escalateCDPStuck ever
// runs), there is nothing left to wait out — round 2's F2 already
// established cdpStuck must never participate in pre-connect-grace timing;
// escalateCDPStuck has no grace concept at all, by design.
func TestChromiumMonitorCDPStuckActsOnFreshGateReadsNotStaleLatches(t *testing.T) {
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
		t.Fatalf("cdpStuck must not restart while the fresh read says headless, got %s", got)
	}

	// Display reconnects. Chromium was already confirmed up on the call
	// above (the endpoint always answers 200) — this is not a cold start,
	// so there is no grace left to grant: the very next tick must act on
	// the still-true cdpStuck exactly as it would with no headless history
	// at all.
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected the fresh gate read to allow exactly one restart once no gate suppresses it, got %s", got)
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

// TestChromiumMonitorCDPStuckDoesNotBypassPreConnectGrace pins ffos-user#356
// review round 2, F2: cdphealth.StuckThreshold runs on controld's own clock
// (anchored to when IT observed the dial go unhealthy), with no ordering
// guarantee against this monitor's own monitorStart — a separate process.
// cdpStuck=true must not cut this device's own, still-unexpired cold-start
// grace short; it may only act once hasEverConnected flips true (the
// cdpStuck case further down the switch) or the grace genuinely elapses.
func TestChromiumMonitorCDPStuckDoesNotBypassPreConnectGrace(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	// Still well within this monitor's own fresh startup grace.
	monitor.SetCDPStuck(true)
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against a closed endpoint")
	}
	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("cdpStuck must not cut this device's own unexpired startup grace short, got restart count %s", got)
	}

	// Now genuinely past CHROMIUM_STARTUP_GRACE on this monitor's own clock
	// (mutated directly rather than waiting out 90s real time) — this must
	// still restart, via the ordinary startup_grace_exceeded path, exactly
	// as it would with no cdpStuck signal involved at all.
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-CHROMIUM_STARTUP_GRACE - time.Second)
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against a closed endpoint")
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected the ordinary startup-grace-exceeded path to restart once the grace genuinely elapsed, got %s", got)
	}
}

// TestChromiumMonitorCDPStuckSuccessDuringUpdateDoesNotSpamOrRestart pins
// ffos-user#356 review round 3's F1: a live OTA/package update can leave
// /json/version answering 200 throughout (the update gate's own doc: its IO
// storm only CAN starve it, not always does), so check()'s success path
// runs repeatedly while commandHandler.updateInProgress() is independently
// true. escalateCDPStuck must suppress every one of those ticks silently —
// it must never call into checkHangState's own "entered update" Info log
// (that belongs to the failure path's narrative, not this one), and a
// cdpStuck report must not fire a restart while the update holds the lock.
func TestChromiumMonitorCDPStuckSuccessDuringUpdateDoesNotSpamOrRestart(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	core, logs := observer.New(zapcore.InfoLevel)
	monitor := NewChromiumMonitor(endpoint, zap.New(core), NewCommandHandler(zap.New(core), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	end := simulateUpdate(t)
	defer end()
	monitor.SetCDPStuck(true)

	for i := 0; i < 5; i++ {
		if err := monitor.check(context.Background()); err != nil {
			t.Fatalf("tick %d: expected success against ok endpoint, got %v", i, err)
		}
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("cdpStuck must not restart while an update holds the lock, got %s", got)
	}
	if n := logs.FilterMessageSnippet("update in progress").Len(); n != 0 {
		t.Fatalf("expected the success path to never log checkHangState's \"entered update\" transition (that belongs to the failure path only), got %d such log(s)", n)
	}
}

// TestChromiumMonitorCDPStuckDoesNotEraseRebootCapOnSuccess pins
// feralfile-bot's review F1 on PR #386 (ffos-user#356): the pre-existing
// #254 "reboot cap across boots" counter was correct back when a
// /json/version success WAS sufficient proof of recovery. This branch adds
// the first failure mode (cdpStuck) that can drive its own restart/
// fallback-hold cycles while /json/version keeps answering 200 throughout —
// a CDP page-target failure surviving a reboot would otherwise erase the
// cap on the very next successful tick and reboot forever instead of
// parking. The persisted count must survive until cdphealth has had a full
// CHROMIUM_STARTUP_GRACE window to report a relapse and did not.
func TestChromiumMonitorCDPStuckDoesNotEraseRebootCapOnSuccess(t *testing.T) {
	restartFile, _, fallbackFile, rebootFile := installFallbackStubs(t)
	statePath := useFallbackStateFile(t, 1) // this boot already used its one fallback-hold reboot
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	monitor.SetCDPStuck(true)

	// A success tick right after boot must NOT forget the persisted cap —
	// the CDP failure that earned it hasn't had a chance to clear, and
	// cdpStuck says it is still true. It must, however, still restart the
	// kiosk via the ordinary cdpStuck escalation.
	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	if n, err := loadChromiumFallbackReboots(statePath); err != nil || n != 1 {
		t.Fatalf("expected the persisted reboot cap to survive a success while cdpStuck is true, got %d, %v", n, err)
	}
	if got := readRestartCount(t, restartFile); got != "1" {
		t.Fatalf("expected exactly one kiosk restart from the cdpStuck escalation, got %s", got)
	}

	// Two more cdpStuck reports in quick succession must accumulate toward
	// the restart budget and re-enter the fallback hold rather than restart
	// a third time — the budget was never erased, so it is already spent.
	for i := 0; i < 2; i++ {
		monitor.SetCDPStuck(true)
		if err := monitor.check(context.Background()); err != nil {
			t.Fatalf("tick %d: expected success against ok endpoint, got %v", i, err)
		}
	}
	if got := readRestartCount(t, fallbackFile); got != "1" {
		t.Fatalf("expected the exhausted restart budget to enter the fallback hold instead of a third kiosk restart, got %s", got)
	}
	if got := readRestartCount(t, restartFile); got != "2" {
		t.Fatalf("expected exactly 2 ordinary kiosk restarts total (this loop's first iteration) before the 3rd hit the exhausted budget and fell into the fallback hold instead, got %s", got)
	}

	// Hold expiry with the persisted count already at the cap must PARK,
	// never reboot a second time this boot — the regression the bot found
	// would have erased the count back to 0 and rebooted here instead. The
	// kiosk is genuinely stopped while the fallback screen is showing, so
	// /json/version fails for real at this point (matching
	// TestChromiumMonitorParksAfterRebootCap's own fixture) — checkHangState's
	// failure-path hold logic, not escalateCDPStuck, is what decides
	// park-vs-reboot here.
	monitor.cdpEndpoint = closedLocalHTTPEndpoint(t)
	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-(CHROMIUM_FALLBACK_HOLD + time.Second))
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against a closed endpoint")
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("expected the device to PARK on an exhausted reboot cap, not reboot again, got %s", got)
	}
	monitor.mu.Lock()
	parked := monitor.fallbackParked
	monitor.mu.Unlock()
	if !parked {
		t.Fatal("expected the monitor to latch parked once the exhausted hold expired")
	}
}
