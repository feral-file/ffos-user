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
		"feral-player.service":       true,
		"feral-setupd.service":       true,
		"feral-controld.service":     true,
		"feral-sys-monitord.service": true,
	}

	// servicesReportInactiveAsFailure lists services whose INACTIVE reading is a
	// hard fault. feral-setupd and feral-controld were formerly
	// PartOf=chromium-ready.target and legitimately torn down with it (the old
	// "expected teardown" window); they have since been decoupled and run
	// unconditionally at boot, so an inactive reading now means they failed to
	// stay up and must be reported (metric + notification) regardless of any
	// target state. feral-player and feral-sys-monitord are intentionally
	// excluded to preserve their existing log-only inactive handling.
	servicesReportInactiveAsFailure = map[string]bool{
		"feral-controld.service": true,
		"feral-setupd.service":   true,
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

		// Collapse the raw state into a single "down" verdict so the failure
		// reporting below is uniform. FAILED is always down (the 90 s
		// recent-exit grace is already applied upstream in
		// checkSystemdUserServiceStatus, which returns ACTIVE for a recently
		// exited service). INACTIVE is down only for the services that must run
		// unconditionally now that setupd/controld are decoupled from
		// chromium-ready.target; for player/sys-monitord inactive stays a
		// log-only observation, preserving their prior behavior.
		lastState := m.lastServiceStates[service]
		wasDown := m.isServiceDown(service, lastState)
		nowDown := m.isServiceDown(service, state)
		hasRecovered := wasDown && *state == SYSTEMD_SERVICE_STATUS_ACTIVE
		hasJustFailed := !wasDown && nowDown

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
			m.logger.Error("Systemd: Service is failed",
				zap.String("service", service),
				zap.String("dependency", service))
		case SYSTEMD_SERVICE_STATUS_INACTIVE:
			// With setupd/controld no longer PartOf=chromium-ready.target there
			// is no "expected teardown" window: every monitored service is meant
			// to be up, so any inactive reading is an Error. setupd/controld
			// additionally escalate to a failure metric via the nowDown block.
			m.logger.Error("Systemd: Service is inactive",
				zap.String("service", service))
		default:
			m.logger.Error("Systemd: Unknown service state",
				zap.String("service", service),
				zap.Any("state", state))
		}

		// Uniform failure reporting keyed off the normalized down verdict, so an
		// inactive setupd/controld raises the same per-service metric + incident
		// notification as a failed service.
		if nowDown {
			hasFailedService = true
			if hasJustFailed {
				if m.vmagentClient != nil {
					m.vmagentClient.SendServiceFailedMetric(ctx, service)
				} else {
					m.logger.Warn("Vmagent client is nil, skipping service failed metric")
				}
			}
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

// isServiceDown reports whether a service is in a state that must be reported as
// a failure. FAILED is down for every service. INACTIVE is down only for the
// services in servicesReportInactiveAsFailure (setupd/controld), which are now
// expected to run unconditionally; for the others inactive is not a reported
// failure. A nil state (never observed yet) is treated as not-down.
func (m *SystemdMonitor) isServiceDown(service string, state *SystemdServiceStatus) bool {
	if state == nil {
		return false
	}
	switch *state {
	case SYSTEMD_SERVICE_STATUS_FAILED:
		return true
	case SYSTEMD_SERVICE_STATUS_INACTIVE:
		return servicesReportInactiveAsFailure[service]
	default:
		return false
	}
}
