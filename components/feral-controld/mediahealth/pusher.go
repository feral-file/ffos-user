package mediahealth

import (
	"context"
	"errors"
	"maps"
	"slices"
	"sync"
	"time"

	"go.uber.org/zap"
)

// HintPusher closes the push-lag gap (#392 follow-up, the issue comment
// "hint-only push"): a verdict reached BETWEEN pushes — by the lookahead,
// the sweep or the outcome drain — used to reach the player only on the
// next cast, cutover or refresh, and a static inline cast is never
// re-pushed, so the player kept playing a work the device knew was dead.
// Since the cast pass probes only its first items (Prober.Items), most of
// a long wall's verdicts are reached exactly there.
//
// The pusher re-sends ONLY the hint map, through the player's
// `setMediaHealth {castSeq, mediaHealth}` command (contract mediaHealth v1,
// feature-detected by its `commands` list). The player replaces its held
// map for the cast on the wall and honors it at the next selection; it
// never unmounts the work on screen, so a push can never interrupt what is
// showing — and it carries no DP-1 body, so nothing is re-verified,
// re-projected or re-rendered.
//
// It is poll-driven, not write-driven: every Interval it recomputes the
// hint map for the wall with the same function every push uses
// (Service.Hints, replayable sources excluded) and sends it when it differs
// from what it believes the player holds. Recomputing catches every way the
// map can change — a new broken verdict, a heal, and a verdict EXPIRING
// past its recheck window, which no store write announces — and the
// Interval is the coalescing: a run of lookahead verdicts becomes one push.
// The cost is one read transaction over the wall's sources per Interval.
//
// What the player holds is a belief, not a read-back: the map the last push
// path attached (Attached) or the last successful send, bound to the cast
// generation (castSeq) it was observed under. An unknown or mismatched
// belief costs at most one redundant send — replace semantics make
// re-sending an equal map harmless — so the belief only has to be good
// enough to avoid chatter, never exact.
//
// Fences: the player refuses a castSeq that is not the cast on the wall
// (ErrStaleCast), so a send racing a new cast can never install one cast's
// hints on another; and the pusher waits Interval after a new castSeq
// before its first send, because the cast's own push already carried the
// map computed at cast time.
type HintPusher struct {
	// Send delivers the map to the player (production: the kiosk CDP
	// client, generation-fenced; see main's wiring). nil disables the
	// pusher.
	Send HintSender
	// Hints computes the map for the wall's sources (Service.Hints).
	Hints func(sources []string) map[string]Hint
	// Supported reports whether the installed player declares the push
	// command (PlayerContract.SupportsHintPush).
	Supported func() bool
	// Interval is the recompute period and the settle after a new cast
	// (DefaultPushInterval when 0).
	Interval time.Duration
	Logger   *zap.Logger
	// Now is the clock (time.Now when nil).
	Now func() time.Time

	mu sync.Mutex
	// The wall as last observed: its sources (as pushed) and the player's
	// cast generation; haveWall is false while no playlist with an outcome
	// ring is on screen.
	sources  []string
	castSeq  int64
	haveWall bool
	castAt   time.Time
	// held is the map the player is believed to hold for heldCast;
	// heldCast is pendingCast between an Attached and the next observation.
	held     map[string]Hint
	heldCast int64
	// parkedCast stops sending to a player that refused the command with
	// anything but staleCast, for the rest of that cast (parked is the
	// flag; castSeq 0 is a real generation).
	parkedCast int64
	parked     bool
}

// HintSender sends one hint-only push and returns the player's
// skippedIndices. Errors: ErrStaleCast when the player is on another cast,
// ErrHintPushRefused when it answered but refused the command (an old or
// lying manifest), anything else is transport weather (retried next tick).
type HintSender func(ctx context.Context, castSeq int64, hints map[string]Hint) ([]int, error)

var (
	// ErrStaleCast: the castSeq is not the cast on the wall.
	ErrStaleCast = errors.New("media health: hint push for a cast that is no longer on the wall")
	// ErrHintPushRefused: the player answered and refused the command.
	ErrHintPushRefused = errors.New("media health: player refused the hint push")
)

const (
	// DefaultPushInterval bounds a verdict's trip to the player at about
	// one item duration while costing one store read per 15 s.
	DefaultPushInterval = 15 * time.Second
	// pendingCast marks a belief attached by a push whose cast generation
	// is not observed yet (castSeq values are >= 0).
	pendingCast = -1
)

