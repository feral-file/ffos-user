package controlgate_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
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
	// skew moves the device clock ahead, past requestAccess's global
	// interval between new pending requests.
	skew *atomic.Int64
}

func newV2Rig(t *testing.T) *v2Rig {
	skew := &atomic.Int64{}
	a, err := control.New(control.Config{Dir: "/s/control", DeviceID: testDevice, FS: control.NewMemFS(),
		Now: func() time.Time { return time.Now().Add(time.Duration(skew.Load())) }})
	if err != nil || a.LockErr() != nil {
		t.Fatal(err, a.LockErr())
	}
	rec := &recorder{}
	g := controlgate.New(a, rec, zap.NewNop())
	adapter := controlgate.NewRelayV2(g)
	adapter.SetSource(func(ctx context.Context) (controlgate.V2Peer, bool) {
		p, ok := ctx.Value(peerKey{}).(*fakePeer)
		return p, ok
	})
	return &v2Rig{a: a, g: g, rec: rec, adapter: adapter, skew: skew}
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

// §13: a v2 frame labeled messageID "system" is a no-op, whatever it carries:
// no reply, no execution, no binding.
func TestV2SystemMessageIDIsNoop(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	p := newFakePeer()
	deliver := func(cast []byte) {
		t.Helper()
		var payload relayer.Payload
		if err := json.Unmarshal([]byte(`{"messageID":"system","message":`+string(cast)+`}`), &payload); err != nil {
			t.Fatal(err)
		}
		if err := r.adapter.Handle(r.from(p), payload); err != nil {
			t.Fatal(err)
		}
	}
	deliver([]byte(`{"command":"getDeviceStatus","request":{}}`))
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	deliver(s.cast)
	p.quiet(t)
	if r.rec.count() != 0 {
		t.Fatal("a system-labeled v2 frame ran")
	}
	// Nothing was bound: a notification reaches no one.
	r.adapter.Publish("device_status", json.RawMessage(`{"v":1}`), true)
	p.quiet(t)
	// The same envelope under a client messageID is answered and runs.
	if reply := owner.open(s, r.cast(t, p, s.cast)); !reply.OK || r.rec.count() != 1 {
		t.Fatalf("control request after the no-op: %+v", reply)
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

// §9 (Q6): mint pairing goes to owner bindings only on v2, is never a
// snapshot (a decided request must not replay to later bindings), and
// Publish reports whether an owner copy was queued. A controller binding is
// skipped, not dropped.
func TestV2MintPairingToOwnersOnly(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	nk := owner.nkFrom(t, r.g)
	// No owner bound yet: nothing queued.
	if r.adapter.Publish("mint_pairing_approval_request", json.RawMessage(`{"n":0}`), true) {
		t.Fatal("queued with no binding")
	}
	cli := addController(t, r, r.g, owner)
	cliNK := cli.nkFrom(t, r.g)
	p, q := newFakePeer(), newFakePeer()
	for _, c := range []struct {
		c *client
		p *fakePeer
	}{{owner, p}, {cli, q}} {
		s := c.c.build(control.OpCmd, "getDeviceStatus", `{}`, "")
		if reply := c.c.open(s, r.cast(t, c.p, s.cast)); !reply.OK {
			t.Fatalf("bind: %+v", reply)
		}
	}
	if !r.adapter.Publish("mint_pairing_approval_request", json.RawMessage(`{"n":1}`), true) {
		t.Fatal("owner copy not queued")
	}
	if n := openNotification(t, p.next(t), owner, nk); n.Type != "mint_pairing_approval_request" || string(n.Body) != `{"n":1}` {
		t.Fatalf("owner got %+v", n)
	}
	q.quiet(t)
	if len(r.adapter.Bound(cli.ctl)) != 1 {
		t.Fatal("controller binding dropped by an owner-only notification")
	}
	// The controller still gets every other type.
	r.adapter.Publish("device_status", json.RawMessage(`{"v":1}`), true)
	openNotification(t, p.next(t), owner, nk)
	openNotification(t, q.next(t), cli, cliNK)
	// A binding made after the request is sent the status snapshot, never
	// the request.
	p2 := newFakePeer()
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	owner.open(s, r.cast(t, p2, s.cast))
	if n := openNotification(t, p2.next(t), owner, nk); n.Type != "device_status" {
		t.Fatalf("late binding got %+v", n)
	}
	p2.quiet(t)
	// Same body twice is not deduplicated: mint pairing is not a snapshot.
	r.adapter.Publish("mint_pairing_approval_outcome", json.RawMessage(`{"n":2}`), true)
	r.adapter.Publish("mint_pairing_approval_outcome", json.RawMessage(`{"n":2}`), true)
	openNotification(t, p.next(t), owner, nk)
	openNotification(t, p.next(t), owner, nk)
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

// lan runs one envelope on g as the LAN route (no relay peer in ctx).
func lan(t *testing.T, g *controlgate.Gate, c *client, s sent) control.ReplyClaims {
	t.Helper()
	wire, perr := g.HandleEnvelope(context.Background(), s.body, control.Origin{Route: control.RouteLAN, Known: true})
	if perr != nil {
		t.Fatal(perr)
	}
	b, _ := json.Marshal(wire)
	return c.open(s, b)
}

// addController gives a second controller an active controller grant
// (requestAccess, owner approval, enroll) over g's LAN route.
func addController(t *testing.T, r *v2Rig, g *controlgate.Gate, owner *client) *client {
	t.Helper()
	a := r.a
	r.skew.Add(int64(11 * time.Second))
	cli := newClient(t, "cli", *a.Status().Profile)
	if r := lan(t, g, cli, cli.build(control.OpRequestAccess, "", "", "")); !r.OK {
		t.Fatalf("requestAccess: %+v", r)
	}
	approve := owner.build(control.OpCmd, control.CmdApproveController, `{"ctl":"`+cli.ctl+`","role":"controller"}`, "")
	if r := lan(t, g, owner, approve); !r.OK {
		t.Fatalf("approve: %+v %s", r, r.Res)
	}
	if r := lan(t, g, cli, cli.build(control.OpEnroll, "", "", "")); !r.OK {
		t.Fatalf("enroll: %+v", r)
	}
	return cli
}

// §9: a revocation on another route (the watched LAN gate) unbinds, and so
// does a reactivation; a grant revoked where nothing reports it is dropped
// when its next copy cannot be sealed.
func TestV2UnbindsOnRevocationFromEveryRoute(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	watched := controlgate.New(r.a, r.rec, zap.NewNop())
	r.adapter.Watch(watched)
	unwatched := controlgate.New(r.a, r.rec, zap.NewNop())
	bindAs := func(c *client, p *fakePeer) {
		t.Helper()
		s := c.build(control.OpCmd, "getDeviceStatus", `{}`, "")
		if reply := c.open(s, r.cast(t, p, s.cast)); !reply.OK {
			t.Fatalf("v2 request: %+v", reply)
		}
	}
	revoke := func(g *controlgate.Gate, c *client) {
		t.Helper()
		s := owner.build(control.OpCmd, control.CmdRevokeController, `{"ctl":"`+c.ctl+`"}`, "")
		if reply := lan(t, g, owner, s); !reply.OK {
			t.Fatalf("revoke: %+v %s", reply, reply.Res)
		}
	}

	cli := addController(t, r, unwatched, owner)
	p := newFakePeer()
	bindAs(cli, p)
	revoke(watched, cli)
	if len(r.adapter.Bound(cli.ctl)) != 0 {
		t.Fatal("LAN revocation left the v2 binding")
	}

	cli2 := addController(t, r, unwatched, owner)
	p2 := newFakePeer()
	bindAs(cli2, p2)
	revoke(unwatched, cli2)
	if len(r.adapter.Bound(cli2.ctl)) != 1 {
		t.Fatal("unwatched revocation reached the binding layer")
	}
	r.adapter.Notify("device_status", map[string]any{}, "")
	if len(r.adapter.Bound(cli2.ctl)) != 0 {
		t.Fatal("binding with an inactive grant kept")
	}
	p2.quiet(t)
	// The request of a revoked grant binds nothing (the race guard).
	r.adapter.Admitted(r.from(p2), &control.Admission{Principal: control.Principal{ControllerID: cli2.ctl}})
	if len(r.adapter.Bound(cli2.ctl)) != 0 {
		t.Fatal("inactive grant bound")
	}

	po := newFakePeer()
	bindAs(owner, po)
	r.adapter.GrantsChanged(context.Background(), &control.Admission{ReactivatedCtls: []string{owner.ctl}})
	if len(r.adapter.Bound(owner.ctl)) != 0 {
		t.Fatal("reactivated ctl kept its old binding")
	}
}

// A self-revoke sent from a peer bound to another controller leaves that
// binding alone.
func TestV2SelfRevokeKeepsPeersOtherBinding(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	cli := addController(t, r, r.g, owner)
	p := newFakePeer()
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	owner.open(s, r.cast(t, p, s.cast))
	s = cli.build(control.OpCmd, control.CmdRevokeController, `{"ctl":"`+cli.ctl+`"}`, "")
	if reply := cli.open(s, r.cast(t, p, s.cast)); !reply.OK {
		t.Fatalf("self-revoke: %+v %s", reply, reply.Res)
	}
	if b := r.adapter.Bound(owner.ctl); len(b) != 1 || b[0] != controlgate.V2Peer(p) {
		t.Fatal("self-revoke dropped the peer's owner binding")
	}
}

// A new binding receives nothing before the reply to the request that made
// it; once released it gets the snapshot first, then what was held.
func TestV2NewBindingHeldUntilReply(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	nk := owner.nkFrom(t, r.g)
	p := newFakePeer()
	r.adapter.Admitted(r.from(p), &control.Admission{Principal: control.Principal{ControllerID: owner.ctl}})
	r.adapter.Notify("controller_approval_request", map[string]any{"n": 1}, "")
	r.adapter.Publish("device_status", json.RawMessage(`{"v":1}`), true)
	p.quiet(t)
	r.adapter.ReleaseAfterReply(p)
	if n := openNotification(t, p.next(t), owner, nk); n.Type != "device_status" {
		t.Fatalf("first after release: %+v", n)
	}
	if n := openNotification(t, p.next(t), owner, nk); n.Type != "controller_approval_request" {
		t.Fatalf("held notification: %+v", n)
	}
	p.quiet(t)
}

// PR review F1: a mint pairing copy that an owner's full queue dropped was
// never queued, so Publish must say so, or the producer would report the
// approval delivered while the browser waits for nothing.
func TestV2MintPairingNotQueuedWhenOwnerQueueIsFull(t *testing.T) {
	r := newV2Rig(t)
	owner := enrollOwner(t, r.g, r.a)
	p := newFakePeer()
	s := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	owner.open(s, r.cast(t, p, s.cast))
	p.block = true
	legacy := &toggleRelayer{connected: make(chan struct{})}
	wrapped := controlgate.WrapRelayer(legacy, r.a, zap.NewNop())
	wrapped.NotifyV2 = func(typ string, body json.RawMessage, snapshot bool) bool {
		return r.adapter.Publish(typ, body, snapshot)
	}
	send := func() error {
		return wrapped.Send(context.Background(), map[string]any{"type": "notification", "notification_type": "mint_pairing_approval_request",
			"message": map[string]any{}, "persist_record_count": 10})
	}
	if err := send(); err != nil {
		t.Fatalf("first copy, queue empty: %v", err)
	}
	full := false
	for i := 0; i < 64 && !full; i++ {
		full = errors.Is(send(), relayer.ErrNotConnected)
	}
	if !full {
		t.Fatal("a blocked owner's queue never reported a dropped copy")
	}
	r.adapter.Admitted(context.Background(), &control.Admission{RevokedCtls: []string{owner.ctl}})
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
