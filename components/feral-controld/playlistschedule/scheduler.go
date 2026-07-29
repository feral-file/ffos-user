// Package playlistschedule filters DP-1 playlists by displayAt and advances
// them on a timer. Controld owns this path so the player can keep playing a
// normal (already filtered) playlist with no scheduling awareness.
package playlistschedule

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"time"

	"github.com/display-protocol/dp1-go/displayat"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const displayAtMaxTick = 60 * time.Second

// pushRetryBaseDelay/pushRetryMaxDelay bound the backoff used to resend a
// cutover push that CDP rejected or failed to deliver. Capped at
// displayAtMaxTick's granularity so a broken link cannot be hammered, while
// still guaranteeing the cutover is retried instead of sticking silently.
const (
	pushRetryBaseDelay   = 5 * time.Second
	pushRetryMaxDelay    = 60 * time.Second
	pushRetryMaxAttempts = 5
)

// errStopped marks a push dropped because the scheduler is shutting down. It
// must stay distinguishable from a genuine push failure: a stopped push must
// never arm a retry, or Stop would spawn the very goroutine it is retiring.
var errStopped = errors.New("playlist scheduler stopped")

//go:generate mockgen -source=scheduler.go -destination=../mocks/playlistschedule.go -package=mocks -mock_names=Scheduler=MockPlaylistScheduler

// Scheduler caches the full displayAt playlist, exposes the active set for
// the player, and re-pushes when the next displayAt threshold is crossed.
type Scheduler interface {
	// Prepare caches a displayAt playlist and returns the active set for the
	// player. Playlists without item-level displayAt clear any prior schedule
	// and are returned unchanged.
	Prepare(playlist *dp1.Playlist) *dp1.Playlist
	// PrepareWithSource is Prepare plus the original refreshable source identity.
	// URL casts must keep playlistUrl on scheduler-owned pushes so the player
	// status remains refreshable and restart validation can tie persisted cache
	// ownership back to a stable source.
	PrepareWithSource(playlist *dp1.Playlist, source Source) *dp1.Playlist
	// RecomputeNow re-filters the cached playlist and force-casts it to the
	// player (now_display, not refresh). Used on timer fire, wake, CDP
	// reconnect, and after a transient network refresh failure that still has
	// a usable cache. Force-cast is required so a displayAt cutover is not
	// deferred until the current artwork finishes its duration. No-op when
	// nothing is cached, or when a restart-restored source has not yet been
	// refetched into an in-memory playlist.
	RecomputeNow(ctx context.Context)
	// ResumePersisted arms timers and force-casts from the in-memory full
	// playlist. It is used only after the refresher has reconstructed scheduler
	// state from a fetched source or after a transient refresh failure while a
	// non-pending in-memory cache already exists.
	ResumePersisted(ctx context.Context)
	// RestoredPending reports whether a durable source was loaded at startup
	// and still needs to be fetched into an in-memory full playlist before
	// normal recompute paths may use it.
	RestoredPending() bool
	// SourceMatches reports whether the current scheduler cache belongs to the
	// supplied refreshable source. Callers use it before falling back to cached
	// displayAt state after a transient refresh failure.
	SourceMatches(source Source) bool
	// Source returns the refreshable identity owned by the scheduler. While it
	// is non-zero, refreshers must resolve this source instead of trusting the
	// player's filtered (and potentially stale) status. It also lets startup
	// reconstruct a persisted schedule before the player has crossed its first
	// displayAt boundary.
	Source() Source
	// Clear drops the cached displayAt playlist and cancels the transition
	// timer under the player-push lock so an in-flight RecomputeNow cannot
	// start a new push after the clear.
	Clear()
	// ClearThenWithPlayerPush clears the cache and runs fn while still holding
	// the player-push lock. Use it only when a caller can prove the paired
	// player write replaces scheduler-owned playback. If fn returns false, the
	// previous cache is restored before releasing the lock because the player
	// did not accept the transition.
	ClearThenWithPlayerPush(fn func() bool)
	// WithPlayerPush serializes CDP playlist updates against timer/wake
	// recomputes. Cast and refresh paths must wrap their displayPlaylist CDP
	// send so a stale RecomputeNow cannot overwrite a newer cast mid-flight.
	WithPlayerPush(fn func())
	// AuthorityToken changes whenever scheduler-owned playlist authority
	// changes. Refreshers snapshot it before slow URL/dynamic resolution and
	// re-check under WithPlayerPush so stale refresh results cannot overwrite a
	// newer scheduler-owned cast.
	AuthorityToken() uint64
	// Commit persists the scheduler state currently accepted by the player.
	// Cast paths call it only after CDP returns ok so durable restart recovery
	// never leads the player-visible playlist.
	Commit()
	Snapshot() Snapshot
	Restore(Snapshot)
	// HasCache reports whether a displayAt playlist is currently cached.
	HasCache() bool
	// Stop latches shutdown and is not reversible: afterwards no recompute pass
	// writes to the player and no new transition timer or push retry is armed.
	// It returns without waiting for a CDP send that is already in flight; see
	// the scheduler's stopped field for that trade-off.
	Stop()
}

