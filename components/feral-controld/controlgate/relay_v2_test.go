package controlgate_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/relaywire"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Exercise the actual binary carrier -> existing authority -> targeted reply.
// Enrollment and crypto use the existing endpoint fixtures, not a new policy.
func TestV2BindsOnlyAfterVerifiedReplayAdmission(t *testing.T) {
	a, _ := newAuthority(t, false)
	recorder := &recorder{}
	g := controlgate.New(a, recorder, zap.NewNop())
	owner := enrollOwner(t, g, a)
	nk := owner.nkFrom(t, g)
	adapter := controlgate.NewRelayV2(g)
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		up := websocket.Upgrader{Subprotocols: []string{relaywire.Subprotocol}}
		conn, err := up.Upgrade(w, req, nil)
		if err == nil {
			accepted <- conn
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	r := relayer.NewV2("ws"+strings.TrimPrefix(server.URL, "http"), testDevice, wrapper.NewWebSocketDialer(websocket.DefaultDialer), wrapper.NewRandomizer(), wrapper.NewClock(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	r.OnRelayerMessage(adapter.Handle)
	r.(interface{ SetPeerDisconnectHandler(func(relayer.Peer)) }).SetPeerDisconnectHandler(adapter.Disconnected)
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	remote := <-accepted
	defer func() { _ = remote.Close() }()
	if err := remote.WriteJSON(map[string]any{"type": "hello", "peers": []string{"owner-peer", "unverified-peer"}}); err != nil {
		t.Fatal(err)
	}
	read := func() (relaywire.Header, map[string]json.RawMessage) {
		t.Helper()
		_ = remote.SetReadDeadline(time.Now().Add(2 * time.Second))
		for {
			kind, data, err := remote.ReadMessage()
			if err != nil {
				t.Fatal(err)
			}
			if kind != websocket.BinaryMessage {
				continue
			}
			h, body, err := relaywire.Decode(data)
			if err != nil {
				t.Fatal(err)
			}
			// Real broker supplies from on the forwarded frame; here it is
			// the raw device write, whose destination is enough to ACK.
			if err = remote.WriteJSON(map[string]string{"type": "ack", "from": h.To, "id": h.ID}); err != nil {
				t.Fatal(err)
			}
			var value map[string]json.RawMessage
			if err = json.Unmarshal(body, &value); err != nil {
				t.Fatal(err)
			}
			return h, value
		}
	}
	send := func(peer, id string, message json.RawMessage) {
		t.Helper()
		body, _ := json.Marshal(map[string]any{"messageID": id, "message": message})
		frame, err := relaywire.Encode(relaywire.Header{Version: 2, ID: id, From: peer, To: "device"}, body)
		if err != nil {
			t.Fatal(err)
		}
		if err = remote.WriteMessage(websocket.BinaryMessage, frame); err != nil {
			t.Fatal(err)
		}
	}
	// No plaintext profile escape hatch and no command execution.
	send("unverified-peer", "plain", json.RawMessage(`{"command":"getControlProfile","request":{}}`))
	h, response := read()
	if h.To != "unverified-peer" || !strings.Contains(string(response["message"]), "bad_envelope") || recorder.count() != 0 {
		t.Fatal("plaintext escaped v2 ingress", response)
	}
	cmd := owner.build(control.OpCmd, "getDeviceStatus", `{}`, "")
	send("owner-peer", "good", cmd.cast)
	h, response = read()
	if h.To != "owner-peer" || !owner.open(cmd, response["message"]).OK {
		t.Fatal("valid request was not targeted")
	}
	// Replaying the same valid bytes from another peer must not bind it.
	send("unverified-peer", "replay", cmd.cast)
	_, response = read()
	if owner.open(cmd, response["message"]).OK || recorder.count() != 1 {
		t.Fatal("replay executed")
	}
	done := make(chan struct{})
	go func() { adapter.Notify(ctx, "device_status", map[string]any{"test": true}, ""); close(done) }()
	h, response = read()
	if h.To != "owner-peer" || string(response["notification_type"]) != `"sealed"` {
		t.Fatal("private notification misrouted", h, response)
	}
	var message struct {
		JWE string `json:"jwe"`
	}
	if err := json.Unmarshal(response["message"], &message); err != nil {
		t.Fatal(err)
	}
	if _, err := control.OpenNotification(message.JWE, owner.ctl, nk); err != nil {
		t.Fatal("notification not independently sealed", err)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("notification fanout included unbound peer")
	}
}
