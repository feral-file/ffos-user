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
		"feral-controld.service":     true,
		"feral-sys-monitord.service": true,
	}
)

// SystemdMonitor monitors systemd services
type SystemdMonitor struct {
	cdpClient               *cdp.Client
	logger                  *zap.Logger
	commandHandler          *CommandHandler
	vmagentClient           *VmagentClient
	lastServiceStates       map[string]*SystemdServiceStatus // Track last state to detect recovery
	failureIncidentReported bool
}

// NewSystemdMonitor creates a new Chromium monitor instance
func NewSystemdMonitor(cdpClient *cdp.Client, logger *zap.Logger, commandHandler *CommandHandler, vmagentClient *VmagentClient) *SystemdMonitor {
	return &SystemdMonitor{
		cdpClient:               cdpClient,
		logger:                  logger,
		commandHandler:          commandHandler,
		vmagentClient:           vmagentClient,
		lastServiceStates:       make(map[string]*SystemdServiceStatus),
		failureIncidentReported: false,
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
	hasFailedService := false

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

		// Check if service just recovered (failed -> active)
		lastState := m.lastServiceStates[service]
		hasRecovered := lastState != nil && *lastState == SYSTEMD_SERVICE_STATUS_FAILED && *state == SYSTEMD_SERVICE_STATUS_ACTIVE
		hasJustFailed := (lastState == nil || *lastState != SYSTEMD_SERVICE_STATUS_FAILED) && *state == SYSTEMD_SERVICE_STATUS_FAILED

		switch *state {
		case SYSTEMD_SERVICE_STATUS_ACTIVE:
			if hasRecovered {
				m.logger.Info("Systemd: Service recovered, resume playlist",
					zap.String("service", service))
				if m.cdpClient != nil {
					if err := m.cdpClient.Navigate(ctx, cdp.DISPLAY_FERALFILE_URL); err != nil {
						m.logger.Error("Systemd: Failed to resume playlist after service recovery",
							zap.String("service", service),
							zap.Error(err))
					} else {
						m.logger.Info("Systemd: Playlist resumed after service recovery",
							zap.String("service", service))
					}
				}
			} else {
				m.logger.Debug("Systemd: Service is active",
					zap.String("service", service))
			}
		case SYSTEMD_SERVICE_STATUS_FAILED:
			hasFailedService = true
			m.logger.Error("Systemd: Service is failed",
				zap.String("service", service),
				zap.String("dependency", service))

			// Send service failed metric to vmagent
			if hasJustFailed {
				if m.vmagentClient != nil {
					m.vmagentClient.SendServiceFailedMetric(ctx, service)
				} else {
					m.logger.Warn("Vmagent client is nil, skipping service failed metric")
				}
			}
		case SYSTEMD_SERVICE_STATUS_INACTIVE:
			m.logger.Error("Systemd: Service is inactive",
				zap.String("service", service),
				zap.String("dependency", service))
		default:
			m.logger.Error("Systemd: Unknown service state",
				zap.String("service", service),
				zap.Any("state", state))
		}

		// Update last state
		m.lastServiceStates[service] = state
	}

	if hasFailedService && !m.failureIncidentReported {
		if m.cdpClient != nil {
			if err := m.cdpClient.SendServiceFailedEvent(ctx); err != nil {
				m.logger.Error("Systemd: Failed to send service failed to start notification via CDP",
					zap.Error(err))
			} else {
				m.logger.Info("Systemd: Sent service failed to start notification via CDP")
			}
		}

		if m.vmagentClient != nil {
			m.vmagentClient.SendServiceFailedIncidentMetric(ctx)
		} else {
			m.logger.Warn("Vmagent client is nil, skipping service failed incident metric")
		}

		m.failureIncidentReported = true
	}

	if !hasFailedService {
		m.failureIncidentReported = false
	}

	return nil
}
