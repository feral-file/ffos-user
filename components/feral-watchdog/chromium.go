package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

// errChromiumHeadless tags a failed health check that happened while no
// display is connected. Headless devices deliberately do not run Chromium
// (the kiosk waits for a display), so these failures are expected and the
// monitor loop logs them at debug instead of warning every check interval.
var errChromiumHeadless = errors.New("chromium down while headless (expected)")

const (
	// Chromium configuration
	CHROMIUM_CHECK_INTERVAL  = 5 * time.Second // Check CDP every 5 seconds
	CHROMIUM_REQUEST_TIMEOUT = 3 * time.Second
	// CHROMIUM_HANG_THRESHOLD is the steady-state budget: once we have seen
	// at least one 200 from /json/version, this much silence is treated as a
	// real renderer hang. Do not use this budget before the first success or
	// immediately after we issue a kiosk restart — Chromium's cold start
	// legitimately exceeds it on FF1 hardware, and conflating the two
	// produces restart loops on healthy devices.
	CHROMIUM_HANG_THRESHOLD = 20 * time.Second
	// CHROMIUM_STARTUP_GRACE is the cold-start budget: the longest we will
	// wait for Chromium to first expose /json/version before declaring it
	// stuck. Sized to cover feral-player.service (TimeoutStartSec=45s) +
	// chromium-kiosk.service RestartSec=5s + Chromium's own bring-up. Used
	// both at boot AND after every kiosk restart, since both situations
	// share the same "Chromium has not spoken to us yet" property.
	CHROMIUM_STARTUP_GRACE          = 90 * time.Second
	CHROMIUM_RESTART_HISTORY_SIZE   = 3 // Store the last 3 restarts
	CHROMIUM_MAX_RESTARTS_WINDOW    = 5 * time.Minute
	CHROMIUM_MAX_RESTARTS_THRESHOLD = 3 // 3 restarts within the window triggers reboot
)

// ChromiumMonitor monitors Chromium browser health via Chrome DevTools Protocol.
//
// Hang detection has two distinct modes that must not be conflated:
//
//   - Pre-connect (hasEverConnected == false): we are still inside the
//     startup-grace window because Chromium has either never spoken to us
//     (cold boot) or has not spoken since we asked systemd to restart it.
//     The only escalation path here is "still no response after
//     CHROMIUM_STARTUP_GRACE", not the steady-state hang threshold.
//
//   - Post-connect (hasEverConnected == true): we know Chromium was alive
//     because we received at least one 200 since the last reset.
//     CHROMIUM_HANG_THRESHOLD silence now means the renderer is genuinely
//     stuck and a kiosk restart is the right answer.
//
// restartChromium intentionally transitions back to the pre-connect mode so
// the next check cycle does not pile a fresh restart onto an in-progress one.
//
// Display gating overrides both modes: while no DRM connector reads
// "connected" (headless — including amdgpu's persistent "unknown" readings on
// empty connectors), Chromium legitimately does not run and every failed check
// is expected, so escalation is suppressed entirely (no restart, no
// reboot-budget accumulation). Detection FAILS OPEN only when no connector
// status is readable at all, and a reconnect re-anchors the pre-connect grace
// window.
type ChromiumMonitor struct {
	mu                 sync.Mutex
	cdpEndpoint        string
	client             *http.Client
	logger             *zap.Logger
	restartHistory     []time.Time
	hasEverConnected   bool
	monitorStart       time.Time
	lastSuccessfulResp time.Time
	commandHandler     *CommandHandler

	// drmSysfsRoot is the sysfs root consulted for display-attach state. It is a
	// field (not the constant directly) so tests can inject a fixture directory.
	drmSysfsRoot string
	// headless latches "we last observed no connected display". It is
	// how a reconnect is detected: on a headless device Chromium legitimately
	// does not run, so escalation is suppressed while headless, and the first
	// check after a display reappears re-anchors the startup-grace window so a
	// just-plugged monitor gets the full grace instead of an instant restart.
	headless bool
}

// NewChromiumMonitor creates a new Chromium monitor instance.
//
// The monitor starts in the pre-connect mode: hasEverConnected is false and
// monitorStart anchors the CHROMIUM_STARTUP_GRACE budget. The first 200 from
// /json/version flips us into post-connect mode; from that point on the
// shorter CHROMIUM_HANG_THRESHOLD applies. This separation is what prevents
// cold-boot devices and post-restart cycles from logging
// "Chromium browser hang detected" while Chromium is legitimately starting up.
func NewChromiumMonitor(cdpEndpoint string, logger *zap.Logger, commandHandler *CommandHandler) *ChromiumMonitor {
	return &ChromiumMonitor{
		cdpEndpoint: cdpEndpoint,
		client: &http.Client{
			Timeout: CHROMIUM_REQUEST_TIMEOUT,
		},
		logger:           logger,
		restartHistory:   make([]time.Time, 0, CHROMIUM_RESTART_HISTORY_SIZE),
		hasEverConnected: false,
		monitorStart:     time.Now(),
		commandHandler:   commandHandler,
		drmSysfsRoot:     defaultDRMSysfsRoot,
	}
}

