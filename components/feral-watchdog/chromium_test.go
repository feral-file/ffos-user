package main

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// TestChromiumMonitorFirstFailureUsesStartupGrace pins the cold-boot
// invariant: the very first failed health check on a freshly-started monitor
// must NOT issue a kiosk restart. Without this, watchdog start ordering on
// FF1 (no After= relationship with chromium-kiosk.service) would produce a
// restart-on-boot for every device that happens to come up faster than
// Chromium.
func TestChromiumMonitorFirstFailureUsesStartupGrace(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	// Pin a connected display so these escalation invariants are exercised for
	// the reason under test (grace/hang), not accidentally suppressed by the
	// host's real DRM state on a Linux CI box.
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected health check to fail against closed endpoint")
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected no immediate kiosk restart during startup grace, got restart count %s", got)
	}
}

// TestChromiumMonitorColdBootGraceSuppressesManyFailures pins the broader
// cold-boot invariant: even after many consecutive failed checks, as long as
// we are within CHROMIUM_STARTUP_GRACE, no restart should fire. This is the
// scenario that produced the "Chromium browser hang detected" log spam on
// many devices after PR #192 — the previous version of this code would have
// fired the noisy log + restart at check #5 (~25 s) and again every check
// after that.
func TestChromiumMonitorColdBootGraceSuppressesManyFailures(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	// Pin a connected display so these escalation invariants are exercised for
	// the reason under test (grace/hang), not accidentally suppressed by the
	// host's real DRM state on a Linux CI box.
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	for i := 0; i < 10; i++ {
		if err := monitor.check(context.Background()); err == nil {
			t.Fatalf("check %d: expected failure against closed endpoint", i)
		}
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected no restart during startup grace despite 10 failures, got restart count %s", got)
	}
}

// TestChromiumMonitorColdBootGraceExpiryTriggersRestart pins the escalation
// boundary: if CHROMIUM_STARTUP_GRACE elapses without any successful
// response, exactly one restart must fire. We simulate "elapsed time" by
// rewinding monitorStart rather than sleeping, because real-time waits
// produce flaky tests and slow down CI.
func TestChromiumMonitorColdBootGraceExpiryTriggersRestart(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	// Pin a connected display so these escalation invariants are exercised for
	// the reason under test (grace/hang), not accidentally suppressed by the
	// host's real DRM state on a Linux CI box.
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	// Push monitorStart far enough into the past that the next failed check
	// must escalate. CHROMIUM_STARTUP_GRACE is 90s; -120s leaves no doubt.
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + 30*time.Second))
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}

	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected exactly one restart after startup grace expired, got %s", got)
	}
}

// TestChromiumMonitorPostRestartReentersStartupGrace pins the load-bearing
// invariant in restartChromium: after issuing a restart, the monitor must
// drop back to pre-connect mode so the next 90 s of failed checks do not
// produce a second restart. Without this reset, the kiosk's own RestartSec
// (~5 s) + Chromium cold start exceeded the 20 s hang threshold and the
// monitor would burn through the 3-restart budget in well under 5 minutes,
// rebooting healthy devices.
func TestChromiumMonitorPostRestartReentersStartupGrace(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	// Pin a connected display so these escalation invariants are exercised for
	// the reason under test (grace/hang), not accidentally suppressed by the
	// host's real DRM state on a Linux CI box.
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	// Drive the monitor into the post-connect hang path: pretend it
	// connected long enough ago that the hang threshold has lapsed.
	monitor.mu.Lock()
	monitor.hasEverConnected = true
	monitor.lastSuccessfulResp = time.Now().Add(-(CHROMIUM_HANG_THRESHOLD + 5*time.Second))
	monitor.mu.Unlock()

	// First failed check must trigger one restart (the genuine hang).
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected one restart after post-connect hang detection, got %s", got)
	}

	// Verify restartChromium dropped us back to pre-connect mode and
	// re-anchored monitorStart so the next 90 s of failures stay quiet.
	monitor.mu.Lock()
	if monitor.hasEverConnected {
		t.Fatal("expected hasEverConnected to be reset after restart")
	}
	if !monitor.lastSuccessfulResp.IsZero() {
		t.Fatalf("expected lastSuccessfulResp to be zeroed after restart, got %v", monitor.lastSuccessfulResp)
	}
	monitor.mu.Unlock()

	// Subsequent failed checks within the post-restart grace must NOT
	// produce additional restarts.
	for i := 0; i < 10; i++ {
		if err := monitor.check(context.Background()); err == nil {
			t.Fatalf("check %d: expected failure against closed endpoint", i)
		}
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected restart count to stay at 1 during post-restart grace, got %s", got)
	}
}

