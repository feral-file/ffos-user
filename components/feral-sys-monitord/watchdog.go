package main

import (
	"context"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"go.uber.org/zap"
)

// Watchdog handles systemd watchdog notifications
type Watchdog struct {
	interval time.Duration
	done     chan struct{}
	logger   *zap.Logger
}

// NewWatchdog creates a new watchdog with the given interval
func NewWatchdog(interval time.Duration, logger *zap.Logger) *Watchdog {
	return &Watchdog{
		interval: interval,
		done:     make(chan struct{}),
		logger:   logger,
	}
}

// Start starts the watchdog process
func (w *Watchdog) Start(ctx context.Context) {
	ticker := time.NewTicker(w.interval)
	defer ticker.Stop()

	w.logger.Info("Starting watchdog", zap.Duration("interval", w.interval))

	for {
		select {
		case <-ticker.C:
			sent, err := daemon.SdNotify(false, daemon.SdNotifyWatchdog)
			if err != nil {
				w.logger.Error("Failed to notify systemd", zap.Error(err))
			}
			if !sent {
				w.logger.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
			}
		case <-ctx.Done():
			w.logger.Info("Stopping watchdog due to context cancellation")
			return
		case <-w.done:
			w.logger.Info("Stopping watchdog")
			return
		}
	}
}

// Stop stops the watchdog process
func (w *Watchdog) Stop() {
	select {
	case <-w.done:
		// Already closed
		return
	default:
		close(w.done)
	}
}
