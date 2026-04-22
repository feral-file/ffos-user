package main

import (
	"context"
	"errors"
	"time"

	"github.com/feral-file/ffos-user/components/feral-watchdog/ff1config"
	"github.com/feral-file/ffos-user/components/feral-watchdog/packages/cdp"

	"go.uber.org/zap"
)

// cdpNavigator is the subset of CDP used for post-recovery playlist navigation (unit-tested).
type cdpNavigator interface {
	Navigate(ctx context.Context, url string) error
}

const (
	// Chromium configuration
	SYSTEMD_CHECK_INTERVAL = 30 * time.Second // Check systemd service status every 30 seconds

	// After dependency recovery, the local static server may lag Chromium by a short window.
	playerNavigateRetries    = 4
	playerNavigateRetryDelay = 400 * time.Millisecond
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
					navURL := ff1config.ResolveWebappURL()
					resumePlaylistAfterServiceRecovery(ctx, m.cdpClient, m.logger, service, navURL, nil)
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

// resumePlaylistAfterServiceRecovery mirrors feral-setupd local player policy.
// waitTCP may be nil (production uses ff1config.WaitLocalBundlePlayerTCP); tests inject fast failures.
func resumePlaylistAfterServiceRecovery(
	ctx context.Context,
	client cdpNavigator,
	log *zap.Logger,
	service string,
	navURL string,
	waitTCP func(context.Context) error,
) {
	waitFn := waitTCP
	if waitFn == nil {
		waitFn = ff1config.WaitLocalBundlePlayerTCP
	}
	var navErr error
	if ff1config.IsLocalBundlePlayerURL(navURL) {
		if err := waitFn(ctx); err != nil {
			navErr = err
		} else {
			navErr = navigatePlayerWithRetries(ctx, client, navURL, log)
		}
	} else {
		navErr = navigatePlayerWithRetries(ctx, client, navURL, log)
	}
	if navErr != nil {
		log.Error("Systemd: Failed to resume playlist after service recovery",
			zap.String("service", service),
			zap.Error(navErr))
		if ff1config.IsLocalBundlePlayerURL(navURL) {
			msgURL := ff1config.LauncherMessageNavigateURL(ff1config.LocalPlayerUnavailableMessage)
			if msgNavErr := client.Navigate(ctx, msgURL); msgNavErr != nil {
				log.Error("Systemd: Failed to navigate local player error message",
					zap.Error(msgNavErr))
			}
		}
	} else {
		log.Info("Systemd: Playlist resumed after service recovery",
			zap.String("service", service))
	}
}

func navigatePlayerWithRetries(ctx context.Context, client cdpNavigator, url string, log *zap.Logger) error {
	var lastErr error
	for attempt := 1; attempt <= playerNavigateRetries; attempt++ {
		if attempt > 1 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(playerNavigateRetryDelay):
			}
		}
		err := client.Navigate(ctx, url)
		if err == nil {
			return nil
		}
		lastErr = err
		if log != nil {
			log.Warn("Systemd: player Navigate failed, retrying",
				zap.Int("attempt", attempt),
				zap.Int("max_attempts", playerNavigateRetries),
				zap.Error(err))
		}
	}
	return lastErr
}
