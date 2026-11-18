package devicectl

import (
	"context"
	"time"

	"go.uber.org/zap"
)

// replayRestoreRefreshWindow bounds the whole ask below. Past it the
// player's own first retry (15s after its failed load) is about to run
// anyway, so a later refresh would only restart that load.
const replayRestoreRefreshWindow = 10 * time.Second

// RefreshArtworkAfterReplayRestore asks the player to load the current work
// again, once, right after kiosk replay restored its remembered scope on a
// no-network start (feral-file/feral-file#3635). The kiosk's first load of
// the work runs before the daemon can attach and arm replay, so it fails,
// and the player otherwise sits on a black screen until its own retry 15s
// later; by the time this runs the kept copy is being served.
//
// It is the same refreshArtwork evaluate boot recovery sends, and nothing
// more: no state machine, no navigate escalation. A refusal, an error or a
// player that never becomes ready is logged and left to the player's retry.
// It waits on the player (up to replayRestoreRefreshWindow), so call it off
// the CDP connect loop.
func RefreshArtworkAfterReplayRestore(ctx context.Context, exec Executor, logger *zap.Logger) {
	r, ok := exec.(interface{ refreshAfterReplayRestore(context.Context) })
	if !ok {
		logger.Warn("Executor does not support the refresh after a replay scope restore")
		return
	}
	r.refreshAfterReplayRestore(ctx)
}

func (e *executor) refreshAfterReplayRestore(ctx context.Context) {
	if e.cdp == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, replayRestoreRefreshWindow)
	defer cancel()
	if !e.awaitPlayerCommandHandlerReady(ctx) {
		e.logger.Info("Replay scope restored: player not ready for a refresh; leaving it to the player's retry")
		return
	}
	if _, err := e.evaluateRefreshArtwork(); err != nil {
		e.logger.Info("Replay scope restored: refresh not taken; leaving it to the player's retry", zap.Error(err))
		return
	}
	e.logger.Info("Replay scope restored: asked the player to load the work again")
}
