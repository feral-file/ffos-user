package refresher

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/display-protocol/dp1-go/displayat"
	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
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
	offlineCache  offlinecache.Service
	json          wrapper.JSON
	scheduler     playlistschedule.Scheduler
	contentPolicy *contentpolicy.Store

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

	// activeVerdict mirrors commandrouter's (SetSignatureVerification): the
	// refresher is the other producer of displayPlaylist pushes, so it
	// re-publishes the verdict of what it re-pushed. The verdict itself is
	// attached by dp1 at fetch time; the refresher computes none (its
	// cached-copy fallback carries no verdict — see
	// loadCachedPlaylistForURL). Set once before Start; read on the
	// background goroutine only.
	activeVerdict *sigverify.Active
	// sessionGeneration, when set (SetSessionGeneration), is the
	// playersession generation getter. A force cast's verdict is published
	// only if the generation is unchanged across the send: a bump in between
	// means the accepted reply came from a page that is gone (the bump hook
	// already cleared the slot), and the new generation's reconciliation
	// establishes a fresh verdict. Mirrors commandrouter.sendCDPRequest.
	sessionGeneration func() uint64

	// mediaHealthHints, when set (SetMediaHealthHints), supplies the
	// per-item hint argument for every send.
	mediaHealthHints func(items []dp1playlist.PlaylistItem) interface{}

	// seriesPlaybackSource, when set (SetSeriesPlaybackSource), is
	// commandrouter's own device-level series-playback getter
	// (commandrouter.SeriesPlaybackSource) — the same store main.go wires
	// into status.Poller, so this package's resync and commandrouter's own
	// cast paths read the identical override and can never disagree about
	// which mode a series narrows under (feral-file/ffos-user#453). nil
	// means "follow the document", mirroring currentSeriesPlaybackMode's
	// own nil-safety when no store was wired (a build before this seam, or
	// a test).
	seriesPlaybackSource func() *dp1.SeriesPlaybackMode

	// randomSeriesMu guards randomSeriesPicks — see narrowStable's doc. Both
	// fields are touched only from this package's own serialized background
	// goroutine (background()'s select loop runs processPlayingPlaylist one
	// pass at a time) today, but a dedicated lock keeps that invariant from
	// being load-bearing if a caller outside that loop is ever added
	// (feralfile-bot F3, 2026-11-12).
	randomSeriesMu    sync.Mutex
	randomSeriesPicks map[dp1.SeriesGroupKey]randomSeriesPick
}

// randomSeriesPick is one series group's last SeriesPlaybackRandom
// selection, remembered so narrowStable does not re-roll it on every
// periodic resync — see that method's doc. Only the Source is kept, never
// the full item: narrowStable always re-looks-up the matching edition from
// the CURRENT tick's group.Editions before returning it, so a field other
// than Source (duration, license, contentRating, …) can never go stale
// behind this memory — storing the struct itself was tried first and
// replayed exactly that stale copy indefinitely (run-reviewer, round 3 on
// this fix, 2026-11-12).
//
// artworkModel is the group's ArtworkModel at pick time. dp1.NarrowEditions
// only ever returns a distinct edition when
// artworkModel.HasDistinctEditions() — a single/multi series always
// collapses to the representative regardless of mode, and narrowStable's own
// top-level gate now keeps such a group out of randomSeriesPicks entirely
// (feralfile-bot F5, 2026-11-12) — and ArtworkModel is read fresh from the
// catalog's answer on every tick's ResolveSeriesCastPlan call, so it is NOT
// invariant across ticks for the same SeriesGroupKey. A stored pick whose
// artworkModel no longer matches the current tick's must still be
// invalidated exactly like a mode change: the top-level gate only catches a
// reclassification INTO single/multi (HasDistinctEditions() false); a
// reclassification between two models that both report true (e.g.
// multi_unique and an unrecognized/empty value) only this equality check
// catches (run-reviewer, round 4 on this fix, 2026-11-12).
type randomSeriesPick struct {
	mode         dp1.SeriesPlaybackMode
	artworkModel dp1.SeriesArtworkModel
	source       string
}

// SetSeriesPlaybackSource injects the device-level series-playback getter
// onto r, if r is the concrete refresher built by New (mirroring
// SetSessionGeneration's contract). Must be called before Start.
func SetSeriesPlaybackSource(r Refresher, fn func() *dp1.SeriesPlaybackMode, logger *zap.Logger) {
	setter, ok := r.(interface {
		setSeriesPlaybackSource(func() *dp1.SeriesPlaybackMode)
	})
	if !ok {
		logger.Warn("Playlist refresher does not support series playback wiring")
		return
	}
	setter.setSeriesPlaybackSource(fn)
}

func (r *refresher) setSeriesPlaybackSource(fn func() *dp1.SeriesPlaybackMode) {
	r.seriesPlaybackSource = fn
}

// currentSeriesPlaybackMode reads the device-level override, mirroring
// commandrouter's own method of the same name: nil means "follow the
// document", either because no override is set or because no source was
// wired (a build before this seam, or a test).
func (r *refresher) currentSeriesPlaybackMode() *dp1.SeriesPlaybackMode {
	if r.seriesPlaybackSource == nil {
		return nil
	}
	return r.seriesPlaybackSource()
}

// SetSessionGeneration injects the generation getter onto r, if r is the
// concrete refresher built by New (mirroring SetSignatureVerification's
// contract). Must be called before Start; nil leaves publication unfenced.
func SetSessionGeneration(r Refresher, fn func() uint64, logger *zap.Logger) {
	setter, ok := r.(interface{ setSessionGeneration(func() uint64) })
	if !ok {
		logger.Warn("Playlist refresher does not support session generation wiring")
		return
	}
	setter.setSessionGeneration(fn)
}

func (r *refresher) setSessionGeneration(fn func() uint64) {
	r.sessionGeneration = fn
}

func (r *refresher) currentGeneration() uint64 {
	if r.sessionGeneration == nil {
		return 0
	}
	return r.sessionGeneration()
}

// SetSignatureVerification wires the shared active-verdict slot onto r, if r
// is the concrete refresher built by New (a foreign implementation logs and
// is left alone, mirroring commandrouter.SetSignatureVerification). Must be
// called before Start.
func SetSignatureVerification(r Refresher, active *sigverify.Active, logger *zap.Logger) {
	setter, ok := r.(interface{ setSignatureVerification(*sigverify.Active) })
	if !ok {
		logger.Warn("Playlist refresher does not support signature verification wiring")
		return
	}
	setter.setSignatureVerification(active)
}

func (r *refresher) setSignatureVerification(active *sigverify.Active) {
	r.activeVerdict = active
}

// SetContentPolicy shares the command router's authoritative store/ordering
// lock with refresh so a policy update cannot race an old-policy projection.
func SetContentPolicy(r Refresher, policy *contentpolicy.Store) {
	if target, ok := r.(*refresher); ok {
		target.contentPolicy = policy
	}
}

// SetMediaHealthHints installs the media-health hint source
// (feral-file/ffos-user#392) onto r, if r is the concrete refresher built by
// New. Every displayPlaylist the refresher sends — soft refresh and force
// cast alike — carries the `mediaHealth` argument the source returns for the
// items being pushed, or none. Must be called before Start; nil sends no
// hints.
func SetMediaHealthHints(r Refresher, fn func(items []dp1playlist.PlaylistItem) interface{}, logger *zap.Logger) {
	setter, ok := r.(interface {
		setMediaHealthHints(func(items []dp1playlist.PlaylistItem) interface{})
	})
	if !ok {
		logger.Warn("Playlist refresher does not support media health wiring")
		return
	}
	setter.setMediaHealthHints(fn)
}

func (r *refresher) setMediaHealthHints(fn func(items []dp1playlist.PlaylistItem) interface{}) {
	r.mediaHealthHints = fn
}

// attachMediaHealthHints sets args["mediaHealth"] for playlist, or leaves
// it absent. Called on every refresher send immediately before the CDP
// write, on the final item list.
func (r *refresher) attachMediaHealthHints(args map[string]interface{}, playlist *dp1.Playlist) {
	delete(args, "mediaHealth")
	if r.mediaHealthHints == nil || playlist == nil {
		return
	}
	if hints := r.mediaHealthHints(playlist.Items); hints != nil {
		args["mediaHealth"] = hints
	}
}

