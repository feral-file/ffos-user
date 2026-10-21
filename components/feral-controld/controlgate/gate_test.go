package controlgate_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

// enrollOwner runs the invitation ceremony through the gate.
func enrollOwner(t *testing.T, g *controlgate.Gate, a *control.Authority) *client {
	t.Helper()
	inv, err := a.IssueInvitation()
	if err != nil {
		t.Fatal(err)
	}
	owner := newClient(t, "owner", *a.Status().Profile)
	s := owner.build(control.OpEnroll, "", "", inv.Value)
	wire, perr := g.HandleEnvelope(context.Background(), s.body, control.Origin{})
	if perr != nil {
		t.Fatal(perr)
	}
	b, _ := json.Marshal(wire)
	if r := owner.open(s, b); !r.OK {
		t.Fatalf("enroll failed: %+v", r)
	}
	return owner
}

func TestHandleEnvelopeRunsAsPrincipal(t *testing.T) {
	a, _ := newAuthority(t, false)
	rec := &recorder{}
	g := controlgate.New(a, rec, zap.NewNop())
	enrolled := false
	g.SetOwnerEnrolledHook(func() { enrolled = true })
	owner := enrollOwner(t, g, a)
	if !enrolled {
		t.Fatal("owner-enrolled hook not called")
	}
	if rec.count() != 0 {
		t.Fatal("enroll reached the router")
	}

	s := owner.build(control.OpCmd, "displayPlaylist", `{"dp1_call":{"items":[]}}`, "")
	wire, perr := g.HandleEnvelope(context.Background(), s.body, control.Origin{})
	if perr != nil {
		t.Fatal(perr)
	}
	b, _ := json.Marshal(wire)
	r := owner.open(s, b)
	if !r.OK || !strings.Contains(string(r.Res), `"ran":"displayPlaylist"`) {
		t.Fatalf("reply = %+v %s", r, r.Res)
	}
	c := rec.last()
	if !c.hasP || c.principal.ControllerID != owner.ctl || c.principal.Role != control.RoleOwner {
		t.Fatalf("principal = %+v", c)
	}
	if string(c.cmd.RawArguments) != `{"dp1_call":{"items":[]}}` || c.cmd.Arguments["dp1_call"] == nil {
		t.Fatalf("command = %+v", c.cmd)
	}

	// Handler errors become command_failed with the relayer-path body.
	rec.err = &commandrouter.RateLimitedError{Command: "displayPlaylist", Reason: "rate"}
	s = owner.build(control.OpCmd, "displayPlaylist", `{}`, "")
	wire, _ = g.HandleEnvelope(context.Background(), s.body, control.Origin{})
	b, _ = json.Marshal(wire)
	r = owner.open(s, b)
	if r.OK || r.Code != control.CodeCommandFailed || !strings.Contains(string(r.Res), `"error":"rate_limited"`) {
		t.Fatalf("reply = %+v %s", r, r.Res)
	}
	rec.err = errors.New("boom")
	s = owner.build(control.OpCmd, "rotate", `{}`, "")
	wire, _ = g.HandleEnvelope(context.Background(), s.body, control.Origin{})
	b, _ = json.Marshal(wire)
	if r = owner.open(s, b); r.Code != control.CodeCommandFailed || !strings.Contains(string(r.Res), `"error":"failed"`) {
		t.Fatalf("reply = %+v %s", r, r.Res)
	}
}

func TestHandleEnvelopeRequestShape(t *testing.T) {
	a, _ := newAuthority(t, false)
	g := controlgate.New(a, &recorder{}, zap.NewNop())
	for _, body := range []string{``, `null`, `{}`, `{"v":2,"jwe":"x"}`, `{"v":1}`, `{"v":1,"jwe":""}`, `{"v":1,"jwe":"a","x":1}`, `{"jwe":"a","x":1}`} {
		_, perr := g.HandleEnvelope(context.Background(), json.RawMessage(body), control.Origin{})
		if perr == nil || perr.Code != control.CodeBadEnvelope {
			t.Errorf("%q: %v", body, perr)
		}
	}
	_, perr := g.HandleEnvelope(context.Background(), json.RawMessage(`{"v":1,"jwe":"not.a.jwe.at.all"}`), control.Origin{})
	if perr == nil || perr.Code != control.CodeBadEnvelope || perr.HTTPStatus() != 401 {
		t.Fatalf("%+v", perr)
	}
}

