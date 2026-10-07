// Package overlay is the single owner of the FF1 screen overlay.
//
// The player shows whichever overlay command arrives last. Two controld paths
// paint into it: the setup/claim narration and the browser-pairing code. Left
// alone, an automatic paint (an expiry refresh) replaces an overlay the owner
// asked for, and nothing on the device records which one is showing. The
// Controller holds that record. Every paint goes through it, so it knows the
// current overlay, who painted it, and when an older owner has been replaced.
//
// Policy, from the owner's rules:
//   - An owner action replaces whatever is current.
//   - An automatic paint is accepted when nothing is current, or when the
//     current overlay is the same listener's own (round 3 review, F1: without
//     this, a listener's own earlier owner-priority narration could
//     permanently block that same listener's later automatic attempts — see
//     Show's doc). It is rejected only by a DIFFERENT listener's overlay.
//   - A replaced owner is told once through OnOverride. It holds no further
//     right to the screen: its Hide returns ErrNotCurrent, so it never sends a
//     hide that could erase the overlay that replaced it.
//
// Callers hold a Handle, not a flag.
//
// # Decision and delivery are two different things
//
// Show/Hide decide who owns the screen under one lock, in memory, and return
// immediately — no caller ever waits on a painter. A review of the first
// version of this package (round 1, feral-file/ffos-user#385) found that
// calling the painter synchronously while deciding reintroduced the exact
// hazard this package exists to remove: a painter that blocks on I/O (a CDP
// send that can take up to 15s, or longer behind a navigation park) stalled
// every other caller sharing the decision lock — including setupui callers
// whose own contract is "pushes never block the caller" (see
// provisioning_wiring.go's inline, must-not-block event loop). The same
// review found a second defect: a caller that sent a clear directly, bypassing
// the controller "since it records nothing" (the old SweepStaleOverlay /
// clearSetup special case), had a window in which a replacing paint could land
// on the real screen before that bypassed clear did, erasing it.
//
// The fix is one delivery queue, owned by the controller, drained by one
// background worker. Show/Hide enqueue a delivery job in the same critical
// section that decides ownership, so delivery order always matches decision
// order — across both owners, not just within one — and no bypass exists:
// every clear, including the boot-time sweep, is an ordinary Show or Hide.
// The worker is the only thing that calls the painter, so a painter is free to
// block (park for navigation, wait on a CDP send) without blocking anyone.
//
// Show/Hide themselves can no longer report whether the painter's actual send
// succeeded, because that happens after they return — a failed delivery is
// never retried, and the controller's decision is not rolled back. This
// matches how setupui's own narration already behaved before this package
// existed: a push "succeeds" once it is queued, not once it is on screen.
//
// A caller that still needs the outcome of specifically its own delivery —
// mintpairing's start command reported display failures to the owner before
// this package existed, and three tests held it to that — gets a Result from
// Show/ShowIf and calls Wait on it. That wait blocks only the calling
// goroutine. It takes no lock and is not awaited by the controller itself, so
// it cannot stall any other caller: the decision it is waiting on has already
// been made and released by the time Wait runs.
package overlay

import (
	"context"
	"errors"
	"sync"
)

// Kind names an overlay the controller can show.
type Kind string

// Overlay is one thing to show on the screen.
type Overlay struct {
	Kind    Kind
	Payload any
}

// Priority says whether a paint was asked for by the owner or by a timer.
type Priority int

const (
	// Owner is a paint the owner asked for: a Browser Pairing tap, a relayer
	// show. It may replace any current overlay.
	Owner Priority = iota
	// Automatic is a paint nobody asked for: an expiry refresh, a sweep. It
	// may only fill an empty screen.
	Automatic
)

// Painter sends overlays to the player. The controller's own worker is the
// only caller, one delivery at a time, so an implementation need not be safe
// for concurrent use — and is free to block (park for a pending navigation,
// wait on a slow or wedged CDP send): nothing else waits on it. Hide receives
// the overlay it is clearing, so one painter can serve several kinds and send
// the right clear for each. An error is for the painter's own logging; it
// reaches no caller of Show or Hide, and the delivery is not retried.
type Painter interface {
	Show(ctx context.Context, o Overlay) error
	Hide(ctx context.Context, o Overlay) error
}

// Condition is checked under the controller's lock, so the check and the
// decision that follows it cannot be interleaved with another decision. cur is
// the overlay on screen; ok is false when nothing is.
type Condition func(cur Overlay, ok bool) bool

// Listener is the owner side of a shown overlay.
type Listener interface {
	// OnOverride is called once, after the replacing overlay has been decided
	// (not necessarily delivered yet), with that replacing overlay. It is the
	// only terminal event a listener gets for a replaced overlay. No lock is
	// held, so a callback may call back into the controller.
	OnOverride(by Overlay)
	// OnClose is reserved for an overlay that ends by itself (timeout). Hide
	// by the current owner is the owner's own action and fires nothing.
	OnClose()
}

