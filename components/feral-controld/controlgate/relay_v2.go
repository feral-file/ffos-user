package controlgate

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// RelayV2 connects an opaque carrier to the existing authority. Bindings are
// created only by Gate after admission; presence never confers authority.
type RelayV2 struct {
	gate     *Gate
	mu       sync.Mutex
	bindings map[relayer.Peer]string
}

// NewRelayV2 installs the transport binding hook on a dedicated v2 gate.
func NewRelayV2(gate *Gate) *RelayV2 {
	r := &RelayV2{gate: gate, bindings: make(map[relayer.Peer]string)}
	gate.onAdmitted = r.bind
	return r
}

func (r *RelayV2) bind(ctx context.Context, ctl string) {
	peer, ok := relayer.SourcePeer(ctx)
	if !ok || !peer.Live() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for p := range r.bindings {
		if !p.Live() {
			delete(r.bindings, p)
		}
	}
	r.bindings[peer] = ctl
}

// Handle admits every v2 request through RouteRelayV2. It never dispatches legacy
// system messages, plaintext commands or public-profile shortcuts.
func (r *RelayV2) Handle(ctx context.Context, payload relayer.Payload) error {
	peer, ok := relayer.SourcePeer(ctx)
	if !ok || payload.MessageID == "" {
		return nil
	}
	var result any
	if payload.Message.Ambiguous || payload.Message.Command == nil || *payload.Message.Command != control.CmdEnvelope {
		result = json.RawMessage(`{"error":{"code":"bad_envelope"}}`)
	} else {
		wire, err := r.gate.HandleEnvelope(ctx, payload.Message.RawRequest, control.Origin{Route: control.RouteRelayV2, Known: true})
		if err != nil {
			result = json.RawMessage(err.Body())
		} else {
			result = wire
		}
	}
	body, err := json.Marshal(relayer.Response{Type: "RPC", MessageID: payload.MessageID, Message: result})
	if err != nil {
		return err
	}
	return peer.Send(ctx, body)
}

// Notify seals independently for each live bound peer. Authority.Seal rechecks
// current grant activity, including expiry and revocation on open sockets.
func (r *RelayV2) Notify(ctx context.Context, typ string, body any, onlyController string) {
	r.mu.Lock()
	peers := make(map[relayer.Peer]string, len(r.bindings))
	for peer, ctl := range r.bindings {
		if !peer.Live() {
			delete(r.bindings, peer)
			continue
		}
		if onlyController == "" || ctl == onlyController {
			peers[peer] = ctl
		}
	}
	r.mu.Unlock()
	for peer, ctl := range peers {
		sealed, err := r.gate.auth.Seal(ctl, typ, body)
		if err != nil {
			continue
		}
		wire, err := sealed.Wire()
		if err == nil {
			_ = peer.Send(ctx, wire)
		}
	}
}

// Disconnected retires routing state immediately; grants remain authority-owned.
func (r *RelayV2) Disconnected(peer relayer.Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.bindings, peer)
}
