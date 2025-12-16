package event

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/cdp"
	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	CPU_CRITICAL_TEMPERATURE       = 90.0             // 90°C critical temperature
	CPU_MONITOR_DURATION_THRESHOLD = 10 * time.Second // Check if temp is above threshold for 10 seconds
)

// CPUHandler handles CPU temperature events and do appropriate actions based on the temperature.
//
//go:generate mockgen -source=cpu.go -destination=../mocks/cpu.go -package=mocks -mock_names=CPUHandler=MockCPUHandler
type CPUHandler interface {
	// HandleTemperature handles the CPU temperature event
	HandleTemperature(ctx context.Context, temperature float64)
}

type cpuHandler struct {
	mu sync.Mutex

	// Dependencies
	logger *zap.Logger
	cdp    cdp.CDP
	clock  wrapper.Clock

	// State
	highTempMonitoring bool
	highTempStartTime  time.Time
}

func NewCPUHandler(logger *zap.Logger, cdpClient cdp.CDP, clock wrapper.Clock) CPUHandler {
	return &cpuHandler{
		logger:             logger,
		cdp:                cdpClient,
		clock:              clock,
		highTempMonitoring: false,
		highTempStartTime:  time.Time{},
	}
}

func (h *cpuHandler) HandleTemperature(ctx context.Context, temperature float64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.logger.Debug("CPU: Handling CPU temperature", zap.Float64("temperature", temperature))

	// If temperature is below threshold, reset monitoring if active
	if temperature < CPU_CRITICAL_TEMPERATURE {
		if h.highTempMonitoring {
			h.resetMonitoring()
		}
		return
	}

	// Temperature is above threshold, start monitoring if not already
	if !h.highTempMonitoring {
		h.logger.Warn("CPU: Temperature exceeds critical threshold, starting monitoring",
			zap.Float64("temperature", temperature),
			zap.Float64("threshold", CPU_CRITICAL_TEMPERATURE))
		h.highTempMonitoring = true
		h.highTempStartTime = h.clock.Now()
		return
	}

	// Check if temperature has been high for long enough
	duration := h.clock.Since(h.highTempStartTime)
	if duration < CPU_MONITOR_DURATION_THRESHOLD {
		h.logger.Warn("CPU: Temperature is still above threshold",
			zap.Float64("temperature", temperature))
		return
	}

	// Temperature has been critical for a while
	h.logger.Error("CPU: Temperature exceeded critical threshold for a while",
		zap.Float64("temperature", temperature),
		zap.Duration("duration", duration))

	// Show critical temperature page
	if h.cdp != nil {
		if err := h.cdp.ShowCriticalTemperature(ctx); err != nil {
			h.logger.Error("Failed to show critical temperature page via CDP",
				zap.Error(err))
		}
	}
}

// Helper method to reset monitoring state
func (h *cpuHandler) resetMonitoring() {
	h.logger.Info("CPU: Resetting temperature monitoring")
	h.highTempMonitoring = false
	h.highTempStartTime = time.Time{}
}