// Handle identifies one shown overlay. The zero Handle is never current.
type Handle struct {
	id uint64
}

var (
	// ErrRejected is returned for an automatic Show while an overlay is current.
	ErrRejected = errors.New("overlay: automatic show rejected while another overlay is current")
	// ErrNotCurrent is returned by Hide for a handle that is not the current overlay.
	ErrNotCurrent = errors.New("overlay: handle is not the current overlay")
	// ErrSuperseded is the Wait result for a delivery that coalescing replaced
	// before the worker ever sent it — see enqueueLocked. It was never shown
	// standalone, so there is nothing to report but that it was superseded.
	ErrSuperseded = errors.New("overlay: delivery superseded by a newer show of the same kind before it was sent")
)

type entry struct {
	id       uint64
	overlay  Overlay
	listener Listener
}

// Result lets a caller learn the outcome of specifically its own queued
// delivery, without blocking anyone else — see the package doc on decision
// versus delivery. Waiting is always optional; the zero Result (from an
// ErrRejected Show) has nothing to wait for.
type Result struct {
	done <-chan error
}

// Wait blocks until the delivery this Result belongs to has been attempted —
// delivered (the painter's own error, nil on success) or superseded
// (ErrSuperseded) — or until ctx is done first, whichever comes first. It
// takes no controller lock, so it never stalls any other caller.
func (r Result) Wait(ctx context.Context) error {
	if r.done == nil {
		return nil
	}
	select {
	case err := <-r.done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

// delivery is one queued call to the painter. hide marks a clear of overlay
// (the overlay that was current when the clear was decided); otherwise it is
// a show of overlay. done, when non-nil, receives exactly one value: the
// painter's error on delivery, or ErrSuperseded if coalescing replaced this
// entry first.
type delivery struct {
	overlay Overlay
	hide    bool
	done    chan error
}

// maxPendingDeliveries bounds the delivery queue. A deeper queue only ever
// means the worker is stalled behind a slow or wedged send; dropping the
// oldest entry is the correct staleness policy for a courtesy overlay, and
// matches setupui's pre-controller queue bound.
const maxPendingDeliveries = 32

// Controller owns the screen overlay. The zero value is not usable; use New.
type Controller struct {
	// mu guards every field below. It is held only for in-memory bookkeeping
	// — deciding ownership and queuing a delivery — never while the painter
	// runs or a listener is called.
	mu      sync.Mutex
	painter Painter
	cur     *entry
	nextID  uint64
	pending []delivery
	running bool
}

// New returns a controller that paints through p.
func New(p Painter) *Controller {
	return &Controller{painter: p}
}

// Show decides o is the current overlay and queues it for delivery. The
// previous owner, if any, is notified with OnOverride once the decision is
// made, before Show returns. A listener replacing its own overlay is not an
// override: it is the same owner moving its own display along, and it has
// nothing to be told.
//
// An automatic Show over a current overlay owned by a DIFFERENT listener
// returns ErrRejected and queues nothing — its zero Result has nothing to
// wait for. A listener's own Automatic show may still replace its own earlier
// overlay (round 3 review, F1): without this exemption, an owner-priority
// narration this same listener painted moments earlier (setupui's
// ShowFinalizing, say) could permanently starve that listener's own later
// Automatic attempts — e.g. the auto-claim loop's claim QR, repainted after
// every reconnect — with no caller ever told why, since Show itself never
// fails on the painter's account; call Wait on the returned Result for that —
// see the package doc on decision versus delivery.
//
// ctx is accepted for API symmetry with Result.Wait, which does use a ctx —
// typically this same one, passed straight through by a caller. The decision
// Show itself makes is in-memory and never blocks, so there is nothing for a
// ctx to bound here (round 2 review, F-ctx); the delivery is queued and run on
// the controller's own worker, detached from any single caller's ctx, since a
// delivery can outlive the request that queued it (see Wait and the package
// doc). The worker's own bound is the painter's — e.g. the CDP client's own
// internal send timeout — not this ctx.
func (c *Controller) Show(ctx context.Context, l Listener, o Overlay, pr Priority) (Handle, Result, error) {
	return c.ShowIf(ctx, l, o, pr, nil)
}

// ShowIf is Show with a condition checked under the same lock as the decision.
// A false condition returns ErrRejected, a zero Result, and queues nothing. A
// nil condition always passes. See Show's doc for why ctx bounds nothing here.
func (c *Controller) ShowIf(_ context.Context, l Listener, o Overlay, pr Priority, cond Condition) (Handle, Result, error) {
	c.mu.Lock()
	cur, has := c.currentLocked()
	// has && c.cur.listener != l: an Automatic show may replace its own
	// listener's earlier overlay (see the doc above) but not another
	// listener's. l != nil is not checked here — every real caller passes a
	// non-nil Listener; a nil one would simply never match c.cur.listener and
	// always be treated as "a different listener," the safe default.
	blockedByOther := pr == Automatic && has && c.cur.listener != l
	if blockedByOther || (cond != nil && !cond(cur, has)) {
		c.mu.Unlock()
		return Handle{}, Result{}, ErrRejected
	}

	c.nextID++
	e := &entry{id: c.nextID, overlay: o, listener: l}
	prev := c.cur
	c.cur = e
	done := make(chan error, 1)
	c.enqueueLocked(delivery{overlay: o, done: done})
	c.mu.Unlock()

	if prev != nil && prev.listener != l {
		prev.listener.OnOverride(o)
	}
	return Handle{id: e.id}, Result{done: done}, nil
}

// Hide decides the screen is cleared and queues that clear for delivery. Only
// the current overlay's handle may hide it; any other handle gets
// ErrNotCurrent and queues nothing. See Show's doc for why ctx bounds nothing
// here — the same holds for Hide.
func (c *Controller) Hide(ctx context.Context, h Handle) error {
	return c.HideIf(ctx, func(cur Overlay, ok bool) bool {
		// isCurrentLocked, not IsCurrent: this condition runs under mu (see
		// HideIf), and IsCurrent would deadlock trying to take it again.
		return ok && c.isCurrentLocked(h)
	})
}

// HideIf clears the screen when cond holds, under the decision lock. A false
// condition — including nothing being current, since cond always sees ok as
// part of its own check — returns ErrNotCurrent and queues nothing.
func (c *Controller) HideIf(_ context.Context, cond Condition) error {
	c.mu.Lock()
	cur, has := c.currentLocked()
	if !cond(cur, has) {
		c.mu.Unlock()
		return ErrNotCurrent
	}
	c.cur = nil
	c.enqueueLocked(delivery{overlay: cur, hide: true})
	c.mu.Unlock()
	return nil
}

// enqueueLocked applies the same coalescing rule setupui's queue used before
// this package existed: a delivery matching the TRAILING queued entry's kind
// and direction (show or hide) replaces it in place, so a burst of the same
// state (OTA progress) collapses to one trailing send; a distinct kind or
// direction appends, so an ordered sequence (Ready then Hidden) still delivers
// both. The replaced entry's done, if anyone is waiting on it, receives
// ErrSuperseded — it was never sent standalone, and a waiter must not block
// forever on a delivery that will never happen. Caller holds mu.
func (c *Controller) enqueueLocked(d delivery) {
	if n := len(c.pending); n > 0 {
		last := c.pending[n-1]
		if last.hide == d.hide && last.overlay.Kind == d.overlay.Kind {
			if last.done != nil {
				last.done <- ErrSuperseded
			}
			c.pending[n-1] = d
			return
		}
	}
	if len(c.pending) >= maxPendingDeliveries {
		dropped := c.pending[0]
		if dropped.done != nil {
			dropped.done <- ErrSuperseded
		}
		c.pending = c.pending[1:]
	}
	c.pending = append(c.pending, d)
	if !c.running {
		c.running = true
		go c.worker()
	}
}

// worker drains pending deliveries one at a time, in order, until the queue is
// empty. It is the only caller of the painter, so the painter may block
// freely — nothing else is waiting on it. Each delivery's own done channel,
// if any, gets exactly the painter's result; see Result.Wait.
func (c *Controller) worker() {
	for {
		c.mu.Lock()
		if len(c.pending) == 0 {
			c.running = false
			c.mu.Unlock()
			return
		}
		d := c.pending[0]
		c.pending = c.pending[1:]
		c.mu.Unlock()

		// Background, not the original caller's ctx: this delivery can run
		// long after that ctx's own caller returned (round 2 review, F-ctx;
		// see Show's doc). The painter bounds itself — e.g. the CDP client's
		// own internal send timeout.
		ctx := context.Background()
		var err error
		if d.hide {
			err = c.painter.Hide(ctx, d.overlay)
		} else {
			err = c.painter.Show(ctx, d.overlay)
		}
		if d.done != nil {
			d.done <- err
		}
	}
}

// currentLocked returns the current overlay. Caller holds mu.
func (c *Controller) currentLocked() (Overlay, bool) {
	if c.cur == nil {
		return Overlay{}, false
	}
	return c.cur.overlay, true
}

// Current reports the overlay decided as on screen, if any. It reflects the
// latest decision, not necessarily what the player has rendered yet — the
// same "intent, not delivery" reading setupui's own Narrating used before this
// package existed.
func (c *Controller) Current() (Overlay, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.currentLocked()
}

// IsCurrent reports whether h is the overlay decided as on screen.
func (c *Controller) IsCurrent(h Handle) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.isCurrentLocked(h)
}

// isCurrentLocked is IsCurrent for a caller that already holds mu — a
// Condition, which ShowIf and HideIf invoke under it. Calling IsCurrent
// instead from inside a Condition deadlocks on the same, non-reentrant lock.
func (c *Controller) isCurrentLocked(h Handle) bool {
	return c.cur != nil && c.cur.id == h.id
}
