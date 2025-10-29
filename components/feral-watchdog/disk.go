package main

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	// Disk monitoring thresholds and constants
	DISK_WARNING_THRESHOLD  = 90.0             // 90% disk usage
	DISK_CRITICAL_THRESHOLD = 95.0             // 95% disk usage triggers reboot
	DISK_MONITOR_COOLDOWN   = 10 * time.Second // Wait 5s after cleanup

	// Archlinux specific paths
	PACMAN_CACHE_PATH = "/var/cache/pacman/pkg/"
	TEMP_FOLDER_PATH  = "/tmp/"
)

type DiskHandler struct {
	mu                  sync.Mutex
	logger              *zap.Logger
	commandHandler      *CommandHandler
	diskCleanupCooldown time.Time
	isCleaned           bool
}

func NewDiskHandler(logger *zap.Logger, commandHandler *CommandHandler) *DiskHandler {
	return &DiskHandler{
		logger:              logger,
		diskCleanupCooldown: time.Time{},
		isCleaned:           false,
		commandHandler:      commandHandler,
	}
}

func (c *DiskHandler) checkDiskUsage(ctx context.Context, metrics *SysMetrics) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Skip if we're in cooldown period after cleanup
	if !c.diskCleanupCooldown.IsZero() && time.Now().Before(c.diskCleanupCooldown) {
		return
	}

	c.diskCleanupCooldown = time.Time{}

	diskUsage, err := metrics.Disk.UsagePercent()
	if err != nil {
		c.logger.Error("DISK: Failed to get disk usage", zap.Error(err))
		return
	}

	// Check if disk usage exceeds warning threshold
	if diskUsage > DISK_CRITICAL_THRESHOLD {
		if c.isCleaned {
			c.logger.Error("DISK: Rebooting, usage remains critical after cleanup.", zap.Float64("usage_percent", diskUsage))
			c.commandHandler.rebootSystem(ctx, "disk_full")
		} else {
			c.logger.Warn("DISK: Critical usage high, cleaning disk", zap.Float64("usage_percent", diskUsage))
			c.cleanupDiskSpace(ctx, diskUsage)
		}

		return
	}

	if diskUsage > DISK_WARNING_THRESHOLD {
		c.logger.Warn("DISK: Usage high, cleaning disk", zap.Float64("usage_percent", diskUsage))
		c.cleanupDiskSpace(ctx, diskUsage)
		return
	}

	// DISK: usage is normal, reset cleaned flag
	c.isCleaned = false

}

func (c *DiskHandler) cleanupDiskSpace(ctx context.Context, diskUsage float64) {
	c.logger.Warn("DISK: usage high",
		zap.Float64("usage_percent", diskUsage),
		zap.Float64("threshold", DISK_WARNING_THRESHOLD))
	c.commandHandler.cleanupPacmanCache(ctx)
	c.isCleaned = true
	c.diskCleanupCooldown = time.Now().Add(DISK_MONITOR_COOLDOWN)
}
