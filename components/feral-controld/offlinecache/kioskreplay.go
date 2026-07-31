package offlinecache

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

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
// A SECOND such assumption now rides on this path: the flat-mode
// child-target auto-attach in kiosktargets.go (Target.setAutoAttach with
// waitForDebuggerOnStart + Runtime.runIfWaitingForDebugger sequencing)
// depends on how this specific Chromium build reports and pauses
// cross-origin iframe (OOPIF) targets. It is exercised in tests only
// against a scripted fake DevTools peer, never a real browser, so it too
// must be validated on real hardware before shipping. offlineCache.enabled
// gates the whole feature and is the rollback valve for both concerns.
//
//go:generate mockgen -source=kioskreplay.go -destination=../mocks/offlinecache_kioskreplay.go -package=mocks -mock_names=KioskReplay=MockOfflineCacheKioskReplay
type KioskReplay interface {
	// AttachOnReconnect dials a fresh event-driven CDP session to the
	// kiosk, re-registers Replayer's Fetch.requestPaused handler on the
	// top-level page target, and enables flat-mode auto-attach so the
	// page's cross-origin child iframe targets are intercepted too (see
	// kiosktargets.go). Call from cdp.CDP's onConnect hook.
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
	// PlaybackGeneration returns a counter that advances every time an
	// AUTHORITATIVE display path (a displayPlaylist command or a
	// playlist-refresher pass) commits a new replay scope via
	// MarkPlaybackChanged. It exists to close a TOCTOU that LockPlayback
	// alone cannot: the corrective resync
	// (commandrouter.resyncKioskReplayScopeToCurrentDisplay) must read the
	// kiosk's current playlist OUTSIDE the lock (that read is network-
	// bound and does not touch scope), then apply the derived scope INSIDE
	// the lock. Between those two steps a concurrent displayPlaylist can
	// switch the kiosk to a different playlist under the lock; the stale
	// resync would then install the OLD playlist's scope over the new one.
	// The resync samples this generation before its status read and
	// re-checks it after acquiring the lock — a change means a newer
	// authoritative display already set scope, so the resync skips rather
	// than clobbering it. The value is meaningful only for equality across
	// one resync's own before/after samples; its magnitude is opaque.
	//
	// Implemented lock-free (atomic) so it is safe to call both with and
	// without LockPlayback held (the resync does both), without
	// reentrancy.
	PlaybackGeneration() uint64
	// MarkPlaybackChanged advances PlaybackGeneration. Authoritative
	// display paths call it while holding LockPlayback, right after their
	// SyncPlaylist, to announce "the on-screen playlist's scope is now
	// mine." The corrective resync deliberately does NOT call it: it is a
	// restore-to-truth action, not a new authoritative change.
	MarkPlaybackChanged()
}