type Snapshot struct {
	full            *dp1.Playlist
	lastActive      []dp1playlist.PlaylistItem
	restoredPending bool
	source          Source
}

// LocationFunc returns the device-local timezone used to resolve timezone-less
// displayAt values. Injected so tests can pin a zone without depending on
// /etc/localtime.
type LocationFunc func() *time.Location

type scheduler struct {
	mu sync.Mutex
	// pushMu serializes player playlist CDP writes from RecomputeNow against
	// cast/refresh sends wrapped in WithPlayerPush. Cache mutations under mu
	// stay unlocked during CDP so Prepare latency is not gated on Chromium.
	pushMu sync.Mutex

	ctx    context.Context
	cdp    cdp.CDP
	clock  wrapper.Clock
	locFn  LocationFunc
	logger *zap.Logger
	store  Store

	// full is the last displayAt playlist (complete item list). The player
	// only ever receives the filtered active set derived from this cache.
	full *dp1.Playlist
	// lastActive tracks the active set last sent or about to be sent by the
	// owning cast path. Bounded timer rechecks use it to absorb wall-clock jumps
	// without force-casting the same playlist every minute.
	lastActive []dp1playlist.PlaylistItem
	// generation increments on every Prepare/clear so a RecomputeNow that
	// snapshotted an older cache can drop its push after a newer cast wins.
	generation uint64
	// restoredPending marks a durable source loaded at startup but not yet
	// refreshed into a full in-memory playlist. CDP reconnect/wake/timer paths
	// must not push until the refresher fetches the source and prepares a fresh
	// playlist.
	restoredPending bool
	// source tracks the refreshable identity for scheduler-owned pushes. The
	// full cached playlist supplies future items; source keeps player status
	// tied to the controller/refresher URL that can be re-resolved later.
	source Source

	// cancelTimer cancels the in-flight next-displayAt wait. Controld must not
	// keep a stale timer after a new cast or after Stop — otherwise a late fire
	// would push an obsolete active set over the current playlist.
	cancelTimer context.CancelFunc

	// pushRetryCancel cancels an in-flight cutover-push retry wait. This is
	// independent of cancelTimer: armTimerLocked only arms for a *future*
	// displayAt boundary, so once the last boundary in the cache has already
	// passed there is nothing left for it to re-arm if the cutover push then
	// fails or is rejected. pushRetry exists specifically to resend that
	// already-due cutover instead of leaving the player stuck on the
	// pre-boundary active set until an unrelated wake/reconnect/Prepare.
	pushRetryCancel context.CancelFunc
	// pushRetryAttempt counts consecutive failed/rejected cutover pushes since
	// the last success, Prepare, Clear, or Restore. Drives capped exponential
	// backoff and a bounded attempt ceiling.
	pushRetryAttempt int

	// stopped latches at Stop and gates player writes during shutdown.
	// Canceling the timer contexts is not enough on its own: a timer or retry
	// goroutine that already returned from SleepContext observes no context
	// afterwards, so it must re-read this flag under mu — both at the top of
	// recompute and again immediately before the CDP send.
	//
	// Upheld: once Stop has latched, no further recompute pass writes to the
	// player, no new timer or retry goroutine is armed, and a goroutine parked
	// on pushMu drops its push on wake instead of sending.
	//
	// Deliberately NOT upheld: a push that already passed the pre-send gate may
	// still begin its CDP send concurrently with Stop. Linearizing "Stop
	// returned" against "send started" would require holding a lock across
	// cdp.Send (bounded at 15s) while main.go force-exits after
	// SHUTDOWN_TIMEOUT (2s) — blocking Stop there would convert one slow player
	// write into a failed shutdown. That racing push is a correct cutover
	// computed from a valid cache, so letting it land is the cheaper trade.
	stopped bool
}

