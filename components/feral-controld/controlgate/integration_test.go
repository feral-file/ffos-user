package controlgate_test

// End-to-end control protocol test (feral-file#3551 M2/M3): a real hub served
// over loopback HTTP and a real mediator, both wired to one real control
// authority through the real controlgate and the real commandrouter storm
// gate. Only the router's inner handler (a recorder) and the relayer/dbus/cdp
// leaves are fakes. It walks the ceremonies a phone and the CLI perform and
// asserts the §7.2 policy on both routes in every state.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/golang/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type rig struct {
	t        *testing.T
	auth     *control.Authority
	rec      *recorder
	server   *httptest.Server
	relay    relayer.Handler
	rpcMu    sync.Mutex
	rpc      []json.RawMessage
	pubRelay *fakeRelayer
	pubWS    *fakeWS
	enrolled int
}

func newRig(t *testing.T, fsys *control.MemFS, enforced bool) *rig {
	t.Helper()
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	a, err := control.New(control.Config{Dir: "/s/control", DeviceID: testDevice, FS: fsys, Enforced: enforced})
	if err != nil || a.LockErr() != nil {
		t.Fatal(err, a.LockErr())
	}
	r := &rig{t: t, auth: a, rec: &recorder{}, pubRelay: &fakeRelayer{}, pubWS: &fakeWS{}}
	cfg := commandrouter.DefaultGateConfig()
	cfg.Enabled = false // the storm budget is not under test; dedupe/tiers are
	handler := commandrouter.NewGate(r.rec, cfg, logger)

	gate := controlgate.New(a, handler, logger)
	gate.SetPublisher(controlgate.NewPublisher(r.pubRelay, r.pubWS, logger))
	gate.SetOwnerEnrolledHook(func() { r.enrolled++ })

	// Hub on a real mux behind a loopback server.
	mux := http.NewServeMux()
	srv := mocks.NewMockHTTPServer(ctrl)
	srv.EXPECT().Handler().Return(mux).AnyTimes()
	h := hub.New(context.Background(), mocks.NewMockWS(ctrl), handler, nil, nil, srv, wrapper.NewJSON(), logger)
	h.(interface{ SetControl(hub.ControlGate) }).SetControl(gate)
	r.server = httptest.NewServer(mux)
	t.Cleanup(r.server.Close)

	// Mediator with a fake relayer that records RPC replies.
	rel := mocks.NewMockRelayer(ctrl)
	rel.EXPECT().OnRelayerMessage(gomock.Any()).Do(func(fn relayer.Handler) { r.relay = fn })
	rel.EXPECT().Send(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, data any) error {
		b, _ := json.Marshal(data)
		r.rpcMu.Lock()
		r.rpc = append(r.rpc, b)
		r.rpcMu.Unlock()
		return nil
	}).AnyTimes()
	dbus := mocks.NewMockDBus(ctrl)
	dbus.EXPECT().OnBusSignal(gomock.Any())
	m := mediator.New(context.Background(), rel, dbus, mocks.NewMockCDP(ctrl), handler, mocks.NewMockExecutor(ctrl),
		mocks.NewMockRefresher(ctrl), wrapper.NewJSON(), logger)
	m.(interface{ SetControl(mediator.ControlGate) }).SetControl(gate)
	m.Start()
	return r
}

