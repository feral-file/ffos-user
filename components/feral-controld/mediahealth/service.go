package mediahealth

import (
	"context"
	"errors"
	"sync"
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
	pusher     *HintPusher
	contract   *PlayerContract
	drainer    Drainer
	logger     *zap.Logger
	now        func() time.Time
	online     func() bool
	replayable func(source string) bool
	// wallCapture judges the wall's own frame (wall_capture.go); nil when
	// off. Set once at wiring, before the poller starts (SetWallCapture).
	wallCapture *WallCapture

	// mu serializes the per-page outcome state below (drainer, the play
	// run, the inferred-failure memory): ObservePlayback runs on the status
	// poller goroutine and ResetOutcomeCursor on the player-session
	// generation-ready worker, so they can overlap on a reconnect.
	mu sync.Mutex

	// Play-run tracking for the corroboration rule: consecutive failing
	// entries for one source with a non-decreasing attempt counter are ONE
	// play (the player reports several stages per attempt and retries in
	// place), so they count once. The run ends on another source, a
	// success, a skip, or an exhausted entry.
	runSource  string
	runAttempt int
	runEnded   bool
	// runRefused is set when the current run's content-type check got a
	// definite refusal (definiteRefusal): a `ready` that follows for the
	// same source is the error page, not the work.
	runRefused bool

	// inferredFailed remembers the last (castSeq-less) renderStatus==3
	// sighting so the fallback counts one failure per item mount, not one
	// per poll.
	inferredIndex  int
	inferredSource string

	// wall / wallIndex are the player's item sources and index as last
	// polled (ObservePlayback), for matching an observed kiosk navigation
	// to its source (ObserveNavigation). Guarded by mu.
	wall      []string
	wallIndex int
}