func (p *HintPusher) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

func (p *HintPusher) interval() time.Duration {
	if p.Interval > 0 {
		return p.Interval
	}
	return DefaultPushInterval
}

// Observe records the wall from a status poll: the item sources as pushed
// and the outcome ring's castSeq. ok false (no playlist, or a player without
// the ring) clears it. No I/O: this runs on the status poller goroutine.
func (p *HintPusher) Observe(sources []string, castSeq int64, ok bool) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !ok || len(sources) == 0 {
		p.haveWall = false
		p.sources = nil
		return
	}
	if !p.haveWall || castSeq != p.castSeq {
		p.castAt = p.now()
	}
	if !slices.Equal(p.sources, sources) {
		p.sources = slices.Clone(sources)
	}
	p.castSeq = castSeq
	p.haveWall = true
	if p.heldCast == pendingCast {
		// The push that attached the belief is the one that opened (or
		// refreshed) this cast. If the poll beat the push, the belief binds
		// to the outgoing cast and the incoming one starts unknown: one
		// redundant send at worst.
		p.heldCast = castSeq
	}
}

// Attached records the map a push path is about to carry (HintArgument).
// hints nil means the push carries none.
func (p *HintPusher) Attached(hints map[string]Hint) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.held = maps.Clone(hints)
	p.heldCast = pendingCast
}

// Run is the pusher loop; it returns when ctx is done.
func (p *HintPusher) Run(ctx context.Context) {
	if p == nil || p.Send == nil || p.Hints == nil {
		return
	}
	ticker := time.NewTicker(p.interval())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			p.step(ctx)
		}
	}
}

// step does at most one send and reports whether it sent.
func (p *HintPusher) step(ctx context.Context) bool {
	if p.Supported != nil && !p.Supported() {
		return false
	}
	logger := p.Logger
	if logger == nil {
		logger = zap.NewNop()
	}
	p.mu.Lock()
	if !p.haveWall || (p.parked && p.parkedCast == p.castSeq) || p.now().Sub(p.castAt) < p.interval() {
		p.mu.Unlock()
		return false
	}
	sources, castSeq := p.sources, p.castSeq
	held, known := p.held, p.heldCast == p.castSeq
	p.mu.Unlock()

	want := p.Hints(sources)
	if known && hintsEqual(held, want) {
		return false
	}
	// An unknown belief sends whatever the map is, the EMPTY map included.
	// The belief is unknown after a controld restart under a surviving
	// player page: the page still holds the hints the previous process
	// pushed, and if those verdicts have healed or expired since, only an
	// explicit empty replacement stops the player skipping works the
	// device no longer judges broken (PR #438 review, F20). The cost is
	// one send per cast with no belief, which replace semantics make
	// harmless.
	skipped, err := p.Send(ctx, castSeq, want)
	switch {
	case err == nil:
		p.mu.Lock()
		if p.castSeq == castSeq {
			p.held, p.heldCast = want, castSeq
		}
		p.mu.Unlock()
		logger.Info("media health: hint-only push delivered",
			zap.Int64("cast_seq", castSeq),
			zap.Int("hinted", len(want)),
			zap.Int("skipped_indices", len(skipped)))
	case errors.Is(err, ErrStaleCast):
		// A new cast is on the wall; its castSeq arrives on the next poll.
		logger.Debug("media health: hint push raced a new cast", zap.Int64("cast_seq", castSeq))
	case errors.Is(err, ErrHintPushRefused):
		p.mu.Lock()
		p.parked, p.parkedCast = true, castSeq
		p.mu.Unlock()
		logger.Warn("media health: player refused the hint-only push; parked until the next cast",
			zap.Int64("cast_seq", castSeq), zap.Error(err))
	default:
		// CDP down, the page generation moved: the belief is unchanged, so
		// the next tick recomputes and retries.
		logger.Debug("media health: hint-only push not delivered; retrying next tick", zap.Error(err))
	}
	return true
}

// hintsEqual compares two hint maps by key and content (nil == empty).
func hintsEqual(a, b map[string]Hint) bool {
	if len(a) != len(b) {
		return false
	}
	for k, va := range a {
		if vb, ok := b[k]; !ok || va != vb {
			return false
		}
	}
	return true
}
