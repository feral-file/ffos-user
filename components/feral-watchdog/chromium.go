package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// Chromium configuration
	CHROMIUM_CHECK_INTERVAL         = 5 * time.Second // Check CDP every 5 seconds
	CHROMIUM_REQUEST_TIMEOUT        = 3 * time.Second
	CHROMIUM_HANG_THRESHOLD         = 20 * time.Second
	CHROMIUM_RESTART_HISTORY_SIZE   = 3 // Store the last 3 restarts
	CHROMIUM_MAX_RESTARTS_WINDOW    = 5 * time.Minute
	CHROMIUM_MAX_RESTARTS_THRESHOLD = 3 // 3 restarts within the window triggers reboot
)

// ChromiumMonitor monitors Chromium browser health via Chrome DevTools Protocol
type ChromiumMonitor struct {
	mu                 sync.Mutex
	cdpEndpoint        string
	client             *http.Client
	logger             *zap.Logger
	restartHistory     []time.Time
	lastSuccessfulResp time.Time
	commandHandler     *CommandHandler
}

// NewChromiumMonitor creates a new Chromium monitor instance
func NewChromiumMonitor(cdpEndpoint string, logger *zap.Logger, commandHandler *CommandHandler) *ChromiumMonitor {
	return &ChromiumMonitor{
		cdpEndpoint: cdpEndpoint,
		client: &http.Client{
			Timeout: CHROMIUM_REQUEST_TIMEOUT,
		},
		logger:             logger,
		restartHistory:     make([]time.Time, 0, CHROMIUM_RESTART_HISTORY_SIZE),
		lastSuccessfulResp: time.Time{},
		commandHandler:     commandHandler,
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
				m.logger.Warn("Chromium: Health check failed", zap.Error(err))
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
		m.checkHangState(ctx)
		return fmt.Errorf("chromium request failed: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Check status code
	if resp.StatusCode != http.StatusOK {
		m.checkHangState(ctx)
		return fmt.Errorf("chromium returned non-200 status: %d", resp.StatusCode)
	}

	// Read and discard response body to free up connections
	// Go uses connection pooling, this helps reuse the connection
	_, err = io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Update last successful response time
	m.mu.Lock()
	m.lastSuccessfulResp = time.Now()
	m.mu.Unlock()

	return nil
}

// checkHangState checks if Chromium is hung and needs to be restarted
func (m *ChromiumMonitor) checkHangState(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if the time since the last successful response exceeds the hang threshold
	timeSinceLastResp := time.Since(m.lastSuccessfulResp)
	if timeSinceLastResp > CHROMIUM_HANG_THRESHOLD {
		m.logger.Error("Chromium: Chromium browser hang detected",
			zap.Duration("time_since_last_response", timeSinceLastResp),
			zap.Duration("threshold", CHROMIUM_HANG_THRESHOLD))

		// Restart Chromium kiosk service
		m.restartChromium(ctx)
	}
}

// restartChromium restarts the Chromium kiosk service
func (m *ChromiumMonitor) restartChromium(ctx context.Context) {
	// Add restart to history
	now := time.Now()
	m.restartHistory = append(m.restartHistory, now)

	// Keep only 3 recent restarts
	if len(m.restartHistory) > CHROMIUM_RESTART_HISTORY_SIZE {
		m.restartHistory = m.restartHistory[1:]
	}

	// Check if we need to trigger a reboot
	if m.shouldTriggerReboot() {
		m.logger.Error("Chromium: Too many chromium restarts in a short period, triggering system reboot")
		m.commandHandler.rebootSystem(ctx)
		return
	}

	// Execute the restart command
	m.logger.Warn("Chromium: Restarting chromium-kiosk.service")
	m.commandHandler.restartKiosk(ctx)

	// Reset the last successful response time to force a new successful check
	// before evaluating hang state again
	m.lastSuccessfulResp = time.Now()
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