func New(
	ctx context.Context,
	dp1 dp1.DP1,
	statusPoller status.Poller,
	cdp cdp.CDP,
	kioskReplay offlinecache.KioskReplay,
	offlineCache offlinecache.Service,
	jsonWrapper wrapper.JSON,
	scheduler playlistschedule.Scheduler,
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
		scheduler:    scheduler,
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
		case <-r.refreshChan:
			// forceCast stays false despite the "ForceRefresh" name: the two
			// "force" notions are unrelated. ForceRefresh only means "run a
			// pass now instead of waiting for the ticker" (replay-scope
			// resync after a CDP reconnect or a cache change), whereas
			// forceCast escalates a PERMANENTLY rejected soft refresh into a
			// now_display cast — an escalation only the startup loop above has
			// evidence for.
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

// logProcessFailure logs one failed refresh pass. CDP absence is the normal
// headless/mid-reconnect state, so the periodic retry remains quiet.
func (r *refresher) logProcessFailure(err error) {
	if errors.Is(err, errCDPNotReady) || errors.Is(err, cdp.ErrCDPConnectionNotInitialized) {
		return
	}
	r.logger.Error("Failed to process playing playlist", zap.Error(err))
}

// processPlayingPlaylist processes the playing playlist and sends it to CDP.
//
// forceCast, when true, sends a forced now_display cast unconditionally
// instead of the normal soft refresh — used by background()'s startup loop to
// escalate out of a permanently rejected soft refresh (see there).
//
// err is a named return so the deferred revert below can inspect the
// pass's final outcome without a separate captured variable.
func (r *refresher) processPlayingPlaylist(forceCast bool) (err error) {
	// The content-policy lock is taken AFTER resolution, not here: URL and
	// dynamic resolution below are network-bound (the shared 30s HTTP timeout),
	// and holding this lock across them would block getContentPolicy and
	// setContentPolicy for that long — an owner could not promptly apply a more
	// restrictive policy, and a slow playlist origin would become a lock-based
	// denial path. It is still held from the projection through the player send,
	// which is the ordering the policy contract needs.
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
	// Revert replay's Fetch-interception scope to whatever the player
	// actually still displays if this pass fails: the SyncPlaylist call
	// further down commits the NEW playlist's scope before the CDP send
	// runs, so a failure there leaves scope pointed at a playlist the kiosk
	// never actually switched to while it is still genuinely showing the
	// old one — see resyncKioskReplayScopeToCurrentDisplay's doc.
	//
	// Keyed on the named return alone, which is enough because a player
	// rejection (ok:false) is itself turned into an error below rather than
	// reported as success — see the send closure. An earlier revision also
	// inspected the raw CDP result here to tell "never attempted a send"
	// apart from "sent but rejected"; folding the rejection into the error
	// makes that distinction unnecessary.
	//
	// Registered BEFORE the kioskReplay LockPlayback/defer UnlockPlayback
	// pair below so that, on return, Go's LIFO defer order runs THAT
	// unlock first and only then this revert — which needs to re-acquire
	// the same non-reentrant lock itself (see KioskReplay.LockPlayback's
	// doc on why nesting would deadlock).
	defer func() {
		if err != nil {
			r.resyncKioskReplayScopeToCurrentDisplay()
		}
	}()

	var schedulerSource playlistschedule.Source
	if r.scheduler != nil {
		schedulerSource = r.scheduler.Source()
	}
	// A scheduler source restored from state written before contentContext
	// existed carries an empty value. It is non-zero, so the player-status
	// recovery below is skipped and nothing else would ever notice.
	legacySchedulerSource := !schedulerSource.IsZero() && schedulerSource.ContentContext == ""

	// staticInline is the "inline playlist with nothing dynamic to
	// re-resolve" case. It never needs a CDP re-send (the playlist cannot
	// have changed), but it DOES still need the replay-scope sync below: a
	// background downloadPlaylistItem/downloadPlaylist can finish, or a
	// cache can be cleared, while this exact static playlist keeps looping
	// on screen, and this periodic pass is the only thing that would ever
	// notice. So it is carried out of the source-derivation switch rather
	// than returned from inside it.
	var staticInline *dp1.Playlist
	var staticInlineContext string
	var playerStatus *status.PlayerStatus
	// contextUnknown marks a source whose origin this daemon cannot know: a
	// player status that omitted contentContext (only a player predating this
	// feature does that), or a scheduler source persisted before the field
	// existed. The router sets the field on every cast it forwards and on every
	// source it hands the scheduler, so in both cases an empty value means
	// "from before", never "curated". See the projection guard below for why it
	// is not just defaulted.
	contextUnknown := false

	if schedulerSource.IsZero() {
		// No scheduler-owned source exists, so the player remains the source of
		// truth for normal URL/dynamic refreshes.
		var statusErr error
		playerStatus, statusErr = r.statusPoller.FetchPlayerStatus(r.context)
		if statusErr != nil {
			return statusErr
		}
		if playerStatus == nil {
			r.logger.Warn("Player status is nil")
			return nil
		}

		if playerStatus.Command == string(commands.CMD_DISPLAY_DEFAULT_PLAYLIST) {
			// Default playback can also warm the automatic artwork cache. Only
			// reconcile replay scope; its playlist lifecycle stays player-owned.
			r.resyncKioskReplayScopeToCurrentDisplay()
			return nil
		}
		if playerStatus.Command != string(commands.CMD_DISPLAY_PLAYLIST) {
			return nil
		}

		switch {
		case playerStatus.PlaylistURL != nil:
			contextUnknown = playerStatus.ContentContext == ""
			schedulerSource = playlistschedule.Source{PlaylistURL: *playerStatus.PlaylistURL, ContentContext: playerStatus.ContentContext}
		case playerStatus.Playlist != nil && playerStatus.Playlist.HasDynamicContent():
			contextUnknown = playerStatus.ContentContext == ""
			schedulerSource = playlistschedule.Source{DynamicPlaylist: playerStatus.Playlist, ContentContext: playerStatus.ContentContext}
			// Prefer the verified inline dynamic document the cast retained:
			// player status drops the verdict (dp1.Playlist.Verification is
			// json:"-"), so re-resolving its copy loses the original verdict
			// in diagnostics. The retained
			// source carries the verdict; ProcessDynamicPlaylist copies the
			// struct by value, so the verdict survives hydration. Matched on
			// id so a retained source from a superseded cast is never used
			// (feral-file/ffos-user#307).
			//
			// The content context travels WITH it. The retained document is the
			// same cast, so it is the same audience decision — rebuilding the
			// source here without the context would silently reclassify a
			// personal cast as curated on the first refresh, which is the one
			// thing the unknown-origin handling below exists to prevent. The
			// verdict is primary and unweakened; the context rides alongside.
			if r.scheduler != nil {
				if retained := r.scheduler.InlineDynamicSource(); retained != nil && retained.ID == playerStatus.Playlist.ID {
					schedulerSource = playlistschedule.Source{
						DynamicPlaylist: retained,
						ContentContext:  playerStatus.ContentContext,
					}
				}
			}
		case playerStatus.Playlist != nil:
			// recastForMode's internal re-cast (feral-file/ffos-user#454)
			// rebuilds the on-screen document as a static dp1_call with its
			// original playlistUrl/dynamicQuery markers stripped (see
			// recastForMode's doc), so an otherwise-correct "nothing
			// refreshable" static document must not be assumed before
			// checking for a source retained behind this SAME id — a
			// retained source from a since-superseded cast (a different
			// playlist, or default playback) is never reused, because
			// RefreshSource only returns non-zero for a matching id
			// (feralfile-bot F1 on PR #454, 2026-11-10 14:15).
			if r.scheduler != nil {
				if retained := r.scheduler.RefreshSource(playerStatus.Playlist.ID); !retained.IsZero() {
					contextUnknown = retained.ContentContext == ""
					schedulerSource = retained
					break
				}
			}
			// Static inline player status only contains the filtered active set and
			// no refreshable source identity, so it cannot rebuild future items.
			r.logger.Debug("Playlist has no dynamic queries, skipping CDP resend")
			contextUnknown = playerStatus.ContentContext == ""
			// Defensive, not load-bearing: see resolveDisplayedPlaylist's own
			// static branch below, which this is the third occurrence of the
			// same condition for (the second is commandrouter/offlinecache.go's
			// resolveDisplayedPlaylist). Every display path now expands a
			// series before the player ever reports a playlist back, so this
			// is a no-op today — but the replay scope this feeds must agree
			// with what resolveDisplayedPlaylist would derive from the same
			// playerStatus, and relying on that invariant holding forever
			// (round 1 found it did not, for this exact function) is how the
			// next reviewer finds it a fourth time.
			// Short-deadlined because this runs on the refresher's one
			// select loop: see dp1.DefensiveSeriesExpansionTimeout.
			expandCtx, cancel := context.WithTimeout(r.context, dp1.DefensiveSeriesExpansionTimeout)
			expanded := r.dp1.ExpandSeriesItems(expandCtx, *playerStatus.Playlist, 0)
			cancel()
			staticInline = &expanded
			staticInlineContext = playerStatus.ContentContext
		default:
			// A displayPlaylist status carrying neither a URL nor an inline playlist
			// is the player's fresh-boot/unconfigured state (nothing assigned yet),
			// not a failure. Returning an error here would pin the startup loop at
			// PLAYER_STATUS_POLLING_INTERVAL and emit an Error every pass for as
			// long as the device sits unconfigured — hours on a first boot with no
			// network. There is nothing to refresh; report success so the refresher
			// settles into its normal PLAYLIST_REFRESH_INTERVAL cadence. This check
			// must stay in sync with resolveDisplayedPlaylist's own default case
			// (which returns an error instead, for
			// resyncKioskReplayScopeToCurrentDisplay's best-effort caller where an
			// error is simply logged and swallowed).
			return nil
		}
	}

	if staticInline != nil {
		return r.refreshStaticInline(staticInline, staticInlineContext, contextUnknown, playerStatus, authorityToken)
	}

	var playlist *dp1.Playlist
	var kind string
	switch {
	case schedulerSource.PlaylistURL != "":
		kind = "playlist URL"
		currentlyDisplayed := r.currentlyDisplayedItems(playerStatus)
		// Built from schedulerSource.ContentContext, not playerStatus — the
		// scheduler-owned branch this case serves may hold no playerStatus
		// yet (currentlyDisplayed's own fetch above is lazy). legacySchedulerSource
		// folds in the same way it does for contextUnknown below, before the
		// projection that actually uses it: both answer the identical "will this
		// tick's cast be filtered at all" question, just at two different points
		// in this function (feralfile-bot F7, 2026-11-13).
		displayFilter := r.contentDisplayFilter(schedulerSource.ContentContext, contextUnknown || legacySchedulerSource)
		playlist, err = r.narrowedPlaylistFromURL(schedulerSource.PlaylistURL, currentlyDisplayed, displayFilter)
		if err != nil {
			// Offline fallback: serve the last successfully cached body for
			// this URL rather than failing the pass outright, mirroring
			// resolveDisplayedPlaylist's own URL branch. Only a cache miss
			// leaves the live error standing.
			if cached, cacheErr := r.loadCachedPlaylistForURL(schedulerSource.PlaylistURL, currentlyDisplayed, displayFilter); cacheErr == nil {
				playlist, err = cached, nil
			}
		}
	case schedulerSource.DynamicPlaylist != nil:
		kind = "dynamic playlist"
		playlist, err = r.dp1.ProcessDynamicPlaylist(r.context, *schedulerSource.DynamicPlaylist, false)
	}
	if err != nil {
		// The source cannot be re-resolved, so this pass will neither project
		// nor re-send — and a policy tightened a moment ago would otherwise
		// leave the blocked work on the wall for as long as the source stays
		// unreachable, while the Content screen reported success. Offline
		// caching defaults OFF, so on a stock device with a non-displayAt
		// playlist there is no fallback here at all.
		//
		// Retire it from what the PLAYER holds, before the early return. No
		// retained state: the on-screen item is judged on its own rating, the
		// same predicate the successful path uses. A blank wall until the
		// source returns is the accepted trade; a blocked work on the wall is
		// not.
		r.retireBlockedCurrentWithoutASource(schedulerSource)
		return r.handleRefreshError(err, kind, schedulerSource)
	}

	contentContext, contextErr := contentpolicy.NormalizeContext(schedulerSource.ContentContext)
	if contextErr != nil {
		return contextErr
	}
	if legacySchedulerSource {
		contextUnknown = true
	}
	// Resolution is done; take the policy lock now and hold it through the
	// projection and the player send (see the note at the top of this function).
	if r.contentPolicy != nil {
		r.contentPolicy.Lock()
		defer r.contentPolicy.Unlock()
	}
	retireBlockedCurrent := false
	// A source rebuilt from a status with no contentContext carries an UNKNOWN
	// origin, not a curated one. Defaulting it to curated silently reclassifies
	// a cast the owner made as personal, so the first refresh would strip the
	// mature items they deliberately put on the wall — the opposite of the
	// documented guarantee that context survives refresh. Leave the playlist as
	// the cast admitted it instead. A player this old has no policy mirror of
	// its own either (setContentPolicy answers unsupported), so there is no
	// enforcement being bypassed here that ever worked on it.
	if contextUnknown {
		r.logger.Warn("Player status omits contentContext; leaving this refresh unprojected rather than reclassifying the cast as curated",
			zap.String("source", schedulerSource.PlaylistURL))
	}
	if r.contentPolicy != nil && !contextUnknown {
		var blocked bool
		projected, blocked, projectErr := r.contentPolicy.ProjectLocked(&playlist.Playlist, contentContext)
		if projectErr != nil {
			return projectErr
		}
		// Whether the current frame must be retired is decided from the item the
		// player actually has on screen, NOT from the refreshed set's contents.
		//
		// Sample status HERE, after resolution: it is network-bound and playback
		// keeps advancing during it, so a read taken beforehand can name an item
		// that has since been replaced. The status poller is already polling on
		// its own short interval, so this is not a new cost of consequence.
		if fresh, statusErr := r.statusPoller.FetchPlayerStatus(r.context); statusErr == nil && fresh != nil {
			playerStatus = fresh
		}
		activePolicy := r.contentPolicy.CurrentLocked()
		// Two independent reasons to retire, and the second is why membership in
		// the refreshed feed cannot be the gate: an item that DISAPPEARS from the
		// source feed during this refresh matches nothing, so the projection is
		// unchanged and the old code never even looked — while a soft refresh
		// defers precisely when its current item is absent from the new list,
		// leaving that blocked frame on screen indefinitely.
		retireBlockedCurrent = currentItemBlocked(playerStatus, activePolicy, contentContext) ||
			(len(projected.Items) != len(playlist.Items) &&
				currentBlockedByRefresh(playerStatus, playlist, activePolicy, contentContext))
		playlist.Playlist = *projected
		if blocked {
			r.logger.Warn("Playlist refresh is all blocked; retiring current content")
		}
	}

	// Re-sync offline-cache replay scope before the re-send: this is the
	// periodic path that keeps interception coherent while a playlist keeps
	// looping. Best-effort — never let a sync failure block the actual
	// refresh, since offline replay is a strict enhancement over the live
	// path this loop exists to maintain.
	if r.kioskReplay != nil {
		r.kioskReplay.LockPlayback()
		defer r.kioskReplay.UnlockPlayback()
		// Skip the sync when playlist authority moved while this pass was
		// resolving: the send below will be skipped for the exact same
		// token mismatch, and syncing anyway would overwrite — under this
		// same lock, AFTER it ran — the scope the new authority's own
		// cast just installed, leaving interception pointed at the
		// previous playlist while the new one is on screen until the next
		// pass (feral-file/ffos-user#229 review finding). Checked under
		// the playback lock so it strictly orders against that cast's own
		// locked sync+send.
		if r.scheduler != nil && r.scheduler.AuthorityToken() != authorityToken {
			r.logger.Debug("Skipping replay scope sync: playlist authority changed during refresh resolution")
			return nil
		}
		r.syncReplayScopeLocked(playlist)
	}

	var schedulerSnapshot playlistschedule.Snapshot
	schedulerMutated := false
	// Captured before the scheduler filters the playlist: the verdict and the
	// identity belong to the document as resolved.
	refreshVerdict := playlist.Verification
	refreshPlaylistID := playlist.ID

	// Send playlist to CDP
	args := map[string]interface{}{
		"dp1_call":       playlist,
		"refresh":        true,
		"contentContext": string(contentContext),
	}
	if retireBlockedCurrent {
		args["retireBlockedCurrent"] = true
	}
	r.attachMediaHealthHints(args, playlist)
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
				if retireBlockedCurrent {
					command.Arguments["dp1_call"] = playlist
					result, sendCDPErr := r.sendCDPRequest(command)
					sendErr = sendCDPErr
					if sendErr == nil && !playerresponse.OK(result) {
						sendErr = errPlayerRejectedRefresh
					}
					if sendErr != nil {
						r.scheduler.Restore(schedulerSnapshot)
					} else {
						r.scheduler.Commit()
					}
					return
				}
				// Keep the future schedule armed, but do not send an empty list:
				// the player rejects it and cannot improve the current artwork.
				r.scheduler.Commit()
				// The schedule now holds THIS document; the cutover that
				// eventually pushes it must promote this verdict, not the
				// one a previous deferred cast parked (#307). An unverified
				// replacement (cached copy) is staged as such so the
				// promotion clears rather than inherits.
				if r.activeVerdict != nil {
					if refreshVerdict != nil {
						r.activeVerdict.SetPending(refreshPlaylistID, schedulerSource.PlaylistURL, refreshVerdict.Status)
					} else {
						r.activeVerdict.SetPendingUnverified()
					}
				}
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
				"dp1_call":       playlist,
				"contentContext": string(contentContext),
			}
			if retireBlockedCurrent {
				command.Arguments["retireBlockedCurrent"] = true
			}
			if schedulerSource.PlaylistURL != "" {
				command.Arguments["playlistUrl"] = schedulerSource.PlaylistURL
			}
		} else {
			command.Arguments["dp1_call"] = playlist
		}
		r.attachMediaHealthHints(command.Arguments, playlist)
		// Invalidate BEFORE the send (#307): from the moment it lands the
		// player may show the new document, and the status poller could
		// match the previous one by URL before the reply is processed. A
		// force cast clears outright and re-publishes below; a soft refresh
		// reconciles now (the player may or may not swap, and acceptance
		// will not say — see Active.ReconcileSoft); a verdict-less push
		// clears. A failed send leaves the slot in that state: an omission.
		if r.activeVerdict != nil {
			switch {
			case refreshVerdict == nil || effectiveForceCast:
				r.activeVerdict.ClearCurrent()
			default:
				r.activeVerdict.ReconcileSoft(refreshPlaylistID, schedulerSource.PlaylistURL, refreshVerdict.Status)
			}
		}
		generationBefore := r.currentGeneration()
		result, sendCDPErr := r.sendCDPRequest(command)
		sendErr = sendCDPErr
		// Transport success with ok:false (or a malformed body) must still
		// fail the refresh pass: otherwise the startup loop treats the reject
		// as initial success and drops from 5s retries to the 5m ticker while
		// the player never accepted the playlist. It is also what makes the
		// replay-scope revert at the top of this function fire on a rejection,
		// not just on a transport failure.
		if sendErr == nil && !playerresponse.OK(result) {
			sendErr = errPlayerRejectedRefresh
		}
		// Publish what this push did to the screen (#307), inside the send
		// closure — under WithPlayerPush — so it is ordered against casts
		// and cutovers. A force cast (now_display) replaces the artwork at
		// once, so its verdict is published outright; a verdict-less push
		// (the cached-copy fallback) CLEARS the slot (pending too) rather
		// than leaving a previous verdict standing for the same URL. A SOFT
		// refresh was reconciled before the send and nothing more is known
		// after it.
		if sendErr == nil && r.activeVerdict != nil {
			switch {
			case refreshVerdict == nil:
				r.activeVerdict.Clear()
			case effectiveForceCast && r.currentGeneration() != generationBefore:
				// The page moved under this send: its accepted reply
				// belongs to a document that is gone. Leave current as
				// the bump hook left it (cleared); the new generation's
				// reconciliation re-pushes and publishes afresh. Pending
				// is still restaged below — the scheduler cache moved
				// regardless of the page.
				r.logger.Info("playlist refresh: page generation changed across the send; verdict left unpublished")
			case effectiveForceCast:
				r.activeVerdict.Set(refreshPlaylistID, schedulerSource.PlaylistURL, refreshVerdict.Status)
			default:
				// Soft refresh: already reconciled before the send.
			}
			// The scheduler's cache now holds THIS document (Commit below),
			// so every later cutover pushes its cohorts: restage pending to
			// match, or a cutover would promote whatever an earlier
			// deferred cast parked. Set above already dropped pending; the
			// soft path did not. Skipped when the send failed: the snapshot
			// is restored below and the prior pending stays true.
			if schedulerMutated && r.scheduler.HasCache() {
				if refreshVerdict != nil {
					r.activeVerdict.SetPending(refreshPlaylistID, schedulerSource.PlaylistURL, refreshVerdict.Status)
				} else {
					r.activeVerdict.SetPendingUnverified()
				}
			}
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
	// Assigned to the named return so the deferred replay-scope revert above
	// observes it.
	err = sendErr
	return err
}

