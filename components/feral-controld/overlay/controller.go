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
//   - An automatic paint is accepted only when nothing is current.
//   - A replaced owner is told once through OnOverride. It holds no further
//     right to the screen: its Hide returns ErrNotCurrent, so it never sends a
//     hide that could erase the overlay that replaced it.
//
// Callers hold a Handle, not a flag.
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

// Painter sends overlays to the player. The controller calls it serially, one
// call at a time, so an implementation need not be safe for concurrent use.
// Hide receives the overlay it is clearing, so one painter can serve several
// kinds and send the right clear for each.
type Painter interface {
	Show(ctx context.Context, o Overlay) error
	Hide(ctx context.Context, o Overlay) error
}

// Condition is checked under the controller's lock, so the check and the paint
// that follows it cannot be interleaved with another paint. cur is the overlay
// on screen; ok is false when nothing is.
type Condition func(cur Overlay, ok bool) bool

// Listener is the owner side of a shown overlay.
type Listener interface {
	// OnOverride is called once, after the replacing overlay is on screen,
	// with that replacing overlay. It is the only terminal event a listener
	// gets for a replaced overlay.
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
)

type entry struct {
	id       uint64
	overlay  Overlay
	listener Listener
}

// Controller owns the screen overlay. The zero value is not usable; use New.
type Controller struct {
	// paint serializes every call to the painter and every change of cur,
	// since a change of cur is always made together with a painter call.
	paint sync.Mutex
	// mu guards cur and nextID. It is never held while the painter runs or a
	// listener is called, so a callback can call back into the controller.
	mu      sync.Mutex
	painter Painter
	cur     *entry
	nextID  uint64
}

// New returns a controller that paints through p.
func New(p Painter) *Controller {
	return &Controller{painter: p}
}

// Show paints o and makes it the current overlay. The previous owner, if any,
// is notified with OnOverride after the paint is done and no lock is held. A
// listener replacing its own overlay is not an override: it is the same owner
// moving its own display along, and it has nothing to be told.
//
// A failed paint leaves the current overlay unchanged and returns the error.
// An automatic Show over a current overlay returns ErrRejected without painting.
func (c *Controller) Show(ctx context.Context, l Listener, o Overlay, pr Priority) (Handle, error) {
	return c.ShowIf(ctx, l, o, pr, nil)
}

// ShowIf is Show with a condition checked under the same lock as the paint. A
// false condition returns ErrRejected and paints nothing. A nil condition
// always passes.
func (c *Controller) ShowIf(ctx context.Context, l Listener, o Overlay, pr Priority, cond Condition) (Handle, error) {
	c.paint.Lock()
	c.mu.Lock()
	// Safe to read here: cur changes only under paint, which we hold.
	cur, has := c.currentLocked()
	c.mu.Unlock()
	if (pr == Automatic && has) || (cond != nil && !cond(cur, has)) {
		c.paint.Unlock()
		return Handle{}, ErrRejected
	}

	if err := c.painter.Show(ctx, o); err != nil {
		c.paint.Unlock()
		return Handle{}, err
	}

	c.mu.Lock()
	c.nextID++
	e := &entry{id: c.nextID, overlay: o, listener: l}
	prev := c.cur
	c.cur = e
	c.mu.Unlock()
	c.paint.Unlock()

	if prev != nil && prev.listener != l {
		prev.listener.OnOverride(o)
	}
	return Handle{id: e.id}, nil
}

// Hide clears the screen. Only the current overlay's handle may hide it; any
// other handle gets ErrNotCurrent and sends nothing. A failed hide leaves the
// current overlay in place.
func (c *Controller) Hide(ctx context.Context, h Handle) error {
	return c.HideIf(ctx, func(cur Overlay, ok bool) bool {
		return ok && c.IsCurrent(h)
	})
}

// HideIf clears the screen when cond holds, under the paint lock. A false
// condition returns ErrNotCurrent and sends nothing.
func (c *Controller) HideIf(ctx context.Context, cond Condition) error {
	c.paint.Lock()
	defer c.paint.Unlock()

	c.mu.Lock()
	cur, has := c.currentLocked()
	c.mu.Unlock()
	if !cond(cur, has) {
		return ErrNotCurrent
	}
	if !has {
		// Nothing shown by this controller: there is nothing to clear, and a
		// condition that passed on an empty screen wants a plain clear.
		return nil
	}

	if err := c.painter.Hide(ctx, cur); err != nil {
		return err
	}
	c.mu.Lock()
	c.cur = nil
	c.mu.Unlock()
	return nil
}

// currentLocked returns the current overlay. Caller holds mu.
func (c *Controller) currentLocked() (Overlay, bool) {
	if c.cur == nil {
		return Overlay{}, false
	}
	return c.cur.overlay, true
}

// Current reports the overlay on screen, if any.
func (c *Controller) Current() (Overlay, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cur == nil {
		return Overlay{}, false
	}
	return c.cur.overlay, true
}

// IsCurrent reports whether h is the overlay on screen.
func (c *Controller) IsCurrent(h Handle) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cur != nil && c.cur.id == h.id
}
