package controlgate

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// v2 binding limits (§9).
const (
	// v2PeersPerCtl caps the connections one controller is bound to; a new
	// binding evicts the least recently bound one.
	v2PeersPerCtl = 2
	// v2QueueDepth bounds the notifications waiting for one peer. A full
	// queue drops: notifications are lossy and a controller refetches state
	// by envelope.
	v2QueueDepth = 16
	// v2SendTimeout bounds one queued send, so a peer that never acknowledges
	// holds up its own queue only.
	v2SendTimeout = 10 * time.Second
)

// v2Peer is the routing handle of one relay source (relayer.Peer).
type v2Peer interface {
	Live() bool
	Send(context.Context, []byte) error
}

// v2Binding is one peer→ctl binding and the queue that feeds it.
type v2Binding struct {
	peer   v2Peer
	ctl    string
	tick   uint64 // last bind, for eviction
	queue  chan []byte
	cancel context.CancelFunc
	// snapshot is set on a new binding: the controller is owed the current
	// status snapshots once its reply has gone out.
	snapshot bool
}

// RelayV2 connects an opaque carrier to the existing authority. Bindings are
// created only by Gate after admission; presence never confers authority.
type RelayV2 struct {
	gate   *Gate
	logger *zap.Logger
	source func(context.Context) (v2Peer, bool)

	mu       sync.Mutex
	bindings map[v2Peer]*v2Binding
	tick     uint64
	// latest is the newest body of each snapshot (persisted) notification
	// type that went out on v2: v2's own dedupe, and what a new binding is
	// sent. It is separate from the status poller's per-channel hashes.
	latest map[string]json.RawMessage
}

// NewRelayV2 installs the transport binding hook on a dedicated v2 gate.
func NewRelayV2(gate *Gate) *RelayV2 {
	r := &RelayV2{
		gate:     gate,
		logger:   gate.logger,
		bindings: make(map[v2Peer]*v2Binding),
		latest:   make(map[string]json.RawMessage),
		source: func(ctx context.Context) (v2Peer, bool) {
			p, ok := relayer.SourcePeer(ctx)
			return p, ok
		},
	}
	gate.onAdmitted = r.admitted
	return r
}

// admitted binds the requesting peer to the principal of every envelope whose
// replay window accepted a fresh ctr: commands the caller executes and those
// the authority answers itself (list, approve, revoke, registerController, an
// approved enroll). The authority sets Principal only when it conferred
// authority, so an authenticated refusal (permission_denied, access_revoked)
// binds nothing even though it spent its ctr, and neither does a replay. An
// internal_error reply means the window write did not commit.
func (r *RelayV2) admitted(ctx context.Context, adm *control.Admission) {
	if adm.Principal.ControllerID != "" && adm.Code != control.CodeInternalError {
		r.bind(ctx, adm.Principal.ControllerID)
	}
	for _, ctl := range adm.RevokedCtls {
		r.unbindCtl(ctl, nil)
	}
}

func (r *RelayV2) bind(ctx context.Context, ctl string) {
	peer, ok := r.source(ctx)
	if !ok || !peer.Live() {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pruneLocked()
	r.tick++
	if b := r.bindings[peer]; b != nil {
		if b.ctl == ctl {
			b.tick = r.tick
			return
		}
		// One connection carries one controller's notifications.
		r.dropLocked(b)
	}
	for {
		var oldest *v2Binding
		n := 0
		for _, b := range r.bindings {
			if b.ctl != ctl {
				continue
			}
			n++
			if oldest == nil || b.tick < oldest.tick {
				oldest = b
			}
		}
		if n < v2PeersPerCtl {
			break
		}
		r.dropLocked(oldest)
	}
	// The binding outlives the request that made it: its sender stops when
	// the binding is dropped, never when the request ends.
	bctx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	b := &v2Binding{peer: peer, ctl: ctl, tick: r.tick, queue: make(chan []byte, v2QueueDepth),
		cancel: cancel, snapshot: true}
	r.bindings[peer] = b
	go r.drain(bctx, b)
}

// drain sends one binding's queue until the binding is dropped.
func (r *RelayV2) drain(bctx context.Context, b *v2Binding) {
	for {
		select {
		case <-bctx.Done():
			return
		case wire := <-b.queue:
			ctx, cancel := context.WithTimeout(bctx, v2SendTimeout)
			err := b.peer.Send(ctx, wire)
			cancel()
			if err != nil {
				r.logger.Debug("V2 notification not delivered", zap.Error(err))
			}
		}
	}
}

func (r *RelayV2) enqueue(b *v2Binding, wire []byte) {
	select {
	case b.queue <- wire:
	default:
		r.logger.Info("V2 notification queue full; dropping notification")
	}
}

// dropLocked retires a binding and stops its sender.
func (r *RelayV2) dropLocked(b *v2Binding) {
	if r.bindings[b.peer] == b {
		delete(r.bindings, b.peer)
	}
	b.cancel()
}

func (r *RelayV2) pruneLocked() {
	for _, b := range r.bindings {
		if !b.peer.Live() {
			r.dropLocked(b)
		}
	}
}

// unbindCtl drops ctl's bindings: all of them, or only those in only (the
// bindings a failed seal was for, so a fresh binding made meanwhile stays).
func (r *RelayV2) unbindCtl(ctl string, only []*v2Binding) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if only != nil {
		for _, b := range only {
			r.dropLocked(b)
		}
		return
	}
	for _, b := range r.bindings {
		if b.ctl == ctl {
			r.dropLocked(b)
		}
	}
}