func TestPlaintextPolicyFollowsState(t *testing.T) {
	a, _ := newAuthority(t, false)
	g := controlgate.New(a, &recorder{}, zap.NewNop())
	cmd := commands.Command{Type: "factoryReset"}
	if d, _ := g.Plaintext(control.RouteLAN, cmd); !d.Allow {
		t.Fatal("legacy refused plaintext")
	}
	enrollOwner(t, g, a)
	if d, _ := g.Plaintext(control.RouteLAN, commands.Command{Type: "displayPlaylist"}); d.Code != control.CodeEnrollmentRequired {
		t.Fatalf("%+v", d)
	}
	if d, _ := g.Plaintext(control.RouteRelayer, cmd); d.Code != control.CodeEnvelopeRequired {
		t.Fatalf("%+v", d)
	}
	if d, _ := g.Plaintext(control.RouteRelayer, commands.Command{Type: "rotate"}); !d.Allow {
		t.Fatal("controller-tier relayer plaintext refused while enrolled")
	}
}

// fakeRelayer records Send; everything else is unused.
type fakeRelayer struct {
	relayer.Relayer
	mu   sync.Mutex
	sent []json.RawMessage
}

func (f *fakeRelayer) IsConnected() bool { return true }
func (f *fakeRelayer) Send(_ context.Context, data any) error {
	b, _ := json.Marshal(data)
	f.mu.Lock()
	f.sent = append(f.sent, b)
	f.mu.Unlock()
	return nil
}

type fakeWS struct {
	ws.WS
	mu   sync.Mutex
	sent []json.RawMessage
}

func (f *fakeWS) SendAll(m any) error {
	b, _ := json.Marshal(m)
	f.mu.Lock()
	f.sent = append(f.sent, b)
	f.mu.Unlock()
	return nil
}

func kinds(msgs []json.RawMessage) (plain, sealed int, ctls []string) {
	for _, m := range msgs {
		var n struct {
			NotificationType string `json:"notification_type"`
			Message          struct {
				Ctl string `json:"ctl"`
			} `json:"message"`
		}
		_ = json.Unmarshal(m, &n)
		if n.NotificationType == "sealed" {
			sealed++
			ctls = append(ctls, n.Message.Ctl)
		} else {
			plain++
		}
	}
	return
}

func TestSealingWrappersPerState(t *testing.T) {
	note := map[string]any{"type": "notification", "notification_type": "device_status", "message": map[string]any{"volume": 3}, "persist_record_count": 1}
	for _, enforced := range []bool{false, true} {
		a, _ := newAuthority(t, enforced)
		g := controlgate.New(a, &recorder{}, zap.NewNop())
		fr := &fakeRelayer{}
		fw := &fakeWS{}
		wr := controlgate.WrapRelayer(fr, a, zap.NewNop())
		ww := controlgate.WrapWS(fw, a, zap.NewNop())

		// legacy: plaintext only, RPC untouched.
		_ = wr.Send(context.Background(), note)
		_ = ww.SendAll(note)
		_ = wr.Send(context.Background(), relayer.Response{Type: "RPC", MessageID: "m", Message: map[string]any{"ok": true}})
		if p, s, _ := kinds(fr.sent); p != 2 || s != 0 {
			t.Fatalf("legacy relayer: plain %d sealed %d", p, s)
		}
		if p, s, _ := kinds(fw.sent); p != 1 || s != 0 {
			t.Fatalf("legacy ws: plain %d sealed %d", p, s)
		}

		owner := enrollOwner(t, g, a)
		fr.sent, fw.sent = nil, nil
		_ = wr.Send(context.Background(), note)
		_ = ww.SendAll(note)
		// mint pairing's struct shape is recognized too.
		_ = wr.Send(context.Background(), relayer.Response{Type: "notification", NotificationType: "mint_pairing_approval_request", Message: map[string]any{"x": 1}})
		rp, rs, ctls := kinds(fr.sent)
		wp, ws, _ := kinds(fw.sent)
		wantRelayerPlain := 2
		if enforced {
			wantRelayerPlain = 0
		}
		if rp != wantRelayerPlain || rs != 2 || wp != 0 || ws != 1 {
			t.Fatalf("enforced=%v: relayer plain %d sealed %d, ws plain %d sealed %d", enforced, rp, rs, wp, ws)
		}
		if ctls[0] != owner.ctl {
			t.Fatalf("sealed for %v", ctls)
		}
		// The owner opens what it got.
		var wire struct {
			Message control.Sealed `json:"message"`
		}
		_ = json.Unmarshal(fw.sent[0], &wire)
		if _, err := control.OpenNotification(wire.Message.JWE, owner.ctl, owner.nkFrom(t, g)); err != nil {
			t.Fatal(err)
		}
	}
}

