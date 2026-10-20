package hub

import (
	"context"
	"encoding/json"
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
	HandleEnvelope(ctx context.Context, raw json.RawMessage) (*controlgate.Wire, *control.PublicError)
	Plaintext(route control.Route, cmd commands.Command) control.Decision
	State() control.State
	Status() control.Status
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
	wire, perr := h.control.HandleEnvelope(h.ctx, raw)
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
// owner (or is locked). A screenshot has no envelope form — it is a raw PNG
// GET — and the screen can carry pairing material, so the route answers 403
// enrollment_required in every state but legacy. In legacy the phase-0 topic
// gate still guards it.
func (h *hub) refuseScreenshotUnlessLegacy(w http.ResponseWriter) bool {
	if h.control == nil || h.control.State() == control.StateLegacy {
		return false
	}
	writeControlError(w, control.CodeEnrollmentRequired)
	return true
}
