package event

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/command"
	"github.com/feral-file/ffos-user/components/feral-watchdog/vmagent"
	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	RAM_CRITICAL_THRESHOLD         = 95.0             // 95% memory usage
	RAM_MONITOR_DURATION_THRESHOLD = 15 * time.Second // Check if RAM is above threshold for 15s
	RAM_RESTART_KIOSK_COOL_DOWN    = 5 * time.Second  // Wait 5 seconds after kiosk restart
	RAM_REBOOT_DURATION_THRESHOLD  = 60 * time.Second // Wait 60 seconds after kiosk restart before rebooting
)

// MemoryHandler handles memory usage events and do appropriate actions based on the usage.
//
//go:generate mockgen -source=memory.go -destination=../mocks/memory.go -package=mocks -mock_names=MemoryHandler=MockMemoryHandler
type MemoryHandler interface {
	// HandleUsage handles the memory usage event
	HandleUsage(ctx context.Context, usagePercent float64)
}

type memoryHandler struct {
	mu sync.Mutex

	// Dependencies
	commandExec command.Executor
	clock       wrapper.Clock
	logger      *zap.Logger

	// State
	highMemoryMonitoring  bool
	highMemStartTime      time.Time
	memoryMonitorCoolDown time.Time
	lastKioskRestart      time.Time
}

func NewMemoryHandler(logger *zap.Logger, commandExec command.Executor, clock wrapper.Clock) MemoryHandler {
	return &memoryHandler{
		logger:      logger,
		commandExec: commandExec,
		clock:       clock,
	}
}

func (h *memoryHandler) HandleUsage(ctx context.Context, usagePercent float64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Skip if in cool down period
	if !h.memoryMonitorCoolDown.IsZero() && h.clock.Now().Before(h.memoryMonitorCoolDown) {
		return
	}

	h.memoryMonitorCoolDown = time.Time{}

	// If memory usage is below threshold, reset monitoring if active
	if usagePercent < RAM_CRITICAL_THRESHOLD {
		// Reset monitoring if active
		if h.highMemoryMonitoring {
			h.resetMonitoring()
		}

		// RAM: usage is below threshold, do nothing
		return
	}

	// Memory is above threshold, start monitoring if not already
	if !h.highMemoryMonitoring {
		h.logger.Warn("RAM: usage exceeds critical threshold, starting monitoring",
			zap.Float64("usage_percent", usagePercent),
			zap.Float64("threshold", RAM_CRITICAL_THRESHOLD))
		h.highMemoryMonitoring = true
		h.highMemStartTime = h.clock.Now()
		return
	}

	// Check if memory has been high for long enough
	duration := h.clock.Since(h.highMemStartTime)
	if duration < RAM_MONITOR_DURATION_THRESHOLD {
		h.logger.Warn("RAM: usage is still above threshold",
			zap.Float64("usage_percent", usagePercent))
		return
	}

	h.logger.Error("RAM: usage exceeded critical threshold for too long",
		zap.Float64("usage_percent", usagePercent),
		zap.Duration("duration", duration))

	if !h.lastKioskRestart.IsZero() && h.clock.Since(h.lastKioskRestart) < RAM_REBOOT_DURATION_THRESHOLD {
		h.logger.Error("RAM: Rebooting. Usage remains critical after kiosk restart.")
		if err := h.commandExec.RebootSystem(ctx, vmagent.CrashReasonRamCritical); err != nil {
			h.logger.Warn("RAM: Failed to reboot system", zap.Error(err))
		}
	} else {
		h.logger.Error("RAM: Restarting kiosk")
		if err := h.commandExec.RestartKiosk(ctx); err != nil {
			h.logger.Warn("RAM: Failed to restart kiosk", zap.Error(err))
		}

		now := h.clock.Now()
		h.lastKioskRestart = now
		h.memoryMonitorCoolDown = now.Add(RAM_RESTART_KIOSK_COOL_DOWN)
		h.resetMonitoring()
	}
}

// Helper method to reset monitoring state
func (h *memoryHandler) resetMonitoring() {
	h.logger.Debug("RAM: Resetting monitoring")
	h.highMemoryMonitoring = false
	h.highMemStartTime = time.Time{}
}
