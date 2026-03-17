package oomrecovery

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

const (
	RetryInterval = 2 * time.Second
	MaxRetries    = 60
)

//go:generate mockgen -source=recoverer.go -destination=../mocks/oom_recoverer.go -package=mocks -mock_names=Recoverer=MockOOMRecoverer

// Recoverer checks for unhandled chromium OOM kill events and, if one is
// found, runs a self-contained background goroutine that polls until the
// webapp is responsive then sends displayDefaultPlaylist.
// Call Start once at boot; it returns immediately.
type Recoverer interface {
	Start(ctx context.Context)
}

type recoverer struct {
	statusPoller  status.Poller
	cmdHandler    commandrouter.Handler
	retryInterval time.Duration
	maxRetries    int
	logger        *zap.Logger
}

func New(
	statusPoller status.Poller,
	cmdHandler commandrouter.Handler,
	logger *zap.Logger,
) Recoverer {
	return NewWithOptions(statusPoller, cmdHandler, RetryInterval, MaxRetries, logger)
}

// NewWithOptions constructs a Recoverer with explicit retry parameters.
func NewWithOptions(
	statusPoller status.Poller,
	cmdHandler commandrouter.Handler,
	retryInterval time.Duration,
	maxRetries int,
	logger *zap.Logger,
) Recoverer {
	return &recoverer{
		statusPoller:  statusPoller,
		cmdHandler:    cmdHandler,
		retryInterval: retryInterval,
		maxRetries:    maxRetries,
		logger:        logger,
	}
}

func (r *recoverer) Start(ctx context.Context) {
	oomKillCount := readCountFile(constants.CHROMIUM_OOM_KILL_COUNT_FILE, r.logger)
	handledCount := readCountFile(constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE, r.logger)

	if oomKillCount <= handledCount {
		return
	}

	r.logger.Warn("Unhandled chromium OOM kill detected, starting recovery",
		zap.Int("chromium_oom_kill_count", oomKillCount),
		zap.Int("chromium_oom_kill_handled_count", handledCount))

	go r.run(ctx, oomKillCount)
}

func (r *recoverer) run(ctx context.Context, oomKillCount int) {
	r.statusPoller.SuppressPlayerNotifications(true)

	r.logger.Warn("OOM recovery started, player notifications suppressed",
		zap.Int("chromium_oom_kill_count", oomKillCount),
		zap.Duration("retry_interval", r.retryInterval),
		zap.Int("max_retries", r.maxRetries))

	// Attempt immediately before waiting on the ticker so we don't
	// waste a full interval when the player is already responsive.
	if r.tryRecover(ctx, oomKillCount, 0) {
		return
	}

	ticker := time.NewTicker(r.retryInterval)
	defer ticker.Stop()

	retries := 1
	for {
		select {
		case <-ctx.Done():
			r.logger.Warn("OOM recovery canceled by context",
				zap.Int("chromium_oom_kill_count", oomKillCount),
				zap.Int("retries", retries))
			r.finish(oomKillCount)
			return

		case <-ticker.C:
			if retries >= r.maxRetries {
				r.logger.Warn("OOM recovery: max retries reached, giving up",
					zap.Int("chromium_oom_kill_count", oomKillCount),
					zap.Int("retries", retries))
				r.finish(oomKillCount)
				return
			}

			if r.tryRecover(ctx, oomKillCount, retries) {
				return
			}
			retries++
		}
	}
}

// tryRecover makes a single recovery attempt: check player, send command.
// Returns true if recovery succeeded and the caller should exit the loop.
func (r *recoverer) tryRecover(ctx context.Context, oomKillCount int, retry int) bool {
	playerStatus, err := r.statusPoller.FetchPlayerStatus(ctx)
	if err != nil || playerStatus == nil {
		r.logger.Debug("OOM recovery: player not ready yet, will retry",
			zap.Int("retry", retry),
			zap.Error(err))
		return false
	}

	cmd := commands.Command{
		Type:      commands.CMD_DISPLAY_DEFAULT_PLAYLIST,
		Arguments: map[string]any{},
	}

	if _, err := r.cmdHandler.Process(ctx, cmd); err != nil {
		r.logger.Warn("OOM recovery: failed to send displayDefaultPlaylist, will retry",
			zap.Error(err),
			zap.Int("retry", retry))
		return false
	}

	r.logger.Info("OOM recovery: sent displayDefaultPlaylist successfully",
		zap.Int("chromium_oom_kill_count", oomKillCount),
		zap.Int("retries", retry))
	r.finish(oomKillCount)
	return true
}

func (r *recoverer) finish(oomKillCount int) {
	r.statusPoller.SuppressPlayerNotifications(false)

	writeCountFile(constants.CHROMIUM_OOM_KILL_HANDLED_COUNT_FILE, oomKillCount, r.logger)

	r.logger.Info("OOM recovery complete, player notifications resumed",
		zap.Int("chromium_oom_kill_count", oomKillCount))
}

func readCountFile(path string, logger *zap.Logger) int {
	data, err := os.ReadFile(path) // #nosec G304 -- path is from trusted constants only
	if err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("Failed to read counter file",
				zap.String("path", path),
				zap.Error(err))
		}
		return 0
	}

	count, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		logger.Warn("Failed to parse counter file",
			zap.String("path", path),
			zap.Error(err))
		return 0
	}

	return count
}

func writeCountFile(path string, count int, logger *zap.Logger) {
	if err := os.WriteFile(path, []byte(fmt.Sprintf("%d\n", count)), 0600); err != nil {
		logger.Error("Failed to write counter file",
			zap.String("path", path),
			zap.Int("count", count),
			zap.Error(err))
	}
}