// TestChromiumMonitorPostConnectHangTriggersRestart pins the steady-state
// hang path: once we have seen a 200, sustained silence beyond
// CHROMIUM_HANG_THRESHOLD must trigger one restart.
func TestChromiumMonitorPostConnectHangTriggersRestart(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	// Pin a connected display so these escalation invariants are exercised for
	// the reason under test (grace/hang), not accidentally suppressed by the
	// host's real DRM state on a Linux CI box.
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.hasEverConnected = true
	monitor.lastSuccessfulResp = time.Now().Add(-(CHROMIUM_HANG_THRESHOLD + 5*time.Second))
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}

	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected exactly one restart from post-connect hang, got %s", got)
	}
}

// TestChromiumMonitorActivatingKioskDefersRestart pins the cross-service
// guard: if chromium-kiosk.service is already in the "activating" state
// (e.g. systemd RestartSec is between attempts, OTA is mid-restart, or a
// human operator just ran `systemctl restart`), the hang detector must
// defer rather than pile a redundant restart on top.
func TestChromiumMonitorActivatingKioskDefersRestart(t *testing.T) {
	countFile := installActivatingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	// Pin a connected display so these escalation invariants are exercised for
	// the reason under test (grace/hang), not accidentally suppressed by the
	// host's real DRM state on a Linux CI box.
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	// Force the post-connect hang path so we are eligible to escalate.
	monitor.mu.Lock()
	monitor.hasEverConnected = true
	monitor.lastSuccessfulResp = time.Now().Add(-(CHROMIUM_HANG_THRESHOLD + 5*time.Second))
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}

	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected zero restarts while kiosk is activating, got %s", got)
	}
}

func closedLocalHTTPEndpoint(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate local port: %v", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("failed to close local port: %v", err)
	}
	return "http://" + addr
}