// Config builds a Service.
type Config struct {
	Store    *Store
	Prober   *Prober
	Contract *PlayerContract
	// Lookahead may be nil (no during-play probing).
	Lookahead *Lookahead
	// Pusher may be nil (verdicts reach the player only on the next push).
	// The Service feeds it the wall (ObservePlayback) and every map a push
	// path attaches (HintArgument); the caller drives its Run.
	Pusher *HintPusher
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
		pusher:        cfg.Pusher,
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

// SetWallCapture attaches the wall-capture runner; call before the status
// poller starts.
func (s *Service) SetWallCapture(w *WallCapture) { s.wallCapture = w }

// ObserveWallFrame folds a classified capture of the wall into the play run
// as a StageFrame outcome, under the same rules as the player's own entries
// (one counted failure per play).
func (s *Service) ObserveWallFrame(o Outcome) {
	online := s.online != nil && s.online()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.applyEntry(o, online)
}

// GatewayOverride is the kiosk interceptor's per-request lookup: the same
// path on the fallback gateway the probe found serving this content, when
// there is one (Store.GatewayOverride).
func (s *Service) GatewayOverride(rawURL, referrer string) (string, bool) {
	return s.store.GatewayOverride(rawURL, referrer)
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

// ResetOutcomeCursor tells the drainer the player-session generation
// bumped. The page MAY have been replaced (its numbering restarting) or
// the bump may be a transport reconnect to the same page; the drainer
// settles which from the next ring (Drainer), and the play run is cleared
// only when the page really restarted — clearing it here let a reconnect
// count one failing play twice.
func (s *Service) ResetOutcomeCursor() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drainer.Reset()
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
	// The pusher's belief of what the player holds: exactly what this push
	// carries, none included (nil-safe).
	s.pusher.Attached(hints)
	if len(hints) == 0 {
		return nil
	}
	return hints
}

// ItemHealths annotates the player's item list; nil on error.
func (s *Service) ItemHealths(items []dp1playlist.PlaylistItem, skipped []int) []ItemHealth {
	sources := make([]string, len(items))
	for i := range items {
		sources[i] = items[i].Source
	}
	records, err := s.store.GetMany(sources)
	if err != nil {
		s.logger.Warn("media health: item health unavailable for this status round", zap.Error(err))
		return nil
	}
	skippedAt := skippedSet(skipped)
	now := s.now()
	out := make([]ItemHealth, len(sources))
	for i, src := range sources {
		rec := records[src]
		h := itemHealthOf(rec)
		h.Skipped = skippedAt[i]
		if h.State == StateBroken && !h.Skipped {
			h.Unskipped = s.unskippedReason(src, rec, now)
		}
		out[i] = h
	}
	return out
}

// unskippedReason says why a broken record is not being skipped by the
// player, in the order the hint pipeline refuses it: no contract declared
// (nothing is ever hinted), the cache can replay it (the hint drops it),
// the verdict expired (not conclusive, so not hinted), else it IS hinted
// now and the player simply does not hold that hint yet — or chose to
// play it (repeat-one, an all-broken map).
func (s *Service) unskippedReason(source string, rec *Record, now time.Time) Unskipped {
	switch {
	case !s.PlayerSupportsHints():
		return UnskippedPlayer
	case s.replayable != nil && s.replayable(source):
		return UnskippedReplayable
	case rec == nil || !rec.Conclusive(now):
		return UnskippedExpired
	default:
		return UnskippedPending
	}
}

func skippedSet(skipped []int) map[int]bool {
	set := make(map[int]bool, len(skipped))
	for _, i := range skipped {
		if i >= 0 {
			set[i] = true
		}
	}
	return set
}

// Report builds the getMediaHealth rows for the items on screen.
func (s *Service) Report(items []dp1playlist.PlaylistItem, skipped []int) ([]ReportItem, error) {
	in := make([]ReportInput, len(items))
	for i := range items {
		in[i] = ReportInput{ID: items[i].ID, Title: items[i].Title, Source: items[i].Source}
	}
	rows, err := s.store.Report(in)
	if err != nil {
		return nil, err
	}
	// The same overlay as ItemHealths, so the two surfaces never disagree.
	health := s.ItemHealths(items, skipped)
	for i := range rows {
		if i < len(health) {
			rows[i].Skipped = health[i].Skipped
			rows[i].Unskipped = health[i].Unskipped
		}
	}
	return rows, nil
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
	if items != nil {
		s.wallCapture.Observe(sources, index, renderStatus)
	} else {
		s.wallCapture.Observe(nil, -1, nil)
	}
	// Only a player with the outcome ring has a cast generation to fence a
	// hint-only push on; without it the pusher has no wall (nil-safe).
	if outcomes != nil && items != nil {
		s.pusher.Observe(sources, outcomes.CastSeq, true)
	} else {
		s.pusher.Observe(nil, 0, false)
	}
	online := s.online != nil && s.online()
	s.mu.Lock()
	defer s.mu.Unlock()
	if items != nil {
		s.wall, s.wallIndex = sources, index
	} else {
		s.wall, s.wallIndex = nil, 0
	}
	if outcomes != nil {
		fresh, gap, restarted := s.drainer.Drain(outcomes)
		if restarted {
			// A new page: a failing run cannot continue across it.
			s.runSource, s.runAttempt, s.runEnded, s.runRefused = "", 0, false, false
		}
		if gap > 0 {
			s.logger.Warn("media health: player outcome ring wrapped between polls; outcomes were lost",
				zap.Int64("missed", gap))
		}
		for _, o := range fresh {
			s.applyEntry(o, online)
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

// applyEntry folds one entry — a ring entry from the player or a
// navigation the daemon observed on the kiosk — through the play-run
// rules and into the store, and reports whether it counted toward a
// verdict. Caller holds s.mu.
func (s *Service) applyEntry(o Outcome, online bool) bool {
	var count bool
	switch {
	case o.Kind == OutcomeFail && !countableFailure(o):
		// History only, and it must not touch the run: an uncountable
		// entry that OPENED the run made every failure after it in that
		// play "the same play", so a work that failed its content-type
		// check on the network and then timed out three times counted
		// nothing, play after play (ipfs.infura.io on the office FF1, 6
		// plays, 0 counted).
	case o.Kind == OutcomeFail:
		count = online && !s.samePlay(o)
		if definiteRefusal(o) {
			s.runRefused = true
		}
	case o.Stage == StageFrame:
		// A rendered frame belongs to the play already on screen: it
		// neither ends nor restarts the run the player's entries
		// describe. Whether it may count as a success against that
		// play's content-type check is the store's call
		// (renderedFrame: a definite absence wins).
	default:
		o.errorPage = o.Kind == OutcomeOK && s.runRefused &&
			o.Source == s.runSource && !readyIsEvidence(o)
		s.samePlay(o)
	}
	if _, err := s.store.ApplyOutcome(o, count); err != nil && !errors.Is(err, ErrLowDisk) {
		s.logger.Warn("media health: recording play outcome failed", zap.String("stage", o.Stage), zap.Error(err))
	}
	return count
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
		s.runSource, s.runAttempt, s.runEnded, s.runRefused = "", 0, false, false
		return false
	}
	// An observed navigation (Attempt 0) carries no attempt counter: it
	// belongs to whatever play of that source is open, and when it opens
	// one, the player's own entries for that play (attempt >= 1) continue
	// it — the frame request happens after the player's content-type
	// check, but the ring is drained only every poll, so the two can
	// arrive in either order.
	same := o.Source == s.runSource && !s.runEnded && (o.Attempt == 0 || o.Attempt >= s.runAttempt)
	if !same {
		s.runRefused = false
	}
	s.runSource, s.runEnded = o.Source, o.Stage == StageExhausted
	if !same || o.Attempt > 0 {
		s.runAttempt = o.Attempt
	}
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
