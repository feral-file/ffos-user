package controlgate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// fakePeer is a relay source. Send records every frame; with block set it
// instead waits for its context, as a peer that never acknowledges would.
type fakePeer struct {
	got      chan []byte
	block    bool
	canceled chan struct{}
	once     sync.Once
}

func newFakePeer() *fakePeer {
	return &fakePeer{got: make(chan []byte, 64), canceled: make(chan struct{})}
}

func (p *fakePeer) Live() bool { return true }

func (p *fakePeer) Send(ctx context.Context, b []byte) error {
	if p.block {
		<-ctx.Done()
		if errors.Is(ctx.Err(), context.Canceled) {
			p.once.Do(func() { close(p.canceled) })
		}
		return ctx.Err()
	}
	p.got <- b
	return nil
}

func (p *fakePeer) next(t *testing.T) map[string]json.RawMessage {
	t.Helper()
	select {
	case b := <-p.got:
		var m map[string]json.RawMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		return m
	case <-time.After(2 * time.Second):
		t.Fatal("peer got nothing")
		return nil
	}
}

func (p *fakePeer) quiet(t *testing.T) {
	t.Helper()
	select {
	case b := <-p.got:
		t.Fatalf("unexpected frame %s", b)
	case <-time.After(150 * time.Millisecond):
	}
}

type peerKey struct{}

type v2Rig struct {
	a       *control.Authority
	g       *controlgate.Gate
	rec     *recorder
	adapter *controlgate.RelayV2
}

func newV2Rig(t *testing.T) *v2Rig {
	a, _ := newAuthority(t, false)
	rec := &recorder{}
	g := controlgate.New(a, rec, zap.NewNop())
	adapter := controlgate.NewRelayV2(g)
	adapter.SetSource(func(ctx context.Context) (controlgate.V2Peer, bool) {
		p, ok := ctx.Value(peerKey{}).(*fakePeer)
		return p, ok
	})
	return &v2Rig{a: a, g: g, rec: rec, adapter: adapter}
}

func (r *v2Rig) from(p *fakePeer) context.Context {
	return context.WithValue(context.Background(), peerKey{}, p)
}

// cast delivers one v2 message from p and returns the RPC message it was
// answered with (nil when none).
func (r *v2Rig) cast(t *testing.T, p *fakePeer, cast []byte) json.RawMessage {
	t.Helper()
	var payload relayer.Payload
	if err := json.Unmarshal([]byte(`{"messageID":"m1","message":`+string(cast)+`}`), &payload); err != nil {
		t.Fatal(err)
	}
	if err := r.adapter.Handle(r.from(p), payload); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-p.got:
		var resp struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
		}
		if err := json.Unmarshal(b, &resp); err != nil || resp.Type != "RPC" {
			t.Fatalf("not an RPC reply: %s", b)
		}
		return resp.Message
	default:
		return nil
	}
}