// refreshStaticInline handles the one displayed shape with no refreshable
// source: an inline playlist with nothing dynamic and no displayAt. It cannot
// be re-resolved, so this path has always only re-synced replay scope.
//
// It must still apply the content policy. A cast from the app is usually
// exactly this shape, so without it an owner who turns mature content OFF
// watches the mature work stay on screen indefinitely — there is no later
// refresh that would remove it, which is precisely the failure the policy
// exists to prevent.
//
// What it CANNOT do is the relaxing direction: the player's status carries only
// the already-filtered set that was cast, so items an earlier projection
// removed are simply not there to restore. That needs the daemon to retain the
// original document, the same retained-state change deferred for scheduled
// playlists.
func (r *refresher) refreshStaticInline(playlist *dp1.Playlist, rawContext string, contextUnknown bool, playerStatus *status.PlayerStatus, authorityToken uint64) error {
	// Both no-send exits below scope replay from what the PLAYER reports, not
	// from the expanded playlist this pass resolved: nothing is sent on either,
	// so a scope built from the expansion would describe items the kiosk will
	// never fetch — see playlistForReplayScope. The re-send path further down
	// is the opposite case and keeps scoping from the playlist it sends.
	displayed := playlistForReplayScope(playerStatus, playlist)

	if r.contentPolicy == nil || contextUnknown {
		if contextUnknown {
			r.logger.Warn("Player status omits contentContext; leaving this inline playlist unprojected rather than reclassifying it as curated")
		}
		return r.syncStaticInlineScope(displayed)
	}

	origin, contextErr := contentpolicy.NormalizeContext(rawContext)
	if contextErr != nil {
		return contextErr
	}

	r.contentPolicy.Lock()
	defer r.contentPolicy.Unlock()
	projected, blocked, projectErr := r.contentPolicy.ProjectLocked(&playlist.Playlist, origin)
	if projectErr != nil {
		return projectErr
	}
	if len(projected.Items) == len(playlist.Items) {
		// Nothing newly blocked: the common case, and it must stay as cheap as
		// it was before this path did any projection at all.
		return r.syncStaticInlineScope(displayed)
	}

	retire := currentBlockedByRefresh(playerStatus, playlist, r.contentPolicy.CurrentLocked(), origin)
	out := *playlist
	out.Playlist = *projected
	if blocked {
		r.logger.Warn("Inline playlist is all blocked by the content policy; retiring current content")
	}

	if !r.cdp.Initialized() {
		return errCDPNotReady
	}
	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"intent":         map[string]interface{}{"action": "now_display"},
			"dp1_call":       &out,
			"contentContext": string(origin),
		},
	}
	if retire {
		command.Arguments["retireBlockedCurrent"] = true
	}
	r.attachMediaHealthHints(command.Arguments, &out)

	// Serialized against every other authoritative display, exactly like the
	// ordinary refresh send below. This re-send is built from a status read
	// taken BEFORE the projection above, so without the push lock and the
	// authority re-check a cast that completed in between would be overwritten
	// by this stale playlist — and its offline-replay scope with it.
	var sendErr error
	send := func() {
		if r.scheduler != nil && r.scheduler.AuthorityToken() != authorityToken {
			r.logger.Debug("Skipping obsolete inline policy re-send: playlist authority changed")
			return
		}
		if r.kioskReplay != nil {
			r.kioskReplay.LockPlayback()
			defer r.kioskReplay.UnlockPlayback()
			r.syncReplayScopeLocked(&out)
		}
		result, sendCDPErr := r.sendCDPRequest(command)
		if sendCDPErr != nil {
			sendErr = sendCDPErr
			return
		}
		if !playerresponse.OK(result) {
			sendErr = errPlayerRejectedRefresh
		}
	}
	if r.scheduler != nil {
		r.scheduler.WithPlayerPush(send)
	} else {
		send()
	}
	return sendErr
}