type kioskReplay struct {
	replayer   Replayer
	store      Store
	endpoint   string
	httpClient wrapper.HTTPClient
	dialer     wrapper.WebSocketDialer
	json       wrapper.JSON
	io         wrapper.IO
	clock      wrapper.Clock
	logger     *zap.Logger
	// redialMu guards lastRedial, the cooldown clock behind
	// redialIfDue. Separate from playbackMu: a re-dial decision has
	// nothing to do with playback serialization, and SyncPlaylist can
	// reach it while a caller already holds playbackMu.
	redialMu sync.Mutex
	// lastRedial is when the most recent replay-owned re-dial was
	// attempted (zero if never), spacing attempts by redialCooldown.
	lastRedial time.Time
	// playbackMu serializes scope-sync + kiosk-navigation sequences
	// across every caller — see LockPlayback's doc.
	playbackMu sync.Mutex
	// playbackGen backs PlaybackGeneration/MarkPlaybackChanged — see
	// PlaybackGeneration's doc. Atomic so the resync can read it both
	// outside and inside playbackMu without reentrancy.
	playbackGen atomic.Uint64
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
	clockWrapper wrapper.Clock,
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
		clock:      clockWrapper,
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
//
// After attaching the top-level target it enables flat-mode auto-attach
// (see enableChildTargetAutoAttach) so the kiosk page's cross-origin,
// out-of-process iframe targets get intercepted too. An auto-attach setup
// failure is returned but is non-fatal to top-level interception: the
// caller (main.go's onConnect hook) logs it and still runs the scope
// re-sync, so at worst only child-iframe replay is missing until the next
// reconnect.
func (k *kioskReplay) AttachOnReconnect(ctx context.Context) error {
	return k.dialAndAttach(ctx)
}

// dialAndAttach is AttachOnReconnect's body, shared with the re-dial
// SyncPlaylist performs when it finds the root retired — both need the
// identical dial + Attach + child-auto-attach sequence, and a recovery
// path that skipped the auto-attach step would silently lose iframe
// replay for as long as that session lived.
func (k *kioskReplay) dialAndAttach(ctx context.Context) error {
	session, err := DialPageSession(ctx, k.endpoint, k.httpClient, k.dialer, k.json, k.io, k.logger)
	if err != nil {
		return fmt.Errorf("offline cache: dial kiosk CDP session for replay: %w", err)
	}
	k.replayer.Attach("", session)
	if err := enableChildTargetAutoAttach(ctx, session, k.replayer, k.json, k.logger); err != nil {
		return fmt.Errorf("offline cache: enable kiosk child-target auto-attach: %w", err)
	}
	return nil
}

// redialCooldown is the minimum spacing between replay-owned re-dial
// attempts. SyncPlaylist runs on every displayPlaylist and every
// playlist-refresher pass, and a dial against a down kiosk costs a real
// (15s, see defaultDialTimeout) blocking round trip — without spacing,
// a kiosk that is simply gone would put that cost on the front of every
// display command. Short enough that recovery still lands within one
// refresher pass; long enough that a burst of commands cannot turn into
// a dial storm.
const redialCooldown = 30 * time.Second

// redialIfDue re-dials the replay session, at most once per
// redialCooldown. Returns false without attempting anything when the
// previous attempt was too recent, so the caller reports its original
// error rather than a misleading dial error.
func (k *kioskReplay) redialIfDue(ctx context.Context) (bool, error) {
	k.redialMu.Lock()
	now := k.clock.Now()
	if !k.lastRedial.IsZero() && now.Sub(k.lastRedial) < redialCooldown {
		k.redialMu.Unlock()
		return false, nil
	}
	k.lastRedial = now
	k.redialMu.Unlock()

	return true, k.dialAndAttach(ctx)
}

// SyncPlaylist scopes replay to whichever of itemIDs already have a
// capture on disk (ready or partial — LoadItem succeeding is enough; a
// partial capture is still worth serving what it has rather than nothing),
// and disables interception entirely if none do. Call after resolving any
// playlist for display and periodically while it keeps looping, since a
// background download can complete (or a cache can be cleared) while the
// same playlist is still on screen.
func (k *kioskReplay) SyncPlaylist(ctx context.Context, itemIDs []string) error {
	cachedIDs, mixed := k.scopeFor(itemIDs)

	err := k.applyScope(ctx, cachedIDs, mixed)
	if err == nil {
		return nil
	}

	// The replay session is a SEPARATE socket from the daemon's primary
	// CDP connection, with its own read pump and its own write
	// deadlines, so it can die on its own while the primary stays
	// perfectly healthy. main.go's onConnect hook (the only other caller
	// of AttachOnReconnect) fires on the PRIMARY connection's reconnect,
	// which in that case never happens — so without a re-dial here,
	// nothing would ever restore replay and every later sync would fail
	// exactly like this one, silently, until the kiosk restarted.
	//
	// RootAttached is what distinguishes the two failure shapes: the
	// scope call retires a session it could not reach (see the
	// replayer's retireIfDeadLocked), so a missing root means the socket
	// died, while a root still installed means the connection is fine
	// and the command itself was refused — re-dialing that would be
	// pointless churn.
	if k.replayer.RootAttached() {
		return err
	}

	attempted, dialErr := k.redialIfDue(ctx)
	if !attempted {
		// Cooled down from a recent attempt: report the real failure
		// rather than pretending a recovery was tried.
		return err
	}
	if dialErr != nil {
		return errors.Join(err, dialErr)
	}

	// Scope restoration is free here: cachedIDs was recomputed from
	// current store state at the top of this call, so re-applying it to
	// the fresh session installs exactly what should be in effect now —
	// no cached "what was enabled before" to go stale, matching the
	// reasoning in AttachOnReconnect's doc.
	k.logger.Info("offline cache: re-dialed kiosk replay session after transport failure, restoring scope",
		zap.Int("cached_items", len(cachedIDs)))
	return k.applyScope(ctx, cachedIDs, mixed)
}

// scopeFor resolves which of itemIDs are actually replayable right now,
// and whether the resulting scope is mixed. Pure (store reads only), so
// SyncPlaylist can compute it once and re-apply it after a re-dial
// without re-deriving it.
func (k *kioskReplay) scopeFor(itemIDs []string) (cachedIDs []string, mixed bool) {
	cachedIDs = make([]string, 0, len(itemIDs))
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
	// mixed is true whenever some (but not all) of the playlist's items
	// are cached: see Replayer.EnableForPlaylist's doc for why that
	// relaxes the miss policy to pass-through for this scope.
	return cachedIDs, len(cachedIDs) < total
}

func (k *kioskReplay) applyScope(ctx context.Context, cachedIDs []string, mixed bool) error {
	if len(cachedIDs) == 0 {
		return k.replayer.Disable(ctx)
	}
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

// PlaybackGeneration returns the current authoritative-display generation
// — see the interface doc for the resync TOCTOU it closes.
func (k *kioskReplay) PlaybackGeneration() uint64 { return k.playbackGen.Load() }

// MarkPlaybackChanged advances the generation. Callers hold LockPlayback
// (this is announced from within their sync+send critical section), so the
// atomic add is ordered before their UnlockPlayback and therefore visible
// to any resync that later acquires the lock — see the interface doc.
func (k *kioskReplay) MarkPlaybackChanged() { k.playbackGen.Add(1) }
