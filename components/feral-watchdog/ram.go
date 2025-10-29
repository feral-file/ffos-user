package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// Memory monitoring thresholds and constants
	RAM_CRITICAL_THRESHOLD         = 95.0             // 95% memory usage
	RAM_MONITOR_DURATION_THRESHOLD = 15 * time.Second // Check if RAM is above threshold for 15s
	RAM_RESTART_KIOSK_COOLDOWN     = 5 * time.Second  // Wait 5 seconds after kiosk restart
	RAM_REBOOT_DURATION_THRESHOLD  = 60 * time.Second
)

type MemoryHandler struct {
	mu                    sync.Mutex
	logger                *zap.Logger
	commandHandler        *CommandHandler
	highMemoryMonitoring  bool
	highMemStartTime      time.Time
	memoryMonitorCoolDown time.Time
	lastKioskRestart      time.Time
}

func NewMemoryHandler(logger *zap.Logger, commandHandler *CommandHandler) *MemoryHandler {
	return &MemoryHandler{
		logger:                logger,
		highMemoryMonitoring:  false,
		highMemStartTime:      time.Time{},
		memoryMonitorCoolDown: time.Time{},
		lastKioskRestart:      time.Time{},
		commandHandler:        commandHandler,
	}
}

func (c *MemoryHandler) checkMemoryUsage(ctx context.Context, metrics *SysMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Skip if in cooldown period
	if !c.memoryMonitorCoolDown.IsZero() && time.Now().Before(c.memoryMonitorCoolDown) {
		return
	}

	c.memoryMonitorCoolDown = time.Time{}

	// Calculate memory usage percentage
	memUsage, err := metrics.Memory.CapacityPercent()
	if err != nil {
		c.logger.Error("RAM: Failed to get memory usage", zap.Error(err))
		return
	}

	// If memory usage is below threshold, reset monitoring if active
	if memUsage < RAM_CRITICAL_THRESHOLD {
		// Reset monitoring if active
		if c.highMemoryMonitoring {
			c.resetMonitoring()
		}

		// RAM: usage is below threshold, do nothing
		return
	}

	// Memory is above threshold, start monitoring if not already
	if !c.highMemoryMonitoring {
		c.logger.Warn("RAM: usage exceeds critical threshold, starting monitoring",
			zap.Float64("usage_percent", memUsage),
			zap.Float64("threshold", RAM_CRITICAL_THRESHOLD))
		c.highMemoryMonitoring = true
		c.highMemStartTime = metrics.Timestamp
		return
	}

	// Check if memory has been high for long enough
	durHigh := time.Since(c.highMemStartTime)
	if durHigh < RAM_MONITOR_DURATION_THRESHOLD {
		c.logger.Warn("RAM: usage is still above threshold",
			zap.Float64("usage_percent", memUsage))
		return
	}

	c.logger.Error("RAM: usage exceeded critical threshold for too long",
		zap.Float64("usage_percent", memUsage),
		zap.Duration("duration", durHigh))

	if !c.lastKioskRestart.IsZero() && time.Since(c.lastKioskRestart) < RAM_REBOOT_DURATION_THRESHOLD {
		c.logger.Error("RAM: Rebooting. Usage remains critical after kiosk restart.")
		c.commandHandler.rebootSystem(ctx, CrashReasonRamCritical)
	} else {
		c.logger.Error("RAM: Restarting kiosk")
		c.commandHandler.restartKiosk(ctx)
		c.lastKioskRestart = time.Now()
		c.memoryMonitorCoolDown = time.Now().Add(RAM_RESTART_KIOSK_COOLDOWN)
		c.resetMonitoring()
	}
}

// Helper method to reset monitoring state
func (c *MemoryHandler) resetMonitoring() {
	c.logger.Debug("RAM: Resetting monitoring")
	c.highMemoryMonitoring = false
	c.highMemStartTime = time.Time{}
}