// syncStaticInlineScope is the pre-existing behavior for this shape: keep
// offline-replay scope coherent while the same playlist keeps looping, and send
// nothing.
func (r *refresher) syncStaticInlineScope(playlist *dp1.Playlist) error {
	if r.kioskReplay != nil {
		r.kioskReplay.LockPlayback()
		defer r.kioskReplay.UnlockPlayback()
		r.syncReplayScopeLocked(playlist)
	}
	return nil
}

// retireBlockedCurrentWithoutASource retires the on-screen item when the policy
// refuses it and the source cannot be resolved to build a replacement.
//
// Acts ONLY on a rating it understands. An unrated item — or one carrying a
// label from a newer spec revision — is left alone: nothing is assumed from a
// label the daemon cannot interpret, so the operator's unrated gate does not
// blank a wall from here either. An unknown content context is left alone for
// the same reason it is left unprojected everywhere else.
//
// Sends under the player-push barrier with the authority re-check, like every
// other send on this path, so a cast that landed while the resolution was
// failing is not overwritten by this retirement.
func (r *refresher) retireBlockedCurrentWithoutASource(source playlistschedule.Source) {
	if r.contentPolicy == nil || source.ContentContext == "" {
		return
	}
	origin, err := contentpolicy.NormalizeContext(source.ContentContext)
	if err != nil {
		return
	}
	playerStatus, statusErr := r.statusPoller.FetchPlayerStatus(r.context)
	if statusErr != nil || playerStatus == nil {
		return
	}
	item, ok := currentPlayerItem(playerStatus)
	if !ok || !contentpolicy.Rated(item) {
		return
	}

	r.contentPolicy.Lock()
	allowed := r.contentPolicy.CurrentLocked().Allows(item, origin)
	r.contentPolicy.Unlock()
	if allowed {
		return
	}
	if !r.cdp.Initialized() {
		return
	}

	authorityToken := uint64(0)
	if r.scheduler != nil {
		authorityToken = r.scheduler.AuthorityToken()
	}
	send := func() {
		if r.scheduler != nil && r.scheduler.AuthorityToken() != authorityToken {
			return
		}
		r.logger.Warn("Retiring the on-screen item: the content policy refuses it and its source cannot be resolved",
			zap.String("source", source.PlaylistURL))
		command := commands.Command{
			Type: commands.CMD_DISPLAY_PLAYLIST,
			Arguments: map[string]interface{}{
				"intent":               map[string]interface{}{"action": "now_display"},
				"dp1_call":             &dp1.Playlist{Playlist: dp1playlist.Playlist{}},
				"contentContext":       string(origin),
				"retireBlockedCurrent": true,
			},
		}
		if _, sendErr := r.sendCDPRequest(command); sendErr != nil {
			r.logger.Warn("Failed to retire the blocked on-screen item", zap.Error(sendErr))
		}
	}
	if r.scheduler != nil {
		r.scheduler.WithPlayerPush(send)
	} else {
		send()
	}
}

