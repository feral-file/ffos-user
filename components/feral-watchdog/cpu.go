package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// CPU temperature monitoring thresholds and constants
	CPU_CRITICAL_TEMPERATURE       = 93.0             // 93°C critical temperature
	CPU_MONITOR_DURATION_THRESHOLD = 10 * time.Second // Check if temp is above threshold for 10 seconds
)

type CPUHandler struct {
	mu                  sync.Mutex
	logger              *zap.Logger
	protect             func(context.Context, bool) error
	highTempMonitoring  bool
	thermalActive       bool
	highTempStartTime   time.Time
	criticalTemperature float64
}

func NewCPUHandler(logger *zap.Logger, protect func(context.Context, bool) error) *CPUHandler {
	return &CPUHandler{
		logger:              logger,
		protect:             protect,
		highTempMonitoring:  false,
		highTempStartTime:   time.Time{},
		criticalTemperature: CPU_CRITICAL_TEMPERATURE,
	}
}

func (c *CPUHandler) checkCPUTemperature(ctx context.Context, currentTemp float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Debug("CPU: Checking CPU temperature", zap.Float64("current_temp", currentTemp))

	// Five-degree hysteresis prevents a repeated teardown/reload at 93 C.
	if currentTemp < c.criticalTemperature-5 {
		c.thermalActive = false
		if c.highTempMonitoring {
			c.resetMonitoring()
		}
		if c.protect != nil {
			if err := c.protect(ctx, false); err != nil {
				c.logger.Warn("CPU: could not clear thermal hold", zap.Error(err))
			}
		}
		return
	}
	if currentTemp < c.criticalTemperature {
		if !c.thermalActive && c.highTempMonitoring {
			c.resetMonitoring()
		}
		if c.thermalActive && c.protect != nil {
			if err := c.protect(ctx, true); err != nil {
				c.logger.Warn("CPU: could not reaffirm thermal hold", zap.Error(err))
			}
		}
		return
	}

	// Temperature is above threshold, start monitoring if not already
	if !c.highTempMonitoring {
		c.logger.Warn("CPU: Temperature exceeds critical threshold, starting monitoring",
			zap.Float64("current_temp", currentTemp),
			zap.Float64("threshold", c.criticalTemperature))
		c.highTempMonitoring = true
		c.highTempStartTime = time.Now()
		return
	}

	// Check if temperature has been high for long enough
	durHigh := time.Since(c.highTempStartTime)
	if durHigh < CPU_MONITOR_DURATION_THRESHOLD {
		c.logger.Warn("CPU: Temperature is still above threshold",
			zap.Float64("current_temp", currentTemp))
		return
	}

	c.thermalActive = true
	// Reaffirm the level so a restarted player also enters the hold.
	c.logger.Error("CPU: Temperature exceeded critical threshold for too long, sending notification",
		zap.Float64("current_temp", currentTemp),
		zap.Duration("duration", durHigh))

	if c.protect != nil {
		if err := c.protect(ctx, true); err != nil {
			c.logger.Error("CPU: could not apply thermal hold", zap.Error(err))
		}
	}

}

// Helper method to reset monitoring state
func (c *CPUHandler) resetMonitoring() {
	c.logger.Info("CPU: Resetting temperature monitoring")
	c.highTempMonitoring = false
	c.highTempStartTime = time.Time{}
}
