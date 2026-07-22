package refresher

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
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
	// ForceRefresh triggers an immediate refresh pass instead of waiting
	// for the next PLAYLIST_REFRESH_INTERVAL tick. Call after any event
	// that can invalidate offline-cache replay scope without this
	// background loop noticing right away — currently only a kiosk/CDP
	// reconnect (see cdp.CDP's onConnect hook in main.go):
	// Fetch-interception scope does not survive a Chromium restart
	// (plain restart or OOM-recovery), so a playlist that was already
	// scoped for offline replay before the restart would otherwise
	// silently fall back to live network for up to
	// PLAYLIST_REFRESH_INTERVAL. Safe to call even when a refresh is
	// already pending or the loop has not started yet (a no-op in the
	// latter case, mirroring Stop's nil-cancel guard).
	ForceRefresh()
}

type refresher struct {
	mu sync.RWMutex

	context      context.Context
	cdp          cdp.CDP
	statusPoller status.Poller
	dp1          dp1.DP1
	// kioskReplay may be nil (feature disabled / not yet wired via
	// config), mirroring the nil-guard pattern used throughout
	// commandrouter for optional offline-cache dependencies.
	kioskReplay offlinecache.KioskReplay
	// offlineCache backs the same cached-playlist-by-URL fallback
	// commandrouter's resolveDisplayedPlaylist uses (see loadCachedPlaylistForURL
	// below): also nil-able when offline caching is disabled/not wired.
	offlineCache offlinecache.Service
	json         wrapper.JSON

	clock  wrapper.Clock
	logger *zap.Logger

	done    chan struct{}
	started bool
	// refreshChan is a 1-buffered, non-blocking signal (mirrors
	// status.Poller.ForceRefresh's channel pattern) that background's
	// select loop drains to run an extra processPlayingPlaylist pass
	// immediately rather than waiting for the next ticker tick. Created
	// once in New and never recreated across Start/Stop cycles (unlike
	// done), since a stale buffered signal surviving a restart is
	// harmless: it just costs one redundant extra pass right after the
	// next Start.
	refreshChan chan struct{}
}

func New(
	ctx context.Context,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	cdp cdp.CDP,
	kioskReplay offlinecache.KioskReplay,
	offlineCache offlinecache.Service,
	jsonWrapper wrapper.JSON,
	clock wrapper.Clock,
	logger *zap.Logger,
) Refresher {
	return &refresher{
		context:      ctx,
		cdp:          cdp,
		statusPoller: statusPoller,
		dp1:          dp1,
		kioskReplay:  kioskReplay,
		offlineCache: offlineCache,
		json:         jsonWrapper,
		clock:        clock,
		logger:       logger,
		done:         make(chan struct{}),
		refreshChan:  make(chan struct{}, 1),
	}
}