// currentPlayerItem returns the item the player reports as on screen.
func currentPlayerItem(playerStatus *status.PlayerStatus) (dp1playlist.PlaylistItem, bool) {
	if playerStatus == nil || playerStatus.Index == nil || *playerStatus.Index < 0 {
		return dp1playlist.PlaylistItem{}, false
	}
	items := playerStatus.Items
	if items == nil && playerStatus.Playlist != nil {
		items = &playerStatus.Playlist.Items
	}
	if items == nil || *playerStatus.Index >= len(*items) {
		return dp1playlist.PlaylistItem{}, false
	}
	return (*items)[*playerStatus.Index], true
}

// currentItemBlocked reports whether the item the player currently has on
// screen is refused by policy, judged on ITS OWN terms — no reference to any
// refreshed playlist. That independence is the point: an item can be blocked
// and simultaneously absent from the refreshed feed, which is exactly when a
// soft refresh defers and leaves it displayed.
//
// Unknown current identity returns false here: currentBlockedByRefresh already
// fails safe for that case against the refreshed set, and guessing "blocked"
// from no information would retire healthy frames on every pass.
func currentItemBlocked(playerStatus *status.PlayerStatus, policy contentpolicy.Policy, origin contentpolicy.Context) bool {
	if playerStatus == nil || playerStatus.Index == nil || *playerStatus.Index < 0 {
		return false
	}
	items := playerStatus.Items
	if items == nil && playerStatus.Playlist != nil {
		items = &playerStatus.Playlist.Items
	}
	if items == nil || *playerStatus.Index >= len(*items) {
		return false
	}
	return !policy.Allows((*items)[*playerStatus.Index], origin)
}

func currentBlockedByRefresh(playerStatus *status.PlayerStatus, fresh *dp1.Playlist, policy contentpolicy.Policy, origin contentpolicy.Context) bool {
	// Unknown current identity fails safe: the refresh changed the allowed set,
	// so remounting is preferable to retaining a newly mature old frame.
	if playerStatus == nil || playerStatus.Index == nil || *playerStatus.Index < 0 {
		return true
	}
	items := playerStatus.Items
	if items == nil && playerStatus.Playlist != nil {
		items = &playerStatus.Playlist.Items
	}
	if items == nil || *playerStatus.Index >= len(*items) {
		return true
	}
	current := (*items)[*playerStatus.Index]
	// Prefer the item ID, which is the exact identity.
	if current.ID != "" {
		for _, item := range fresh.Items {
			if item.ID == current.ID {
				return !policy.Allows(item, origin)
			}
		}
	}
	// Then fall back to the source URL. A refreshed feed can re-mint IDs while
	// the artwork behind a source is unchanged; without this fallback, a work
	// that keeps its source but gains a "mature" label on refresh matches
	// nothing, and the current frame is never retired.
	if current.Source != "" {
		for _, item := range fresh.Items {
			if item.Source == current.Source {
				return !policy.Allows(item, origin)
			}
		}
	}
	// Genuinely absent from the fresh set: the refresh itself replaces the
	// frame, so there is nothing to retire here.
	return false
}

// playlistForReplayScope returns the playlist whose items offline-replay
// scope must be derived from, for a pass that sends the player NOTHING.
//
// Interception is keyed on the URLs the kiosk actually fetches, so the scope
// has to describe what the PLAYER holds — not what the daemon would resolve
// the same status to now. Whenever player status carries a concrete item list
// (a static inline playlist: the filtered active set the player was cast),
// that list IS what it holds, and re-resolving it can only diverge from the
// screen: a seriesRegistry item cast while the catalog was unreachable keeps
// its representative, so a later pass whose expansion succeeds would scope
// replay to editions that were never displayed and never cached. SyncPlaylist
// then finds nothing cached and disables replay outright
// (offlinecache.applyScope), while the cached representative is still on the
// wall (feralfile-bot #421 F7).
//
// A URL status carries no item list at all, and a dynamic one carries queries
// rather than resolved items, so there the resolved playlist is the only
// answer available — and it is also the one the player was given.
//
// Callers that DO send a replacement playlist must scope from what they send,
// not from this: see refreshStaticInline's re-send path. Mirrored by
// commandrouter/offlinecache.go's own copy, for the same reason
// resolveDisplayedPlaylist is duplicated there (see that function's doc).
func playlistForReplayScope(playerStatus *status.PlayerStatus, resolved *dp1.Playlist) *dp1.Playlist {
	if playerStatus != nil && playerStatus.Playlist != nil && !playerStatus.Playlist.HasDynamicContent() {
		return playerStatus.Playlist
	}
	return resolved
}