// installCountingSystemctl drops a stub `systemctl` ahead of the real one on
// PATH. It counts `--user restart chromium-kiosk.service` invocations into a
// file and reports the kiosk as inactive for `is-active` queries — which is
// what we want for the no-deferral test branches.
func installCountingSystemctl(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	countFile := filepath.Join(dir, "restart-count")
	if err := os.WriteFile(countFile, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("failed to create restart count file: %v", err)
	}

	systemctlPath := filepath.Join(dir, "systemctl")
	systemctlScript := `#!/bin/sh
count_file="` + countFile + `"
if [ "$1" = "--user" ] && [ "$2" = "restart" ] && [ "$3" = "chromium-kiosk.service" ]; then
  count="$(cat "$count_file")"
  count=$((count + 1))
  printf "%s\n" "$count" > "$count_file"
  exit 0
fi
if [ "$1" = "--user" ] && [ "$2" = "is-active" ] && [ "$3" = "chromium-kiosk.service" ]; then
  printf "inactive\n"
  exit 3
fi
exit 0
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(systemctlPath, []byte(systemctlScript), 0o755); err != nil {
		t.Fatalf("failed to create fake systemctl: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return countFile
}

// installActivatingSystemctl is the variant for the deferral test: it still
// counts restart invocations, but reports the kiosk as "activating" so the
// hang detector should suppress its restart call. Restart count is expected
// to stay at zero in that test.
func installActivatingSystemctl(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()
	countFile := filepath.Join(dir, "restart-count")
	if err := os.WriteFile(countFile, []byte("0\n"), 0o600); err != nil {
		t.Fatalf("failed to create restart count file: %v", err)
	}

	systemctlPath := filepath.Join(dir, "systemctl")
	systemctlScript := `#!/bin/sh
count_file="` + countFile + `"
if [ "$1" = "--user" ] && [ "$2" = "restart" ] && [ "$3" = "chromium-kiosk.service" ]; then
  count="$(cat "$count_file")"
  count=$((count + 1))
  printf "%s\n" "$count" > "$count_file"
  exit 0
fi
if [ "$1" = "--user" ] && [ "$2" = "is-active" ] && [ "$3" = "chromium-kiosk.service" ]; then
  printf "activating\n"
  exit 3
fi
exit 0
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(systemctlPath, []byte(systemctlScript), 0o755); err != nil {
		t.Fatalf("failed to create fake systemctl: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return countFile
}

func readRestartCount(t *testing.T, path string) string {
	t.Helper()
	// #nosec G304 -- path is created by the test inside t.TempDir.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("failed to read restart count: %v", err)
	}
	return strings.TrimSpace(string(b))
}

// drmRootWithStatuses builds a fake /sys/class/drm layout: one directory per
// connector, each holding a `status` file with the given value ("connected" or
// "disconnected"). It returns the root, suitable for ChromiumMonitor.drmSysfsRoot.
func drmRootWithStatuses(t *testing.T, statuses map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for connector, status := range statuses {
		dir := filepath.Join(root, connector)
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatalf("failed to create connector dir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "status"), []byte(status+"\n"), 0o600); err != nil {
			t.Fatalf("failed to write connector status: %v", err)
		}
	}
	return root
}

// connectedDRMRoot is the common fixture: a single connector reporting connected.
func connectedDRMRoot(t *testing.T) string {
	t.Helper()
	return drmRootWithStatuses(t, map[string]string{"card0-HDMI-A-1": "connected"})
}

// disconnectedDRMRoot is the headless fixture: a single connector reporting
// disconnected, i.e. a KNOWN-disconnected display.
func disconnectedDRMRoot(t *testing.T) string {
	t.Helper()
	return drmRootWithStatuses(t, map[string]string{"card0-HDMI-A-1": "disconnected"})
}

// installCountingSystemctlWithReboot extends installCountingSystemctl with a
// stub `sudo` that records `systemctl reboot` invocations, so reboot-path tests
// can assert no real reboot was attempted (rebootSystem shells `sudo systemctl
// reboot` unstubbed otherwise). Returns (restartCountFile, rebootCountFile).
func installCountingSystemctlWithReboot(t *testing.T) (string, string) {
	t.Helper()

	dir := t.TempDir()
	restartFile := filepath.Join(dir, "restart-count")
	rebootFile := filepath.Join(dir, "reboot-count")
	for _, f := range []string{restartFile, rebootFile} {
		if err := os.WriteFile(f, []byte("0\n"), 0o600); err != nil {
			t.Fatalf("failed to seed counter %s: %v", f, err)
		}
	}

	systemctlScript := `#!/bin/sh
count_file="` + restartFile + `"
if [ "$1" = "--user" ] && [ "$2" = "restart" ] && [ "$3" = "chromium-kiosk.service" ]; then
  count="$(cat "$count_file")"
  count=$((count + 1))
  printf "%s\n" "$count" > "$count_file"
  exit 0
fi
if [ "$1" = "--user" ] && [ "$2" = "is-active" ] && [ "$3" = "chromium-kiosk.service" ]; then
  printf "inactive\n"
  exit 3
fi
exit 0
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctlScript), 0o755); err != nil {
		t.Fatalf("failed to create fake systemctl: %v", err)
	}

	sudoScript := `#!/bin/sh
reboot_file="` + rebootFile + `"
if [ "$1" = "systemctl" ] && [ "$2" = "reboot" ]; then
  count="$(cat "$reboot_file")"
  count=$((count + 1))
  printf "%s\n" "$count" > "$reboot_file"
  exit 0
fi
exit 0
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(sudoScript), 0o755); err != nil {
		t.Fatalf("failed to create fake sudo: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return restartFile, rebootFile
}

// TestChromiumMonitorHeadlessSuppressesEscalation pins the core headless
// invariant: with a KNOWN-disconnected display and a dead CDP endpoint, even
// with the startup grace long expired AND the reboot budget one restart away
// from tripping, the monitor must issue zero kiosk restarts and zero reboots.
func TestChromiumMonitorHeadlessSuppressesEscalation(t *testing.T) {
	restartFile, rebootFile := installCountingSystemctlWithReboot(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)

	// Worst case: grace expired and two restarts already banked in-window, so a
	// single further restart would trip the 3-in-5-minutes reboot. If display
	// gating regressed this would both restart and reboot.
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.restartHistory = []time.Time{time.Now(), time.Now()}
	monitor.mu.Unlock()

	for i := 0; i < 5; i++ {
		if err := monitor.check(context.Background()); err == nil {
			t.Fatalf("check %d: expected failure against closed endpoint", i)
		}
	}

	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("headless device must not restart kiosk, got %s", got)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("headless device must not reboot, got %s", got)
	}
}

// TestChromiumMonitorUnknownDisplayFailsOpen pins the fail-open invariant: an
// empty sysfs directory (no connectors exposed) is an UNKNOWN display state and
// must preserve normal escalation — startup-grace expiry still restarts.
func TestChromiumMonitorUnknownDisplayFailsOpen(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = t.TempDir() // empty -> glob matches nothing -> fail open

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + 30*time.Second))
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}

	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("unknown display state must preserve escalation (fail open), got %s", got)
	}
}