// New builds a scheduler. locFn may be nil; LocalTimezone is used in that case.
func New(
	ctx context.Context,
	cdpClient cdp.CDP,
	clock wrapper.Clock,
	locFn LocationFunc,
	logger *zap.Logger,
) Scheduler {
	return NewWithStore(ctx, cdpClient, clock, locFn, nil, logger)
}

// NewWithStore builds a scheduler that restores the last refreshable scheduled
// source from durable state. Used by production so a controld-only restart can
// refetch URL/dynamic displayAt schedules before future cutovers resume.
func NewWithStore(
	ctx context.Context,
	cdpClient cdp.CDP,
	clock wrapper.Clock,
	locFn LocationFunc,
	store Store,
	logger *zap.Logger,
) Scheduler {
	if locFn == nil {
		locFn = sleepschedule.LocalTimezone
	}
	s := &scheduler{
		ctx:    ctx,
		cdp:    cdpClient,
		clock:  clock,
		locFn:  locFn,
		logger: logger,
		store:  store,
	}
	s.restorePersisted()
	return s
}

func (s *scheduler) HasCache() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.full != nil
}

func (s *scheduler) RestoredPending() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.restoredPending
}

func (s *scheduler) SourceMatches(source Source) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.source.Matches(source)
}

func (s *scheduler) Source() Source {
	s.mu.Lock()
	defer s.mu.Unlock()
	return snapshotSource(s.source)
}

func (s *scheduler) WithPlayerPush(fn func()) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	fn()
}

func (s *scheduler) AuthorityToken() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.generation
}

func (s *scheduler) Commit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.persistLocked()
}

func (s *scheduler) Clear() {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	s.mu.Lock()
	s.clearLocked()
	s.mu.Unlock()
}

// ClearThenWithPlayerPush clears under pushMu then runs fn before releasing,
// so a paired replacement CDP send cannot race an in-flight RecomputeNow push.
func (s *scheduler) ClearThenWithPlayerPush(fn func() bool) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	s.mu.Lock()
	snapshot := s.snapshotLocked()
	s.clearLocked()
	s.mu.Unlock()
	if !fn() {
		s.mu.Lock()
		s.restoreLocked(snapshot)
		s.mu.Unlock()
		return
	}
	s.Commit()
}

func (s *scheduler) Snapshot() Snapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.snapshotLocked()
}

func (s *scheduler) Restore(snapshot Snapshot) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.restoreLocked(snapshot)
}

func (s *scheduler) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopped = true
	s.cancelTimerLocked()
	s.cancelPushRetryLocked()
}

func (s *scheduler) Prepare(playlist *dp1.Playlist) *dp1.Playlist {
	return s.PrepareWithSource(playlist, Source{})
}

func (s *scheduler) PrepareWithSource(playlist *dp1.Playlist, source Source) *dp1.Playlist {
	if playlist == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range playlist.Items {
		if item.DisplayAt != nil && !displayat.Parse(*item.DisplayAt, s.locFn()).IsValid() {
			// Do not let an invalid scheduling value quietly remove an item from
			// playback. Returning nil makes the cast/refresh fail before CDP sees
			// a filtered (possibly empty) playlist.
			s.logger.Warn("Rejecting playlist with invalid displayAt",
				zap.String("itemID", item.ID), zap.String("displayAt", *item.DisplayAt))
			return nil
		}
	}

	if !HasDisplayAtSchedule(playlist) {
		// A non-scheduled cast must cancel any prior Daily timer; otherwise the
		// previous playlist's next displayAt would overwrite the new cast.
		s.clearLocked()
		return playlist
	}

	s.generation++
	s.full = clonePlaylist(playlist)
	s.source = source
	s.restoredPending = false
	// A newly prepared schedule starts with a clean retry slate: a pending
	// retry from the previous schedule's failed cutover must not fire against
	// this one, and this cast must not inherit the previous schedule's
	// backoff/give-up state.
	s.cancelPushRetryLocked()
	s.pushRetryAttempt = 0
	active := s.activeLocked()
	s.lastActive = cloneItems(active.Items)
	s.armTimerLocked()

	s.logger.Info("Prepared displayAt active set",
		zap.Int("fullItems", len(playlist.Items)),
		zap.Int("activeItems", len(active.Items)))
	return active
}

