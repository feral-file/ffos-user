package offlinecache

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// KioskReplay is the seam commandrouter's displayPlaylist branch,
// playlist-refresher's periodic re-send, and main.go's CDP onConnect hook
// call into to keep Replayer's live interception scope in sync with the
// kiosk's actual CDP connection and the currently-displayed playlist.
// Defined as an interface (like Store/Capturer/Replayer above) purely so
// commandrouter and playlist-refresher can inject a gomock fake instead of
// depending on this package's real dial/store plumbing in their tests.
//
// IMPORTANT / UNVALIDATED ON DEVICE: AttachOnReconnect opens a second CDP
// connection to the same kiosk page target the daemon's existing
// synchronous cdp.CDP client already holds (see cdp/cdp.go). The plan's
// "Open risks" section flags multi-client CDP behavior on this Chromium
// build as unverified — if the device's Chromium does not handle two
// simultaneous DevTools clients on one target well, this must be disabled
// via config (offlineCache.enabled=false) until that is confirmed on
// real hardware.
//
//go:generate mockgen -source=kioskreplay.go -destination=../mocks/offlinecache_kioskreplay.go -package=mocks -mock_names=KioskReplay=MockOfflineCacheKioskReplay
type KioskReplay interface {
	// AttachOnReconnect dials a fresh event-driven CDP session to the
	// kiosk and re-registers Replayer's Fetch.requestPaused handler on
	// it. Call from cdp.CDP's onConnect hook.
	AttachOnReconnect(ctx context.Context) error
	// SyncPlaylist scopes replay to whichever of itemIDs are already
	// cached, disabling interception entirely if none are.
	SyncPlaylist(ctx context.Context, itemIDs []string) error
	// LockPlayback/UnlockPlayback serialize a full "sync replay scope
	// then navigate/refresh the kiosk" sequence against every other such
	// sequence. Replay scope and kiosk navigation are two separate
	// operations (SyncPlaylist here + the CDP display send in
	// commandrouter/playlist-refresher), and BOTH the commandrouter's
	// displayPlaylist path and playlist-refresher's periodic pass run
	// them independently, concurrently (the storm gate admits multiple
	// heavy displayPlaylist commands at once, and the refresher ticks on
	// its own goroutine). Without serialization the two halves can
	// interleave as Sync(A) -> Sync(B) -> send(A), leaving playlist A on
	// screen but Fetch interception scoped to B — under fail_closed, A's
	// own requests are then misclassified as misses and fail offline.
	//
	// Callers must hold this lock across BOTH their scope sync and the
	// corresponding navigation/refresh send (but NOT across slow DP-1
	// resolution, which does not touch scope — acquire it only just
	// before syncing). This is the single process-wide playback
	// coordinator: it lives here because KioskReplay is the one shared
	// dependency both the commandrouter handler and playlist-refresher
	// already inject, and neither depends on the other (see AGENTS.md's
	// service-boundary guidance). It is a plain non-reentrant mutex —
	// no caller acquires it twice on one goroutine (the displayPlaylist
	// failure/rejection resync runs from a deferred handler only AFTER
	// the send-path unlock, never nested inside it).
	LockPlayback()
	UnlockPlayback()
}

type kioskReplay struct {
	replayer   Replayer
	store      Store
	endpoint   string
	httpClient wrapper.HTTPClient
	dialer     wrapper.WebSocketDialer
	json       wrapper.JSON
	io         wrapper.IO
	logger     *zap.Logger
	// playbackMu serializes scope-sync + kiosk-navigation sequences
	// across every caller — see LockPlayback's doc.
	playbackMu sync.Mutex
}

// NewKioskReplay constructs a KioskReplay. endpoint is the kiosk
// Chromium's DevTools HTTP endpoint (e.g. "http://127.0.0.1:9222"),
// intentionally passed separately from cdp.CDP's own connection since
// this dials an independent CDPSession rather than reusing that client.
func NewKioskReplay(
	replayer Replayer,
	store Store,
	endpoint string,
	httpClient wrapper.HTTPClient,
	dialer wrapper.WebSocketDialer,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	logger *zap.Logger,
) KioskReplay {
	return &kioskReplay{
		replayer:   replayer,
		store:      store,
		endpoint:   endpoint,
		httpClient: httpClient,
		dialer:     dialer,
		json:       jsonWrapper,
		io:         ioWrapper,
		logger:     logger,
	}
}

// AttachOnReconnect dials a fresh event-driven CDP session to the kiosk
// and re-registers Replayer's Fetch.requestPaused handler on it. Intended
// to be called from cdp.CDP's onConnect hook: Fetch-domain enablement and
// any previously-enabled item scope do not survive a Chromium process
// restart (whether from a plain kiosk restart or OOM-recovery — both go
// through the same cdp.CDP reconnect loop), so both the handler
// registration and the scope must be redone after every reconnect. Scope
// itself is intentionally NOT re-applied here from stale state: whichever
// caller next invokes SyncPlaylist (the next displayPlaylist command, or
// playlist-refresher's next periodic pass, at most PLAYLIST_REFRESH_INTERVAL
// later) re-establishes it from current cache state, which is simpler and
// safer than caching "what was enabled before" across a page reload that
// may have changed what is actually on screen.
func (k *kioskReplay) AttachOnReconnect(ctx context.Context) error {
	session, err := DialPageSession(ctx, k.endpoint, k.httpClient, k.dialer, k.json, k.io, k.logger)
	if err != nil {
		return fmt.Errorf("offline cache: dial kiosk CDP session for replay: %w", err)
	}
	k.replayer.Attach(session)
	return nil
}

// SyncPlaylist scopes replay to whichever of itemIDs already have a
// capture on disk (ready or partial — LoadItem succeeding is enough; a
// partial capture is still worth serving what it has rather than nothing),
// and disables interception entirely if none do. Call after resolving any
// playlist for display and periodically while it keeps looping, since a
// background download can complete (or a cache can be cleared) while the
// same playlist is still on screen.
func (k *kioskReplay) SyncPlaylist(ctx context.Context, itemIDs []string) error {
	cachedIDs := make([]string, 0, len(itemIDs))
	total := 0
	for _, id := range itemIDs {
		if id == "" {
			continue
		}
		total++
		if _, err := k.store.LoadItem(id); err == nil {
			cachedIDs = append(cachedIDs, id)
			continue
		} else if !errors.Is(err, ErrItemNotFound) {
			k.logger.Warn("offline cache: failed to check cache state for playlist item, treating as uncached",
				zap.String("item_id", id), zap.Error(err))
		}
	}

	if len(cachedIDs) == 0 {
		return k.replayer.Disable(ctx)
	}
	// mixed is true whenever some (but not all) of the playlist's items
	// are cached: see Replayer.EnableForPlaylist's doc for why that
	// relaxes the miss policy to pass-through for this scope.
	mixed := len(cachedIDs) < total
	return k.replayer.EnableForPlaylist(ctx, cachedIDs, mixed)
}

// LockPlayback acquires the process-wide playback coordinator. See the
// KioskReplay interface doc for the interleaving hazard this prevents and
// the caller contract (hold across scope sync + navigation, not DP-1
// resolution).
func (k *kioskReplay) LockPlayback() { k.playbackMu.Lock() }

// UnlockPlayback releases the playback coordinator acquired by
// LockPlayback.
func (k *kioskReplay) UnlockPlayback() { k.playbackMu.Unlock() }