// TestChromiumMonitorDisplayReconnectReanchorsGrace pins the reconnect path: a
// device that was headless (grace long expired, escalation suppressed) must,
// when a display reappears, be granted a FRESH startup-grace window rather than
// an instant restart, and only escalate once that fresh grace itself expires.
func TestChromiumMonitorDisplayReconnectReanchorsGrace(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")

	// Phase 1 — headless with grace already expired: no restart, latch headless.
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.mu.Unlock()
	for i := 0; i < 3; i++ {
		if err := monitor.check(context.Background()); err == nil {
			t.Fatalf("phase1 check %d: expected failure against closed endpoint", i)
		}
	}
	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("headless phase must not restart, got %s", got)
	}

	// Phase 2 — display reappears: first failing check re-anchors grace, does
	// NOT restart despite the ancient monitorStart from the headless phase.
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("phase2: expected failure against closed endpoint")
	}
	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("reconnect must grant fresh grace, not an instant restart, got %s", got)
	}
	monitor.mu.Lock()
	sinceStart := time.Since(monitor.monitorStart)
	hasEver := monitor.hasEverConnected
	headless := monitor.headless
	monitor.mu.Unlock()
	if sinceStart > CHROMIUM_STARTUP_GRACE {
		t.Fatalf("expected monitorStart re-anchored within grace, elapsed %v", sinceStart)
	}
	if hasEver {
		t.Fatal("expected pre-connect mode after reconnect")
	}
	if headless {
		t.Fatal("expected headless latch cleared after reconnect")
	}

	// Phase 3 — let the fresh grace expire: escalation resumes normally.
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + 30*time.Second))
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("phase3: expected failure against closed endpoint")
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("escalation must resume after fresh grace expires, got %s", got)
	}
}

