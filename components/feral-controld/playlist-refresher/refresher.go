package refresher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	PLAYLIST_REFRESH_INTERVAL      = 5 * time.Minute
	PLAYER_STATUS_POLLING_INTERVAL = 5 * time.Second
)

// errCDPNotReady marks a refresh pass skipped because CDP is not connected. On a
// headless boot (no monitor) Chromium never starts, so CDP can stay absent for
// hours; that is an expected state, not a failure, and must not surface as
// Error-level log spam every retry interval.
var errCDPNotReady = errors.New("CDP not connected")

//go:generate mockgen -source=refresher.go -destination=../mocks/refresher.go -package=mocks -mock_names=Refresher=MockRefresher
type Refresher interface {
	Start()
	Stop()
}

type refresher struct {
	mu sync.RWMutex

	context      context.Context
	cdp          cdp.CDP
	statusPoller status.Poller
	dp1          dp1.DP1
	scheduler    playlistschedule.Scheduler

	clock  wrapper.Clock
	logger *zap.Logger

	done    chan struct{}
	started bool
}

func New(
	ctx context.Context,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	cdp cdp.CDP,
	scheduler playlistschedule.Scheduler,
	clock wrapper.Clock,
	logger *zap.Logger,
) Refresher {
	return &refresher{
		context:      ctx,
		cdp:          cdp,
		statusPoller: statusPoller,
		dp1:          dp1,
		scheduler:    scheduler,
		clock:        clock,
		logger:       logger,
		done:         make(chan struct{}),
	}
}

func (r *refresher) Start() {
	r.mu.Lock()

	if r.started {
		r.mu.Unlock()
		return
	}

	r.started = true
	r.done = make(chan struct{}) // Recreate the done channel for each start
	done := r.done
	r.mu.Unlock()

	go r.background(done)
}

func (r *refresher) background(done <-chan struct{}) {
	r.logger.Info("Refresher background goroutine started")
	runCtx, cancel := context.WithCancel(r.context)
	defer cancel()

	go func() {
		select {
		case <-done:
			cancel()
		case <-runCtx.Done():
		}
	}()

	// Process playing playlist until it succeeds
	for {
		if err := r.processPlayingPlaylist(); err != nil {
			r.logProcessFailure(err)
			if err := r.clock.SleepContext(runCtx, PLAYER_STATUS_POLLING_INTERVAL); err != nil {
				r.logger.Info("Refresher background goroutine stopped before initial success")
				return
			}
			continue
		}
		break
	}

	// Start ticker to refresh playlist
	ticker := r.clock.NewTicker(PLAYLIST_REFRESH_INTERVAL)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			if err := r.processPlayingPlaylist(); err != nil {
				r.logProcessFailure(err)
			}
		case <-done:
			ticker.Stop()
			r.logger.Info("Refresher background goroutine stopped due to done channel")
			return
		case <-runCtx.Done():
			ticker.Stop()
			r.logger.Info("Refresher background goroutine stopped due to context cancellation")
			return
		}
	}

}

func (r *refresher) Stop() {
	r.mu.Lock()
	if !r.started {
		r.mu.Unlock()
		return
	}

	r.started = false
	done := r.done
	r.mu.Unlock()

	select {
	case <-done:
		// Already closed
	default:
		close(done)
	}

	r.logger.Info("Refresher stopped")
}

// logProcessFailure logs one failed refresh pass. CDP absence stays at Debug:
// it is the normal headless/mid-reconnect state, and both retry loops would
// otherwise emit an Error every interval for hours on a monitor-less device.
func (r *refresher) logProcessFailure(err error) {
	if errors.Is(err, errCDPNotReady) || errors.Is(err, cdp.ErrCDPConnectionNotInitialized) {
		r.logger.Debug("Skipping playlist refresh: CDP not connected")
		return
	}
	r.logger.Error("Failed to process playing playlist", zap.Error(err))
}

