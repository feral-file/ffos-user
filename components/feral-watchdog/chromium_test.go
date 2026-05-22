package main

import (
	"context"
	"net"
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

func TestChromiumMonitorDetachedDisplaySkipsHealthCheckAndRestart(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.displayDetector = staticDisplayDetector{state: DisplayState{Known: true, Connected: false}}

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + 30*time.Second))
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected detached display check to be skipped without error, got %v", err)
	}
	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected no restart while display is detached, got %s", got)
	}
}

func TestChromiumMonitorDetachedDisplayResetsPostConnectState(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.displayDetector = staticDisplayDetector{state: DisplayState{Known: true, Connected: false}}

	monitor.mu.Lock()
	monitor.hasEverConnected = true
	monitor.lastSuccessfulResp = time.Now().Add(-(CHROMIUM_HANG_THRESHOLD + 5*time.Second))
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err != nil {
		t.Fatalf("expected detached display check to be skipped without error, got %v", err)
	}

	monitor.mu.Lock()
	if monitor.hasEverConnected {
		t.Fatal("expected detached display to reset post-connect state")
	}
	if !monitor.lastSuccessfulResp.IsZero() {
		t.Fatalf("expected detached display to clear last success, got %v", monitor.lastSuccessfulResp)
	}
	monitor.mu.Unlock()
	if got := readRestartCount(t, countFile); got != "0" {
		t.Fatalf("expected no restart while display is detached, got %s", got)
	}
}

func TestChromiumMonitorUnknownDisplayPreservesStartupEscalation(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))
	monitor.displayDetector = staticDisplayDetector{state: DisplayState{Known: false}}

	monitor.mu.Lock()
	monitor.monitorStart = time.Now().Add(-(CHROMIUM_STARTUP_GRACE + 30*time.Second))
	monitor.mu.Unlock()

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected health check to fail against closed endpoint")
	}
	if got := readRestartCount(t, countFile); got != "1" {
		t.Fatalf("expected existing restart behavior when display state is unknown, got %s", got)
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