// ForceRefresh triggers an immediate refresh pass. See the Refresher
// interface doc for why this exists.
func (r *refresher) ForceRefresh() {
	select {
	case r.refreshChan <- struct{}{}:
	default:
		// A refresh is already pending; it will observe whatever is
		// currently displayed by the time it runs, so a second signal
		// here would be redundant.
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
	r.mu.Unlock()

	go r.background()
}

func (r *refresher) background() {
	r.logger.Info("Refresher background goroutine started")

	// Process playing playlist until it succeeds
	for {
		if err := r.processPlayingPlaylist(); err != nil {
			r.logProcessFailure(err)
			r.clock.Sleep(PLAYER_STATUS_POLLING_INTERVAL)
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
		case <-r.refreshChan:
			if err := r.processPlayingPlaylist(); err != nil {
				r.logProcessFailure(err)
			}
		case <-r.done:
			ticker.Stop()
			r.logger.Info("Refresher background goroutine stopped due to done channel")
			return
		case <-r.context.Done():
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
	r.mu.Unlock()

	select {
	case <-r.done:
		// Already closed
	default:
		close(r.done)
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
	// skipCDPResend preserves the original "a static inline playlist never
	// changes, so do not re-send it to CDP every refresh pass" behavior.
	// It must NOT also skip the offline-cache resync below: a background
	// downloadPlaylistItem/downloadPlaylist can finish, or a cache can be
	// cleared, while this exact static playlist keeps looping on screen,
	// and the periodic refresher is the only thing that would ever notice
	// that for a playlist with nothing dynamic to re-resolve.
	skipCDPResend := false
	switch {
	case playerStatus.PlaylistURL != nil:
		url := *playerStatus.PlaylistURL
		playlist, err = r.dp1.ProcessPlaylistURL(r.context, url, false)
		if err != nil {
			// Fall back to a previously downloaded copy of this exact
			// URL before giving up — mirrors commandrouter's
			// resolveDisplayedPlaylist (this
			// refresher path lacked the same fallback the
			// commandrouter/clear-resync path already has). Without
			// this, a device that goes offline while displaying a
			// playlist it already downloaded would return early here
			// on every periodic pass AND on the reconnect-triggered
			// ForceRefresh (main.go's onConnect hook), permanently
			// skipping the kioskReplay.SyncPlaylist call below and
			// leaving Fetch interception disabled after any Chromium
			// restart that happens while offline.
			cached, cacheErr := r.loadCachedPlaylistForURL(url)
			if cacheErr != nil {
				return err
			}
			playlist = cached
		}
	case playerStatus.Playlist != nil:
		if !playerStatus.Playlist.HasDynamicContent() {
			r.logger.Debug("Playlist has no dynamic queries, skipping CDP resend")
			playlist = playerStatus.Playlist
			skipCDPResend = true
			break
		}

		playlist, err = r.dp1.ProcessDynamicPlaylist(r.context, *playerStatus.Playlist, false)
		if err != nil {
			return err
		}
	default:
		return errors.New("player status has no playlist URL or playlist")
	}

	// Re-sync offline-cache replay scope before every re-send: this is
	// the periodic path (see plan "display-integration") that keeps
	// interception coherent while a playlist keeps looping — a
	// background download can finish, or a cache can be cleared, between
	// refresh passes. Best-effort: never let a sync failure block the
	// actual refresh, since offline replay is a strict enhancement over
	// the live path this loop exists to maintain.
	//
	// The playback coordinator is held across BOTH this scope sync and
	// the CDP re-send below (or the skipCDPResend early return), so this
	// refresher pass cannot interleave its sync+send with a concurrent
	// displayPlaylist command's own sync+send and leave scope pointing
	// at a different playlist than the one actually on screen (see
	// KioskReplay.LockPlayback's doc). Acquired only now, after the
	// network-bound DP-1 resolution above, which does not touch scope.
	if r.kioskReplay != nil {
		r.kioskReplay.LockPlayback()
		defer r.kioskReplay.UnlockPlayback()
		itemIDs := make([]string, 0, len(playlist.Items))
		for _, item := range playlist.Items {
			itemIDs = append(itemIDs, item.ID)
		}
		if syncErr := r.kioskReplay.SyncPlaylist(r.context, itemIDs); syncErr != nil {
			r.logger.Warn("offline cache: failed to sync kiosk replay scope during refresh", zap.Error(syncErr))
		}
		// Announce this authoritative scope change (under the lock) so a
		// concurrent corrective resync defers to it rather than clobbering
		// it with a stale playlist's scope — see
		// KioskReplay.PlaybackGeneration's doc.
		r.kioskReplay.MarkPlaybackChanged()
	}

	if skipCDPResend {
		return nil
	}

	// Send playlist to CDP
	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlist,
			"refresh":  true,
		},
	}

	if _, err := r.sendCDPRequest(command); err != nil {
		return err
	}

	return nil
}

// loadCachedPlaylistForURL mirrors commandrouter's handler.loadCachedPlaylistForURL
// (see its doc): the raw body Service.CachedPlaylistForURL returns was already
// fully resolved and signature-verified once, back when downloadPlaylist
// originally saved it, so unmarshaling it here needs no further DP-1
// processing. Returns an error whenever there is nothing to fall back to
// (offline caching disabled, url was never downloaded, or the downloaded
// copy has since been cleared), so the caller can report the original
// live resolution error instead.
func (r *refresher) loadCachedPlaylistForURL(url string) (*dp1.Playlist, error) {
	if r.offlineCache == nil {
		return nil, fmt.Errorf("offline cache: disabled, no cached fallback for %s", url)
	}
	raw, err := r.offlineCache.CachedPlaylistForURL(url)
	if err != nil {
		return nil, fmt.Errorf("offline cache: no cached playlist for %s: %w", url, err)
	}
	var playlist *dp1.Playlist
	if err := r.json.Unmarshal(raw, &playlist); err != nil {
		return nil, fmt.Errorf("offline cache: parse cached playlist for %s: %w", url, err)
	}
	return playlist, nil
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
