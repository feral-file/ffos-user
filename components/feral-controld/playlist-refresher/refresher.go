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
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	PLAYLIST_REFRESH_INTERVAL      = 5 * time.Minute
	PLAYER_STATUS_POLLING_INTERVAL = 5 * time.Second
)

// startupForceCastEscalationThreshold bounds how many consecutive
// errPlayerRejectedRefresh failures the startup loop tolerates from the
// normal soft-refresh path before it escalates to a forced now_display cast.
// See the loop in background() for the scenario this guards against.
const startupForceCastEscalationThreshold = 3

// startupEscalation tracks the startup loop's consecutive-rejection streak
// and the forced-cast escalation it justifies. forceCast is a symmetric
// latch: it sets when the streak reaches the threshold and clears the
// instant the streak's justification goes away — a success, or a
// NON-rejection response breaking the streak (record is the single place
// both happen, so the two can never drift apart). Without the symmetric
// clear, an unrelated later error (e.g. errCDPNotReady while Chromium
// restarts) would reset the streak counter but leave forceCast latched, and
// the next attempt would force a visible now_display restart long after the
// rejection streak that justified it ended.
type startupEscalation struct {
	consecutiveRejects int
	forceCast          bool
}

// record updates escalation state after one processPlayingPlaylist attempt.
// err is that attempt's result (nil on success).
func (e *startupEscalation) record(err error) {
	switch {
	case err == nil:
		// Success: nothing left to escalate out of.
		e.consecutiveRejects = 0
		e.forceCast = false
	case errors.Is(err, errPlayerRejectedRefresh):
		e.consecutiveRejects++
		if e.consecutiveRejects >= startupForceCastEscalationThreshold {
			e.forceCast = true
		}
	default:
		// Any other response — including errCDPNotReady — is not a
		// rejection and breaks the streak. The escalation's sole
		// justification (a live rejection streak) is gone, so forceCast
		// clears with it: the next attempt gets the normal soft-refresh
		// path, and a fresh streak must re-earn the escalation from zero.
		e.consecutiveRejects = 0
		e.forceCast = false
	}
}

// errCDPNotReady marks a refresh pass skipped because CDP is not connected. On a
// headless boot (no monitor) Chromium never starts, so CDP can stay absent for
// hours; that is an expected state, not a failure, and must not surface as
// Error-level log spam every retry interval.
var errCDPNotReady = errors.New("CDP not connected")