// nkFrom asks the device (enroll is idempotent) for this controller's nk.
func (c *client) nkFrom(t *testing.T, g *controlgate.Gate) []byte {
	t.Helper()
	if c.nk != nil {
		return c.nk
	}
	s := c.build(control.OpEnroll, "", "", "")
	wire, perr := g.HandleEnvelope(context.Background(), s.body, control.Origin{})
	if perr != nil {
		t.Fatal(perr)
	}
	b, _ := json.Marshal(wire)
	r := c.open(s, b)
	var res struct {
		NK string `json:"nk"`
	}
	_ = json.Unmarshal(r.Res, &res)
	c.nk, _ = b64.DecodeString(res.NK)
	return c.nk
}

// Review F3: the Play-shape bypass. The typed decode merged the case-variant
// (or duplicate) "Request" into Arguments while the policy judged the exact
// "request" token; such messages are refused, and an allowed command runs
// with Arguments decoded from the very bytes the policy judged.
func TestPlaintextAmbiguityAndNormalization(t *testing.T) {
	a, _ := newAuthority(t, true)
	g := controlgate.New(a, &recorder{}, zap.NewNop())
	enrollOwner(t, g, a) // enforced: relayer plaintext is the exact Play shape only
	for _, msg := range []string{
		`{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{}},"Request":{"evil":1}}`,
		`{"command":"displayPlaylist","Request":{"evil":1},"request":{"intent":{"action":"now_display"},"dp1_call":{}}}`,
		`{"command":"displayPlaylist","request":{"evil":1},"request":{"intent":{"action":"now_display"},"dp1_call":{}}}`,
	} {
		var cmd commands.Command
		if err := json.Unmarshal([]byte(msg), &cmd); err != nil {
			t.Fatal(err)
		}
		if d, _ := g.Plaintext(control.RouteRelayer, cmd); d.Allow || d.Code != control.CodeInvalidMessage {
			t.Errorf("%s: %+v", msg, d)
		}
	}
	// A clean Play cast is allowed and its Arguments are exactly the raw bytes.
	var cmd commands.Command
	_ = json.Unmarshal([]byte(`{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{"items":[]}}}`), &cmd)
	cmd.Arguments = map[string]any{"evil": 1} // whatever the typed decode said
	d, out := g.Plaintext(control.RouteRelayer, cmd)
	if !d.Allow || out.Arguments["evil"] != nil || out.Arguments["dp1_call"] == nil {
		t.Fatalf("normalized command: %+v %+v", d, out.Arguments)
	}
}

// Review F1: a live invitation protects the screen.
func TestScreenProtectedWhileInvitationLive(t *testing.T) {
	a, _ := newAuthority(t, false)
	g := controlgate.New(a, &recorder{}, zap.NewNop())
	if g.ScreenProtected() {
		t.Fatal("protected with no invitation")
	}
	if _, err := a.IssueInvitation(); err != nil {
		t.Fatal(err)
	}
	if !g.ScreenProtected() {
		t.Fatal("live invitation not protected")
	}
	a.ClearInvitation()
	if g.ScreenProtected() {
		t.Fatal("cleared invitation still protected")
	}
}

// Review L8: a sealed copy carries the original notification's messageID
// (routing metadata, not content). Review round 4 M2: it is never persisted
// (persist_record_count 0), whatever the original's count.
func TestSealedCopyCarriesMessageIDNotPersisted(t *testing.T) {
	a, _ := newAuthority(t, false)
	g := controlgate.New(a, &recorder{}, zap.NewNop())
	enrollOwner(t, g, a)
	fr := &fakeRelayer{}
	wr := controlgate.WrapRelayer(fr, a, zap.NewNop())
	_ = wr.Send(context.Background(), relayer.Response{Type: "notification", MessageID: "appr-1",
		NotificationType: "mint_pairing_approval_request", PersistRecordCount: 10, Message: map[string]any{"x": 1}})
	var sealed []map[string]any
	for _, m := range fr.sent {
		var v map[string]any
		_ = json.Unmarshal(m, &v)
		if v["notification_type"] == "sealed" {
			sealed = append(sealed, v)
		}
	}
	if len(sealed) != 1 || sealed[0]["messageID"] != "appr-1" || sealed[0]["persist_record_count"] != float64(0) {
		t.Fatalf("sealed copy = %+v", sealed)
	}
}

