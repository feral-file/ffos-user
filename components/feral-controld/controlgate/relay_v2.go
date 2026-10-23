package controlgate

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
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
	// pending is set on a new binding until the reply to the request that
	// made it has gone out: its queue is held (a client pins the peer from
	// that reply and would drop anything earlier) and it is then sent the
	// current status snapshots first.
	pending bool
	release chan struct{}
}

// RelayV2 connects an opaque carrier to the existing authority. Bindings are
// created only by Gate after admission; presence never confers authority.
type RelayV2 struct {
	gate   *Gate
	logger *zap.Logger
	source func(context.Context) (v2Peer, bool)

	// mu also orders every seal and enqueue, so the seq of the copies one
	// binding receives follows the order the notifications were produced.
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

// Watch makes another gate's admissions (the LAN and legacy relayer routes)
// retire v2 bindings: a revocation or reactivation counts from every route
// (§9). Call before serving.
func (r *RelayV2) Watch(g *Gate) { g.onAdmitted = r.grantsChanged }

func (r *RelayV2) grantsChanged(_ context.Context, adm *control.Admission) {
	for _, ctl := range adm.RevokedCtls {
		r.unbindCtl(ctl)
	}
	for _, ctl := range adm.ReactivatedCtls {
		r.unbindCtl(ctl)
	}
}

// admitted binds the requesting peer to the principal of every envelope whose
// replay window accepted a fresh ctr: commands the caller executes and those
// the authority answers itself (list, approve, revoke, registerController, an
// approved enroll). The authority sets Principal only when it conferred
// authority, so an authenticated refusal (permission_denied, access_revoked)
// binds nothing even though it spent its ctr, and neither does a replay. An
// internal_error reply means the window write did not commit. A self-revoke
// binds nothing, so the peer keeps any binding it had.
func (r *RelayV2) admitted(ctx context.Context, adm *control.Admission) {
	ctl := adm.Principal.ControllerID
	if ctl != "" && adm.Code != control.CodeInternalError && !slices.Contains(adm.RevokedCtls, ctl) {
		r.bind(ctx, ctl)
	}
	r.grantsChanged(ctx, adm)
}

func (r *RelayV2) bind(ctx context.Context, ctl string) {
	peer, ok := r.source(ctx)
	if !ok {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	// Both rechecked under mu: a revocation committed on another route
	// after this request was admitted unbinds under mu too, so either it
	// sees this binding or this sees the inactive grant.
	if !peer.Live() || !r.gate.auth.GrantActive(ctl) {
		return
	}
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
		cancel: cancel, pending: true, release: make(chan struct{})}
	r.bindings[peer] = b
	go r.drain(bctx, b)
}

// drain sends one binding's queue, once released, until the binding is dropped.
func (r *RelayV2) drain(bctx context.Context, b *v2Binding) {
	select {
	case <-bctx.Done():
		return
	case <-b.release:
	}
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

// unbindCtl drops every binding of ctl.
func (r *RelayV2) unbindCtl(ctl string) {
	r.mu.Lock()
	defer r.mu.Unlock()
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
	// §13: on v2 the client picks the messageID, so "system" is no
	// relayer-originated frame and MUST be a no-op: not run, not answered.
	if payload.MessageID == relayer.MESSAGE_ID_SYSTEM {
		return nil
	}
	// After the reply (deferred calls run after the Send below), so a client
	// that pins the peer from its first verified reply drops nothing.
	defer r.releaseAfterReply(peer)
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
	return peer.Send(ctx, body)
}

// releaseAfterReply releases a new binding's queue after its reply, first
// queueing the current status snapshots for it (§9).
func (r *RelayV2) releaseAfterReply(peer v2Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	b := r.bindings[peer]
	if b == nil || !b.pending {
		return
	}
	b.pending = false
	types := make([]string, 0, len(r.latest))
	for typ := range r.latest {
		types = append(types, typ)
	}
	sort.Strings(types)
	// Front of the queue: anything queued meanwhile is newer.
	held := make([][]byte, 0, len(b.queue))
	for len(b.queue) > 0 {
		held = append(held, <-b.queue)
	}
	for _, typ := range types {
		if !r.deliverLocked(b.ctl, typ, r.latest[typ], []*v2Binding{b}) {
			break
		}
	}
	for _, wire := range held {
		r.enqueue(b, wire)
	}
	close(b.release)
}

// Publish is the notification producers' path (SealingRelayer). Mint pairing
// stays legacy-only (§9). A snapshot type (one the legacy relayer persists:
// the status types) is deduplicated against what v2 last sent and kept for
// bindings made later; a pending binding gets it with its snapshot instead.
func (r *RelayV2) Publish(typ string, body json.RawMessage, snapshot bool) {
	if strings.HasPrefix(typ, "mint_pairing_") {
		return
	}
	if len(body) == 0 {
		body = json.RawMessage("null")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if snapshot {
		if prev, ok := r.latest[typ]; ok && bytes.Equal(prev, body) {
			return
		}
		r.latest[typ] = append(json.RawMessage(nil), body...)
	}
	r.notifyLocked(typ, body, "", snapshot)
}

// Notify seals once per bound controller and queues the copy for each of its
// live peers; it never waits on a peer. Authority.Seal rechecks current grant
// activity, including expiry and revocation on open sockets.
func (r *RelayV2) Notify(typ string, body any, onlyController string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.notifyLocked(typ, body, onlyController, false)
}

func (r *RelayV2) notifyLocked(typ string, body any, onlyController string, skipPending bool) {
	r.pruneLocked()
	byCtl := make(map[string][]*v2Binding)
	for _, b := range r.bindings {
		if (onlyController == "" || b.ctl == onlyController) && (!skipPending || !b.pending) {
			byCtl[b.ctl] = append(byCtl[b.ctl], b)
		}
	}
	for ctl, bs := range byCtl {
		r.deliverLocked(ctl, typ, body, bs)
	}
}

// deliverLocked seals one copy for ctl and queues it for bs. It reports false
// when ctl's bindings were dropped because its grant is no longer active
// (§9: a later reactivation needs a fresh request); a transient seal failure
// keeps them.
func (r *RelayV2) deliverLocked(ctl, typ string, body any, bs []*v2Binding) bool {
	sealed, err := r.gate.auth.Seal(ctl, typ, body)
	if err == nil {
		var wire []byte
		if wire, err = sealed.Wire(); err == nil {
			for _, b := range bs {
				r.enqueue(b, wire)
			}
			return true
		}
	}
	if !r.gate.auth.GrantActive(ctl) {
		for _, b := range bs {
			r.dropLocked(b)
		}
		return false
	}
	r.logger.Warn("Failed to seal v2 notification", zap.String("notification_type", typ), zap.Error(err))
	return true
}

// Disconnected retires routing state immediately; grants remain authority-owned.
func (r *RelayV2) Disconnected(peer relayer.Peer) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if b := r.bindings[peer]; b != nil {
		r.dropLocked(b)
	}
}
