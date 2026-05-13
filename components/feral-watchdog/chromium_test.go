package main

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestChromiumMonitorFirstFailureUsesStartupGrace(t *testing.T) {
	countFile := installCountingSystemctl(t)
	endpoint := closedLocalHTTPEndpoint(t)
	monitor := NewChromiumMonitor(endpoint, zap.NewNop(), NewCommandHandler(zap.NewNop(), nil))

	if err := monitor.check(context.Background()); err == nil {
		t.Fatal("expected health check to fail against closed endpoint")
	}

	// #nosec G304 -- countFile is created by this test inside t.TempDir.
	countBytes, err := os.ReadFile(countFile)
	if err != nil {
		t.Fatalf("failed to read restart count: %v", err)
	}
	if got := strings.TrimSpace(string(countBytes)); got != "0" {
		t.Fatalf("expected no immediate kiosk restart during startup grace, got restart count %s", got)
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
exit 0
`
	// #nosec G306 -- test helper script must be executable.
	if err := os.WriteFile(systemctlPath, []byte(systemctlScript), 0o755); err != nil {
		t.Fatalf("failed to create fake systemctl: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return countFile
}
