package mediahealth

import (
	"context"
	"errors"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"go.uber.org/zap"
)

// Service is the one object main.go wires: the store, the prober and the
// lookahead behind the three seams the rest of the daemon uses.
//
//   - commandrouter: Preflight (admission + record) and Hints (the cast
//     field) — through its own consumer-owned interface.
//   - playlistschedule / playlist-refresher: Hints, for their pushes.
//   - status: ObservePlayback (outcome drain + lookahead feed) and
//     ItemHealths (the player_status annotation); Report for getMediaHealth.
//   - devicectl: Reset (factory reset).
type Service struct {
	store      *Store
	prober     *Prober
	lookahead  *Lookahead
	contract   *PlayerContract
	drainer    Drainer
	logger     *zap.Logger
	now        func() time.Time
	online     func() bool
	replayable func(source string) bool

	// Play-run tracking for the corroboration rule: consecutive failing
	// entries for one source with a non-decreasing attempt counter are ONE
	// play (the player reports several stages per attempt and retries in
	// place), so they count once. The run ends on another source, a
	// success, a skip, or an exhausted entry.
	runSource  string
	runAttempt int
	runEnded   bool

	// inferredFailed remembers the last (castSeq-less) renderStatus==3
	// sighting so the fallback counts one failure per item mount, not one
	// per poll.
	inferredIndex  int
	inferredSource string
}

// Config builds a Service.
type Config struct {
	Store    *Store
	Prober   *Prober
	Contract *PlayerContract
	// Lookahead may be nil (no during-play probing).
	Lookahead *Lookahead
	// Online reports the device's current internet verdict (the mediator's
	// applied connectivity state). Play failures observed while it is false
	// are recorded but never counted toward a verdict. nil means "unknown",
	// which is treated as OFFLINE: until the daemon has a verdict, a
	// failure cannot be blamed on the source.
	Online func() bool
	// Replayable, when set, reports whether the offline cache can serve a
	// source from a confirmed capture. A replayable source is never hinted:
	// the hint would make the player skip a work that replay interception
	// could have shown (the same reason the cast rescue exists).
	Replayable func(source string) bool
	Logger     *zap.Logger
	Now        func() time.Time
}

// New wires a Service. The lookahead's Run must be driven by the caller.
func New(cfg Config) *Service {
	s := &Service{
		store:         cfg.Store,
		prober:        cfg.Prober,
		lookahead:     cfg.Lookahead,
		contract:      cfg.Contract,
		logger:        cfg.Logger,
		now:           cfg.Now,
		online:        cfg.Online,
		replayable:    cfg.Replayable,
		inferredIndex: -1,
	}
	if s.logger == nil {
		s.logger = zap.NewNop()
	}
	if s.now == nil {
		s.now = time.Now
	}
	return s
}

// Store exposes the store for the surfaces that read it directly.
func (s *Service) Store() *Store { return s.store }

// Preflight is the cast-time pass; see Prober.Preflight.
func (s *Service) Preflight(ctx context.Context, sources []string) []ProbeResult {
	return s.prober.Preflight(ctx, sources)
}

// PlayerSupportsHints reports whether the installed player declares the
// mediaHealth contract; callers send the hint field only then.
func (s *Service) PlayerSupportsHints() bool {
	return s.contract.Supported()
}

// Hints returns the hint map for sources (conclusive broken only, minus
// anything the offline cache can replay). On a store error it returns nil:
// a push with no hints is the fail-open shape.
func (s *Service) Hints(sources []string) map[string]Hint {
	hints, err := s.store.Hints(sources, s.now())
	if err != nil {
		s.logger.Warn("media health: hints unavailable for this push", zap.Error(err))
		return nil
	}
	if s.replayable != nil {
		for src := range hints {
			if s.replayable(src) {
				// A dead ORIGIN is not a dead ITEM when replay can serve
				// it; the hint must not hide what the cache would show.
				delete(hints, src)
			}
		}
	}
	if len(hints) == 0 {
		return nil
	}
	return hints
}

// ResetOutcomeCursor forgets the outcome-ring cursor and the play run: the
// player page was replaced (a player-session generation bump), so its
// numbering restarts and nothing in the next ring was seen before.
func (s *Service) ResetOutcomeCursor() {
	s.drainer.Reset()
	s.runSource, s.runAttempt, s.runEnded = "", 0, false
}

