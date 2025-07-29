package main

import (
	"context"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"go.uber.org/zap"
)

const (
	// Systemd watchdog configuration
	SYSTEMD_NOTIFY_INTERVAL = 10 * time.Second // Notify systemd every 10 seconds
)

// SystemdWatchdog handles the systemd watchdog notifications
type SystemdWatchdog struct {
	logger *zap.Logger
}

// NewSystemdWatchdog creates a new systemd watchdog handler
func NewSystemdWatchdog(logger *zap.Logger) *SystemdWatchdog {
	return &SystemdWatchdog{
		logger: logger,
	}
}

// NotifyReady notifies systemd that the service is ready
func (sw *SystemdWatchdog) NotifyReady() error {
	if _, err := daemon.SdNotify(false, daemon.SdNotifyReady); err != nil {
		sw.logger.Error("Failed to notify systemd ready", zap.Error(err))
		return err
	}
	sw.logger.Info("Notified systemd that we're ready")
	return nil
}

// Start periodically sends watchdog keep-alive pings to systemd
func (sw *SystemdWatchdog) Start(ctx context.Context) {
	ticker := time.NewTicker(SYSTEMD_NOTIFY_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			sw.logger.Info("Systemd watchdog goroutine shutting down")
			return
		case <-ticker.C:
			if _, err := daemon.SdNotify(false, daemon.SdNotifyWatchdog); err != nil {
				sw.logger.Error("Failed to send watchdog ping to systemd", zap.Error(err))
			}
		}
	}
}