func openNotification(t *testing.T, frame map[string]json.RawMessage, c *client, nk []byte) control.NotificationClaims {
	t.Helper()
	if string(frame["notification_type"]) != `"sealed"` {
		t.Fatalf("not a sealed notification: %v", frame)
	}
	var w struct {
		JWE string `json:"jwe"`
	}
	if err := json.Unmarshal(frame["message"], &w); err != nil {
		t.Fatal(err)
	}
	n, err := control.OpenNotification(w.JWE, c.ctl, nk)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// §7.2: plaintext on v2 is refused with the code of the state, never run.
func TestV2PlaintextRefusedByState(t *testing.T) {
	r := newV2Rig(t)
	p := newFakePeer()
	code := func(msg json.RawMessage) string {
		var e struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		_ = json.Unmarshal(msg, &e)
		return e.Error.Code
	}
	if got := code(r.cast(t, p, []byte(`{"command":"getDeviceStatus","request":{}}`))); got != control.CodeEnrollmentRequired {
		t.Fatalf("legacy plaintext on v2: %q", got)
	}
	enrollOwner(t, r.g, r.a)
	if got := code(r.cast(t, p, []byte(`{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{}}}`))); got != control.CodeEnvelopeRequired {
		t.Fatalf("enrolled plaintext on v2: %q", got)
	}
	if got := code(r.cast(t, p, []byte(`{"command":"getDeviceStatus","request":{},"Request":{}}`))); got != control.CodeInvalidMessage {
		t.Fatalf("ambiguous plaintext on v2: %q", got)
	}
	if msg := r.cast(t, p, []byte(`{"error":{"code":2000}}`)); msg != nil {
		t.Fatalf("a frame without a command was answered: %s", msg)
	}
	if r.rec.count() != 0 {
		t.Fatal("plaintext ran on v2")
	}
}

// §9: an op the authority answers itself binds like an executed command, and
// the binding is followed by the current status snapshot after the reply.
func TestV2BindsOnAuthorityAnsweredOpAndSendsSnapshot(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	nk := owner.nkFrom(t, r.g)
	r.adapter.Publish("device_status", json.RawMessage(`{"v":1}`), true)
	p := newFakePeer()
	s := owner.build(control.OpCmd, control.CmdListControllers, `{}`, "")
	if reply := owner.open(s, r.cast(t, p, s.cast)); !reply.OK {
		t.Fatalf("listControllers: %+v", reply)
	}
	if r.rec.count() != 0 {
		t.Fatal("listControllers reached the router")
	}
	if n := openNotification(t, p.next(t), owner, nk); n.Type != "device_status" || string(n.Body) != `{"v":1}` {
		t.Fatalf("snapshot: %+v", n)
	}
	// v2 dedupes on its own: the same body is not sent again, a new one is.
	r.adapter.Publish("device_status", json.RawMessage(`{"v":1}`), true)
	p.quiet(t)
	r.adapter.Publish("device_status", json.RawMessage(`{"v":2}`), true)
	if n := openNotification(t, p.next(t), owner, nk); string(n.Body) != `{"v":2}` {
		t.Fatalf("update: %+v", n)
	}
	// A second request on the same binding owes no second snapshot.
	s = owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	if reply := owner.open(s, r.cast(t, p, s.cast)); !reply.OK {
		t.Fatalf("getDeviceStatus: %+v", reply)
	}
	p.quiet(t)
}

// §9: mint pairing is legacy-only.
func TestV2NeverCarriesMintPairing(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	p := newFakePeer()
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	owner.open(s, r.cast(t, p, s.cast))
	r.adapter.Publish("mint_pairing_approval_request", json.RawMessage(`{}`), true)
	r.adapter.Publish("mint_pairing_approval_outcome", json.RawMessage(`{}`), true)
	p.quiet(t)
}

// §9: at most 2 peers per ctl; a third evicts the least recently bound.
func TestV2BindingCapEvictsOldest(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	peers := []*fakePeer{newFakePeer(), newFakePeer(), newFakePeer()}
	for _, p := range peers {
		s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
		owner.open(s, r.cast(t, p, s.cast))
	}
	bound := r.adapter.Bound(owner.ctl)
	if len(bound) != 2 {
		t.Fatalf("bound %d peers", len(bound))
	}
	for _, b := range bound {
		if b == controlgate.V2Peer(peers[0]) {
			t.Fatal("oldest binding survived")
		}
	}
	// A replay from the evicted peer does not rebind it.
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	owner.open(s, r.cast(t, peers[1], s.cast))
	r.cast(t, peers[0], s.cast)
	for _, b := range r.adapter.Bound(owner.ctl) {
		if b == controlgate.V2Peer(peers[0]) {
			t.Fatal("replay bound a peer")
		}
	}
}

// §9: RevokedCtls unbinds, and a binding whose Seal fails is dropped.
func TestV2UnbindsOnRevokeAndSealFailure(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	p := newFakePeer()
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	owner.open(s, r.cast(t, p, s.cast))
	if len(r.adapter.Bound(owner.ctl)) != 1 {
		t.Fatal("not bound")
	}
	r.adapter.Admitted(context.Background(), &control.Admission{RevokedCtls: []string{owner.ctl}})
	if len(r.adapter.Bound(owner.ctl)) != 0 {
		t.Fatal("revoked ctl still bound")
	}
	r.adapter.Admitted(r.from(p), &control.Admission{Principal: control.Principal{ControllerID: "ghost"}})
	if len(r.adapter.Bound("ghost")) != 1 {
		t.Fatal("admitted principal not bound")
	}
	r.adapter.Notify("device_status", map[string]any{}, "")
	if len(r.adapter.Bound("ghost")) != 0 {
		t.Fatal("binding with an inactive grant kept")
	}
	p.quiet(t)
}

// Review item 12: a peer that never acknowledges stalls neither the producer
// nor PublishAdmission, its queue drops when full, and its sender exits when
// the binding goes.
func TestV2SendsNeverBlockProducers(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	p := newFakePeer()
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	owner.open(s, r.cast(t, p, s.cast))
	p.block = true
	start := time.Now()
	for i := 0; i < 100; i++ {
		r.adapter.Publish("player_status", json.RawMessage(`{"i":`+strings.Repeat("1", i+1)+`}`), true)
	}
	if d := time.Since(start); d > time.Second {
		t.Fatalf("producer blocked for %v", d)
	}
	r.adapter.Admitted(context.Background(), &control.Admission{RevokedCtls: []string{owner.ctl}})
	select {
	case <-p.canceled:
	case <-time.After(2 * time.Second):
		t.Fatal("sender kept waiting after the binding was dropped")
	}
}
