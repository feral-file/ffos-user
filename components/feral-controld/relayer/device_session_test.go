package relayer

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/feral-file/ffos-user/components/feral-controld/relaywire"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestDeviceSessionRoutesReplyAndChunks(t *testing.T) {
	serverConn := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		up := websocket.Upgrader{}
		conn, err := up.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		serverConn <- conn
	}))
	defer server.Close()
	conn, _, err := wrapper.NewWebSocketDialer(websocket.DefaultDialer).DialContext(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	remote := <-serverConn
	defer func() { _ = remote.Close() }()
	s := newDeviceSession(conn)
	defer s.close()
	if _, _, err = s.receive(websocket.TextMessage, []byte(`{"type":"hello","peers":["controller-a","controller-b"]}`)); err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			kind, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_, _, _ = s.receive(kind, body)
		}
	}()
	ctx := context.WithValue(context.Background(), relaySourceKey{}, relaySource{s, "controller-b"})
	body := []byte(`{"type":"RPC","messageID":"same-id","message":"` + strings.Repeat("x", 100000) + `"}`)
	done := make(chan error, 1)
	go func() { done <- s.send(ctx, body) }()
	var assembly relaywire.Assembler
	var result []byte
	_ = remote.SetReadDeadline(time.Now().Add(5 * time.Second))
	for result == nil {
		kind, frame, err := remote.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.BinaryMessage {
			t.Fatal("expected binary")
		}
		h, part, err := relaywire.Decode(frame)
		if err != nil {
			t.Fatal(err)
		}
		if h.To != "controller-b" {
			t.Fatal("response leaked to other controller")
		}
		result, err = assembly.Add(part, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		ack, _ := json.Marshal(map[string]string{"type": "ack", "from": h.To, "id": h.ID})
		if err = remote.WriteMessage(websocket.TextMessage, ack); err != nil {
			t.Fatal(err)
		}
	}
	if string(result) != string(body) {
		t.Fatal("payload changed")
	}
	if err = <-done; err != nil {
		t.Fatal(err)
	}
	other := newDeviceSession(conn)
	if err = other.send(ctx, body); err == nil {
		t.Fatal("old generation reply accepted")
	}
}

func TestDeviceSessionAcknowledgesRejectedApplicationPayload(t *testing.T) {
	conn := &fakeShedConn{}
	s := newDeviceSession(conn)
	defer s.close()
	_, _, err := s.receive(websocket.TextMessage, []byte(`{"type":"hello","peers":["controller-a"]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, payload := range []string{`not-json`, `{"relayChunk":{"id":"transfer","index":1,"count":2,"data":"eA=="}}`} {
		frame, err := relaywire.Encode(relaywire.Header{Version: 2, ID: "frame", To: "player", From: "controller-a"}, []byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		body, _, err := s.receive(websocket.BinaryMessage, frame)
		if err == nil || body != nil {
			t.Fatal("invalid payload was dispatched")
		}
	}
	if conn.writeCount() != 2 {
		t.Fatal("invalid payloads retained transport credit")
	}
	for i := 0; i < conn.writeCount(); i++ {
		var ack map[string]string
		if err := json.Unmarshal(conn.writeAt(i), &ack); err != nil {
			t.Fatal(err)
		}
		if ack["type"] != "ack" || ack["from"] != "controller-a" || ack["id"] != "frame" {
			t.Fatalf("wrong receipt: %v", ack)
		}
	}
}
