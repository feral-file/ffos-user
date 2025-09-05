package main

import (
	"context"
	"errors"
	"time"

	"github.com/feral-file/ffos-user/components/feral-watchdog/packages/cdp"

	"go.uber.org/zap"
)

const (
	// Chromium configuration
	SYSTEMD_CHECK_INTERVAL = 30 * time.Second // Check systemd service status every 30 seconds
)

var (
	systemdServices = map[string]bool{
		"feral-setupd.service":       true,
		"feral-connectd.service":     true,
		"feral-sys-monitord.service": true,
		"feral-app-monitord.service": true,
	}
)

// SystemdMonitor monitors systemd services
type SystemdMonitor struct {
	cdpClient      *cdp.Client
	logger         *zap.Logger
	commandHandler *CommandHandler
}

// NewSystemdMonitor creates a new Chromium monitor instance
func NewSystemdMonitor(cdpClient *cdp.Client, logger *zap.Logger, commandHandler *CommandHandler) *SystemdMonitor {
	return &SystemdMonitor{
		cdpClient:      cdpClient,
		logger:         logger,
		commandHandler: commandHandler,
	}
}

// Start begins the systemd monitoring process
func (m *SystemdMonitor) Start(ctx context.Context) {
	m.logger.Info("Systemd: Starting systemd monitor",
		zap.Duration("check_interval", SYSTEMD_CHECK_INTERVAL))

	ticker := time.NewTicker(SYSTEMD_CHECK_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("Systemd: Monitor shutting down")
			return
		case <-ticker.C:
			if err := m.check(ctx); err != nil {
				m.logger.Warn("Systemd: Health check failed", zap.Error(err))
			}
		}
	}
}

func (m *SystemdMonitor) check(ctx context.Context) error {
	for service := range systemdServices {
		state, err := m.commandHandler.checkSystemdUserServiceStatus(ctx, service)
		if err != nil {
			m.logger.Error("Systemd: Failed to get service state",
				zap.String("service", service),
				zap.Error(err))
			return err
		}
		if state == nil {
			m.logger.Error("Systemd: Service state is nil",
				zap.String("service", service))
			return errors.New("service state is nil")
		}

		switch *state {
		case SYSTEMD_SERVICE_STATUS_ACTIVE:
			m.logger.Debug("Systemd: Service is active",
				zap.String("service", service))
		case SYSTEMD_SERVICE_STATUS_FAILED:
			m.logger.Warn("Systemd: Service is failed",
				zap.String("service", service))
			// Send service failed to start notification to website via CDP
			if m.cdpClient != nil {
				if err := m.cdpClient.ShowServiceFailedToStartPage(ctx); err != nil {
					m.logger.Error("Systemd: Failed to send service failed to start notification via CDP",
						zap.String("service", service),
						zap.Error(err))
				} else {
					m.logger.Info("Systemd: Sent service failed to start notification via CDP",
						zap.String("service", service))
				}
			}
		case SYSTEMD_SERVICE_STATUS_INACTIVE:
			m.logger.Warn("Systemd: Service is inactive",
				zap.String("service", service))
		default:
			m.logger.Error("Systemd: Unknown service state",
				zap.String("service", service),
				zap.Any("state", state))
		}
	}

	return nil
}
