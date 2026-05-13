package metric

import (
	"context"
	"errors"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func TestTickSuppressesBestEffortMetricErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	core, logs := observer.New(zapcore.DebugLevel)
	monitor := NewSysResMonitor(ctx, zap.New(core))

	monitor.tick(ctx, time.Hour, func(context.Context) error {
		return errBestEffortMetricUnavailable
	})

	if logs.FilterMessage("Failed to monitor system resources").Len() != 0 {
		t.Fatal("best-effort metric errors should not be promoted to generic error logs")
	}
}

func TestTickLogsUnexpectedMetricErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	core, logs := observer.New(zapcore.DebugLevel)
	monitor := NewSysResMonitor(ctx, zap.New(core))

	monitor.tick(ctx, time.Hour, func(context.Context) error {
		return errors.New("unexpected metric failure")
	})

	entry := logs.FilterMessage("Failed to monitor system resources")
	if entry.Len() != 1 {
		t.Fatalf("unexpected metric errors should be logged once, got %d", entry.Len())
	}
	if entry.All()[0].Level != zapcore.ErrorLevel {
		t.Fatalf("unexpected metric error should remain error-level, got %s", entry.All()[0].Level)
	}
}
