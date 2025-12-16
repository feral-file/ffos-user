package monitor

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/command"
	"github.com/feral-file/ffos-user/components/feral-watchdog/vmagent"
	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	CHROMIUM_CHECK_INTERVAL         = 5 * time.Second
	CHROMIUM_REQUEST_TIMEOUT        = 3 * time.Second
	CHROMIUM_HANG_THRESHOLD         = 20 * time.Second
	CHROMIUM_RESTART_HISTORY_SIZE   = 3
	CHROMIUM_MAX_RESTARTS_WINDOW    = 5 * time.Minute
	CHROMIUM_MAX_RESTARTS_THRESHOLD = 3
)

// ChromiumMonitor monitors Chromium browser health via Chrome DevTools Protocol

//go:generate mockgen -source=chromium.go -destination=../mocks/chromium.go -package=mocks -mock_names=ChromiumMonitor=MockChromiumMonitor
type ChromiumMonitor interface {
	// Start begins the Chromium monitor process
	Start(ctx context.Context)

	// Stop stops the Chromium monitor process
	Stop()
}

type chromiumMonitor struct {
	mu sync.Mutex

	// Dependencies
	httpClient  wrapper.HTTPClient
	clock       wrapper.Clock
	io          wrapper.IO
	commandExec command.Executor
	logger      *zap.Logger

	// State
	cdpEndpoint            string
	started                bool
	doneChan               chan struct{}
	chromiumRestartHistory []time.Time
	lastSuccessfulResp     time.Time
}

// NewChromiumMonitor creates a new Chromium monitor instance
func NewChromiumMonitor(cdpEndpoint string, logger *zap.Logger, commandExec command.Executor, httpClient wrapper.HTTPClient, clock wrapper.Clock, io wrapper.IO) ChromiumMonitor {
	return &chromiumMonitor{
		httpClient:  httpClient,
		logger:      logger,
		commandExec: commandExec,
		clock:       clock,
		cdpEndpoint: cdpEndpoint,
		io:          io,
	}
}

// Start begins the CDP monitoring process
func (m *chromiumMonitor) Start(ctx context.Context) {
	m.logger.Info("Chromium: Starting Chromium monitor",
		zap.String("endpoint", m.cdpEndpoint),
		zap.Duration("check_interval", CHROMIUM_CHECK_INTERVAL),
		zap.Duration("hang_threshold", CHROMIUM_HANG_THRESHOLD))

	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		m.logger.Info("Chromium: Monitor already started")
		return
	}

	m.chromiumRestartHistory = make([]time.Time, 0, CHROMIUM_RESTART_HISTORY_SIZE)
	m.lastSuccessfulResp = time.Time{}
	m.doneChan = make(chan struct{})
	m.started = true
	m.mu.Unlock()

	go m.background(ctx)
}

func (m *chromiumMonitor) background(ctx context.Context) {
	m.logger.Info("Chromium: Monitor background goroutine started")

	ticker := m.clock.NewTicker(CHROMIUM_CHECK_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			ticker.Stop()
			m.logger.Info("Chromium: Monitor shutting down due to context cancellation")
			return
		case <-m.doneChan:
			ticker.Stop()
			m.logger.Info("Chromium: Monitor shutting down due to done channel")
			return
		case <-ticker.C:
			if err := m.check(ctx); err != nil {
				m.logger.Warn("Chromium: Health check failed", zap.Error(err))
			}
		}
	}
}

func (m *chromiumMonitor) Stop() {
	m.logger.Info("Chromium: Stopping Chromium monitor")

	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		m.logger.Info("Chromium: Monitor already stopped")
		return
	}

	m.started = false
	m.mu.Unlock()

	select {
	case <-m.doneChan:
		// Already closed
		return
	default:
		close(m.doneChan)
	}

	m.logger.Info("Chromium: Monitor stopped")
}

func (m *chromiumMonitor) check(ctx context.Context) error {
	url := fmt.Sprintf("%s/json/version", m.cdpEndpoint)

	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, CHROMIUM_REQUEST_TIMEOUT)
	defer cancel()

	resp, err := m.httpClient.GetWithContext(timeoutCtx, url)

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
	_, err = m.io.Copy(io.Discard, resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read response body: %w", err)
	}

	// Update last successful response time
	m.mu.Lock()
	m.lastSuccessfulResp = m.clock.Now()
	m.mu.Unlock()

	return nil
}

// checkHangState checks if Chromium is hung and needs to be restarted
func (m *chromiumMonitor) checkHangState(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Check if the time since the last successful response exceeds the hang threshold
	timeSinceLastResp := m.clock.Since(m.lastSuccessfulResp)
	if timeSinceLastResp > CHROMIUM_HANG_THRESHOLD {
		m.logger.Error("Chromium: Chromium browser hang detected",
			zap.Duration("time_since_last_response", timeSinceLastResp),
			zap.Duration("threshold", CHROMIUM_HANG_THRESHOLD))

		// Restart Chromium kiosk service
		m.restartChromium(ctx)
	}
}

// restartChromium restarts the Chromium kiosk service
func (m *chromiumMonitor) restartChromium(ctx context.Context) {
	// Add restart to history
	now := m.clock.Now()
	m.chromiumRestartHistory = append(m.chromiumRestartHistory, now)

	// Keep only 3 recent restarts
	if len(m.chromiumRestartHistory) > CHROMIUM_RESTART_HISTORY_SIZE {
		m.chromiumRestartHistory = m.chromiumRestartHistory[1:]
	}

	// Check if we need to trigger a reboot
	if m.shouldReboot() {
		m.logger.Error("Chromium: Too many chromium restarts in a short period, triggering system reboot")
		_ = m.commandExec.RebootSystem(ctx, vmagent.CrashReasonChromiumCrash)
		return
	}

	// Execute the restart command
	m.logger.Warn("Chromium: Restarting chromium-kiosk.service")
	_ = m.commandExec.RestartKiosk(ctx)

	// Reset the last successful response time to force a new successful check
	// before evaluating hang state again
	m.lastSuccessfulResp = m.clock.Now()
}

// shouldReboot determines if we should reboot system based on the chromium restart history
func (m *chromiumMonitor) shouldReboot() bool {
	if len(m.chromiumRestartHistory) < CHROMIUM_MAX_RESTARTS_THRESHOLD {
		return false
	}

	// If the oldest of the recent restarts is within the window, we need to reboot
	return m.clock.Since(m.chromiumRestartHistory[0]) <= CHROMIUM_MAX_RESTARTS_WINDOW
}