// errPlayerRejectedRefresh marks a transport-successful CDP send that the
// player rejected (ok:false). Distinguished from other processPlayingPlaylist
// errors so the startup loop in background() can recognize a permanently
// rejected soft refresh (see there) instead of only counting failures
// generically.
var errPlayerRejectedRefresh = errors.New("player rejected playlist refresh")

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

	// Process playing playlist until it succeeds. A soft refresh (refresh:true)
	// can be a PERMANENT rejection rather than a transient one: the player
	// returns ok:false when it has no active playlist to refresh, and that
	// state persists across retries on its own. PrepareWithSource's own
	// force-cast escape (scheduler.HasCache() && (!hadDisplayAtCache ||
	// hadRestoredPending), see processPlayingPlaylist) does not fire here
	// because the scheduler cache is already warm — only the player's live
	// state is empty. Without escalation this loop would then retry the same
	// rejected soft refresh at PLAYER_STATUS_POLLING_INTERVAL forever. After
	// startupForceCastEscalationThreshold consecutive rejections, force a
	// now_display cast instead, which hydrates an empty player regardless of
	// prior cache state. Transport/fetch errors are a different error value
	// and do not count toward this escalation.
	esc := startupEscalation{}
	for {
		if err := r.processPlayingPlaylist(esc.forceCast); err != nil {
			r.logProcessFailure(err)
			esc.record(err)
			if err := r.clock.SleepContext(runCtx, PLAYER_STATUS_POLLING_INTERVAL); err != nil {
				r.logger.Info("Refresher background goroutine stopped before initial success")
				return
			}
			continue
		}
		esc.record(nil)
		break
	}

	// Start ticker to refresh playlist
	ticker := r.clock.NewTicker(PLAYLIST_REFRESH_INTERVAL)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C():
			if err := r.processPlayingPlaylist(false); err != nil {
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

// processPlayingPlaylist processes the playing playlist and sends it to CDP.
// forceCast, when true, sends a forced now_display cast unconditionally
// instead of the normal soft refresh — used by background()'s startup loop to
// escalate out of a permanently rejected soft refresh (see there).
func (r *refresher) processPlayingPlaylist(forceCast bool) error {
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

	var playlist *dp1.Playlist
	var schedulerSource playlistschedule.Source
	var err error
	if r.scheduler != nil {
		schedulerSource = r.scheduler.Source()
	}
	if schedulerSource.IsZero() {
		// No scheduler-owned source exists, so the player remains the source of
		// truth for normal URL/dynamic refreshes.
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

		switch {
		case playerStatus.PlaylistURL != nil:
			schedulerSource = playlistschedule.Source{PlaylistURL: *playerStatus.PlaylistURL}
		case playerStatus.Playlist != nil && playerStatus.Playlist.HasDynamicContent():
			schedulerSource = playlistschedule.Source{DynamicPlaylist: playerStatus.Playlist}
		case playerStatus.Playlist != nil:
			// Static inline player status only contains the filtered active set and
			// no refreshable source identity, so it cannot rebuild future items.
			r.logger.Debug("Playlist has no dynamic queries, skipping")
			return nil
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
	}

	var kind string
	switch {
	case schedulerSource.PlaylistURL != "":
		kind = "playlist URL"
		playlist, err = r.dp1.ProcessPlaylistURL(r.context, schedulerSource.PlaylistURL, false)
	case schedulerSource.DynamicPlaylist != nil:
		kind = "dynamic playlist"
		playlist, err = r.dp1.ProcessDynamicPlaylist(r.context, *schedulerSource.DynamicPlaylist, false)
	}
	if err != nil {
		return r.handleRefreshError(err, kind, schedulerSource)
	}

	var schedulerSnapshot playlistschedule.Snapshot
	schedulerMutated := false

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
		effectiveForceCast := forceCast
		if r.scheduler != nil {
			if r.scheduler.AuthorityToken() != authorityToken {
				r.logger.Debug("Skipping obsolete playlist refresh after playlist authority changed")
				return
			}
			// Sampled here, under the same push lock as the PrepareWithSource
			// call below, not earlier at the top of this function: an earlier,
			// unlocked read could be flipped by a concurrent cast/RecomputeNow in
			// the window before this closure runs, which would force-cast a soft
			// refresh (a visible artwork restart) off a stale "cache was cold"
			// reading even though the cache is actually already warm.
			hadDisplayAtCache := r.scheduler.HasCache()
			hadRestoredPending := r.scheduler.RestoredPending()
			schedulerSnapshot = r.scheduler.Snapshot()
			playlist = r.scheduler.PrepareWithSource(playlist, schedulerSource)
			schedulerMutated = true
			if playlist == nil {
				sendErr = errors.New("playlist has invalid displayAt")
				r.scheduler.Restore(schedulerSnapshot)
				return
			}
			if len(playlist.Items) == 0 {
				// Keep the future schedule armed, but do not send an empty list:
				// the player rejects it and cannot improve the current artwork.
				r.scheduler.Commit()
				return
			}
			effectiveForceCast = effectiveForceCast ||
				(r.scheduler.HasCache() && (!hadDisplayAtCache || hadRestoredPending))
		}
		if effectiveForceCast {
			// effectiveForceCast is true for either of two reasons. (1) After a
			// controld restart the memory cache may be empty, so the refresher may
			// be the first path to reconstruct scheduler ownership from
			// URL/dynamic/player status; force-cast that first scheduled
			// reconstruction, since a soft refresh can defer when the current item
			// disappeared from the new set. (2) The caller (background()'s startup
			// loop) is escalating out of a soft refresh the player has rejected
			// repeatedly, forceCast, independent of scheduler cache state.
			command.Arguments = map[string]interface{}{
				"intent": map[string]interface{}{
					"action": "now_display",
				},
				"dp1_call": playlist,
			}
			if schedulerSource.PlaylistURL != "" {
				command.Arguments["playlistUrl"] = schedulerSource.PlaylistURL
			}
		} else {
			command.Arguments["dp1_call"] = playlist
		}
		result, err := r.sendCDPRequest(command)
		sendErr = err
		// Transport success with ok:false (or a malformed body) must still
		// fail the refresh pass: otherwise the startup loop treats the reject
		// as initial success and drops from 5s retries to the 5m ticker while
		// the player never accepted the playlist.
		if sendErr == nil && !playerresponse.OK(result) {
			sendErr = errPlayerRejectedRefresh
		}
		if schedulerMutated && sendErr != nil {
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

// handleRefreshError degrades to the displayAt cache only for transient fetch
// failures (see isTransientPlaylistRefreshError): those recompute from the
// still-valid cached schedule and report success. Schema/parse/permanent
// errors are returned as-is instead of being swallowed here, so the caller
// logs them at Error level and a persistently bad feed stays visible in the
// journal — but this function never clears the scheduler's cached active set
// (s.full/s.source) either way, for either error class. A wall display
// deliberately keeps showing its last-known-good playlist through a broken
// feed rather than going blank; only Clear or a new Prepare/PrepareWithSource
// ever drops that cache.
func (r *refresher) handleRefreshError(err error, kind string, source playlistschedule.Source) error {
	if r.scheduler != nil &&
		r.scheduler.HasCache() &&
		!r.scheduler.RestoredPending() &&
		r.scheduler.SourceMatches(source) &&
		isTransientPlaylistRefreshError(err) {
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
