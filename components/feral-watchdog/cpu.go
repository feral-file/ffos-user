package main

import (
	"context"
	"sync"
	"time"

	"github.com/Feral-File/ffos-user/components/feral-watchdog/packages/cdp"
	"go.uber.org/zap"
)

const (
	// CPU temperature monitoring thresholds and constants
	CPU_CRITICAL_TEMPERATURE       = 85.0             // 80°C critical temperature
	CPU_MONITOR_DURATION_THRESHOLD = 10 * time.Second // Check if temp is above threshold for 10 seconds
)

type CPUHandler struct {
	mu                  sync.Mutex
	logger              *zap.Logger
	cdpClient           *cdp.Client
	highTempMonitoring  bool
	highTempStartTime   time.Time
	criticalTemperature float64
}

func NewCPUHandler(logger *zap.Logger, cdpClient *cdp.Client) *CPUHandler {
	return &CPUHandler{
		logger:              logger,
		cdpClient:           cdpClient,
		highTempMonitoring:  false,
		highTempStartTime:   time.Time{},
		criticalTemperature: CPU_CRITICAL_TEMPERATURE,
	}
}

func (c *CPUHandler) checkCPUTemperature(ctx context.Context, currentTemp float64) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.logger.Debug("CPU: Checking CPU temperature", zap.Float64("current_temp", currentTemp))

	// If temperature is below threshold, reset monitoring if active
	if currentTemp < c.criticalTemperature {
		if c.highTempMonitoring {
			c.resetMonitoring()
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

	// Temperature has been critical for too long, send notification if not already sent
	c.logger.Error("CPU: Temperature exceeded critical threshold for too long, sending notification",
		zap.Float64("current_temp", currentTemp),
		zap.Duration("duration", durHigh))

	// Send critical temperature notification to website
	if c.cdpClient != nil {
		if err := c.cdpClient.SendCriticalCPUTemperatureNotification(ctx); err != nil {
			c.logger.Error("Failed to send critical CPU temperature notification to website",
				zap.Error(err))
		} else {
			c.logger.Info("CPU: Sent critical CPU temperature notification via CDP")
		}
	}

}

// Helper method to reset monitoring state
func (c *CPUHandler) resetMonitoring() {
	c.logger.Info("CPU: Resetting temperature monitoring")
	c.highTempMonitoring = false
	c.highTempStartTime = time.Time{}
}