func (s *scheduler) RecomputeNow(ctx context.Context) {
	s.recompute(ctx, true)
}

func (s *scheduler) ResumePersisted(ctx context.Context) {
	if s.resumePersisted(ctx) {
		// A transient source outage must not restart the current artwork just
		// because the cached active set is unchanged. Timers, wakes, and CDP
		// reconnects still use RecomputeNow's force-cast semantics.
		s.recompute(ctx, false)
	}
}

func (s *scheduler) resumePersisted(ctx context.Context) bool {
	s.mu.Lock()
	if s.full == nil {
		s.mu.Unlock()
		return false
	}
	if s.restoredPending {
		s.restoredPending = false
		s.generation++
		s.armTimerLocked()
	}
	s.mu.Unlock()

	return true
}

func (s *scheduler) recompute(ctx context.Context, force bool) {
	// Hold pushMu for the whole read+push loop so a cast/refresh
	// WithPlayerPush cannot interleave mid-update. If Prepare bumps generation
	// during CDP Send, loop once more and push the newer active set before
	// releasing — avoiding a stale playlist stuck on screen.
	s.pushMu.Lock()
	defer s.pushMu.Unlock()

	for {
		s.mu.Lock()
		if s.stopped || s.full == nil || s.restoredPending {
			s.mu.Unlock()
			return
		}
		gen := s.generation
		active := s.activeLocked()
		source := snapshotSource(s.source)
		s.armTimerLocked()
		if len(active.Items) == 0 {
			// The player rejects an empty displayPlaylist. Keep the complete
			// schedule and its timer so the first future cohort can take over,
			// but never turn an empty active set into a retry storm.
			s.lastActive = cloneItems(active.Items)
			s.mu.Unlock()
			s.logger.Debug("Skipping empty displayAt active set")
			return
		}
		if !force && reflect.DeepEqual(active.Items, s.lastActive) {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		if err := s.push(ctx, active, source); err != nil {
			if errors.Is(err, errStopped) {
				s.logger.Debug("Dropped displayAt push: scheduler stopped")
				return
			}
			s.logger.Warn("Failed to push recomputed displayAt playlist", zap.Error(err))
			s.mu.Lock()
			s.armPushRetryLocked()
			s.mu.Unlock()
			return
		}
		s.mu.Lock()
		s.lastActive = cloneItems(active.Items)
		// A successful push clears any retry armed by a prior failure so it
		// cannot resend a now-superseded active set later.
		s.pushRetryAttempt = 0
		s.cancelPushRetryLocked()
		s.mu.Unlock()
		s.logger.Info("Pushed recomputed displayAt active set",
			zap.Int("activeItems", len(active.Items)))

		s.mu.Lock()
		changed := gen != s.generation
		s.mu.Unlock()
		if !changed {
			return
		}
		s.logger.Debug("displayAt cache changed during push; pushing again")
	}
}

func (s *scheduler) clearLocked() {
	s.cancelTimerLocked()
	s.cancelPushRetryLocked()
	s.pushRetryAttempt = 0
	s.generation++
	s.full = nil
	s.lastActive = nil
	s.restoredPending = false
	s.source = Source{}
}

func (s *scheduler) cancelTimerLocked() {
	if s.cancelTimer != nil {
		s.cancelTimer()
		s.cancelTimer = nil
	}
}

func (s *scheduler) cancelPushRetryLocked() {
	if s.pushRetryCancel != nil {
		s.pushRetryCancel()
		s.pushRetryCancel = nil
	}
}

func (s *scheduler) snapshotLocked() Snapshot {
	return Snapshot{
		full:            clonePlaylist(s.full),
		lastActive:      cloneItems(s.lastActive),
		restoredPending: s.restoredPending,
		source:          snapshotSource(s.source),
	}
}

func (s *scheduler) restoreLocked(snapshot Snapshot) {
	if s.cancelTimer != nil {
		s.cancelTimer()
		s.cancelTimer = nil
	}
	s.cancelPushRetryLocked()
	s.pushRetryAttempt = 0
	s.full = clonePlaylist(snapshot.full)
	s.lastActive = cloneItems(snapshot.lastActive)
	s.restoredPending = snapshot.restoredPending
	s.source = snapshotSource(snapshot.source)
	s.generation++
	if s.full != nil && !s.restoredPending {
		s.armTimerLocked()
	}
	if s.restoredPending && s.full == nil && !s.source.IsZero() {
		s.persistSourceLocked()
		return
	}
	s.persistLocked()
}

func (s *scheduler) restorePersisted() {
	if s.store == nil {
		return
	}
	source, err := s.store.Load()
	if err != nil {
		s.logger.Warn("Failed to load persisted displayAt source", zap.Error(err))
		return
	}
	if source == nil || source.IsZero() {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.source = snapshotSource(*source)
	s.restoredPending = true
	s.logger.Info("Restored persisted displayAt source")
}

func (s *scheduler) persistLocked() {
	if s.store == nil {
		return
	}
	var err error
	if s.full == nil {
		err = s.store.Clear()
	} else {
		err = s.store.Save(snapshotSource(s.source))
	}
	if err != nil {
		s.logger.Warn("Failed to persist displayAt playlist cache", zap.Error(err))
	}
}

func (s *scheduler) persistSourceLocked() {
	if s.store == nil {
		return
	}
	if err := s.store.Save(snapshotSource(s.source)); err != nil {
		s.logger.Warn("Failed to persist displayAt source cache", zap.Error(err))
	}
}

func (s *scheduler) activeLocked() *dp1.Playlist {
	now := s.clock.Now()
	loc := s.locFn()
	items := computeActiveSet(&s.full.Playlist, now, loc)
	out := clonePlaylist(s.full)
	out.Items = items
	return out
}

// armTimerLocked schedules one wake for the next future displayAt. The wait uses
// a cancelable child context so Prepare/Stop can replace or drop it without
// leaving a goroutine that still pushes after the playlist changed.
func (s *scheduler) armTimerLocked() {
	if s.cancelTimer != nil {
		s.cancelTimer()
		s.cancelTimer = nil
	}
	// Shutdown must not re-arm: recompute arms before it pushes, so a recompute
	// racing Stop could otherwise leave a live goroutine behind Stop's cancel.
	if s.stopped || s.full == nil {
		return
	}

	now := s.clock.Now()
	next := nextDisplayAt(&s.full.Playlist, now, s.locFn())
	if next == nil {
		s.logger.Debug("No future displayAt; holding current active set")
		return
	}

	wait := next.Sub(now)
	if wait < 0 {
		wait = 0
	}
	if wait > displayAtMaxTick {
		wait = displayAtMaxTick
	}

	timerCtx, cancel := context.WithCancel(s.ctx)
	s.cancelTimer = cancel
	fireAt := *next

	s.logger.Info("Armed displayAt transition timer",
		zap.Time("nextDisplayAt", fireAt),
		zap.Duration("wait", wait))

	go s.waitAndFire(timerCtx, wait)
}

func (s *scheduler) waitAndFire(timerCtx context.Context, wait time.Duration) {
	if err := s.clock.SleepContext(timerCtx, wait); err != nil {
		return
	}
	// Recompute from cache (not from the player): the player only holds the
	// previous active set, so archive/future items must come from s.full.
	s.recompute(s.ctx, false)
}

// armPushRetryLocked schedules a resend of the cutover push after it failed
// or was rejected by the player. This must exist separately from
// armTimerLocked: that function only arms a wait for a *future* displayAt
// boundary, so once the schedule's last boundary has already passed (the
// exact moment a cutover push is being attempted), armTimerLocked installs no
// timer at all. Without an independent retry, a failed final-boundary push
// would leave the player on the pre-cutover active set until an unrelated
// wake, CDP reconnect, or new Prepare happened to trigger a recompute.
// Bounded exponential backoff avoids hammering CDP while it or the player is
// unhealthy; giving up after pushRetryMaxAttempts still leaves those other
// triggers as a backstop.
func (s *scheduler) armPushRetryLocked() {
	s.cancelPushRetryLocked()
	if s.stopped {
		return
	}
	s.pushRetryAttempt++
	if s.pushRetryAttempt > pushRetryMaxAttempts {
		s.logger.Error("Giving up on displayAt cutover push retry; relying on next wake/reconnect/cast",
			zap.Int("attempts", s.pushRetryAttempt-1))
		return
	}

	wait := pushRetryBaseDelay << (s.pushRetryAttempt - 1)
	if wait <= 0 || wait > pushRetryMaxDelay {
		wait = pushRetryMaxDelay
	}

	timerCtx, cancel := context.WithCancel(s.ctx)
	s.pushRetryCancel = cancel

	s.logger.Warn("Scheduling displayAt cutover push retry",
		zap.Int("attempt", s.pushRetryAttempt), zap.Duration("wait", wait))

	go s.waitAndRetryPush(timerCtx, wait)
}

// waitAndRetryPush fires the retry after wait elapses. Like waitAndFire, it
// recomputes against s.ctx (the scheduler's own lifetime), not whatever ctx
// happened to trigger the failed push — that caller may have already
// returned (e.g. a request-scoped RecomputeNow), and this retry can fire long
// after that call completed.
func (s *scheduler) waitAndRetryPush(timerCtx context.Context, wait time.Duration) {
	if err := s.clock.SleepContext(timerCtx, wait); err != nil {
		return // canceled: success, Prepare, Clear, Restore, or Stop already handled it
	}
	s.recompute(s.ctx, true) // force: resend even if the active set looks unchanged from lastActive
}

func (s *scheduler) push(ctx context.Context, playlist *dp1.Playlist, source Source) error {
	if s.cdp == nil {
		return fmt.Errorf("cdp not configured")
	}
	if !s.cdp.Initialized() {
		return fmt.Errorf("cdp not connected")
	}

	// Force cast via now_display — never refresh:true. Player refreshPlaylist
	// defers when the current item is absent from the new list (typical
	// displayAt day/slot swap), which would miss the wall-clock threshold.
	// URL/dynamic playlist-refresher keeps refresh:true on its own path.
	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"intent": map[string]interface{}{
				"action": "now_display",
			},
			"dp1_call": playlist,
		},
	}
	if source.PlaylistURL != "" {
		command.Arguments["playlistUrl"] = source.PlaylistURL
	}
	payload, err := command.JSON()
	if err != nil {
		return fmt.Errorf("marshal displayAt playlist command: %w", err)
	}

	// Last gate before the write leaves controld. A timer or retry goroutine
	// may have been released from SleepContext just before Stop latched, in
	// which case it must not reach the player.
	s.mu.Lock()
	stopped := s.stopped
	s.mu.Unlock()
	if stopped {
		return errStopped
	}

	result, err := s.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(payload)),
	})
	if err != nil {
		return fmt.Errorf("send displayAt playlist to CDP: %w", err)
	}
	if !playerresponse.OK(result) {
		return fmt.Errorf("player rejected displayAt playlist")
	}
	return nil
}