// Start begins the CDP monitoring process
func (m *ChromiumMonitor) Start(ctx context.Context) {
	m.logger.Info("Chromium: Starting Chromium monitor",
		zap.String("endpoint", m.cdpEndpoint),
		zap.Duration("check_interval", CHROMIUM_CHECK_INTERVAL),
		zap.Duration("hang_threshold", CHROMIUM_HANG_THRESHOLD))

	ticker := time.NewTicker(CHROMIUM_CHECK_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			m.logger.Info("Chromium: Monitor shutting down")
			return
		case <-ticker.C:
			if err := m.check(ctx); err != nil {
				if errors.Is(err, errChromiumHeadless) {
					// Expected while no display is attached; the headless
					// transition itself is logged once in checkHangState.
					m.logger.Debug("Chromium: Health check failed while headless", zap.Error(err))
				} else {
					m.logger.Warn("Chromium: Health check failed", zap.Error(err))
				}
			}
		}
	}
}

func (m *ChromiumMonitor) Stop() {
	if m.client != nil {
		m.client.CloseIdleConnections()
	}
}

// check performs a single CDP health check
func (m *ChromiumMonitor) check(ctx context.Context) error {
	versionURL := fmt.Sprintf("%s/json/version", m.cdpEndpoint)

	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, CHROMIUM_REQUEST_TIMEOUT)
	defer cancel()

	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodGet, versionURL, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := m.client.Do(req)

	// Check for response and connection errors
	if err != nil {
		if m.checkHangState(ctx) {
			return fmt.Errorf("%w: chromium request failed: %w", errChromiumHeadless, err)
		}
		return fmt.Errorf("chromium request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		if m.checkHangState(ctx) {
			return fmt.Errorf("%w: chromium returned non-200 status: %d", errChromiumHeadless, resp.StatusCode)
		}
		return fmt.Errorf("chromium returned non-200 status: %d", resp.StatusCode)
	}

	// Read and discard response body to free up connections
	// Go uses connection pooling, this helps reuse the connection
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// First success since (re)start arms post-connect hang detection. Until
	// this flips, sustained failures must use the larger startup-grace budget
	// instead of the steady-state hang threshold.
	m.mu.Lock()
	m.lastSuccessfulResp = time.Now()
	m.hasEverConnected = true
	// A 200 proves a display is attached and Chromium is up. Clear the headless
	// latch so a later genuine hang is escalated normally rather than being
	// mistaken for a display reconnect and granted a fresh grace window.
	m.headless = false
	m.mu.Unlock()

	return nil
}