// installFallbackStubs installs fake `systemctl` and `sudo` binaries that
// count the four commands the Chromium escalation path can issue: kiosk
// restart, kiosk stop, fallback-unit start (via `sudo -n`) and reboot (via
// `sudo`). Returns the counter files in that order.
func installFallbackStubs(t *testing.T) (restartFile, stopFile, fallbackFile, rebootFile string) {
	t.Helper()

	dir := t.TempDir()
	restartFile = filepath.Join(dir, "restart-count")
	stopFile = filepath.Join(dir, "stop-count")
	fallbackFile = filepath.Join(dir, "fallback-count")
	rebootFile = filepath.Join(dir, "reboot-count")
	for _, f := range []string{restartFile, stopFile, fallbackFile, rebootFile} {
		if err := os.WriteFile(f, []byte("0\n"), 0o600); err != nil {
			t.Fatalf("failed to seed counter %s: %v", f, err)
		}
	}

	systemctlScript := `#!/bin/sh
bump() { c="$(cat "$1")"; c=$((c + 1)); printf "%s\n" "$c" > "$1"; }
if [ "$1" = "--user" ] && [ "$2" = "restart" ] && [ "$3" = "chromium-kiosk.service" ]; then
  bump "` + restartFile + `"; exit 0
fi
if [ "$1" = "--user" ] && [ "$2" = "stop" ] && [ "$3" = "chromium-kiosk.service" ]; then
  bump "` + stopFile + `"
  [ -e "` + stopFile + `.fail" ] && exit 1
  exit 0
fi
if [ "$1" = "--user" ] && [ "$2" = "is-active" ] && [ "$3" = "chromium-kiosk.service" ]; then
  printf "inactive\n"; exit 3
fi
exit 0
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(filepath.Join(dir, "systemctl"), []byte(systemctlScript), 0o755); err != nil {
		t.Fatalf("failed to create fake systemctl: %v", err)
	}

	sudoScript := `#!/bin/sh
bump() { c="$(cat "$1")"; c=$((c + 1)); printf "%s\n" "$c" > "$1"; }
if [ "$1" = "-n" ] && [ "$2" = "systemctl" ] && [ "$3" = "start" ] && [ "$4" = "` + KIOSK_FALLBACK_UNIT + `" ]; then
  bump "` + fallbackFile + `"
  # An image without the unit: creating <fallbackFile>.fail in a test makes
  # the start fail the way systemctl start does for an unknown unit.
  [ -e "` + fallbackFile + `.fail" ] && exit 5
  exit 0
fi
if [ "$1" = "systemctl" ] && [ "$2" = "reboot" ]; then
  bump "` + rebootFile + `"; exit 0
fi
exit 0
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(sudoScript), 0o755); err != nil {
		t.Fatalf("failed to create fake sudo: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return restartFile, stopFile, fallbackFile, rebootFile
}

// TestChromiumMonitorDevConsoleSuppressesEscalation pins the developer-console
// gate: with a display connected but tty2 active (a developer on getty@tty2),
// a dead CDP endpoint must produce no kiosk restart, no fallback and no reboot
// even with the grace expired and the budget one restart from exhaustion —
// start-kiosk.sh's wait_for_vt1 is holding cage back on purpose.
func TestChromiumMonitorDevConsoleSuppressesEscalation(t *testing.T) {
	restartFile, stopFile, fallbackFile, rebootFile := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty2")

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.restartHistory = []time.Time{time.Now(), time.Now()}
	monitor.mu.Unlock()

	for i := 0; i < 5; i++ {
		err := monitor.check(context.Background())
		if err == nil {
			t.Fatalf("check %d: expected failure against closed endpoint", i)
		}
		if !strings.Contains(err.Error(), errChromiumHeadless.Error()) {
			t.Fatalf("check %d: developer-console failure must be tagged expected, got %v", i, err)
		}
	}
	for name, f := range map[string]string{"restart": restartFile, "stop": stopFile, "fallback": fallbackFile, "reboot": rebootFile} {
		if got := readRestartCount(t, f); got != "0" {
			t.Fatalf("developer console must suppress %s, got %s", name, got)
		}
	}
	monitor.mu.Lock()
	devConsole := monitor.devConsole
	history := len(monitor.restartHistory)
	monitor.mu.Unlock()
	if !devConsole {
		t.Fatal("expected developer-console latch set")
	}
	if history != 2 {
		t.Fatalf("developer console must not accumulate restart history, got %d", history)
	}
}

// TestChromiumMonitorDevConsoleReturnReanchorsGrace pins the return path: when
// tty1 becomes active again the monitor grants a fresh startup grace (cage is
// only now allowed to start) instead of restarting on the stale monitorStart,
// and escalates normally once that fresh grace expires.
func TestChromiumMonitorDevConsoleReturnReanchorsGrace(t *testing.T) {
	restartFile, _, _, _ := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	// Phase 1 — developer on tty2, grace long expired: suppressed.
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty2")
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("phase1: expected failure against closed endpoint")
	}
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("phase1 must not restart, got %s", got)
	}

	// Phase 2 — back on tty1: fresh grace, no instant restart.
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("phase2: expected failure against closed endpoint")
	}
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("return to tty1 must grant fresh grace, got %s restarts", got)
	}
	monitor.mu.Lock()
	sinceStart := time.Since(monitor.monitorStart)
	devConsole := monitor.devConsole
	hasEver := monitor.hasEverConnected
	monitor.mu.Unlock()
	if sinceStart > CHROMIUM_STARTUP_GRACE {
		t.Fatalf("expected monitorStart re-anchored within grace, elapsed %v", sinceStart)
	}
	if devConsole || hasEver {
		t.Fatalf("expected latch cleared and pre-connect mode, devConsole=%v hasEver=%v", devConsole, hasEver)
	}

	// Phase 3 — fresh grace expires: escalation resumes.
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + 30*time.Second))
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("phase3: expected failure against closed endpoint")
	}
	if got := readRestartCount(t, restartFile); got != "1" {
		t.Fatalf("escalation must resume after fresh grace expires, got %s", got)
	}
}

// TestChromiumMonitorBudgetExhaustionShowsFallback pins the new escalation
// end: the restart that exhausts the 3-in-5-minutes budget must stop the
// kiosk and start feral-kiosk-fallback.service instead of rebooting, enter the
// fallback hold, and keep every later failed check quiet (no restart, no
// reboot) while the hold runs.
func TestChromiumMonitorBudgetExhaustionShowsFallback(t *testing.T) {
	restartFile, stopFile, fallbackFile, rebootFile := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.restartHistory = []time.Time{time.Now(), time.Now()}
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("budget exhaustion must not restart the kiosk, got %s", got)
	}
	if got := readRestartCount(t, stopFile); got != "1" {
		t.Fatalf("expected kiosk stopped once before fallback, got %s", got)
	}
	if got := readRestartCount(t, fallbackFile); got != "1" {
		t.Fatalf("expected fallback unit started once, got %s", got)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("budget exhaustion must not reboot immediately, got %s", got)
	}
	monitor.mu.Lock()
	inFallback := !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if !inFallback {
		t.Fatal("expected fallback hold to be armed")
	}

	// During the hold every failed check is expected and quiet.
	for i := 0; i < 5; i++ {
		err := monitor.check(context.Background())
		if err == nil {
			t.Fatalf("hold check %d: expected failure against closed endpoint", i)
		}
		if !strings.Contains(err.Error(), errChromiumHeadless.Error()) {
			t.Fatalf("hold check %d: failure must be tagged expected, got %v", i, err)
		}
	}
	for name, f := range map[string]string{"restart": restartFile, "stop": stopFile, "fallback": fallbackFile, "reboot": rebootFile} {
		want := "0"
		if name == "stop" || name == "fallback" {
			want = "1"
		}
		if got := readRestartCount(t, f); got != want {
			t.Fatalf("during hold %s count = %s, want %s", name, got, want)
		}
	}
}