// HintArgument returns the displayPlaylist `mediaHealth` argument for
// items: the conclusively broken ones, or nil when there are none or the
// installed player does not declare the contract (an old player must never
// receive the field).
func (s *Service) HintArgument(items []dp1playlist.PlaylistItem) interface{} {
	if !s.PlayerSupportsHints() {
		return nil
	}
	sources := make([]string, len(items))
	for i := range items {
		sources[i] = items[i].Source
	}
	hints := s.Hints(sources)
	if len(hints) == 0 {
		return nil
	}
	return hints
}

// ItemHealths annotates the player's item list; nil on error.
func (s *Service) ItemHealths(items []dp1playlist.PlaylistItem) []ItemHealth {
	sources := make([]string, len(items))
	for i := range items {
		sources[i] = items[i].Source
	}
	out, err := s.store.ItemHealths(sources)
	if err != nil {
		s.logger.Warn("media health: item health unavailable for this status round", zap.Error(err))
		return nil
	}
	return out
}

// Report builds the getMediaHealth rows for the items on screen.
func (s *Service) Report(items []dp1playlist.PlaylistItem) ([]ReportItem, error) {
	in := make([]ReportInput, len(items))
	for i := range items {
		in[i] = ReportInput{ID: items[i].ID, Title: items[i].Title, Source: items[i].Source}
	}
	return s.store.Report(in)
}

// ObservePlayback is the status poller's hook: it drains the player's
// outcome ring into the store, feeds the lookahead the current snapshot,
// and — on a player without the contract — infers one failure per mount
// from renderStatus==3. items nil means no playlist is on screen.
func (s *Service) ObservePlayback(items []dp1playlist.PlaylistItem, index int, renderStatus *int, outcomes *Outcomes) {
	sources := make([]string, len(items))
	for i := range items {
		sources[i] = items[i].Source
	}
	if s.lookahead != nil {
		s.lookahead.Observe(sources, index)
	}
	online := s.online != nil && s.online()
	if outcomes != nil {
		fresh, gap := s.drainer.Drain(outcomes)
		if gap > 0 {
			s.logger.Warn("media health: player outcome ring wrapped between polls; outcomes were lost",
				zap.Int64("missed", gap))
		}
		for _, o := range fresh {
			count := online && !s.samePlay(o)
			if _, err := s.store.ApplyOutcome(o, count); err != nil && !errors.Is(err, ErrLowDisk) {
				s.logger.Warn("media health: recording play outcome failed", zap.String("stage", o.Stage), zap.Error(err))
			}
		}
		return
	}
	// Fallback inference for a player without the contract: a failed
	// render for the item at index counts once per mount.
	if renderStatus == nil || *renderStatus != 3 || index < 0 || index >= len(sources) {
		if renderStatus != nil && *renderStatus != 3 {
			s.inferredIndex, s.inferredSource = -1, ""
		}
		return
	}
	src := sources[index]
	if src == "" || IsDataURI(src) || (s.inferredIndex == index && s.inferredSource == src) {
		return
	}
	s.inferredIndex, s.inferredSource = index, src
	if _, err := s.store.ApplyOutcome(Outcome{Source: src, Kind: OutcomeFail, Stage: StageInferred}, online); err != nil && !errors.Is(err, ErrLowDisk) {
		s.logger.Warn("media health: recording inferred failure failed", zap.Error(err))
	}
}

// samePlay reports whether a failing outcome continues the play run already
// counted, and advances the run. Rules: a success, a skip, or another
// source ends the run; an exhausted entry ends it after itself (the next
// entry for that source is a new mount); a failing entry whose attempt
// counter did not go backwards continues it. Errs toward "same play", the
// safe direction — an undercounted failure delays a verdict, an overcounted
// one hides a work.
func (s *Service) samePlay(o Outcome) bool {
	if o.Kind != OutcomeFail {
		s.runSource, s.runAttempt, s.runEnded = "", 0, false
		return false
	}
	same := o.Source == s.runSource && !s.runEnded && o.Attempt >= s.runAttempt
	s.runSource, s.runAttempt, s.runEnded = o.Source, o.Attempt, o.Stage == StageExhausted
	return same
}

// Reset closes the store and removes its directory (factory reset). The
// Service is unusable afterwards; the daemon reboots in that path.
func (s *Service) Reset() error {
	if err := s.store.Close(); err != nil {
		s.logger.Warn("media health: closing store before reset", zap.Error(err))
	}
	return Destroy(s.store.Path())
}

// Close releases the store.
func (s *Service) Close() error {
	return s.store.Close()
}