// HasDisplayAtSchedule reports whether a playlist carries at least one timed
// item. Item-level displayAt is the scheduling contract.
func HasDisplayAtSchedule(p *dp1.Playlist) bool {
	if p == nil {
		return false
	}
	for _, item := range p.Items {
		if item.DisplayAt != nil {
			return true
		}
	}
	return false
}

func computeActiveSet(p *dp1playlist.Playlist, now time.Time, loc *time.Location) []dp1playlist.PlaylistItem {
	return displayat.ComputeActiveSet(p, now, loc)
}

func nextDisplayAt(p *dp1playlist.Playlist, now time.Time, loc *time.Location) *time.Time {
	return displayat.NextDisplayAt(p, now, loc)
}

func clonePlaylist(p *dp1.Playlist) *dp1.Playlist {
	if p == nil {
		return nil
	}
	out := *p
	if p.Items != nil {
		out.Items = append([]dp1playlist.PlaylistItem(nil), p.Items...)
	}
	if p.DynamicQueries != nil {
		out.DynamicQueries = append([]dp1.LegacyDynamicQuery(nil), p.DynamicQueries...)
	}
	return &out
}

func cloneItems(items []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	if items == nil {
		return nil
	}
	return append(make([]dp1playlist.PlaylistItem, 0, len(items)), items...)
}