// Hardware follow-up: a request refused after its signature verified is
// logged with the verified controller, its role, the op, the command name
// and the code, and nothing of the request body, rk, nk or JWE.
func TestRefusedRequestAuditLog(t *testing.T) {
	a, _ := newAuthority(t, false)
	core, logs := observer.New(zapcore.InfoLevel)
	g := controlgate.New(a, &recorder{}, zap.New(core))
	owner := enrollOwner(t, g, a)
	cli := newClient(t, "cli", owner.profile)
	send := func(c *client, s sent) control.ReplyClaims {
		t.Helper()
		wire, perr := g.HandleEnvelope(context.Background(), s.body, control.Origin{})
		if perr != nil {
			t.Fatal(perr)
		}
		b, _ := json.Marshal(wire)
		return c.open(s, b)
	}
	send(cli, cli.build(control.OpRequestAccess, "", "", ""))
	send(owner, owner.build(control.OpCmd, control.CmdApproveController, `{"ctl":"`+cli.ctl+`","role":"controller"}`, ""))
	secret := `{"enabled":true,"publicKey":"ssh-ed25519 SECRET"}` //nolint:gosec // G101: a marker string the test checks never reaches a log.
	if r := send(cli, cli.build(control.OpCmd, "sshAccess", secret, "")); r.Code != control.CodePermissionDenied {
		t.Fatalf("code %q", r.Code)
	}
	entries := logs.FilterMessage("Control request answered by the authority").All()
	last := entries[len(entries)-1].ContextMap()
	want := map[string]any{"op": "cmd", "command": "sshAccess", "controller": cli.ctl, "role": "controller", "code": "permission_denied"}
	for k, v := range want {
		if last[k] != v {
			t.Errorf("audit field %s = %v, want %v", k, last[k], v)
		}
	}
	for _, e := range logs.All() {
		for k, v := range e.ContextMap() {
			s, _ := v.(string)
			if k == "jwe" || k == "rk" || k == "nk" || k == "req" || strings.Contains(s, "SECRET") {
				t.Fatalf("log %q leaks %s=%v", e.Message, k, v)
			}
		}
	}
}

// toggleRelayer reports disconnected until connect is closed.
type toggleRelayer struct {
	fakeRelayer
	connected chan struct{}
}

func (r *toggleRelayer) IsConnected() bool {
	select {
	case <-r.connected:
		return true
	default:
		return false
	}
}

// Hardware follow-up: on an enforced boot the stored plaintext history on
// the relayer is overwritten once the relayer connects; nothing is sent in
// any other state.
func TestPurgeStalePlaintextOnEnforcedBoot(t *testing.T) {
	for _, enforced := range []bool{false, true} {
		a, _ := newAuthority(t, enforced)
		g := controlgate.New(a, &recorder{}, zap.NewNop())
		enrollOwner(t, g, a)
		r := &toggleRelayer{connected: make(chan struct{})}
		done := make(chan int)
		go func() {
			done <- controlgate.PurgeStalePlaintext(context.Background(), a, r, zap.NewNop(), time.Millisecond, 5*time.Second)
		}()
		time.Sleep(20 * time.Millisecond)
		close(r.connected)
		n := <-done
		if !enforced {
			if n != 0 || len(r.sent) != 0 {
				t.Fatalf("purged in enrolled state: %d", n)
			}
			continue
		}
		if n != 6 || len(r.sent) != 6 {
			t.Fatalf("purge sent %d", n)
		}
		types := map[string]bool{}
		for _, m := range r.sent {
			var v map[string]any
			_ = json.Unmarshal(m, &v)
			if v["type"] != "notification" || v["persist_record_count"] != float64(1) || fmt.Sprint(v["message"]) != "map[]" {
				t.Fatalf("purge message shape: %s", m)
			}
			types[v["notification_type"].(string)] = true
		}
		for _, want := range []string{"device_status", "player_status", "ddc_status", "offline_cache_status",
			"mint_pairing_approval_request", "mint_pairing_approval_outcome"} {
			if !types[want] {
				t.Fatalf("purged types %v, missing %s", types, want)
			}
		}
	}
}
