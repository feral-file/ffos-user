package setupui

import (
	"context"
	"strings"

	"github.com/feral-file/ffos-user/components/feral-controld/overlay"
)

// setupKindPrefix namespaces setup narration inside the shared overlay
// controller. Every setup overlay is "setup:<state>"; mintpairing uses its own
// prefix, so the router can send each kind to its painter.
const setupKindPrefix = "setup:"

func setupKind(state string) overlay.Kind {
	return overlay.Kind(setupKindPrefix + state)
}

// isSetup reports whether o is a setup narration overlay.
func isSetup(o overlay.Overlay) bool {
	return strings.HasPrefix(string(o.Kind), setupKindPrefix)
}

// setupState returns the narration state of a setup overlay, or "" for any
// other kind, so state predicates read a non-setup overlay as "no state".
func setupState(o overlay.Overlay) string {
	if !isSetup(o) {
		return ""
	}
	return strings.TrimPrefix(string(o.Kind), setupKindPrefix)
}

// payloadOf returns the wire request a setup overlay carries.
func payloadOf(o overlay.Overlay) map[string]any {
	req, _ := o.Payload.(map[string]any)
	return req
}

// narrationOf is the view a state predicate gets: the current setup request,
// or an empty map when nothing is shown or another kind holds the screen.
func narrationOf(cur overlay.Overlay, has bool) map[string]any {
	if !has || !isSetup(cur) {
		return map[string]any{}
	}
	return payloadOf(cur)
}

// setupPainter is the transport the controller paints setup overlays through.
// It queues the request for the worker, which keeps narration in order and
// coalesces progress bursts, exactly as before the controller existed.
type setupPainter struct {
	s *Service
}

func (p setupPainter) Show(_ context.Context, o overlay.Overlay) error {
	p.s.enqueue(payloadOf(o))
	return nil
}

func (p setupPainter) Hide(_ context.Context, _ overlay.Overlay) error {
	p.s.enqueue(map[string]any{"state": stateHidden})
	return nil
}

// Painter returns the setup transport for a shared overlay controller. The
// composition root routes setup kinds to it.
func (s *Service) Painter() overlay.Painter {
	return setupPainter{s: s}
}

// SetController makes the service paint through a controller shared with the
// other overlay owners. Call once at wiring time, before the first narration.
func (s *Service) SetController(c *overlay.Controller) {
	s.ctrl = c
}

// OnOverride is called when another overlay replaces the setup overlay. Setup
// states still queued have not reached the player yet; sending them now would
// make the player yield the newer overlay. The queue is dropped. A state
// already handed to the worker can still arrive late — the window is one send.
func (s *Service) OnOverride(_ overlay.Overlay) {
	s.mu.Lock()
	s.pending = nil
	s.mu.Unlock()
}

// OnClose is unused: setup overlays end by replacement or by an explicit hide.
func (s *Service) OnClose() {}

// enqueue hands one request to the worker. The worker sends it in order.
func (s *Service) enqueue(req map[string]any) {
	s.mu.Lock()
	s.narrated = true
	s.enqueueLocked(req)
	starting := !s.running
	s.running = true
	s.mu.Unlock()
	if starting {
		go s.worker()
	}
}

// hideIf clears the screen when match holds, under the controller's lock.
func (s *Service) hideIf(match overlay.Condition) {
	_ = s.ctrl.HideIf(context.Background(), match)
}

// clearSetup is a plain hide. It clears setup narration the controller shows,
// and it clears an empty screen too: the player may still show an overlay from
// an earlier process that this controller does not know about. A browser-pairing
// code on screen belongs to another owner and is left alone.
//
// The empty-screen clear is a clear, not a show, so it goes straight to the
// transport: there is no overlay for the controller to record.
func (s *Service) clearSetup() {
	emptyScreen := false
	err := s.ctrl.HideIf(context.Background(), func(cur overlay.Overlay, has bool) bool {
		if has && !isSetup(cur) {
			return false
		}
		emptyScreen = !has
		return true
	})
	if err == nil && emptyScreen {
		s.enqueue(map[string]any{"state": stateHidden})
	}
}

// showOwned is the one place setup narration is painted. A nil condition always
// shows; a condition that fails drops the narration, as the old pushIf did.
func (s *Service) showOwned(req map[string]any, pr overlay.Priority, ok func(last map[string]any) bool) {
	var cond overlay.Condition
	if ok != nil {
		cond = func(cur overlay.Overlay, has bool) bool {
			return ok(narrationOf(cur, has))
		}
	}
	_, _ = s.ctrl.ShowIf(context.Background(), s, overlay.Overlay{
		Kind:    setupKind(stringField(req, "state")),
		Payload: req,
	}, pr, cond)
}
