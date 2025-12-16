package monitor

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/feral-file/ffos-user/components/feral-watchdog/cdp"
	"github.com/feral-file/ffos-user/components/feral-watchdog/command"

	"go.uber.org/zap"
)

const (
	SYSTEMD_CHECK_INTERVAL = 30 * time.Second

	// Systemd services
	SYSTEMD_SERVICE_FERAL_SETUPD       = "feral-setupd.service"
	SYSTEMD_SERVICE_FERAL_CONTROLD     = "feral-controld.service"
	SYSTEMD_SERVICE_FERAL_SYS_MONITORD = "feral-sys-monitord.service"
)

var systemdServices = []string{
	SYSTEMD_SERVICE_FERAL_SETUPD,
	SYSTEMD_SERVICE_FERAL_CONTROLD,
	SYSTEMD_SERVICE_FERAL_SYS_MONITORD,
}

// SystemdMonitor monitors systemd services
//
//go:generate mockgen -source=systemd.go -destination=../mocks/systemd.go -package=mocks -mock_names=SystemdMonitor=MockSystemdMonitor
type SystemdMonitor interface {
	// Start begins the systemd monitoring process
	Start(ctx context.Context)

	// Stop stops the systemd monitoring process
	Stop()
}
type systemdMonitor struct {
	mu sync.RWMutex

	// Dependencies
	cdp         cdp.CDP
	commandExec command.Executor
	logger      *zap.Logger

	// State
	doneChan chan struct{}
	started  bool
}

// NewSystemdMonitor creates a new Systemd monitor instance
func NewSystemdMonitor(cdp cdp.CDP, commandExec command.Executor, logger *zap.Logger) SystemdMonitor {
	return &systemdMonitor{
		cdp:         cdp,
		commandExec: commandExec,
		logger:      logger,
	}
}

// Start begins the systemd monitoring process
func (s *systemdMonitor) Start(ctx context.Context) {
	s.logger.Info("Systemd: Starting systemd monitor",
		zap.Duration("check_interval", SYSTEMD_CHECK_INTERVAL))

	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		s.logger.Info("Systemd: Monitor already started")
		return
	}

	s.started = true
	s.doneChan = make(chan struct{})
	s.mu.Unlock()

	go s.background(ctx)
}

func (s *systemdMonitor) background(ctx context.Context) {
	s.logger.Info("Systemd: Monitor background goroutine started")

	ticker := time.NewTicker(SYSTEMD_CHECK_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			s.logger.Info("Systemd: Monitor shutting down due to context cancellation")
			return
		case <-s.doneChan:
			ticker.Stop()
			s.logger.Info("Systemd: Monitor shutting down due to done channel")
			return
		case <-ticker.C:
			if err := s.check(ctx); err != nil {
				s.logger.Warn("Systemd: Health check failed", zap.Error(err))
			}
		}
	}
}

func (s *systemdMonitor) check(ctx context.Context) error {
	for _, svc := range systemdServices {
		status, err := s.commandExec.CheckSystemdUserServiceStatus(ctx, svc)
		if err != nil {
			s.logger.Error("Systemd: Failed to get service state",
				zap.String("service", svc),
				zap.Error(err))
			return err
		}
		if status == nil {
			s.logger.Error("Systemd: Service state is nil",
				zap.String("service", svc))
			return errors.New("service state is nil")
		}

		switch *status {
		case command.SYSTEMD_SERVICE_STATUS_ACTIVE:
			s.logger.Debug("Systemd: Service is active",
				zap.String("service", svc))
		case command.SYSTEMD_SERVICE_STATUS_FAILED:
			s.logger.Error("Systemd: Service is failed",
				zap.String("service", svc),
				zap.String("dependency", svc))
			// Show service failed to start page on website via CDP
			if s.cdp != nil && s.cdp.Initialized() {
				if err := s.cdp.ShowServiceFailedToStart(ctx); err != nil {
					s.logger.Error("Systemd: Failed to show service failed to start page via CDP",
						zap.String("service", svc),
						zap.Error(err))
				} else {
					s.logger.Info("Systemd: Showed service failed to start page via CDP",
						zap.String("service", svc))
				}
			}
		case command.SYSTEMD_SERVICE_STATUS_INACTIVE:
			s.logger.Error("Systemd: Service is inactive",
				zap.String("service", svc),
				zap.String("dependency", svc))
		default:
			s.logger.Error("Systemd: Unknown service state",
				zap.String("service", svc),
				zap.Any("state", status))
		}
	}

	return nil
}

func (s *systemdMonitor) Stop() {
	s.logger.Info("Systemd: Stopping systemd monitor")

	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		s.logger.Info("Systemd: Monitor already stopped")
		return
	}

	s.started = false
	s.mu.Unlock()

	select {
	case <-s.doneChan:
		// Already closed
		return
	default:
		close(s.doneChan)
	}

	s.logger.Info("Systemd: Monitor stopped")
}
