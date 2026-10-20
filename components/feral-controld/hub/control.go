package hub

import (
	"context"
	"encoding/json"
	"net"
	"net/http"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
)

// ControlGate is the hub's view of the feral-file#3551 control gate
// (controlgate.Gate). Nil — the control.enabled=false default — leaves every
// route exactly as it was before the protocol: an ff1Envelope cast then falls
// through to the router like any unknown command.
type ControlGate interface {
	HandleEnvelope(ctx context.Context, raw json.RawMessage, origin control.Origin) (*controlgate.Wire, *control.PublicError)
	Plaintext(route control.Route, cmd commands.Command) (control.Decision, commands.Command)
	State() control.State
	Status() control.Status
	ScreenProtected() bool
}

// SetControl wires the control gate. Call before Start.
func (h *hub) SetControl(g ControlGate) { h.control = g }

// writeControlError writes a §4.4 public error: {"error":{"code":…}} with
// 401, or 403 for enrollment_required / envelope_required.
func writeControlError(w http.ResponseWriter, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(control.PublicErrorStatus(code))
	_, _ = w.Write(append(control.PublicErrorBody(code), '\n'))
}

// serveEnvelope answers an ff1Envelope cast (§10): 200 with {"v":1,"jwe"}
// for every authenticated outcome (encrypted errors included), or the public
// error in clear.
func (h *hub) serveEnvelope(w http.ResponseWriter, r *http.Request, payload commands.Command) {
	raw := payload.RawArguments
	if len(raw) == 0 {
		writeControlError(w, control.CodeBadEnvelope)
		return
	}
	// The peer address is not authority; it only feeds the per-host
	// pending-request cap (§5.2).
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	wire, perr := h.control.HandleEnvelope(h.ctx, raw, control.Origin{Route: control.RouteLAN, Remote: host, Known: true})
	if perr != nil {
		h.logger.Warn("Hub envelope refused", zap.String("code", perr.Code), zap.String("remote_addr", r.RemoteAddr))
		writeControlError(w, perr.Code)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if err := h.respondJSON(w, http.StatusOK, wire); err != nil {
		h.logger.Warn("Failed to write envelope reply", zap.Error(err))
	}
}

// refuseScreenshotUnlessLegacy closes /api/screenshot once the device has an
// owner (or is locked): a screenshot has no envelope form — it is a raw PNG
// GET — so the route answers 403 enrollment_required in every state but
// legacy. In legacy it answers 403 screen_protected while an invitation is
// live: the claim QR then carries it, and an unclaimed device has no topic
// gate, so any LAN host could read the invitation off a screenshot and
// become the sole owner (review F1). Otherwise the phase-0 topic gate still
// guards the route.
func (h *hub) refuseScreenshotUnlessLegacy(w http.ResponseWriter) bool {
	if h.control == nil {
		return false
	}
	if h.control.State() != control.StateLegacy {
		writeControlError(w, control.CodeEnrollmentRequired)
		return true
	}
	if h.control.ScreenProtected() {
		writeControlError(w, control.CodeScreenProtected)
		return true
	}
	return false
}
