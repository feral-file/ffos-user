// Package playlistschedule filters DP-1 playlists by displayAt and advances
// them on a timer. Controld owns this path so the player can keep playing a
// normal (already filtered) playlist with no scheduling awareness.
package playlistschedule

import (
	"context"
	"fmt"
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

//go:generate mockgen -source=scheduler.go -destination=../mocks/playlistschedule.go -package=mocks -mock_names=Scheduler=MockPlaylistScheduler

// Scheduler caches the full byDisplayAt playlist, exposes the active set for
// the player, and re-pushes when the next displayAt threshold is crossed.
type Scheduler interface {
	// Prepare caches a byDisplayAt playlist and returns the active set for the
	// player. Playlists without byDisplayAt clear any prior schedule and are
	// returned unchanged.
	Prepare(playlist *dp1.Playlist) *dp1.Playlist
	// RecomputeNow re-filters the cached playlist and pushes it to the player.
	// Used on wake, boot, and after a failed network refresh that still has a
	// usable cache. No-op when nothing is cached.
	RecomputeNow(ctx context.Context)
	// Clear drops the cached byDisplayAt playlist and cancels the transition
	// timer. Call when playback leaves displayPlaylist (e.g. displayDefaultPlaylist)
	// so wake/reconnect/timer cannot resurrect the previous scheduled cast.
	Clear()
	// WithPlayerPush serializes CDP playlist updates against timer/wake
	// recomputes. Cast and refresh paths must wrap their displayPlaylist CDP
	// send so a stale RecomputeNow cannot overwrite a newer cast mid-flight.
	WithPlayerPush(fn func())
	// HasCache reports whether a byDisplayAt playlist is currently cached.
	HasCache() bool
	Stop()
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

	// full is the last byDisplayAt playlist (complete item list). The player
	// only ever receives the filtered active set derived from this cache.
	full *dp1.Playlist
	// generation increments on every Prepare/clear so a RecomputeNow that
	// snapshotted an older cache can drop its push after a newer cast wins.
	generation uint64

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
	if locFn == nil {
		locFn = sleepschedule.LocalTimezone
	}
	return &scheduler{
		ctx:    ctx,
		cdp:    cdpClient,
		clock:  clock,
		locFn:  locFn,
		logger: logger,
	}
}

func (s *scheduler) HasCache() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.full != nil
}

func (s *scheduler) WithPlayerPush(fn func()) {
	s.pushMu.Lock()
	defer s.pushMu.Unlock()
	fn()
}

func (s *scheduler) Prepare(playlist *dp1.Playlist) *dp1.Playlist {
	if playlist == nil {
		return nil
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if !byDisplayAtEnabled(playlist) {
		// A non-scheduled cast must cancel any prior Daily timer; otherwise the
		// previous playlist's next displayAt would overwrite the new cast.
		s.clearLocked()
		return playlist
	}

	s.generation++
	s.full = clonePlaylist(playlist)
	active := s.activeLocked()
	s.armTimerLocked()

	s.logger.Info("Prepared displayAt active set",
		zap.Int("fullItems", len(playlist.Items)),
		zap.Int("activeItems", len(active.Items)))
	return active
}

func (s *scheduler) RecomputeNow(ctx context.Context) {
	// Hold pushMu for the whole read+push loop so a cast/refresh
	// WithPlayerPush cannot interleave mid-update. If Prepare bumps generation
	// during CDP Send, loop once more and push the newer active set before
	// releasing — avoiding a stale playlist stuck on screen.
	s.pushMu.Lock()
	defer s.pushMu.Unlock()

	for {
		s.mu.Lock()
		if s.full == nil {
			s.mu.Unlock()
			return
		}
		gen := s.generation
		active := s.activeLocked()
		s.armTimerLocked()
		s.mu.Unlock()

		if err := s.push(ctx, active); err != nil {
			s.logger.Warn("Failed to push recomputed displayAt playlist", zap.Error(err))
			return
		}
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

func (s *scheduler) Clear() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clearLocked()
}

func (s *scheduler) Stop() {
	s.Clear()
}

func (s *scheduler) clearLocked() {
	if s.cancelTimer != nil {
		s.cancelTimer()
		s.cancelTimer = nil
	}
	if s.full != nil {
		s.generation++
	}
	s.full = nil
}

func (s *scheduler) activeLocked() *dp1.Playlist {
	now := s.clock.Now()
	loc := s.locFn()
	items := displayat.ComputeActiveSet(&s.full.Playlist, now, loc)
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
	next := displayat.NextDisplayAt(&s.full.Playlist, now, s.locFn())
	if next == nil {
		s.logger.Debug("No future displayAt; holding current active set")
		return
	}

	wait := next.Sub(now)
	if wait < 0 {
		wait = 0
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
	s.RecomputeNow(s.ctx)
}

func (s *scheduler) push(ctx context.Context, playlist *dp1.Playlist) error {
	if s.cdp == nil {
		return fmt.Errorf("cdp not configured")
	}
	if !s.cdp.Initialized() {
		return fmt.Errorf("cdp not connected")
	}

	command := commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{
			"dp1_call": playlist,
			"refresh":  true,
		},
	}
	payload, err := command.JSON()
	if err != nil {
		return fmt.Errorf("marshal displayAt playlist command: %w", err)
	}

	_, err = s.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(payload)),
	})
	if err != nil {
		return fmt.Errorf("send displayAt playlist to CDP: %w", err)
	}
	return nil
}

func byDisplayAtEnabled(p *dp1.Playlist) bool {
	return p != nil && p.Schedule != nil && p.Schedule.ByDisplayAt
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
