// Package playlistschedule filters DP-1 playlists by displayAt and advances
// them on a timer. Controld owns this path so the player can keep playing a
// normal (already filtered) playlist with no scheduling awareness.
package playlistschedule

import (
	"context"
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
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const displayAtMaxTick = 60 * time.Second

//go:generate mockgen -source=scheduler.go -destination=../mocks/playlistschedule.go -package=mocks -mock_names=Scheduler=MockPlaylistScheduler

// Scheduler caches the full displayAt playlist, exposes the active set for
// the player, and re-pushes when the next displayAt threshold is crossed.
type Scheduler interface {
	// Prepare caches a displayAt playlist and returns the active set for the
	// player. Playlists without schedule.byDisplayAt plus item-level displayAt
	// clear any prior schedule and are returned unchanged.
	Prepare(playlist *dp1.Playlist) *dp1.Playlist
	// RecomputeNow re-filters the cached playlist and force-casts it to the
	// player (now_display, not refresh). Used on timer fire, wake, CDP
	// reconnect, and after a transient network refresh failure that still has
	// a usable cache. Force-cast is required so a displayAt cutover is not
	// deferred until the current artwork finishes its duration. No-op when
	// nothing is cached, or when a restart-restored cache has not yet been
	// validated against current player status.
	RecomputeNow(ctx context.Context)
	// ResumePersisted validates that player status still describes a scheduled
	// displayPlaylist command after a controld restart, then arms timers and
	// force-casts from the persisted full playlist.
	ResumePersisted(ctx context.Context)
	// RestoredPending reports whether the in-memory cache came from durable
	// state and still needs current player-status validation before normal
	// recompute paths may use it.
	RestoredPending() bool
	// Clear drops the cached displayAt playlist and cancels the transition
	// timer under the player-push lock so an in-flight RecomputeNow cannot
	// start a new push after the clear. Prefer ClearThenWithPlayerPush when
	// the clear must be paired with a CDP send (displayDefaultPlaylist).
	Clear()
	// ClearThenWithPlayerPush clears the cache and runs fn while still holding
	// the player-push lock. Used for displayDefaultPlaylist so a stale
	// RecomputeNow cannot overwrite the default player state after Clear. If
	// fn returns false, the previous cache is restored before releasing the
	// lock because the player did not accept the default transition.
	ClearThenWithPlayerPush(fn func() bool)
	// WithPlayerPush serializes CDP playlist updates against timer/wake
	// recomputes. Cast and refresh paths must wrap their displayPlaylist CDP
	// send so a stale RecomputeNow cannot overwrite a newer cast mid-flight.
	WithPlayerPush(fn func())
	// AuthorityToken changes whenever scheduler-owned playlist authority
	// changes. Refreshers snapshot it before slow URL/dynamic resolution and
	// re-check under WithPlayerPush so stale refresh results cannot overwrite a
	// newer cast or default playlist.
	AuthorityToken() uint64
	// Commit persists the scheduler state currently accepted by the player.
	// Cast paths call it only after CDP returns ok so durable restart recovery
	// never leads the player-visible playlist.
	Commit()
	Snapshot() Snapshot
	Restore(Snapshot)
	// HasCache reports whether a displayAt playlist is currently cached.
	HasCache() bool
	Stop()
}

type Snapshot struct {
	full            *dp1.Playlist
	lastActive      []dp1playlist.PlaylistItem
	restoredPending bool
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
	// restoredPending marks a durable cache loaded at startup but not yet
	// validated against the player's current displayPlaylist status. CDP
	// reconnect/wake/timer paths must not resurrect it until the refresher sees
	// that the player is still on a scheduled playlist command.
	restoredPending bool

	// cancelTimer cancels the in-flight next-displayAt wait. Controld must not
	// keep a stale timer after a new cast or after Stop — otherwise a late fire
	// would push an obsolete active set over the current playlist.
	cancelTimer context.CancelFunc
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

// NewWithStore builds a scheduler that restores the last full scheduled
// playlist from durable state. Used by production so a controld-only restart
// can recover future displayAt items that were already filtered out of the
// player-visible playlist.
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
// so displayDefaultPlaylist CDP cannot race an in-flight RecomputeNow push.
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
	s.cancelTimerLocked()
}

func (s *scheduler) Prepare(playlist *dp1.Playlist) *dp1.Playlist {
	if playlist == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !HasDisplayAtSchedule(playlist) {
		// A non-scheduled cast must cancel any prior Daily timer; otherwise the
		// previous playlist's next displayAt would overwrite the new cast.
		s.clearLocked()
		return playlist
	}

	s.generation++
	s.full = clonePlaylist(playlist)
	s.restoredPending = false
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
	s.mu.Lock()
	if s.full == nil {
		s.mu.Unlock()
		return
	}
	if s.restoredPending {
		s.restoredPending = false
		s.generation++
		s.armTimerLocked()
	}
	s.mu.Unlock()

	s.recompute(ctx, true)
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
		if s.full == nil || s.restoredPending {
			s.mu.Unlock()
			return
		}
		gen := s.generation
		active := s.activeLocked()
		s.armTimerLocked()
		if !force && reflect.DeepEqual(active.Items, s.lastActive) {
			s.mu.Unlock()
			return
		}
		s.mu.Unlock()

		if err := s.push(ctx, active); err != nil {
			s.logger.Warn("Failed to push recomputed displayAt playlist", zap.Error(err))
			return
		}
		s.mu.Lock()
		s.lastActive = cloneItems(active.Items)
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
	s.generation++
	s.full = nil
	s.lastActive = nil
	s.restoredPending = false
}

func (s *scheduler) cancelTimerLocked() {
	if s.cancelTimer != nil {
		s.cancelTimer()
		s.cancelTimer = nil
	}
}

func (s *scheduler) snapshotLocked() Snapshot {
	return Snapshot{
		full:            clonePlaylist(s.full),
		lastActive:      cloneItems(s.lastActive),
		restoredPending: s.restoredPending,
	}
}

func (s *scheduler) restoreLocked(snapshot Snapshot) {
	if s.cancelTimer != nil {
		s.cancelTimer()
		s.cancelTimer = nil
	}
	s.full = clonePlaylist(snapshot.full)
	s.lastActive = cloneItems(snapshot.lastActive)
	s.restoredPending = snapshot.restoredPending
	s.generation++
	if s.full != nil && !s.restoredPending {
		s.armTimerLocked()
	}
	s.persistLocked()
}

func (s *scheduler) restorePersisted() {
	if s.store == nil {
		return
	}
	playlist, err := s.store.Load()
	if err != nil {
		s.logger.Warn("Failed to load persisted displayAt playlist", zap.Error(err))
		return
	}
	if playlist == nil {
		return
	}
	if !HasDisplayAtSchedule(playlist) {
		s.logger.Warn("Ignoring persisted playlist without displayAt items")
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.full = clonePlaylist(playlist)
	active := s.activeLocked()
	s.lastActive = cloneItems(active.Items)
	s.restoredPending = true
	s.logger.Info("Restored persisted displayAt playlist",
		zap.Int("fullItems", len(s.full.Items)),
		zap.Int("activeItems", len(active.Items)))
}

func (s *scheduler) persistLocked() {
	if s.store == nil {
		return
	}
	var err error
	if s.full == nil {
		err = s.store.Clear()
	} else {
		err = s.store.Save(s.full)
	}
	if err != nil {
		s.logger.Warn("Failed to persist displayAt playlist cache", zap.Error(err))
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
	if s.full == nil {
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

func (s *scheduler) push(ctx context.Context, playlist *dp1.Playlist) error {
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
	payload, err := command.JSON()
	if err != nil {
		return fmt.Errorf("marshal displayAt playlist command: %w", err)
	}

	result, err := s.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(payload)),
	})
	if err != nil {
		return fmt.Errorf("send displayAt playlist to CDP: %w", err)
	}
	if !playerResponseOK(result) {
		return fmt.Errorf("player rejected displayAt playlist")
	}
	return nil
}

func playerResponseOK(result interface{}) bool {
	m, ok := result.(map[string]interface{})
	if !ok {
		return false
	}
	if msg, ok := m["message"].(map[string]interface{}); ok {
		okVal, _ := msg["ok"].(bool)
		return okVal
	}
	okVal, _ := m["ok"].(bool)
	return okVal
}

// HasDisplayAtSchedule reports whether a playlist opts into item-level
// displayAt scheduling and carries at least one timed item. DP-1 treats
// schedule.byDisplayAt as the scheduling gate; item displayAt fields alone are
// metadata and must not filter playback.
func HasDisplayAtSchedule(p *dp1.Playlist) bool {
	if p == nil {
		return false
	}
	if !DisplayAtScheduleEnabled(p) {
		return false
	}
	for _, item := range p.Items {
		if item.DisplayAt != nil {
			return true
		}
	}
	return false
}

func DisplayAtScheduleEnabled(p *dp1.Playlist) bool {
	return p != nil && p.Schedule != nil && p.Schedule.ByDisplayAt
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
	if p.Schedule != nil {
		sched := *p.Schedule
		out.Schedule = &sched
	}
	return &out
}

func cloneItems(items []dp1playlist.PlaylistItem) []dp1playlist.PlaylistItem {
	return append([]dp1playlist.PlaylistItem(nil), items...)
}
