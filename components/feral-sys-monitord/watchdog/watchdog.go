package watchdog

import (
	"context"
	"time"

	"github.com/coreos/go-systemd/v22/daemon"
	"go.uber.org/zap"
)

const INTERVAL = 15 * time.Second

//go:generate mockgen -source=watchdog.go -destination=../mocks/watchdog.go -package=mocks -mock_names=Watchdog=MockWatchdog

type Watchdog interface {
	// Start starts the watchdog
	Start(ctx context.Context)

	// Stop stops the watchdog
	Stop()
}

// watchdog handles systemd watchdog notifications
type watchdog struct {
	done   chan struct{}
	logger *zap.Logger
}

// New creates a new watchdog with the given interval
func New(logger *zap.Logger) Watchdog {
	return &watchdog{
		done:   make(chan struct{}),
		logger: logger,
	}
}

// Start starts the watchdog process
func (w *watchdog) Start(ctx context.Context) {
	go func() {
		ticker := time.NewTicker(INTERVAL)
		defer ticker.Stop()

		w.logger.Info("Starting watchdog", zap.Duration("interval", INTERVAL))

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
	}()
}

// Stop stops the watchdog process
func (w *watchdog) Stop() {
	select {
	case <-w.done:
		// Already closed
		return
	default:
		close(w.done)
	}
}
