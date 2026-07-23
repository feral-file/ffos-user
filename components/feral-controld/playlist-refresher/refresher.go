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
	// Snapshot the freshly created channel under mu and hand it to
	// background as a parameter rather than letting background read
	// r.done directly: background's select loop re-evaluates its case
	// expressions on every iteration, so an unsynchronized field read
	// there could race against a LATER Start() call's write to r.done
	// once this goroutine's own done channel has since been closed by
	// Stop and a new Start/background pair is already running (the
	// exact shape of TestRefresher_ConcurrentStartStop, which caught
	// this race under -race: Start's write at this line vs Stop's read
	// of r.done below, both previously unsynchronized). Passing the
	// value as a parameter means this goroutine only ever observes the
	// one done channel that was live when it started, with no further
	// field access needed for its whole lifetime.
	done := r.done
	r.mu.Unlock()

	go r.background(done)
}

func (r *refresher) background(done chan struct{}) {
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
		case <-done:
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
	// Snapshot r.done under mu (symmetric with Start's write above) and
	// operate on the local copy from here on — reading the field again
	// after unlocking is exactly the race -race caught: a concurrent
	// Start() call can write a brand-new channel into r.done between
	// this unlock and an unsynchronized re-read below.
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
//
// err is a named return so the deferred revert below can inspect the
// pass's final outcome without a separate captured variable.
func (r *refresher) processPlayingPlaylist() (err error) {
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

	// result is set only once the CDP re-send below actually runs, so
	// the deferred revert (registered next) can tell "never attempted a
	// send" (result stays nil, e.g. the skipCDPResend early return) apart
	// from "sent but the player rejected it".
	var result interface{}
	// Revert replay's Fetch-interception scope to whatever the player
	// actually still displays if the CDP re-send below fails outright or
	// is rejected by the player (ok:false) — see
	// resyncKioskReplayScopeToCurrentDisplay's doc for why: the
	// SyncPlaylist call further down already committed the NEW
	// playlist's scope before the send ran, so a failure/rejection
	// there leaves scope pointed at a playlist the kiosk never actually
	// switched to while it is still genuinely showing the old one.
	//
	// Registered BEFORE the kioskReplay LockPlayback/defer UnlockPlayback
	// pair below so that, on return, Go's LIFO defer order runs THAT
	// unlock first and only then this revert — which needs to
	// re-acquire the same non-reentrant lock itself (see
	// KioskReplay.LockPlayback's doc on why nesting would deadlock).
	defer func() {
		if err != nil {
			r.resyncKioskReplayScopeToCurrentDisplay()
			return
		}
		if result != nil && !isPlayerResponseOk(result) {
			r.logger.Warn("offline cache: playlist refresh rejected by player, reverting replay scope",
				zap.Any("result", result))
			r.resyncKioskReplayScopeToCurrentDisplay()
		}
	}()

	// A displayPlaylist status carrying neither a URL nor an inline playlist
	// is the player's fresh-boot/unconfigured state (nothing assigned yet),
	// not a failure. Returning an error here would pin the startup loop at
	// PLAYER_STATUS_POLLING_INTERVAL and emit an Error every pass for as
	// long as the device sits unconfigured — hours on a first boot with no
	// network. There is nothing to refresh; report success so the refresher
	// settles into its normal PLAYLIST_REFRESH_INTERVAL cadence. This check
	// must stay in sync with resolveDisplayedPlaylist's own default case
	// (which returns an error instead, for resyncKioskReplayScopeToCurrentDisplay's
	// best-effort caller where an error is simply logged and swallowed).
	if playerStatus.PlaylistURL == nil && playerStatus.Playlist == nil {
		r.logger.Debug("Player has no playlist URL or playlist; nothing to refresh")
		return nil
	}

	playlist, err := r.resolveDisplayedPlaylist(playerStatus)
	if err != nil {
		return err
	}

	// skipCDPResend preserves the original "a static inline playlist never
	// changes, so do not re-send it to CDP every refresh pass" behavior —
	// mirrors resolveDisplayedPlaylist's own no-dynamic-content branch
	// (only reachable when PlaylistURL is unset, matching that switch's
	// case-priority order). It must NOT also skip the offline-cache
	// resync below: a background downloadPlaylistItem/downloadPlaylist
	// can finish, or a cache can be cleared, while this exact static
	// playlist keeps looping on screen, and the periodic refresher is
	// the only thing that would ever notice that for a playlist with
	// nothing dynamic to re-resolve.
	skipCDPResend := playerStatus.PlaylistURL == nil &&
		playerStatus.Playlist != nil && !playerStatus.Playlist.HasDynamicContent()
	if skipCDPResend {
		r.logger.Debug("Playlist has no dynamic queries, skipping CDP resend")
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

	result, err = r.sendCDPRequest(command)
	if err != nil {
		return err
	}

	return nil
}

// resolveDisplayedPlaylist resolves the playlist described by playerStatus
// (a URL to re-fetch live via DP-1, or an inline playlist to expand if it
// carries dynamic content) into a concrete *dp1.Playlist. Shared by
// processPlayingPlaylist's own resolve-then-resend pass and
// resyncKioskReplayScopeToCurrentDisplay's corrective resolve below, so
// both agree on exactly what "the playlist playerStatus describes" means
// for the same input shape.
//
// The PlaylistURL branch falls back to loadCachedPlaylistForURL on a live
// DP-1 failure — see that method's own doc for why: a device that is
// offline and already displaying a playlist via that same fallback must
// still be able to re-resolve "what is currently displayed" here, both for
// the main refresh pass and for a corrective resync.
//
// The default case's error is intentionally NOT the "fresh-boot/unconfigured
// player" signal processPlayingPlaylist's own pre-check treats as a
// successful no-op (see there): processPlayingPlaylist filters that state
// out before ever calling this, so reaching default here would indicate a
// genuinely unexpected playerStatus shape. resyncKioskReplayScopeToCurrentDisplay
// does not pre-filter and simply logs+swallows this error as best-effort.
func (r *refresher) resolveDisplayedPlaylist(playerStatus *status.PlayerStatus) (*dp1.Playlist, error) {
	switch {
	case playerStatus.PlaylistURL != nil:
		url := *playerStatus.PlaylistURL
		playlist, err := r.dp1.ProcessPlaylistURL(r.context, url, false)
		if err != nil {
			cached, cacheErr := r.loadCachedPlaylistForURL(url)
			if cacheErr != nil {
				return nil, err
			}
			return cached, nil
		}
		return playlist, nil
	case playerStatus.Playlist != nil:
		if !playerStatus.Playlist.HasDynamicContent() {
			return playerStatus.Playlist, nil
		}
		return r.dp1.ProcessDynamicPlaylist(r.context, *playerStatus.Playlist, false)
	default:
		return nil, errors.New("player status has no playlist URL or playlist")
	}
}

// resyncKioskReplayScopeToCurrentDisplay re-syncs replay's live Fetch-
// interception scope with whatever the player actually reports itself as
// currently displaying (via a fresh FetchPlayerStatus call, not any
// locally-held state) — the corrective counterpart to the authoritative
// sync processPlayingPlaylist's own defer above triggers this from.
// Mirrors commandrouter/offlinecache.go's own
// resyncKioskReplayScopeToCurrentDisplay, kept as its own independent
// instance rather than a shared cross-package helper: commandrouter and
// playlist-refresher intentionally do not depend on each other (see
// AGENTS.md's service-boundary guidance), so each package owns its own
// small copy of this FetchPlayerStatus->resolve->SyncPlaylist shape for
// its own input.
//
// Best-effort and fire-and-forget: a failure here must never turn an
// already-decided outcome (the refresh pass's own error/rejection) into
// something reported differently to the caller, and a failure here just
// leaves scope stale until the NEXT periodic pass — the same
// bounded-staleness trade-off commandrouter's twin already accepts.
func (r *refresher) resyncKioskReplayScopeToCurrentDisplay() {
	if r.kioskReplay == nil {
		return
	}
	// Sample the authoritative-display generation BEFORE reading the
	// kiosk's current playlist below: that read (FetchPlayerStatus +
	// resolveDisplayedPlaylist) is network-bound and runs OUTSIDE the
	// playback lock, so a concurrent displayPlaylist/refresher pass can
	// commit a newer scope while it is in flight. After acquiring the
	// lock we re-check this generation and bail if it advanced, so this
	// corrective resync can never install a stale playlist's scope over
	// a newer authoritative one — see KioskReplay.PlaybackGeneration's
	// doc.
	genBeforeResolve := r.kioskReplay.PlaybackGeneration()

	playerStatus, err := r.statusPoller.FetchPlayerStatus(r.context)
	if err != nil {
		r.logger.Warn("offline cache: failed to fetch player status to resync replay scope after refresh failure", zap.Error(err))
		return
	}
	if playerStatus == nil || playerStatus.Command != string(commands.CMD_DISPLAY_PLAYLIST) {
		return
	}

	playlist, err := r.resolveDisplayedPlaylist(playerStatus)
	if err != nil {
		r.logger.Warn("offline cache: failed to resolve currently displayed playlist to resync replay scope after refresh failure", zap.Error(err))
		return
	}
	if playlist == nil {
		return
	}

	itemIDs := make([]string, 0, len(playlist.Items))
	for _, item := range playlist.Items {
		itemIDs = append(itemIDs, item.ID)
	}
	// Serialize this scope resync against any concurrent displayPlaylist/
	// refresher sync+send (see KioskReplay.LockPlayback's doc). Acquired
	// only around the SyncPlaylist call, not the FetchPlayerStatus/
	// resolve above, which is network-bound and does not touch scope.
	// This never nests inside the failed pass's own send-path lock: that
	// lock is released by its own defer (registered after this
	// function's caller's defer, so it runs first — see
	// processPlayingPlaylist's doc) before this runs.
	r.kioskReplay.LockPlayback()
	defer r.kioskReplay.UnlockPlayback()
	// An authoritative displayPlaylist/refresher pass committed a new
	// scope between our pre-resolve sample and here: its scope is fresher
	// and matches what is actually on screen now, so applying our stale
	// snapshot would be the exact "install A's scope over B" regression
	// this guard exists to prevent (see KioskReplay.PlaybackGeneration).
	if r.kioskReplay.PlaybackGeneration() != genBeforeResolve {
		return
	}
	if syncErr := r.kioskReplay.SyncPlaylist(r.context, itemIDs); syncErr != nil {
		r.logger.Warn("offline cache: failed to sync kiosk replay scope after refresh failure", zap.Error(syncErr))
	}
}

// isPlayerResponseOk mirrors commandrouter's own isPlayerResponseOk (see
// resyncKioskReplayScopeToCurrentDisplay's doc for why this is an
// independent copy rather than a shared helper): ff-player's
// window.handleCDPRequest response is a {"message":{"ok":bool,...}}
// envelope, and anything that does not match that exact shape (a
// transport-level nil, a malformed reply, ...) is treated as NOT ok —
// this must fail closed toward "assume rejected, revert scope" rather
// than "assume accepted", since a false negative here only costs one
// redundant-but-harmless corrective resync, while a false positive would
// leave scope pointed at a playlist the kiosk never actually switched to.
func isPlayerResponseOk(result interface{}) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	msg, ok := m["message"].(map[string]interface{})
	if !ok {
		return false
	}
	okVal, _ := msg["ok"].(bool)
	return okVal
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
