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
	DISK_WARNING_THRESHOLD  = 95.0             // 95% disk usage
	DISK_CRITICAL_THRESHOLD = 99.0             // 99% disk usage triggers reboot
	DISK_MONITOR_COOL_DOWN  = 10 * time.Second // Wait 10s after cleanup

	PACMAN_CACHE_PATH = "/var/cache/pacman/pkg/"
	TEMP_FOLDER_PATH  = "/tmp/"
)

// DiskHandler handles disk usage events and do appropriate actions based on the usage.
//
//go:generate mockgen -source=disk.go -destination=../mocks/disk.go -package=mocks -mock_names=DiskHandler=MockDiskHandler
type DiskHandler interface {
	// HandleUsage handles the disk usage event
	HandleUsage(ctx context.Context, usagePercent float64)
}

type diskHandler struct {
	mu sync.Mutex

	// Dependencies
	logger      *zap.Logger
	commandExec command.Executor
	clock       wrapper.Clock

	// State
	diskCleanupCoolDown time.Time
	isCleanedUp         bool
}

func NewDiskHandler(logger *zap.Logger, commandExec command.Executor, clock wrapper.Clock) DiskHandler {
	return &diskHandler{
		logger:              logger,
		commandExec:         commandExec,
		clock:               clock,
		diskCleanupCoolDown: time.Time{},
		isCleanedUp:         false,
	}
}

func (h *diskHandler) HandleUsage(ctx context.Context, usagePercent float64) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// Skip if we're in cool down period after a cleanup
	if !h.diskCleanupCoolDown.IsZero() && h.clock.Now().Before(h.diskCleanupCoolDown) {
		return
	}

	h.diskCleanupCoolDown = time.Time{}

	// Check if disk usage exceeds warning threshold
	if usagePercent >= DISK_CRITICAL_THRESHOLD {
		if h.isCleanedUp {
			h.logger.Error("DISK: Rebooting, usage remains critical after cleanup.", zap.Float64("usage_percent", usagePercent))
			_ = h.commandExec.RebootSystem(ctx, vmagent.CrashReasonDiskFull)
		} else {
			h.logger.Warn("DISK: Critical usage high, cleaning disk", zap.Float64("usage_percent", usagePercent))
			h.cleanupDiskSpace(ctx)
		}

		return
	}

	if usagePercent >= DISK_WARNING_THRESHOLD {
		h.logger.Warn("DISK: Usage high, cleaning disk", zap.Float64("usage_percent", usagePercent))
		h.cleanupDiskSpace(ctx)
		return
	}

	// Usage is normal, reset cleaned up flag
	h.isCleanedUp = false
}

// cleanupDiskSpace cleans up the disk space
func (h *diskHandler) cleanupDiskSpace(ctx context.Context) {
	// Cleanup pacman cache
	_ = h.commandExec.CleanupPacmanCache(ctx)

	// TODO: clean up more disk space

	// Set cleaned up flag and cool down period
	h.isCleanedUp = true
	h.diskCleanupCoolDown = h.clock.Now().Add(DISK_MONITOR_COOL_DOWN)
}