// TestChromiumMonitorFallbackHoldExpiryRebootsOnce pins the end of the hold:
// once CHROMIUM_FALLBACK_HOLD has elapsed the monitor reboots exactly once and
// leaves the hold, so a reboot command that fails cannot re-fire every tick.
func TestChromiumMonitorFallbackHoldExpiryRebootsOnce(t *testing.T) {
	restartFile, _, fallbackFile, rebootFile := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-(CHROMIUM_FALLBACK_HOLD + time.Second))
	// Grace still fresh, so the post-reboot ladder does not restart in this test.
	monitor.monitorStart = time.Now()
	monitor.mu.Unlock()

	for i := 0; i < 3; i++ {
		if err := monitor.check(context.Background()); err == nil {
			t.Fatalf("check %d: expected failure against closed endpoint", i)
		}
	}
	if got := readRestartCount(t, rebootFile); got != "1" {
		t.Fatalf("expected exactly one reboot after hold expiry, got %s", got)
	}
	if got := readRestartCount(t, fallbackFile); got != "0" {
		t.Fatalf("hold expiry must not re-show fallback, got %s", got)
	}
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("fresh grace after hold must not restart, got %s", got)
	}
	monitor.mu.Lock()
	inFallback := !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if inFallback {
		t.Fatal("expected fallback hold cleared after reboot was issued")
	}
}

// TestChromiumMonitorRecoveryDuringFallbackClearsState pins recovery: a
// successful check while the fallback screen is up (manual kiosk restart, OTA)
// clears the hold and forgets the exhausted budget, so a later fault gets the
// full restart ladder rather than an instant fallback.
func TestChromiumMonitorRecoveryDuringFallbackClearsState(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	endpoint, closeServer := okLocalHTTPEndpoint(t)
	defer closeServer()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-time.Minute)
	monitor.restartHistory = []time.Time{time.Now(), time.Now(), time.Now()}
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected success against ok endpoint, got %v", err)
	}
	monitor.mu.Lock()
	inFallback := !monitor.fallbackSince.IsZero()
	history := len(monitor.restartHistory)
	hasEver := monitor.hasEverConnected
	monitor.mu.Unlock()
	if inFallback {
		t.Fatal("expected fallback hold cleared on recovery")
	}
	if history != 0 {
		t.Fatalf("expected restart history reset on recovery, got %d", history)
	}
	if !hasEver {
		t.Fatal("expected post-connect mode after a successful check")
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("recovery must not reboot, got %s", got)
	}
}

