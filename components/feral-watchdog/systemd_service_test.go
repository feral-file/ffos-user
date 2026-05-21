package main

import (
	"context"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestSystemdMonitor_ServiceFailedMetric_EmittedOnceWhileStuck(t *testing.T) {
	monitor, collector := newTestSystemdMonitor(t)
	ctx := context.Background()

	setServiceStates(t, "active", "active", "active", "active")
	requireNoError(t, monitor.check(ctx))

	setServiceStates(t, "active", "active", "failed", "active")
	requireNoError(t, monitor.check(ctx))
	requireNoError(t, monitor.check(ctx))

	metrics := collector.Metrics()
	assertMetricCount(t, metrics, `ff_service_failed{service="feral-controld.service"} 1`, 1)
	assertMetricCount(t, metrics, "service_failed_incident 1", 1)
}

func TestSystemdMonitor_ServiceFailedIncident_OneForCascade(t *testing.T) {
	monitor, collector := newTestSystemdMonitor(t)
	ctx := context.Background()

	setServiceStates(t, "active", "active", "active", "active")
	requireNoError(t, monitor.check(ctx))

	// First service fails -> incident opens.
	setServiceStates(t, "active", "active", "failed", "active")
	requireNoError(t, monitor.check(ctx))

	// Additional service fails while incident is already open.
	setServiceStates(t, "active", "failed", "failed", "active")
	requireNoError(t, monitor.check(ctx))

	metrics := collector.Metrics()
	assertMetricCount(t, metrics, "service_failed_incident 1", 1)
	assertMetricCount(t, metrics, `ff_service_failed{service="feral-controld.service"} 1`, 1)
	assertMetricCount(t, metrics, `ff_service_failed{service="feral-setupd.service"} 1`, 1)
}

func TestSystemdMonitor_ServiceFailedIncident_ReopensAfterRecovery(t *testing.T) {
	monitor, collector := newTestSystemdMonitor(t)
	ctx := context.Background()

	setServiceStates(t, "active", "active", "active", "active")
	requireNoError(t, monitor.check(ctx))

	setServiceStates(t, "active", "active", "failed", "active")
	requireNoError(t, monitor.check(ctx))

	// All services recover -> incident latch resets.
	setServiceStates(t, "active", "active", "active", "active")
	requireNoError(t, monitor.check(ctx))

	setServiceStates(t, "active", "failed", "active", "active")
	requireNoError(t, monitor.check(ctx))

	metrics := collector.Metrics()
	assertMetricCount(t, metrics, "service_failed_incident 1", 2)
	assertMetricCount(t, metrics, `ff_service_failed{service="feral-controld.service"} 1`, 1)
	assertMetricCount(t, metrics, `ff_service_failed{service="feral-setupd.service"} 1`, 1)
}

func TestSystemdMonitor_PlayerServiceFailedIsTracked(t *testing.T) {
	monitor, collector := newTestSystemdMonitor(t)
	ctx := context.Background()

	setServiceStates(t, "active", "active", "active", "active")
	requireNoError(t, monitor.check(ctx))

	setServiceStates(t, "failed", "active", "active", "active")
	requireNoError(t, monitor.check(ctx))
	requireNoError(t, monitor.check(ctx))

	metrics := collector.Metrics()
	assertMetricCount(t, metrics, `ff_service_failed{service="feral-player.service"} 1`, 1)
	assertMetricCount(t, metrics, "service_failed_incident 1", 1)
}

// When chromium-ready.target is down, an inactive feral-controld/feral-setupd
// is the expected teardown window and is logged at Info; feral-player and
// feral-sys-monitord are not gated, so their inactive state stays an Error.
func TestSystemdMonitor_InactiveServiceLogLevel_TargetDown(t *testing.T) {
	installFakeSystemctl(t)
	t.Setenv("FF_TEST_CHROMIUM_READY_STATE", "inactive")

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	monitor := NewSystemdMonitor(nil, logger, NewCommandHandler(logger, nil), nil)

	setServiceStates(t, "inactive", "inactive", "inactive", "inactive")
	requireNoError(t, monitor.check(context.Background()))

	assertInactiveLogLevels(t, logs, map[string]zapcore.Level{
		"feral-controld.service":     zapcore.InfoLevel,
		"feral-setupd.service":       zapcore.InfoLevel,
		"feral-player.service":       zapcore.ErrorLevel,
		"feral-sys-monitord.service": zapcore.ErrorLevel,
	})
}

// When chromium-ready.target is active, feral-controld/feral-setupd must be
// running; an inactive reading there is a genuine fault and is logged at Error.
func TestSystemdMonitor_InactiveServiceLogLevel_TargetActive(t *testing.T) {
	installFakeSystemctl(t)
	t.Setenv("FF_TEST_CHROMIUM_READY_STATE", "active")

	core, logs := observer.New(zapcore.DebugLevel)
	logger := zap.New(core)
	monitor := NewSystemdMonitor(nil, logger, NewCommandHandler(logger, nil), nil)

	// Only controld/setupd inactive; player/sys-monitord stay active.
	setServiceStates(t, "active", "inactive", "inactive", "active")
	requireNoError(t, monitor.check(context.Background()))

	assertInactiveLogLevels(t, logs, map[string]zapcore.Level{
		"feral-controld.service": zapcore.ErrorLevel,
		"feral-setupd.service":   zapcore.ErrorLevel,
	})
}

// assertInactiveLogLevels checks that every "Systemd: Service is inactive" log
// entry was emitted at the level expected for its service, and that every
// expected service produced exactly such an entry.
func assertInactiveLogLevels(t *testing.T, logs *observer.ObservedLogs, want map[string]zapcore.Level) {
	t.Helper()
	seen := make(map[string]bool)
	for _, entry := range logs.FilterMessage("Systemd: Service is inactive").All() {
		service, _ := entry.ContextMap()["service"].(string)
		wantLevel, ok := want[service]
		if !ok {
			t.Fatalf("inactive log for unexpected service %q", service)
		}
		if entry.Level != wantLevel {
			t.Fatalf("service %q inactive logged at %v, want %v", service, entry.Level, wantLevel)
		}
		seen[service] = true
	}
	for service := range want {
		if !seen[service] {
			t.Fatalf("no inactive log entry recorded for %q", service)
		}
	}
}

func newTestSystemdMonitor(t *testing.T) (*SystemdMonitor, *metricCollectorRoundTripper) {
	t.Helper()
	installFakeSystemctl(t)
	logger := zap.NewNop()

	collector := &metricCollectorRoundTripper{}
	vmagentClient := &VmagentClient{
		client: &http.Client{Transport: collector},
		logger: logger,
		url:    "http://vmagent.test/api/v1/import/prometheus",
	}

	commandHandler := NewCommandHandler(logger, nil)
	monitor := NewSystemdMonitor(nil, logger, commandHandler, vmagentClient)
	return monitor, collector
}

func installFakeSystemctl(t *testing.T) {
	t.Helper()

	dir := t.TempDir()
	systemctlPath := filepath.Join(dir, "systemctl")
	systemctlScript := `#!/bin/sh
verb=""
unit=""
prev=""
for arg in "$@"; do
  if [ "$prev" = "show" ] || [ "$prev" = "is-active" ]; then
    verb="$prev"
    unit="$arg"
    break
  fi
  prev="$arg"
done

if [ "$verb" = "is-active" ]; then
  case "$unit" in
    "chromium-ready.target")
      echo "${FF_TEST_CHROMIUM_READY_STATE:-inactive}"
      ;;
    *)
      echo "active"
      ;;
  esac
  exit 0
fi

state="active"
case "$unit" in
  "feral-player.service")
    state="${FF_TEST_PLAYER_STATE:-active}"
    ;;
  "feral-setupd.service")
    state="${FF_TEST_SETUPD_STATE:-active}"
    ;;
  "feral-controld.service")
    state="${FF_TEST_CONTROLD_STATE:-active}"
    ;;
  "feral-sys-monitord.service")
    state="${FF_TEST_SYSMONITORD_STATE:-active}"
    ;;
esac

echo "ActiveState=$state"
echo "ExecMainExitTimestampMonotonic=0"
`

	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(systemctlPath, []byte(systemctlScript), 0o755); err != nil {
		t.Fatalf("failed to create fake systemctl: %v", err)
	}

	catPath := filepath.Join(dir, "cat")
	catScript := `#!/bin/sh
if [ "$1" = "/proc/uptime" ]; then
  echo "1000.00 0.00"
  exit 0
fi

exec /bin/cat "$@"
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(catPath, []byte(catScript), 0o755); err != nil {
		t.Fatalf("failed to create fake cat: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))

	resolved, err := exec.LookPath("systemctl")
	if err != nil {
		t.Fatalf("failed to resolve fake systemctl: %v", err)
	}
	if resolved != systemctlPath {
		t.Fatalf("systemctl resolves to %q, expected %q", resolved, systemctlPath)
	}

	resolvedCat, err := exec.LookPath("cat")
	if err != nil {
		t.Fatalf("failed to resolve fake cat: %v", err)
	}
	if resolvedCat != catPath {
		t.Fatalf("cat resolves to %q, expected %q", resolvedCat, catPath)
	}
}

func setServiceStates(t *testing.T, player, setupd, controld, sysMonitord string) {
	t.Helper()
	t.Setenv("FF_TEST_PLAYER_STATE", player)
	t.Setenv("FF_TEST_SETUPD_STATE", setupd)
	t.Setenv("FF_TEST_CONTROLD_STATE", controld)
	t.Setenv("FF_TEST_SYSMONITORD_STATE", sysMonitord)
}

func requireNoError(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func assertMetricCount(t *testing.T, metrics []string, target string, want int) {
	t.Helper()
	got := 0
	for _, metric := range metrics {
		if metric == target {
			got++
		}
	}
	if got != want {
		t.Fatalf("metric %q count mismatch: got=%d want=%d metrics=%v", target, got, want, metrics)
	}
}

type metricCollectorRoundTripper struct {
	mu      sync.Mutex
	metrics []string
}

func (m *metricCollectorRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	statusCode := http.StatusNoContent
	if req.Method == http.MethodGet {
		statusCode = http.StatusOK
	}

	if req.Method == http.MethodPost {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		m.mu.Lock()
		m.metrics = append(m.metrics, strings.TrimSpace(string(body)))
		m.mu.Unlock()
	}

	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(strings.NewReader("")),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

func (m *metricCollectorRoundTripper) Metrics() []string {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([]string, len(m.metrics))
	copy(out, m.metrics)
	return out
}