// Handle admits every v2 request through RouteRelayV2. It never dispatches legacy
// system messages, plaintext commands or public-profile shortcuts.
func (r *RelayV2) Handle(ctx context.Context, payload relayer.Payload) error {
	peer, ok := r.source(ctx)
	// §10: a frame with no command is never answered, and nothing without a
	// messageID has a caller to answer.
	if !ok || payload.MessageID == "" || payload.Message.Command == nil {
		return nil
	}
	var result any
	switch {
	case *payload.Message.Command != control.CmdEnvelope:
		// §7.2: every plaintext command is refused on v2, with the code of
		// the current state (or invalid_message for an ambiguous message).
		d, _ := r.gate.Plaintext(control.RouteRelayV2, commands.Command{
			Type:         commands.Type(*payload.Message.Command),
			RawArguments: payload.Message.RawRequest,
			Ambiguous:    payload.Message.Ambiguous,
		})
		code := d.Code
		if d.Allow {
			code = control.CodeEnvelopeRequired // unreachable: the v2 policy allows nothing
		}
		result = json.RawMessage(control.PublicErrorBody(code))
	case payload.Message.Ambiguous:
		result = json.RawMessage(control.PublicErrorBody(control.CodeBadEnvelope))
	default:
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
	err = peer.Send(ctx, body)
	// After the reply, so a client that pins the peer from its first verified
	// reply does not drop the snapshot as coming from an unpinned peer.
	r.sendSnapshot(peer)
	return err
}

// sendSnapshot queues the current status snapshots for a new binding (§9).
func (r *RelayV2) sendSnapshot(peer v2Peer) {
	r.mu.Lock()
	b := r.bindings[peer]
	if b == nil || !b.snapshot {
		r.mu.Unlock()
		return
	}
	b.snapshot = false
	types := make([]string, 0, len(r.latest))
	for typ := range r.latest {
		types = append(types, typ)
	}
	sort.Strings(types)
	bodies := make([]json.RawMessage, len(types))
	for i, typ := range types {
		bodies[i] = r.latest[typ]
	}
	r.mu.Unlock()
	for i, typ := range types {
		r.deliver(b.ctl, typ, bodies[i], []*v2Binding{b})
	}
}

// Publish is the notification producers' path (SealingRelayer). Mint pairing
// stays legacy-only (§9). A snapshot type (one the legacy relayer persists:
// the status types) is deduplicated against what v2 last sent and kept for
// bindings made later.
func (r *RelayV2) Publish(typ string, body json.RawMessage, snapshot bool) {
	if strings.HasPrefix(typ, "mint_pairing_") {
		return
	}
	if len(body) == 0 {
		body = json.RawMessage("null")
	}
	if snapshot {
		r.mu.Lock()
		if prev, ok := r.latest[typ]; ok && bytes.Equal(prev, body) {
			r.mu.Unlock()
			return
		}
		r.latest[typ] = append(json.RawMessage(nil), body...)
		r.mu.Unlock()
	}
	r.Notify(typ, body, "")
}

// Notify seals once per bound controller and queues the copy for each of its
// live peers; it never waits on a peer. Authority.Seal rechecks current grant
// activity, including expiry and revocation on open sockets.
func (r *RelayV2) Notify(typ string, body any, onlyController string) {
	r.mu.Lock()
	r.pruneLocked()
	byCtl := make(map[string][]*v2Binding)
	for _, b := range r.bindings {
		if onlyController == "" || b.ctl == onlyController {
			byCtl[b.ctl] = append(byCtl[b.ctl], b)
		}
	}
	r.mu.Unlock()
	for ctl, bs := range byCtl {
		r.deliver(ctl, typ, body, bs)
	}
}

func (r *RelayV2) deliver(ctl, typ string, body any, bs []*v2Binding) {
	sealed, err := r.gate.auth.Seal(ctl, typ, body)
	if err != nil {
		// An inactive grant: a later reactivation must not resume
		// notifications on these sockets without a fresh request (§9).
		r.unbindCtl(ctl, bs)
		return
	}
	wire, err := sealed.Wire()
	if err != nil {
		r.logger.Warn("Failed to encode v2 notification", zap.String("notification_type", typ), zap.Error(err))
		return
	}
	for _, b := range bs {
		r.enqueue(b, wire)
	}
}

// Disconnected retires routing state immediately; grants remain authority-owned.
func (r *RelayV2) Disconnected(peer relayer.Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.bindings[peer]; b != nil {
		r.dropLocked(b)
	}
}