// okLocalHTTPEndpoint serves 200 on /json/version so recovery paths can be
// exercised; the returned func shuts the server down.
func okLocalHTTPEndpoint(t *testing.T) (string, func()) {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"Browser":"test"}`))
	}))
	return server.URL, server.Close
}

// TestChromiumMonitorFallbackUnavailableRebootsImmediately pins the version-skew
// path: the watchdog ships on the package rail, the fallback unit on the image
// rail, so on an image that predates the unit `systemctl start` fails. The
// kiosk has already been stopped by then, so the monitor must reboot at once
// (the pre-fallback behavior) instead of holding 15 minutes on a black screen.
func TestChromiumMonitorFallbackUnavailableRebootsImmediately(t *testing.T) {
	restartFile, stopFile, fallbackFile, rebootFile := installFallbackStubs(t)
	if err := os.WriteFile(fallbackFile+".fail", nil, 0o600); err != nil {
		t.Fatalf("failed to arm fallback failure: %v", err)
	}
	endpoint := closedLocalHTTPEndpoint(t)
	handler := NewCommandHandler(zap.NewNop(), nil)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), handler)
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.restartHistory = []time.Time{time.Now(), time.Now()}
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	for name, want := range map[string]string{restartFile: "0", stopFile: "1", fallbackFile: "1", rebootFile: "1"} {
		if got := readRestartCount(t, name); got != want {
			t.Fatalf("%s count = %s, want %s", filepath.Base(name), got, want)
		}
	}
	monitor.mu.Lock()
	inFallback := !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if inFallback {
		t.Fatal("hold must not be armed when the fallback screen failed to start")
	}
	if handler.isFallbackShown() {
		t.Fatal("fallbackShown must stay false when the unit failed to start")
	}
}

// TestChromiumMonitorFallbackHoldAbandonedWhenHeadless pins the display-gate
// invariant inside the hold: a monitor unplugged while the error screen is up
// must not end in a reboot; the hold is dropped, the monitor goes headless,
// and restartKiosk is allowed again so the reconnect path can relaunch.
func TestChromiumMonitorFallbackHoldAbandonedWhenHeadless(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	handler := NewCommandHandler(zap.NewNop(), nil)
	handler.mu.Lock()
	handler.fallbackShown = true
	handler.mu.Unlock()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), handler)
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-(CHROMIUM_FALLBACK_HOLD + time.Second))
	monitor.restartHistory = []time.Time{time.Now(), time.Now(), time.Now()}
	monitor.mu.Unlock()

	err := monitor.check(context.Background())
	if err == nil || !strings.Contains(err.Error(), errChromiumHeadless.Error()) {
		t.Fatalf("expected an expected-tagged failure, got %v", err)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("headless during hold must not reboot, got %s", got)
	}
	monitor.mu.Lock()
	inFallback := !monitor.fallbackSince.IsZero()
	headless := monitor.headless
	monitor.mu.Unlock()
	if inFallback {
		t.Fatal("hold must be abandoned when the display goes away")
	}
	if !headless {
		t.Fatal("monitor must latch headless after abandoning the hold")
	}
	monitor.mu.Lock()
	history := len(monitor.restartHistory)
	monitor.mu.Unlock()
	if history != 0 {
		t.Fatalf("abandoning the hold must forget the exhausted budget, got %d stamps", history)
	}
	if handler.isFallbackShown() {
		t.Fatal("fallbackShown must be cleared so the reconnect path can restart the kiosk")
	}
}

// TestChromiumMonitorFallbackHoldDefersRebootForDevConsole pins that a
// developer on tty2 while the error screen is up (the exact moment the console
// exists for) is not rebooted out of their shell: an expired hold is
// re-anchored while another VT is active and a full hold starts over on tty1.
func TestChromiumMonitorFallbackHoldDefersRebootForDevConsole(t *testing.T) {
	_, _, _, rebootFile := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty2")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-(CHROMIUM_FALLBACK_HOLD + time.Second))
	monitor.mu.Unlock()

	for i := 0; i < 3; i++ {
		err := monitor.check(context.Background())
		if err == nil || !strings.Contains(err.Error(), errChromiumHeadless.Error()) {
			t.Fatalf("check %d: expected an expected-tagged failure, got %v", i, err)
		}
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("developer console during hold must defer the reboot, got %s reboots", got)
	}
	monitor.mu.Lock()
	since := monitor.fallbackSince
	monitor.mu.Unlock()
	if since.IsZero() || time.Since(since) > time.Minute {
		t.Fatalf("hold must stay armed and re-anchored while tty2 is active, fallbackSince=%v", since)
	}

	// Back on tty1 with the (re-anchored) hold still running: no reboot yet.
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("re-anchored hold must not reboot immediately on return to tty1, got %s", got)
	}
}

// TestCommandHandlerRestartKioskRefusedWhileFallbackShown pins the RAM/GPU
// handler interaction: their restartKiosk calls must not run the kiosk's
// ExecStartPre (which stops the fallback unit) while the error screen is
// deliberately up, and must work again once the monitor clears it.
func TestCommandHandlerRestartKioskRefusedWhileFallbackShown(t *testing.T) {
	restartFile, _, _, _ := installFallbackStubs(t)
	handler := NewCommandHandler(zap.NewNop(), nil)
	if got := handler.showKioskFallback(context.Background()); got != kioskFallbackShown {
		t.Fatalf("expected fallback screen to be shown by the stub, got %v", got)
	}
	if !handler.isFallbackShown() {
		t.Fatal("expected fallbackShown after a successful show")
	}
	handler.restartKiosk(context.Background())
	if got := readRestartCount(t, restartFile); got != "0" {
		t.Fatalf("restartKiosk must be refused while the fallback is showing, got %s", got)
	}
	handler.clearKioskFallback()
	handler.restartKiosk(context.Background())
	if got := readRestartCount(t, restartFile); got != "1" {
		t.Fatalf("restartKiosk must work again after clearKioskFallback, got %s", got)
	}
}

// TestChromiumMonitorFallbackBusyRetriesWithoutReboot pins the contention
// path: if a RAM/GPU kiosk restart holds the kiosk lock at the moment the
// budget is exhausted, nothing was stopped, so the monitor must neither reboot
// nor arm the hold; the next tick retries once the lock is free.
func TestChromiumMonitorFallbackBusyRetriesWithoutReboot(t *testing.T) {
	restartFile, stopFile, fallbackFile, rebootFile := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	handler := NewCommandHandler(zap.NewNop(), nil)
	handler.mu.Lock()
	handler.kioskOpInFlight = true
	handler.mu.Unlock()
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), handler)
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.restartHistory = []time.Time{time.Now(), time.Now()}
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	for name, f := range map[string]string{"restart": restartFile, "stop": stopFile, "fallback": fallbackFile, "reboot": rebootFile} {
		if got := readRestartCount(t, f); got != "0" {
			t.Fatalf("busy path must do nothing, %s count = %s", name, got)
		}
	}
	monitor.mu.Lock()
	inFallback := !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if inFallback {
		t.Fatal("hold must not be armed while the fallback could not be shown")
	}

	// Lock released: the next tick shows the screen and arms the hold.
	handler.mu.Lock()
	handler.kioskOpInFlight = false
	handler.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	if got := readRestartCount(t, fallbackFile); got != "1" {
		t.Fatalf("expected fallback shown on retry, got %s", got)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("retry must not reboot, got %s", got)
	}
	monitor.mu.Lock()
	inFallback = !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if !inFallback {
		t.Fatal("expected hold armed after the retry")
	}
}

// TestChromiumMonitorReconnectAfterAbandonedHoldRestartsKiosk pins the
// customer-visible path behind forgetting the budget: unplug the display
// during the error screen, plug it back within five minutes, wait out the
// reconnect grace — the stopped kiosk must be RESTARTED, not shown a second
// fallback because three stale stamps still sit inside the window.
func TestChromiumMonitorReconnectAfterAbandonedHoldRestartsKiosk(t *testing.T) {
	restartFile, _, fallbackFile, rebootFile := installFallbackStubs(t)
	endpoint := closedLocalHTTPEndpoint(t)
	handler := NewCommandHandler(zap.NewNop(), nil)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), handler)
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = disconnectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.fallbackSince = time.Now().Add(-time.Minute)
	monitor.restartHistory = []time.Time{time.Now(), time.Now(), time.Now()}
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure while headless")
	}

	// Display back; the reconnect grants a fresh grace, which we then let expire.
	monitor.drmSysfsRoot = connectedDRMRoot(t)
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.mu.Unlock()
	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	if got := readRestartCount(t, restartFile); got != "1" {
		t.Fatalf("expected a kiosk restart after the reconnect grace, got %s", got)
	}
	if got := readRestartCount(t, fallbackFile); got != "0" {
		t.Fatalf("reconnect must not re-enter the fallback, got %s", got)
	}
	if got := readRestartCount(t, rebootFile); got != "0" {
		t.Fatalf("reconnect must not reboot, got %s", got)
	}
}

// TestChromiumMonitorFallbackNotArmedWhenKioskStopFails pins that a failed
// `systemctl --user stop chromium-kiosk.service` takes the immediate-reboot
// path: cage would still own DRM, so plymouth could not render and a 15-minute
// hold would sit on a frozen kiosk.
func TestChromiumMonitorFallbackNotArmedWhenKioskStopFails(t *testing.T) {
	_, stopFile, fallbackFile, rebootFile := installFallbackStubs(t)
	if err := os.WriteFile(stopFile+".fail", nil, 0o600); err != nil {
		t.Fatalf("failed to arm stop failure: %v", err)
	}
	endpoint := closedLocalHTTPEndpoint(t)
	handler := NewCommandHandler(zap.NewNop(), nil)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), handler)
	monitor.ttyActiveFile = ttyActiveFixture(t, "tty1")
	monitor.drmSysfsRoot = connectedDRMRoot(t)

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + time.Minute))
	monitor.restartHistory = []time.Time{time.Now(), time.Now()}
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected failure against closed endpoint")
	}
	if got := readRestartCount(t, fallbackFile); got != "0" {
		t.Fatalf("fallback unit must not be started after a failed kiosk stop, got %s", got)
	}
	if got := readRestartCount(t, rebootFile); got != "1" {
		t.Fatalf("expected the immediate reboot path, got %s reboots", got)
	}
	if handler.isFallbackShown() {
		t.Fatal("fallbackShown must stay false after a failed stop")
	}
	monitor.mu.Lock()
	inFallback := !monitor.fallbackSince.IsZero()
	monitor.mu.Unlock()
	if inFallback {
		t.Fatal("hold must not be armed after a failed stop")
	}
}