// syncReplayScopeLocked points offline-cache replay at playlist's items and
// announces the change. Callers MUST already hold the playback coordinator
// (KioskReplay.LockPlayback) and keep holding it across whatever CDP send
// follows, so this pass's scope commit and send cannot interleave with a
// concurrent displayPlaylist's own pair — see that lock's doc. Kept
// lock-free itself so the two call sites keep their LockPlayback/defer
// UnlockPlayback visibly adjacent rather than hiding half the pair.
//
// Best-effort by design: a sync failure is logged, never returned, since
// offline replay is an enhancement over the live path.
func (r *refresher) syncReplayScopeLocked(playlist *dp1.Playlist) {
	if playlist == nil {
		return
	}
	sources := make([]string, 0, len(playlist.Items))
	for _, item := range playlist.Items {
		sources = append(sources, item.Source)
	}
	if _, syncErr := r.kioskReplay.SyncPlaylist(r.context, sources); syncErr != nil {
		r.logger.Warn("offline cache: failed to sync kiosk replay scope during refresh", zap.Error(syncErr))
	}
	// Announce this authoritative scope change (under the lock) so a
	// concurrent corrective resync defers to it rather than clobbering it
	// with a stale playlist's scope — see KioskReplay.PlaybackGeneration's
	// doc.
	r.kioskReplay.MarkPlaybackChanged()
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
		currentlyDisplayed := r.currentlyDisplayedItems(playerStatus)
		displayFilter := r.contentDisplayFilter(playerStatus.ContentContext, playerStatus.ContentContext == "")
		playlist, err := r.narrowedPlaylistFromURL(url, currentlyDisplayed, displayFilter)
		if err != nil {
			cached, cacheErr := r.loadCachedPlaylistForURL(url, currentlyDisplayed, displayFilter)
			if cacheErr != nil {
				return nil, err
			}
			return cached, nil
		}
		return playlist, nil
	case playerStatus.Playlist != nil:
		if !playerStatus.Playlist.HasDynamicContent() {
			// Defensive, not load-bearing: see
			// commandrouter/offlinecache.go's resolveDisplayedPlaylist,
			// which mirrors this same shape for the same reason — and
			// dp1.DefensiveSeriesExpansionTimeout for why a defensive
			// expansion on this loop gets its own short deadline.
			expandCtx, cancel := context.WithTimeout(r.context, dp1.DefensiveSeriesExpansionTimeout)
			defer cancel()
			expanded := r.dp1.ExpandSeriesItems(expandCtx, *playerStatus.Playlist, 0)
			return &expanded, nil
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
	//
	// Unlike commandrouter's clear-path resync — whose sample must be
	// taken UNDER the playback lock because a cache deletion precedes it
	// and an in-flight installer's pre-clear store reads must not win
	// (see that function's doc) — this sample deliberately stays
	// unlocked: no store mutation precedes a refresh-failure resync, so
	// a concurrent installer's scope was computed from the same store
	// state this resync would read, and deferring to it is always safe.
	genBeforeResolve := r.kioskReplay.PlaybackGeneration()

	playerStatus, err := r.statusPoller.FetchPlayerStatus(r.context)
	if err != nil {
		r.logger.Warn("offline cache: failed to fetch player status to resync replay scope after refresh failure", zap.Error(err))
		return
	}
	if playerStatus == nil || (playerStatus.Command != string(commands.CMD_DISPLAY_PLAYLIST) && playerStatus.Command != string(commands.CMD_DISPLAY_DEFAULT_PLAYLIST)) {
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

	// This resync never sends the player a playlist — it only re-points
	// interception at what is already on screen — so the scope comes from the
	// items the player reported, not from the resolve above. See
	// playlistForReplayScope.
	scoped := playlistForReplayScope(playerStatus, playlist)
	sources := make([]string, 0, len(scoped.Items))
	for _, item := range scoped.Items {
		sources = append(sources, item.Source)
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
	if _, syncErr := r.kioskReplay.SyncPlaylist(r.context, sources); syncErr != nil {
		r.logger.Warn("offline cache: failed to sync kiosk replay scope after refresh failure", zap.Error(syncErr))
	}
}

// fromOfflineCacheSegments converts offlinecache's own SeriesSegment mirror
// type back into dp1.CastSegment, for loadCachedPlaylistForURL below to
// build a dp1.CastPlan and call its existing Narrow(deviceMode). Duplicated
// from commandrouter's identical helper (offlinecache.go's
// toOfflineCacheSegments/fromOfflineCacheSegments) rather than shared, for
// the same reason this package already carries its own
// loadCachedPlaylistForURL/resolveDisplayedPlaylist copies — see
// AGENTS.md's service-boundary guidance. Keep the two in sync.
func fromOfflineCacheSegments(segments []offlinecache.SeriesSegment) []dp1.CastSegment {
	out := make([]dp1.CastSegment, 0, len(segments))
	for _, seg := range segments {
		if seg.Item != nil {
			item := *seg.Item
			out = append(out, dp1.CastSegment{Item: &item})
			continue
		}
		group := seg.Group
		var documentMode *dp1.SeriesPlaybackMode
		if group.DocumentMode != nil {
			mode := dp1.SeriesPlaybackMode(*group.DocumentMode)
			documentMode = &mode
		}
		out = append(out, dp1.CastSegment{
			Group: &dp1.ResolvedSeriesGroup{
				Key: dp1.SeriesGroupKey{
					Chain:    group.Key.Chain,
					Address:  group.Key.Address,
					SeriesID: dp1playlist.SeriesID(group.Key.SeriesID),
				},
				DocumentMode:   documentMode,
				Representative: group.Representative,
				Editions:       group.Editions,
				Resolved:       group.Resolved,
				ArtworkModel:   dp1.SeriesArtworkModel(group.ArtworkModel),
			},
		})
	}
	return out
}

// narrowedPlaylistFromURL fetches url and narrows any seriesRegistry item it
// carries by the device's current series-playback override — the same
// dp1.CastPlan.Narrow logic a live cast already applies — instead of the
// unconditional, mode-ignorant full expansion r.dp1.ProcessPlaylistURL used
// to apply at both of this method's call sites (feral-file/ffos-user#453: a
// resync on this LIVE-reachable path was re-expanding a series back to full
// after an earlier cast had narrowed it, and resolveDisplayedPlaylist's own
// replay-scope resolve had the identical gap).
//
// Deliberately NOT ProcessPlaylistURLForCast: that variant caps at
// CAST_PLAYLIST_ITEMS_LIMIT, which this resync path must not newly start
// truncating — it resyncs whatever is already playing, however large, the
// same uncapped contract ProcessPlaylistURL always had.
//
// currentlyDisplayed is narrowStable's seed source — see that method's doc.
// May be nil (no player status in hand at this call site); narrowStable
// treats that exactly like "nothing to seed from" and rolls fresh.
//
// displayFilter is narrowStable's occurrence predicate — see that method's
// doc. May be nil (no content-policy context known at this call site);
// narrowStable then counts every item, exactly as before displayFilter
// existed.
func (r *refresher) narrowedPlaylistFromURL(url string, currentlyDisplayed func() []dp1playlist.PlaylistItem, displayFilter func(dp1playlist.PlaylistItem) bool) (*dp1.Playlist, error) {
	playlist, rawPlaylist, err := r.dp1.FetchPlaylist(r.context, url)
	if err != nil {
		return nil, err
	}
	if playlist.HasDynamicContent() {
		return r.dp1.ProcessDynamicPlaylist(r.context, *playlist, false)
	}
	deviceMode := r.currentSeriesPlaybackMode()
	plan := r.dp1.ResolveSeriesCastPlan(r.context, *playlist, rawPlaylist, deviceMode, 0)
	narrowed := *playlist
	narrowed.Items = r.narrowStable(plan, deviceMode, currentlyDisplayed, displayFilter)
	return &narrowed, nil
}

// narrowStable is dp1.CastPlan.Narrow with one difference: a group currently
// resolving to dp1.SeriesPlaybackRandom reuses its last pick's SOURCE for
// that dp1.SeriesGroupKey (randomSeriesPicks) instead of asking
// dp1.NarrowEditions to roll a new one, as long as (a) the mode resolved for
// it is still random, (b) the group's ArtworkModel hasn't changed since the
// pick was made (see randomSeriesPick's doc — a change can flip whether a
// distinct pick is even allowed), and (c) that source is still present among
// the group's current editions — in which case the matching edition is
// re-looked-up fresh from THIS tick's Editions (editionBySource), never
// replayed from the pick itself, so a field other than Source (duration,
// license, contentRating, …) never goes stale behind this memory
// (run-reviewer, round 3 on this fix, 2026-11-12). Either condition failing
// forces a fresh pick: a mode change ("setting change") or the catalog's
// answer no longer carrying the previously shown edition's source ("that
// edition disappears") — the two escape hatches feralfile-bot's F3 asked
// for, 2026-11-12. A non-random group, OR one whose ArtworkModel does not
// have distinct editions at all (single/multi — nothing for random to
// pick between, dp1.NarrowEditions always collapses it to the
// representative regardless of mode), clears any stale memory for its key
// and goes straight to dp1.NarrowEditions: such a group is never seeded,
// reused, or memoized by anything below, so it can never pick up a
// non-representative edition this way — a single/multi group whose one
// edition's Source happened to coincide with the representative's, or with
// whatever the player currently shows, used to be memoized and replayed as
// if it were a genuine distinct pick (feralfile-bot F5, 2026-11-12). A
// later mode change back to random, or a correction INTO multi_unique,
// starts from a real roll rather than a stale leftover from before.
//
// Scoped to this package's own live-reachable resync and its cached-sidecar
// fallback (both of narrowedPlaylistFromURL's and loadCachedPlaylistForURL's
// callers): a live setSeriesPlayback cast or the initial display makes its
// own fresh pick through commandrouter's separate dp1.CastPlan.Narrow call,
// which this memory does not see or influence by itself — the two packages
// deliberately do not share state (AGENTS.md's service-boundary guidance).
// currentlyDisplayed is this package's only window onto that live choice,
// so every tick for a still-random, distinct-editions group checks it
// FIRST, before any remembered pick: called at most once per narrowStable
// call (memoized below — a plan with several unseeded random groups costs
// at most one call, however many groups ask for it). The check is by
// OCCURRENCE, not by scanning the whole displayed list for any matching
// Source: displayed is itself a flatten of this same plan (live or stale),
// so this group's own slot is whatever position items has already reached
// in THIS loop — editionAtOccurrence reads exactly that index. A whole-list
// scan used to let an unrelated item elsewhere in the playlist that happens
// to share a Source with one of this group's editions be mistaken for the
// group's own live pick — e.g. an explicit standalone item ahead of a
// multi_unique series also containing that same edition (feralfile-bot F7,
// 2026-11-12). When the item at that occurrence is among THIS group's
// editions, that is adopted as the pick — seeded if none was remembered
// yet, or OVERWRITING a remembered pick that disagrees with it. The
// overwrite is what lets a newer accepted cast's own choice (a
// setSeriesPlayback recast of the same URL that rolls a different edition,
// or a setting recast back into random) replace this memory instead of
// being silently reverted by the next periodic resync (feralfile-bot F6,
// 2026-11-12): on an ORDINARY tick the player is still showing exactly
// what was last remembered, so this only reconfirms the existing pick,
// never changes it. An out-of-range occurrence (no player status in hand,
// or a shorter displayed list) or a non-match falls through to the
// remembered pick, then to a fresh roll, exactly as before this check
// existed.
//
// displayFilter reports whether an item would survive the SAME content-
// policy projection that produced displayed's own on-screen state — nil
// when no such projection could have happened (no content-policy store, or
// the context behind this tick's cast is unknown), in which case occurrence
// counts every earlier item exactly as it did before displayFilter existed.
// It exists because displayed is the PLAYER's state, already filtered,
// while items (built up across this very loop) is not: content-policy
// projection runs AFTER narrowStable, over the finished, unfiltered plan
// (see processPlayingPlaylist's own ProjectLocked call). Without it, an
// earlier curated item this tick's policy would also remove (a mature
// item under the default policy, say) still counts toward occurrence,
// shifting every later group's slot by however many such items precede it
// — reading the WRONG position out of displayed and overwriting a correct
// remembered pick with an unrelated item (feralfile-bot F7, 2026-11-13;
// this is a second, distinct failure mode from the whole-list-scan F7 the
// occurrence check above itself replaced on 2026-11-12 — that one needed
// no filtering mismatch at all to misfire).
func (r *refresher) narrowStable(plan dp1.CastPlan, device *dp1.SeriesPlaybackMode, currentlyDisplayed func() []dp1playlist.PlaylistItem, displayFilter func(dp1playlist.PlaylistItem) bool) []dp1playlist.PlaylistItem {
	r.randomSeriesMu.Lock()
	defer r.randomSeriesMu.Unlock()
	if r.randomSeriesPicks == nil {
		r.randomSeriesPicks = make(map[dp1.SeriesGroupKey]randomSeriesPick)
	}

	// Memoized so a plan with several unseeded random groups costs at most
	// one call to currentlyDisplayed (which may itself be a live
	// FetchPlayerStatus round trip), not one per group.
	var displayed []dp1playlist.PlaylistItem
	displayedFetched := false
	fetchDisplayed := func() []dp1playlist.PlaylistItem {
		if !displayedFetched {
			displayedFetched = true
			if currentlyDisplayed != nil {
				displayed = currentlyDisplayed()
			}
		}
		return displayed
	}

	// scheduleActive answers, per segment, the SAME displayAt question
	// playlistschedule.Scheduler.PrepareWithSource asks of the flattened
	// playlist narrowStable returns — before this, occurrence counted a
	// future-scheduled segment that PrepareWithSource goes on to strip out
	// entirely, so every later group's slot in displayed was shifted by
	// however many such segments preceded it (feralfile-bot F7,
	// 2026-11-13; a second, distinct failure mode from the content-policy
	// one filteredOccurrence/displayFilter already closed one round
	// earlier — this one needs no policy mismatch to misfire, only an
	// item-level displayAt schedule). See scheduleActiveSegments' doc.
	// time.Now() directly, not r.clock: r.clock is a gomock-backed seam many
	// existing tests construct without stubbing Now() (narrowStable never
	// called it before), and this check only needs to agree with wall-clock
	// reality at the moment PrepareWithSource will apply the same filter a
	// few lines later in the caller — not to be independently mockable.
	scheduleActive := scheduleActiveSegments(plan.Segments, time.Now(), sleepschedule.LocalTimezone())

	items := make([]dp1playlist.PlaylistItem, 0, len(plan.Segments))
	// active is items' own parallel slice: active[i] is whether items[i]'s
	// originating segment is schedule-active, so filteredOccurrence can
	// count past both filters without re-deriving per-segment activity
	// from an already-flattened item.
	active := make([]bool, 0, len(plan.Segments))
	// seen is this tick's own group keys, used below to prune
	// randomSeriesPicks down to groups the CURRENT plan still carries — the
	// mode-change delete a few lines down only fires for a key the plan
	// still has, so a group that stops appearing at all (the device moves
	// on to a different playlist/exhibition) was otherwise never removed,
	// growing this map for the life of the process (run-reviewer, round 1
	// on this fix, 2026-11-12).
	seen := make(map[dp1.SeriesGroupKey]struct{}, len(plan.Segments))
	for i, seg := range plan.Segments {
		if seg.Item != nil {
			items = append(items, *seg.Item)
			active = append(active, scheduleActive[i])
			continue
		}
		group := seg.Group
		seen[group.Key] = struct{}{}
		mode := dp1.ResolveSeriesPlaybackMode(group.DocumentMode, device)
		// A single/multi group has nothing distinct to pick between — a
		// fresh dp1.NarrowEditions call always collapses it to the
		// representative regardless of mode — so it must never enter
		// randomSeriesPicks at all: reusing, seeding, or memoizing a pick
		// for it risks replaying some OTHER edition whose Source happens to
		// coincide with the representative's or with whatever is on
		// screen, which NarrowEditions itself would never choose
		// (feralfile-bot F5, 2026-11-12).
		if mode != dp1.SeriesPlaybackRandom || !group.ArtworkModel.HasDistinctEditions() {
			delete(r.randomSeriesPicks, group.Key)
			narrowedNonRandom := dp1.NarrowEditions(mode, group.ArtworkModel, group.Representative, group.Editions)
			items = append(items, narrowedNonRandom...)
			for range narrowedNonRandom {
				active = append(active, scheduleActive[i])
			}
			continue
		}
		// Check what the player actually shows BEFORE any remembered pick:
		// a live cast commandrouter sent independently of this refresher
		// (a setSeriesPlayback recast of the same URL, or a setting recast
		// back into random) rolls its own edition through its own
		// dp1.CastPlan.Narrow call, which this memory never sees. Without
		// this, the first periodic resync after such a cast would silently
		// revert its choice back to whatever was remembered from before
		// (feralfile-bot F6, 2026-11-12). On an ordinary tick the player is
		// still showing exactly what was last remembered, so this only
		// reconfirms the existing pick (or leaves a not-yet-seeded one
		// matching) rather than changing anything.
		//
		// Only consulted when THIS group's own segment is itself
		// schedule-active: a future-scheduled group has no active slot in
		// displayed at all to compare against, so checking occurrence for
		// it is meaningless and must fall straight through to the
		// remembered-pick/fresh-roll path below, preserving whatever was
		// remembered rather than reading an unrelated, coincidentally-
		// positioned item as if it were this group's live pick
		// (feralfile-bot F7, 2026-11-13).
		//
		// occurrence is this group's own slot in the flattened list: items
		// already holds every earlier segment's own contribution, so its
		// current length — counting only the items both schedule-active
		// AND displayFilter admits, the same ones a content-policy
		// projection of this tick's cast would keep after displayAt
		// scheduling strips future segments — is exactly where this
		// group's narrowed output will land in displayed too — matching
		// displayed only at that one position, never anywhere else a
		// coincidental Source match could sit (feralfile-bot F7,
		// 2026-11-12; corrected to count past content-policy filtering,
		// feralfile-bot F7, 2026-11-13; corrected again to count past
		// displayAt scheduling, feralfile-bot F7, 2026-11-13).
		if scheduleActive[i] {
			occurrence := filteredOccurrence(items, active, displayFilter)
			if seeded, ok := editionAtOccurrence(fetchDisplayed(), occurrence, group.Editions); ok {
				// Written unconditionally, even when the source already
				// matched what was remembered: a conditional write here (only
				// on a source change) left mode/artworkModel stale on every
				// tick that merely reconfirmed the pick, so a later tick
				// consulting the remembered-pick branch below (display
				// unavailable) could still see a since-changed artworkModel as
				// unchanged and skip the invalidation that field exists for
				// (run-reviewer, round 1 on this fix, 2026-11-12).
				r.randomSeriesPicks[group.Key] = randomSeriesPick{mode: mode, artworkModel: group.ArtworkModel, source: seeded.Source}
				items = append(items, seeded)
				active = append(active, scheduleActive[i])
				continue
			}
		}
		// Nothing currently displayed for this group (player status
		// unavailable, this group isn't part of it, or this group's own
		// segment is itself schedule-inactive) — fall back to the
		// remembered pick, then to a fresh roll, exactly as before the
		// display check above existed.
		if pick, ok := r.randomSeriesPicks[group.Key]; ok && pick.mode == mode && pick.artworkModel == group.ArtworkModel {
			if fresh, found := editionBySource(group.Editions, pick.source); found {
				items = append(items, fresh)
				active = append(active, scheduleActive[i])
				continue
			}
			// The picked source is gone from this tick's editions — the
			// catalog's answer moved on. Falls through to a fresh pick
			// below, exactly like "that edition disappears" already did.
		}
		narrowed := dp1.NarrowEditions(mode, group.ArtworkModel, group.Representative, group.Editions)
		// Only remember a pick that is actually one of group.Editions:
		// NarrowEditions' representative-fallback case (len(editions)==0)
		// can return a representative whose own Source is not guaranteed
		// to be among Editions (see CastPlan.Full's doc, feralfile-bot F1)
		// — memoizing it would just miss every lookup next tick. Leaving
		// it unmemoized costs nothing: that case is already deterministic,
		// so re-deriving it fresh every tick returns the identical result
		// anyway.
		if len(narrowed) == 1 {
			if _, found := editionBySource(group.Editions, narrowed[0].Source); found {
				r.randomSeriesPicks[group.Key] = randomSeriesPick{mode: mode, artworkModel: group.ArtworkModel, source: narrowed[0].Source}
			}
		}
		items = append(items, narrowed...)
		for range narrowed {
			active = append(active, scheduleActive[i])
		}
	}
	for key := range r.randomSeriesPicks {
		if _, ok := seen[key]; !ok {
			delete(r.randomSeriesPicks, key)
		}
	}
	return items
}

// editionBySource returns the item in editions whose Source exactly
// matches source — the same no-normalization comparison commandrouter's
// findPlaylistItemBySource uses — so narrowStable always returns and
// re-stores the CURRENT tick's edition for a remembered pick, never a
// stale copy captured when the pick was first made (run-reviewer, round 3
// on this fix, 2026-11-12: storing the item itself let every field but
// Source — duration, license, contentRating, … — go stale for as long as
// the pick was held).
func editionBySource(editions []dp1playlist.PlaylistItem, source string) (dp1playlist.PlaylistItem, bool) {
	for _, edition := range editions {
		if edition.Source == source {
			return edition, true
		}
	}
	return dp1playlist.PlaylistItem{}, false
}

// playerStatusDisplayedItems returns ps's own record of what the player
// currently shows — Items when the reply carries it, else the inline
// Playlist's Items, else nil — for narrowStable's seed source. ps may be
// nil.
func playerStatusDisplayedItems(ps *status.PlayerStatus) []dp1playlist.PlaylistItem {
	if ps == nil {
		return nil
	}
	if ps.Items != nil {
		return *ps.Items
	}
	if ps.Playlist != nil {
		return ps.Playlist.Items
	}
	return nil
}

// currentlyDisplayedItems builds narrowStable's seed-source closure: ps's
// own items when already in hand, else one FetchPlayerStatus call made
// lazily (narrowStable's own memoization ensures at most once per
// narrowStable call, however many unseeded groups it finds) — the case
// processPlayingPlaylist reaches once a scheduler source is already
// committed and therefore skips its own player-status fetch (see that
// function's schedulerSource.IsZero() branch).
func (r *refresher) currentlyDisplayedItems(ps *status.PlayerStatus) func() []dp1playlist.PlaylistItem {
	return func() []dp1playlist.PlaylistItem {
		if ps != nil {
			return playerStatusDisplayedItems(ps)
		}
		fetched, err := r.statusPoller.FetchPlayerStatus(r.context)
		if err != nil {
			return nil
		}
		return playerStatusDisplayedItems(fetched)
	}
}

// contentDisplayFilter builds narrowStable's occurrence predicate (see that
// method's doc) from a resolved content context. nil — "count every item,
// nothing is filtered" — when contextUnknown (processPlayingPlaylist's own
// projection guard skips ProjectLocked in exactly that case too, so
// occurrence must agree: nothing will be removed), or when there is no
// content-policy store to ask at all.
//
// policy is read via Snapshot, not CurrentLocked: this runs ahead of, and
// without, the policy lock processPlayingPlaylist takes only after
// resolution (see that function's own doc on why), and Snapshot is the
// store's established lock-free read for exactly that kind of caller — see
// contentpolicy.Store's snap field doc. A policy update racing this read
// against the later ProjectLocked call is the same bounded, accepted gap
// every other lock-free Snapshot read in this codebase already carries,
// and strictly narrows the prior gap: before this existed, occurrence was
// never corrected for filtering AT ALL, on every tick, not just across a
// policy-change race.
func (r *refresher) contentDisplayFilter(contentContext string, contextUnknown bool) func(dp1playlist.PlaylistItem) bool {
	if r.contentPolicy == nil || contextUnknown {
		return nil
	}
	origin, err := contentpolicy.NormalizeContext(contentContext)
	if err != nil {
		return nil
	}
	policy := r.contentPolicy.Snapshot()
	return func(item dp1playlist.PlaylistItem) bool {
		return policy.Allows(item, origin)
	}
}

// editionAtOccurrence reports whether the item at position index in
// displayed — this group's own slot in the flattened list, not any
// position — has the same Source as one of editions, and returns that
// edition (not the displayed copy, whose other fields may be a player-side
// projection) so narrowStable can seed a fresh group's pick from what a
// live cast already put on screen instead of rolling a new one.
//
// Matching by occurrence rather than scanning displayed for any Source
// match anywhere is deliberate: a playlist can carry the same edition's
// Source more than once — an explicit standalone item alongside a series
// that also contains that edition, or two groups sharing one — and a
// whole-list scan would mistake that OTHER item's position for this
// group's own live pick (feralfile-bot F7, 2026-11-12: an explicit item
// ahead of a multi_unique series containing the same edition was read as
// the group's pick, discarding the live random choice actually shown at
// the group's own, later position). index out of range, or no match at
// that position, returns false — exactly like "nothing displayed for this
// occurrence."
//
// Accepted risk: this assumes displayed was itself flattened with the
// SAME per-segment item counts this tick's plan computes for every
// segment ahead of index — true whenever nothing upstream changed, but
// not guaranteed. An EARLIER full-mode, distinct-editions group
// contributes its WHOLE editions slice (dp1.NarrowEditions's full case),
// and narrowedPlaylistFromURL re-resolves that group fresh from the
// catalog on every tick (no caching) — so its contributed count can
// change between the live display and this tick purely from routine
// catalog churn (editions added/removed) or that series item's own
// document-mode changing, not only a device-level SeriesPlaybackMode
// override (run-reviewer, round 3 on this fix, 2026-11-12: narrower than
// "a mode change" first suggested). When that drift happens, index can
// point at the wrong slot for a LATER random-mode group; the match still
// requires that wrong slot's Source to coincide with one of THIS group's
// OWN editions before anything is adopted, so a misfire can only replay
// a different edition of the same group, never a foreign item.
func editionAtOccurrence(displayed []dp1playlist.PlaylistItem, index int, editions []dp1playlist.PlaylistItem) (dp1playlist.PlaylistItem, bool) {
	if index < 0 || index >= len(displayed) {
		return dp1playlist.PlaylistItem{}, false
	}
	return editionBySource(editions, displayed[index].Source)
}

// filteredOccurrence counts how many of items are BOTH schedule-active
// (active[i], see scheduleActiveSegments) AND admitted by displayFilter —
// nil displayFilter admits all of them. active must be the same length as
// items (narrowStable keeps the two in lockstep). See narrowStable's doc
// for why this, and not len(items), is this group's own slot in displayed.
func filteredOccurrence(items []dp1playlist.PlaylistItem, active []bool, displayFilter func(dp1playlist.PlaylistItem) bool) int {
	n := 0
	for i, item := range items {
		if !active[i] {
			continue
		}
		if displayFilter != nil && !displayFilter(item) {
			continue
		}
		n++
	}
	return n
}

// scheduleActiveSegments reports, for each segment in plan order, whether
// that segment's own items would survive displayAt scheduling — the same
// filter playlistschedule.Scheduler.PrepareWithSource applies to the
// flattened playlist narrowStable returns, which this package's own
// resync never otherwise sees applied until after narrowStable has already
// built its occurrence counts (feralfile-bot F7, 2026-11-13). All of a
// group's Editions (and its Representative) share the single series item's
// own DisplayAt — dp1/series.go's expansion clears then re-applies the
// parent item's DisplayAt to every edition — so one representative value
// per segment decides the whole segment's activity; no edition needs to be
// picked first. now/loc mirror exactly what the scheduler itself resolves
// them to in production (clock.Now(), and a nil locFn defaulting to
// sleepschedule.LocalTimezone — see playlistschedule.New's doc): this
// package has no way to read the scheduler's own configured locFn back
// out, so it recomputes the one value production never overrides rather
// than risk silently drifting from the scheduler's actual answer. A
// synthetic index-keyed ID is enough to map
// displayat.ComputeActiveSet's filtered subset back to segment positions,
// since ComputeActiveSet otherwise leaves every field (including ID)
// untouched.
func scheduleActiveSegments(segments []dp1.CastSegment, now time.Time, loc *time.Location) []bool {
	preview := make([]dp1playlist.PlaylistItem, len(segments))
	for i, seg := range segments {
		id := strconv.Itoa(i)
		switch {
		case seg.Item != nil:
			preview[i] = dp1playlist.PlaylistItem{ID: id, DisplayAt: seg.Item.DisplayAt}
		case seg.Group != nil:
			preview[i] = dp1playlist.PlaylistItem{ID: id, DisplayAt: seg.Group.Representative.DisplayAt}
		default:
			preview[i] = dp1playlist.PlaylistItem{ID: id}
		}
	}
	active := make([]bool, len(segments))
	for _, item := range displayat.ComputeActiveSet(&dp1playlist.Playlist{Items: preview}, now, loc) {
		if idx, err := strconv.Atoi(item.ID); err == nil && idx >= 0 && idx < len(active) {
			active[idx] = true
		}
	}
	return active
}

// loadCachedPlaylistForURL mirrors commandrouter's handler.loadCachedPlaylistForURL
// (see its doc, including why the returned playlist carries NO signature
// verdict: the saved body is a typed, hydrated re-marshal, not the signed
// bytes — feral-file/ffos-user#307): the raw body Service.CachedPlaylistForURL
// returns was already fully resolved back when downloadPlaylist originally
// saved it, so unmarshaling it here needs no further DP-1 resolution.
// Returns an error whenever there is nothing to fall back to (offline
// caching disabled, url was never downloaded, or the downloaded copy has
// since been cleared), so the caller can report the original live
// resolution error instead.
//
// If the cached copy carries series-group identity (feral-file/ffos-user
// #453), the returned playlist's Items are narrowed by the device's current
// series-playback override first — see commandrouter's identical fix on its
// own loadCachedPlaylistForURL for the full rationale; the two are kept in
// step by hand (this package deliberately does not depend on commandrouter
// — see AGENTS.md's service-boundary guidance).
//
// currentlyDisplayed is narrowStable's seed source — see that method's
// doc. May be nil. displayFilter is narrowStable's occurrence predicate —
// see that method's doc. May be nil.
func (r *refresher) loadCachedPlaylistForURL(url string, currentlyDisplayed func() []dp1playlist.PlaylistItem, displayFilter func(dp1playlist.PlaylistItem) bool) (*dp1.Playlist, error) {
	if r.offlineCache == nil {
		return nil, fmt.Errorf("offline cache: disabled, no cached fallback for %s", url)
	}
	raw, segments, err := r.offlineCache.CachedPlaylistForURL(url)
	if err != nil {
		return nil, fmt.Errorf("offline cache: no cached playlist for %s: %w", url, err)
	}
	if err := contentrating.ValidatePlaylistFragment(raw); err != nil {
		return nil, fmt.Errorf("playlistInvalid: offline cache content rating for %s: %w", url, err)
	}
	var playlist *dp1.Playlist
	if err := r.json.Unmarshal(raw, &playlist); err != nil {
		return nil, fmt.Errorf("offline cache: parse cached playlist for %s: %w", url, err)
	}
	if len(segments) > 0 {
		plan := dp1.CastPlan{Segments: fromOfflineCacheSegments(segments)}
		deviceMode := r.currentSeriesPlaybackMode()
		playlist.Items = r.narrowStable(plan, deviceMode, currentlyDisplayed, displayFilter)
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