func (r *rig) post(body []byte) (int, []byte) {
	r.t.Helper()
	resp, err := http.Post(r.server.URL+"/api/cast", "application/json", bytes.NewReader(body))
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func (r *rig) get(path string) (int, []byte) {
	r.t.Helper()
	resp, err := http.Get(r.server.URL + path)
	if err != nil {
		r.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// relayerCast delivers a cast over the relayer path and returns the RPC
// message the mediator answered with (nil when it sent none).
func (r *rig) relayerCast(cast []byte) json.RawMessage {
	r.t.Helper()
	var p relayer.Payload
	if err := json.Unmarshal([]byte(`{"messageID":"m1","message":`+string(cast)+`}`), &p); err != nil {
		r.t.Fatal(err)
	}
	r.rpcMu.Lock()
	before := len(r.rpc)
	r.rpcMu.Unlock()
	if err := r.relay(context.Background(), p); err != nil {
		r.t.Fatal(err)
	}
	r.rpcMu.Lock()
	defer r.rpcMu.Unlock()
	if len(r.rpc) == before {
		return nil
	}
	var resp struct {
		Message json.RawMessage `json:"message"`
	}
	_ = json.Unmarshal(r.rpc[len(r.rpc)-1], &resp)
	return resp.Message
}

func (r *rig) profile() (control.State, control.Profile, string) {
	r.t.Helper()
	code, b := r.get("/api/v2/status")
	if code != 200 {
		r.t.Fatalf("status %d", code)
	}
	var st struct {
		TopicID string          `json:"topic_id"`
		Control *control.Status `json:"control"`
	}
	if err := json.Unmarshal(b, &st); err != nil || st.Control == nil {
		r.t.Fatalf("no control block: %s", b)
	}
	var p control.Profile
	if st.Control.Profile != nil {
		p = *st.Control.Profile
	}
	return st.Control.State, p, st.TopicID
}

// lanEnvelope posts an envelope on the LAN and opens the reply.
func (r *rig) lanEnvelope(c *client, s sent) control.ReplyClaims {
	r.t.Helper()
	code, b := r.post(s.cast)
	if code != 200 {
		r.t.Fatalf("envelope: HTTP %d %s", code, b)
	}
	return c.open(s, b)
}

func expectCode(t *testing.T, r control.ReplyClaims, code string) {
	t.Helper()
	if r.OK || r.Code != code {
		t.Fatalf("reply ok=%v code=%q res=%s, want %q", r.OK, r.Code, r.Res, code)
	}
}

func TestControlProtocolEndToEnd(t *testing.T) {
	fsys := control.NewMemFS()
	r := newRig(t, fsys, false)

	// --- legacy: plaintext works on both routes, profile is published ---
	state, profile, _ := r.profile()
	if state != control.StateLegacy || profile.Dev != testDevice {
		t.Fatalf("state %s profile %+v", state, profile)
	}
	if code, _ := r.post([]byte(`{"command":"displayPlaylist","request":{"dp1_call":{}}}`)); code != 200 {
		t.Fatalf("legacy LAN plaintext: %d", code)
	}
	if c := r.rec.last(); c.hasP || c.cmd.Type != "displayPlaylist" {
		t.Fatalf("legacy plaintext ran as %+v", c)
	}
	if msg := r.relayerCast([]byte(`{"command":"factoryReset","request":{}}`)); !strings.Contains(string(msg), `"ran":"factoryReset"`) {
		t.Fatalf("legacy relayer plaintext: %s", msg)
	}

	// --- first owner via the invitation (the QR carries it) ---
	inv, err := r.auth.IssueInvitation()
	if err != nil {
		t.Fatal(err)
	}
	if inv.Pins.Sig != profile.Sig.Thumbprint() {
		t.Fatal("QR pin does not match the published profile")
	}
	owner := newClient(t, "owner", profile)
	reply := r.lanEnvelope(owner, owner.build(control.OpEnroll, "", "", inv.Value))
	if !reply.OK || r.enrolled != 1 {
		t.Fatalf("owner enroll: %+v (hook %d)", reply, r.enrolled)
	}
	state, _, topic := r.profile()
	if state != control.StateEnrolled || topic != "" {
		t.Fatalf("after enroll: state %s topic %q", state, topic)
	}

	// --- plaintext LAN refused; relayer plaintext is tiered ---
	code, body := r.post([]byte(`{"command":"displayPlaylist","request":{"dp1_call":{}}}`))
	if code != 403 || strings.TrimSpace(string(body)) != `{"error":{"code":"enrollment_required"}}` {
		t.Fatalf("enrolled LAN plaintext: %d %s", code, body)
	}
	if code, body := r.get("/api/screenshot"); code != 403 || !strings.Contains(string(body), "enrollment_required") {
		t.Fatalf("enrolled screenshot: %d %s", code, body)
	}
	if msg := r.relayerCast([]byte(`{"command":"rotate","request":{}}`)); !strings.Contains(string(msg), `"ran":"rotate"`) {
		t.Fatalf("enrolled relayer controller-tier plaintext: %s", msg)
	}
	before := r.rec.count()
	msg := r.relayerCast([]byte(`{"command":"factoryReset","request":{}}`))
	if string(msg) != `{"command":"factoryReset","error":"envelope_required","message":"this FF1 requires a signed control envelope for factoryReset; update the app","ok":false}` || r.rec.count() != before {
		t.Fatalf("enrolled relayer owner-tier plaintext: %s", msg)
	}

	// --- a controller asks, the owner approves, the controller enrolls ---
	cli := newClient(t, "cli", profile)
	s := cli.build(control.OpRequestAccess, "", "", "")
	reply = r.lanEnvelope(cli, s)
	wantCode := control.ApprovalCode(cli.ctl, profile.Sig.Thumbprint())
	if !reply.OK || !strings.Contains(string(reply.Res), wantCode) {
		t.Fatalf("requestAccess: %+v %s", reply, reply.Res)
	}
	// The owner got a sealed approval request on both channels, and only
	// the owner did.
	if _, sealed, ctls := kinds(r.pubWS.sent); sealed != 1 || ctls[0] != owner.ctl {
		t.Fatalf("approval notification on WS: %d %v", sealed, ctls)
	}
	if _, sealed, _ := kinds(r.pubRelay.sent); sealed != 1 {
		t.Fatal("approval notification missing on the relayer")
	}
	var ntf struct {
		Message control.Sealed `json:"message"`
	}
	_ = json.Unmarshal(r.pubWS.sent[0], &ntf)
	n, err := control.OpenNotification(ntf.Message.JWE, owner.ctl, owner.nkFrom(t, controlgate.New(r.auth, r.rec, zap.NewNop())))
	if err != nil || n.Type != control.NotificationApprovalRequest || !strings.Contains(string(n.Body), wantCode) {
		t.Fatalf("approval notification: %v %+v", err, n)
	}
	expectCode(t, r.lanEnvelope(cli, cli.build(control.OpEnroll, "", "", "")), control.CodePending)

	approve := owner.build(control.OpCmd, control.CmdApproveController, `{"ctl":"`+cli.ctl+`","role":"controller"}`, "")
	if reply = r.lanEnvelope(owner, approve); !reply.OK {
		t.Fatalf("approve: %+v %s", reply, reply.Res)
	}
	reply = r.lanEnvelope(cli, cli.build(control.OpEnroll, "", "", ""))
	if !reply.OK || !strings.Contains(string(reply.Res), `"role":"controller"`) {
		t.Fatalf("controller enroll: %+v", reply)
	}

	// --- the controller plays, but cannot reset; tiers hold on both routes ---
	s = cli.build(control.OpCmd, "displayPlaylist", `{"dp1_call":{}}`, "")
	if reply = r.lanEnvelope(cli, s); !reply.OK {
		t.Fatalf("controller cast: %+v", reply)
	}
	if c := r.rec.last(); c.principal != (control.Principal{ControllerID: cli.ctl, Role: control.RoleController}) {
		t.Fatalf("ran as %+v", c)
	}
	before = r.rec.count()
	expectCode(t, r.lanEnvelope(cli, cli.build(control.OpCmd, "factoryReset", `{}`, "")), control.CodePermissionDenied)
	rs := cli.build(control.OpCmd, "sshAccess", `{"enabled":true}`, "")
	if msg := r.relayerCast(rs.cast); true {
		expectCode(t, cli.open(rs, msg), control.CodePermissionDenied)
	}
	if r.rec.count() != before {
		t.Fatal("a denied command reached the router")
	}

	// --- replay is refused (LAN and relayer carry the same bytes) ---
	code, body = r.post(s.cast)
	if code != 200 {
		t.Fatalf("replay: %d", code)
	}
	expectCode(t, cli.open(s, body), control.CodeReplay)
	expectCode(t, cli.open(s, r.relayerCast(s.cast)), control.CodeReplay)

	// --- owner over the relayer: an envelope works there too ---
	ls := owner.build(control.OpCmd, control.CmdListControllers, `{}`, "")
	if reply = owner.open(ls, r.relayerCast(ls.cast)); !reply.OK || !strings.Contains(string(reply.Res), cli.ctl) {
		t.Fatalf("relayer list: %+v", reply)
	}

	// --- revoke: access_revoked, and sealed notifications skip the grant ---
	if reply = r.lanEnvelope(owner, owner.build(control.OpCmd, control.CmdRevokeController, `{"ctl":"`+cli.ctl+`"}`, "")); !reply.OK {
		t.Fatalf("revoke: %+v", reply)
	}
	expectCode(t, r.lanEnvelope(cli, cli.build(control.OpCmd, "displayPlaylist", `{}`, "")), control.CodeAccessRevoked)
	fw := &fakeWS{}
	ww := controlgate.WrapWS(fw, r.auth, zap.NewNop())
	_ = ww.SendAll(map[string]any{"type": "notification", "notification_type": "device_status", "message": map[string]any{}})
	if plain, sealed, ctls := kinds(fw.sent); plain != 0 || sealed != 1 || ctls[0] != owner.ctl {
		t.Fatalf("after revoke: plain %d sealed %d %v", plain, sealed, ctls)
	}

	// --- a bad envelope is a public error, in clear ---
	code, body = r.post([]byte(`{"command":"ff1Envelope","request":{"v":1,"jwe":"x"}}`))
	if code != 401 || strings.TrimSpace(string(body)) != `{"error":{"code":"bad_envelope"}}` {
		t.Fatalf("bad envelope: %d %s", code, body)
	}
	if msg := r.relayerCast([]byte(`{"command":"ff1Envelope","request":{"v":1,"jwe":"x"}}`)); string(msg) != `{"error":{"code":"bad_envelope"}}` {
		t.Fatalf("relayer bad envelope: %s", msg)
	}

	// --- enforced (same grants, flag flipped): relayer plaintext is Play-only ---
	r2 := newRig(t, fsys, true)
	if st, _, _ := r2.profile(); st != control.StateEnforced {
		t.Fatalf("state %s", st)
	}
	if msg := r2.relayerCast([]byte(`{"command":"rotate","request":{}}`)); !strings.Contains(string(msg), `"error":"envelope_required"`) {
		t.Fatalf("enforced relayer rotate: %s", msg)
	}
	play := `{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{"dpVersion":"1.0.0","items":[]}}}`
	// Review F3: the case-variant / duplicate request bypass is refused on
	// both routes, and the router never sees it.
	before = r2.rec.count()
	bypass := `{"command":"displayPlaylist","request":{"intent":{"action":"now_display"},"dp1_call":{}},"Request":{"intent":{"action":"queue"},"dp1_call":{"evil":1}}}` //nolint:gosec // G101: a test message, not a credential.
	if msg := r2.relayerCast([]byte(bypass)); !strings.Contains(string(msg), `"error":"invalid_message"`) {
		t.Fatalf("enforced relayer bypass: %s", msg)
	}
	if code, body := r2.post([]byte(bypass)); code != 400 || !strings.Contains(string(body), "invalid_message") {
		t.Fatalf("LAN bypass: %d %s", code, body)
	}
	if r2.rec.count() != before {
		t.Fatal("an ambiguous message reached the router")
	}
	if msg := r2.relayerCast([]byte(play)); !strings.Contains(string(msg), `"ran":"displayPlaylist"`) {
		t.Fatalf("enforced relayer Play cast: %s", msg)
	}
	ls = owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	if reply = owner.open(ls, r2.relayerCast(ls.cast)); !reply.OK {
		t.Fatalf("enforced relayer envelope: %+v", reply)
	}
}