// checkHangState decides whether sustained failure to reach /json/version
// warrants escalating to a kiosk restart. It is called on every failed check,
// so the cost of false positives is high: any spurious restart will be
// repeated on the next 5-second tick.
//
// It reports whether the failure happened while headless (no connected
// display), so the caller can tag it as expected rather than warn-worthy.
//
// The decision splits on hasEverConnected. Pre-connect, we wait through
// CHROMIUM_STARTUP_GRACE; post-connect, the shorter CHROMIUM_HANG_THRESHOLD
// applies. Both branches additionally consult the chromium-kiosk.service
// activating state so we don't pile a fresh restart onto a restart that
// systemd or someone else (OTA, user) is already running.
func (m *ChromiumMonitor) checkHangState(ctx context.Context) (headless bool) {
	// Display gating comes first. On a headless device the kiosk deliberately
	// waits for a display before launching Chromium, so /json/version is
	// legitimately absent and is NOT a Chromium failure. While no connector
	// reads "connected" we skip the entire escalation path: no kiosk restart
	// and — critically — no restartHistory accumulation, so a device that sits
	// headless for hours cannot later trip the 3-restarts-in-5-minutes reboot
	// budget. The read is done outside m.mu because it touches the filesystem.
	displayConnected := isDisplayConnected(m.drmSysfsRoot)

	m.mu.Lock()
	if !displayConnected {
		// Latch headless so the first check after a reconnect re-anchors grace.
		// Log the transition once instead of warning every check interval.
		enteredHeadless := !m.headless
		m.headless = true
		m.mu.Unlock()
		if enteredHeadless {
			m.logger.Info("Chromium: No display connected; suppressing health-check escalation until a display reconnects")
		}
		return true
	}
	reconnected := m.headless
	if m.headless {
		// Display (re)appeared after a headless period. Escalation resumes, but
		// with a FRESH pre-connect grace window: a just-plugged monitor must get
		// the full CHROMIUM_STARTUP_GRACE for Chromium to cold-start, not an
		// instant restart driven by the stale monitorStart from before it was
		// unplugged.
		m.headless = false
		m.hasEverConnected = false
		m.monitorStart = time.Now()
		m.lastSuccessfulResp = time.Time{}
	}
	hasEverConnected := m.hasEverConnected
	timeSinceLast := time.Since(m.lastSuccessfulResp)
	timeSinceStart := time.Since(m.monitorStart)
	m.mu.Unlock()

	if reconnected {
		m.logger.Info("Chromium: Display reconnected; resuming health-check escalation with a fresh startup grace",
			zap.Duration("startup_grace", CHROMIUM_STARTUP_GRACE))
	}

	var (
		shouldRestart bool
		reason        string
	)
	switch {
	case !hasEverConnected:
		if timeSinceStart <= CHROMIUM_STARTUP_GRACE {
			// Cold boot or post-restart bring-up still in progress. Stay
			// quiet — the noisy "Chromium browser hang detected" line is
			// reserved for genuine post-connect renderer hangs.
			return
		}
		shouldRestart = true
		reason = "startup_grace_exceeded"
	case timeSinceLast > CHROMIUM_HANG_THRESHOLD:
		shouldRestart = true
		reason = "hang_threshold_exceeded"
	}

	if !shouldRestart {
		return
	}

	// systemctl call deliberately outside the monitor mutex. The call shells
	// out and can block tens of milliseconds; holding m.mu would block the
	// next check() unnecessarily. The state we read above is sufficient to
	// reach this point — re-acquiring is only needed for the restart write.
	if m.commandHandler != nil && m.commandHandler.isKioskActivating(ctx) {
		m.logger.Warn("Chromium: Restart trigger met but chromium-kiosk.service is activating; deferring",
			zap.String("reason", reason),
			zap.Duration("time_since_last_response", timeSinceLast),
			zap.Duration("time_since_monitor_start", timeSinceStart))
		return
	}

	if reason == "startup_grace_exceeded" {
		m.logger.Error("Chromium: Chromium failed to come up within startup grace",
			zap.Duration("budget", CHROMIUM_STARTUP_GRACE),
			zap.Duration("elapsed", timeSinceStart))
	} else {
		m.logger.Error("Chromium: Chromium browser hang detected",
			zap.Duration("time_since_last_response", timeSinceLast),
			zap.Duration("threshold", CHROMIUM_HANG_THRESHOLD))
	}

	m.mu.Lock()
	m.restartChromium(ctx)
	m.mu.Unlock()
	return false
}

// restartChromium issues a kiosk restart (or, if we've burned through the
// restart budget, a full system reboot) and then drops the monitor back into
// pre-connect mode.
//
// The pre-connect reset is load-bearing: Chromium will be unavailable for
// longer than CHROMIUM_HANG_THRESHOLD after a kiosk restart, and without this
// reset the next check 20 seconds later will see "no response" and issue
// another restart, exhausting the 3-restart budget in under a minute and
// rebooting healthy devices.
//
// Callers must hold m.mu.
func (m *ChromiumMonitor) restartChromium(ctx context.Context) {
	now := time.Now()
	m.restartHistory = append(m.restartHistory, now)

	// Keep only 3 recent restarts
	if len(m.restartHistory) > CHROMIUM_RESTART_HISTORY_SIZE {
		m.restartHistory = m.restartHistory[1:]
	}

	// Check if we need to trigger a reboot
	if m.shouldTriggerReboot() {
		m.logger.Error("Chromium: Too many chromium restarts in a short period, triggering system reboot")
		m.commandHandler.rebootSystem(ctx, CrashReasonChromiumCrash)
		return
	}

	// Execute the restart command
	m.logger.Warn("Chromium: Restarting chromium-kiosk.service")
	m.commandHandler.restartKiosk(ctx)

	// Drop back to pre-connect mode. The next check cycle will then use the
	// startup-grace budget (90s) rather than the steady-state hang threshold
	// (20s), so we don't fire a second restart while Chromium is still
	// coming up from the first one.
	m.hasEverConnected = false
	m.monitorStart = now
	m.lastSuccessfulResp = time.Time{}
}

// shouldTriggerReboot determines if we should trigger a system reboot
// based on the restart history
func (m *ChromiumMonitor) shouldTriggerReboot() bool {
	if len(m.restartHistory) < CHROMIUM_MAX_RESTARTS_THRESHOLD {
		return false
	}

	// If the oldest of the recent restarts is within the window, we need to reboot
	return time.Since(m.restartHistory[0]) <= CHROMIUM_MAX_RESTARTS_WINDOW
}
