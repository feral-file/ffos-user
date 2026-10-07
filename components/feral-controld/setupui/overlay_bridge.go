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

// setupPainter is the transport the controller delivers setup overlays
// through. It is called only from the controller's own worker goroutine (see
// the overlay package doc), never from a caller of Show/Hide/ShowClaimQR/etc,
// so parking for a pending navigation and the blocking CDP send below cannot
// stall anything else — ordering and coalescing of the actual sends are the
// controller's own queue's job now, not this type's.
type setupPainter struct {
	s *Service
}

func (p setupPainter) Show(_ context.Context, o overlay.Overlay) error {
	p.s.parkForNavigation()
	p.s.trySend(payloadOf(o))
	return nil
}

func (p setupPainter) Hide(_ context.Context, _ overlay.Overlay) error {
	p.s.parkForNavigation()
	p.s.trySend(map[string]any{"state": stateHidden})
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

// OnOverride is called when another overlay replaces the setup overlay. There
// is nothing queued locally to drop any more: the controller's own delivery
// queue is the only queue, and it already orders this replacement correctly
// (see the overlay package doc on decision versus delivery).
func (s *Service) OnOverride(overlay.Overlay) {}

// OnClose is unused: setup overlays end by replacement or by an explicit hide.
func (s *Service) OnClose() {}

// hideIf clears the screen when match holds. Nothing current, or something
// current that match rejects, sends nothing — there is no bypass for an empty
// screen; see SweepStaleOverlay for the one legitimate case of painting
// without the owner having asked (a stale overlay from an earlier process). A
// hide that actually commits marks this service's intent as hidden, so Resync
// can replay it on reconnect even if its own CDP send failed — see
// hiddenIntent's doc.
//
// The write runs inside the CommitHook, under the controller's own decision
// lock, not after HideIf returns: a concurrent showOwned racing this hideIf
// can commit either before or after it, and only the hook runs in true commit
// order — after HideIf returns, a racing show's own return can be delayed
// (round 6 review, F4) past this one, and then overwrite hiddenIntent with
// its own, now-stale "shown" bookkeeping. See CommitHook's doc.
func (s *Service) hideIf(match overlay.Condition) {
	_ = s.ctrl.HideIf(context.Background(), match, func() {
		s.mu.Lock()
		s.hiddenIntent = true
		s.mu.Unlock()
	})
}

// showOwned is the one place setup narration is painted. A nil condition always
// shows; a condition that fails drops the narration, as the old pushIf did.
// A successful show marks this process as having narrated, which is what lets
// SweepStaleOverlay tell a stale leftover overlay from its own live one, and
// clears hiddenIntent: there is now a shown overlay again for Resync to
// replay through the ordinary ctrl.Current() path.
//
// The write runs inside the CommitHook, under the controller's own decision
// lock, not after ShowIf returns — same reasoning as hideIf's CommitHook use:
// a concurrent hideIf racing this show can return before or after it
// regardless of which one actually committed last, and only the hook's
// in-lock invocation order matches true commit order (round 6 review, F4).
func (s *Service) showOwned(req map[string]any, pr overlay.Priority, ok func(last map[string]any) bool) {
	var cond overlay.Condition
	if ok != nil {
		cond = func(cur overlay.Overlay, has bool) bool {
			return ok(narrationOf(cur, has))
		}
	}
	_, _, _ = s.ctrl.ShowIf(context.Background(), s, overlay.Overlay{
		Kind:    setupKind(stringField(req, "state")),
		Payload: req,
	}, pr, cond, func() {
		s.mu.Lock()
		s.narrated = true
		s.hiddenIntent = false
		s.mu.Unlock()
	})
}