// processPlayingPlaylist processes the playing playlist and sends it to CDP
func (r *refresher) processPlayingPlaylist() error {
	// FetchPlayerStatus and the final Send both need a live CDP connection; bail
	// out before them while it is absent so headless boots do not poll Chromium
	// that intentionally is not running. The connection can still drop between
	// this check and the sends, which is why logProcessFailure also matches
	// cdp.ErrCDPConnectionNotInitialized.
	if !r.cdp.Initialized() {
		return errCDPNotReady
	}

	var authorityToken uint64
	if r.scheduler != nil {
		authorityToken = r.scheduler.AuthorityToken()
	}

	// Get player status
	playerStatus, err := r.statusPoller.FetchPlayerStatus(r.context)
	if err != nil {
		return err
	}
	if playerStatus == nil {
		r.logger.Warn("Player status is nil")
		return nil
	}

	if playerStatus.Command != string(commands.CMD_DISPLAY_PLAYLIST) {
		r.logger.Debug("Player command is not display any playlist", zap.String("command", string(playerStatus.Command)))
		return nil
	}

	// Process playlist
	var playlist *dp1.Playlist
	switch {
	case playerStatus.PlaylistURL != nil:
		playlist, err = r.dp1.ProcessPlaylistURL(r.context, *playerStatus.PlaylistURL, false)
		if err != nil {
			return r.handleRefreshError(err, "playlist URL")
		}
	case playerStatus.Playlist != nil:
		if playerStatus.Playlist.HasDynamicContent() {
			playlist, err = r.dp1.ProcessDynamicPlaylist(r.context, *playerStatus.Playlist, false)
			if err != nil {
				return r.handleRefreshError(err, "dynamic playlist")
			}
		} else {
			if r.scheduler != nil && r.scheduler.RestoredPending() && playlistschedule.DisplayAtScheduleEnabled(playerStatus.Playlist) {
				// Static inline displayAt player status only contains the active
				// set, not the full playlist. For all-future schedules that active
				// set can be empty, so the displayPlaylist command itself is the
				// restart ownership signal when schedule.byDisplayAt is still set;
				// the persisted full playlist remains the only source that can arm
				// the first future boundary.
				r.scheduler.ResumePersisted(r.context)
				return nil
			}
			r.logger.Debug("Playlist has no dynamic queries, skipping")
			return nil
		}
	default:
		// A displayPlaylist status carrying neither a URL nor an inline playlist
		// is the player's fresh-boot/unconfigured state (nothing assigned yet),
		// not a failure. Returning an error here would pin the startup loop at
		// PLAYER_STATUS_POLLING_INTERVAL and emit an Error every pass for as
		// long as the device sits unconfigured — hours on a first boot with no
		// network. There is nothing to refresh; report success so the refresher
		// settles into its normal PLAYLIST_REFRESH_INTERVAL cadence.
		r.logger.Debug("Player has no playlist URL or playlist; nothing to refresh")
		return nil
	}

	hadDisplayAtCache := false
	hadRestoredPending := false
	var schedulerSnapshot playlistschedule.Snapshot
	schedulerMutated := false
	if r.scheduler != nil {
		hadDisplayAtCache = r.scheduler.HasCache()
		hadRestoredPending = r.scheduler.RestoredPending()
	}

	// Send playlist to CDP
	args := map[string]interface{}{
		"dp1_call": playlist,
		"refresh":  true,
	}
	command := commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: args,
	}

	sendErr := error(nil)
	send := func() {
		if r.scheduler != nil {
			if r.scheduler.AuthorityToken() != authorityToken {
				r.logger.Debug("Skipping obsolete playlist refresh after playlist authority changed")
				return
			}
			schedulerSnapshot = r.scheduler.Snapshot()
			playlist = r.scheduler.Prepare(playlist)
			schedulerMutated = true
			if r.scheduler.HasCache() && (!hadDisplayAtCache || hadRestoredPending) {
				// After a controld restart the memory cache may be empty, so
				// the refresher may be the first path to reconstruct scheduler
				// ownership from URL/dynamic/player status. Force-cast that first
				// scheduled reconstruction; a soft refresh can defer when the current
				// item disappeared from the new set.
				command.Arguments = map[string]interface{}{
					"intent": map[string]interface{}{
						"action": "now_display",
					},
					"dp1_call": playlist,
				}
			} else {
				command.Arguments["dp1_call"] = playlist
			}
		}
		result, err := r.sendCDPRequest(command)
		sendErr = err
		if schedulerMutated && (sendErr != nil || !playerResponseOK(result)) {
			r.scheduler.Restore(schedulerSnapshot)
		} else if schedulerMutated {
			r.scheduler.Commit()
		}
	}
	if r.scheduler != nil {
		r.scheduler.WithPlayerPush(send)
	} else {
		send()
	}
	return sendErr
}

func playerResponseOK(result interface{}) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	if msg, ok := m["message"].(map[string]interface{}); ok {
		okVal, ok := msg["ok"].(bool)
		return ok && okVal
	}
	okVal, ok := m["ok"].(bool)
	return ok && okVal
}

// handleRefreshError degrades to the displayAt cache only for transient fetch
// failures. Schema/parse/logic errors must surface so a bad feed cannot pin the
// device on a stale active set forever.
func (r *refresher) handleRefreshError(err error, kind string) error {
	if r.scheduler != nil && r.scheduler.HasCache() && !r.scheduler.RestoredPending() && isTransientPlaylistRefreshError(err) {
		r.logger.Warn("Playlist refresh failed transiently; recomputing from displayAt cache",
			zap.String("kind", kind),
			zap.Error(err))
		r.scheduler.ResumePersisted(r.context)
		return nil
	}
	return err
}

// isTransientPlaylistRefreshError reports transport / upstream-availability
// failures where retrying from the cached displayAt playlist is safe.
// Deterministic data/config errors (malformed JSON, invalid dynamicQuery,
// permanent DNS, non-timeout URL failures) return false.
func isTransientPlaylistRefreshError(err error) bool {
	if err == nil {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		// Prefer DNSError fields over deprecated net.Error.Temporary().
		return dnsErr.IsTemporary || dnsErr.IsTimeout
	}
	var netErr net.Error
	if errors.As(err, &netErr) {
		// Timeouts are the well-defined transient class; Temporary() is
		// deprecated (SA1019) and not reliable across net implementations.
		return netErr.Timeout()
	}
	var urlErr *url.Error
	if errors.As(err, &urlErr) {
		// url.Error wraps both transport blips and permanent config mistakes
		// (bad scheme, invalid URL). Only the inner error decides.
		return isTransientPlaylistRefreshError(urlErr.Err)
	}
	// dp1.fetchPlaylist wraps non-2xx as "fetch playlist failed: <Status>".
	// Treat 5xx / 429 as temporary upstream unavailability; 4xx stays hard-fail.
	msg := err.Error()
	if strings.Contains(msg, "fetch playlist failed: 5") ||
		strings.Contains(msg, "fetch playlist failed: 429") {
		return true
	}
	return false
}

// sendCDPRequest marshals payload and sends to CDP
func (r *refresher) sendCDPRequest(command commands.Command) (interface{}, error) {
	p, err := command.JSON()
	if err != nil {
		return nil, err
	}

	result, err := r.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(p)),
	})
	if err != nil {
		return nil, err
	}

	return result, nil
}
