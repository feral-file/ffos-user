// Package controlgate is the glue between the transport-independent control
// authority (package control, docs/ff1-control-v1.md) and controld's
// transports and router. The hub and the mediator hand it ff1Envelope bodies
// and plaintext commands; it opens envelopes, runs admitted commands through
// commandrouter as the authenticated Principal, builds the encrypted reply,
// and decides the plaintext policy. It also wraps the notification producers'
// relayer and WebSocket handles so every notification is sealed per grant
// (§9) without each producer knowing about the protocol.
//
// Nothing here is constructed when control.enabled is false: the transports
// then behave exactly as before this package existed.
package controlgate

import (
	"context"
	"encoding/json"
	"errors"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// Wire is the §10 reply object on both routes: {"v":1,"jwe":"<compact>"}.
type Wire struct {
	V   int    `json:"v"`
	JWE string `json:"jwe"`
}

// Gate is the per-daemon control gate.
type Gate struct {
	auth    *control.Authority
	handler commandrouter.Handler
	logger  *zap.Logger

	publisher       *Publisher
	onOwnerEnrolled func()
}

// New builds a gate over auth that executes admitted commands on handler
// (the storm-gated router, the same one plaintext commands use).
func New(auth *control.Authority, handler commandrouter.Handler, logger *zap.Logger) *Gate {
	return &Gate{auth: auth, handler: handler, logger: logger}
}

// Authority returns the underlying authority.
func (g *Gate) Authority() *control.Authority { return g.auth }

// SetPublisher wires where sealed notifications produced by the authority
// itself (controller_approval_request) are sent. Call before serving.
func (g *Gate) SetPublisher(p *Publisher) { g.publisher = p }

// SetOwnerEnrolledHook is called (on the request goroutine) after the first
// owner enrolled with the invitation, so the claim QR can come down (§5.1).
func (g *Gate) SetOwnerEnrolledHook(fn func()) { g.onOwnerEnrolled = fn }

// State is the current control state.
func (g *Gate) State() control.State { return g.auth.State() }

// Status is the §11 status block.
func (g *Gate) Status() control.Status { return g.auth.Status() }

// Plaintext applies the §7.2 plaintext policy to a non-envelope command.
func (g *Gate) Plaintext(route control.Route, cmd commands.Command) control.Decision {
	return g.auth.Policy(route, string(cmd.Type), cmd.RawArguments)
}

// envelopeRequest is the ff1Envelope `request` object (§10), exactly.
type envelopeRequest struct {
	V   int    `json:"v"`
	JWE string `json:"jwe"`
}

func parseEnvelopeRequest(raw json.RawMessage) (string, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil || len(m) != 2 {
		return "", errors.New("ff1Envelope request must be exactly {v, jwe}")
	}
	var req envelopeRequest
	if err := json.Unmarshal(raw, &req); err != nil || req.V != 1 || req.JWE == "" {
		return "", errors.New("ff1Envelope request must be {\"v\":1,\"jwe\":string}")
	}
	if _, ok := m["v"]; !ok {
		return "", errors.New("ff1Envelope request missing v")
	}
	return req.JWE, nil
}

// HandleEnvelope opens one ff1Envelope request (the `request` object of the
// cast, raw) and, when admitted, executes it. It returns the wire reply, or a
// public error to send in clear (HTTP status / RPC message per §4.4).
func (g *Gate) HandleEnvelope(ctx context.Context, raw json.RawMessage) (*Wire, *control.PublicError) {
	compact, err := parseEnvelopeRequest(raw)
	if err != nil {
		return nil, &control.PublicError{Code: control.CodeBadEnvelope, Err: err}
	}
	adm, err := g.auth.Open(compact)
	if err != nil {
		var pe *control.PublicError
		if errors.As(err, &pe) {
			g.logger.Warn("Control envelope refused", zap.String("code", pe.Code), zap.Error(pe.Err))
			return nil, pe
		}
		return nil, &control.PublicError{Code: control.CodeBadEnvelope, Err: err}
	}
	if len(adm.Notifications) > 0 && g.publisher != nil {
		g.publisher.PublishSealed(ctx, adm.Notifications)
	}
	if adm.OwnerEnrolled && g.onOwnerEnrolled != nil {
		g.onOwnerEnrolled()
	}
	if !adm.Execute() {
		g.logger.Info("Control request answered by the authority",
			zap.String("op", string(adm.Op)), zap.String("command", adm.Command),
			zap.String("controller", adm.Principal.ControllerID), zap.String("code", adm.Code))
		return &Wire{V: 1, JWE: adm.Final}, nil
	}
	g.logger.Info("Control request admitted",
		zap.String("command", adm.Command), zap.String("controller", adm.Principal.ControllerID),
		zap.String("role", string(adm.Principal.Role)))
	reply, err := g.execute(ctx, adm)
	if err != nil {
		// Only an entropy/encoding failure gets here: nothing can be
		// encrypted, so the caller gets a public error.
		g.logger.Error("Failed to build control reply", zap.Error(err))
		return nil, &control.PublicError{Code: control.CodeBadEnvelope, Err: err}
	}
	return &Wire{V: 1, JWE: reply}, nil
}

// execute runs an admitted command as its principal and seals the reply.
func (g *Gate) execute(ctx context.Context, adm *control.Admission) (string, error) {
	cmd := commands.Command{Type: commands.Type(adm.Command), RawArguments: adm.Request}
	if err := json.Unmarshal(adm.Request, &cmd.Arguments); err != nil {
		return adm.Reply(false, control.CodeInvalidRequest, nil)
	}
	result, err := g.handler.Process(control.WithPrincipal(ctx, adm.Principal), cmd)
	if err != nil {
		return adm.Reply(false, control.CodeCommandFailed, failureBody(cmd.Type, err))
	}
	if result == nil {
		return adm.Reply(true, "", nil)
	}
	return adm.Reply(true, "", result)
}

// failureBody is the command_failed `res` (§4.3): the same error bodies the
// relayer path returns for these failures today, so a client can share its
// parsing between the legacy and the envelope route.
func failureBody(t commands.Type, err error) map[string]any {
	code := "failed"
	switch {
	case commandrouter.IsRateLimited(err):
		code = "rate_limited"
	case commandrouter.IsSourceUnreachable(err):
		code = "sourceUnreachable"
	case commandrouter.IsContentBlocked(err):
		code = "contentBlocked"
	case commandrouter.IsPlaylistInvalid(err):
		code = "playlistInvalid"
	}
	return map[string]any{"ok": false, "error": code, "command": string(t), "message": err.Error()}
}
