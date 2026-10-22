package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/relaywire"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestV2Eligibility(t *testing.T) {
	for _, tc := range []struct {
		endpoint, id    string
		authority, want bool
	}{
		{"", "FF1-SKYZ2E3A", true, false},
		{"wss://relay.test", "FF1-SKYZ2E3A", false, false},
		{"wss://relay.test", "FF1", true, false},
		{"wss://relay.test", "FF1-UNKNOWN", true, false},
		{"wss://relay.test", "FF1-SKYZ2E3A", true, true},
	} {
		if got := V2Eligible(tc.endpoint, tc.id, tc.authority); got != tc.want {
			t.Errorf("%+v: %v", tc, got)
		}
	}
}

func TestPeerDisconnectHookRetiresGeneration(t *testing.T) {
	s := newDeviceSession(nil)
	s.peers["one"] = &relayPeer{}
	s.peers["two"] = &relayPeer{}
	one := Peer{relaySource{s, "one"}}
	two := Peer{relaySource{s, "two"}}
	gone := map[Peer]bool{}
	s.onDisconnect = func(p Peer) {
		gone[p] = true
		if p.Live() {
			t.Error("callback before retirement")
		}
	}
	_, _, err := s.receive(1, []byte(`{"type":"peer_disconnected","connectionId":"one"}`))
	if err != nil || one.Live() || !two.Live() || !gone[one] {
		t.Fatal("peer retirement failed", err)
	}
	s.close()
	if two.Live() || !gone[two] {
		t.Fatal("generation did not clear all peers")
	}
}

// §10: v2 refusals are transient; redials back off, capped, instead of a
// fixed tight loop.
func TestV2RetryDelayBacksOffCapped(t *testing.T) {
	want := []time.Duration{0, 5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second,
		80 * time.Second, 160 * time.Second, 5 * time.Minute, 5 * time.Minute}
	for n, w := range want {
		if got := v2RetryDelay(n); got != w {
			t.Errorf("attempt %d: %v, want %v", n, got, w)
		}
	}
	if got := v2RetryDelay(1000); got != v2RetryCeiling {
		t.Errorf("attempt 1000: %v", got)
	}
}

type fastRandomizer struct{}

func (fastRandomizer) Intn(int) int { return 0 }
func (fastRandomizer) Duration(time.Duration, time.Duration) time.Duration {
	return 10 * time.Millisecond
}

// §10 liveness: the device sends the text frame "ping" on its own schedule
// and ignores the relay's text "pong".
func TestV2SendsTextPing(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		up := websocket.Upgrader{Subprotocols: []string{relaywire.Subprotocol}}
		if conn, err := up.Upgrade(w, req, nil); err == nil {
			accepted <- conn
		}
	}))
	defer server.Close()
	r := NewV2("ws"+strings.TrimPrefix(server.URL, "http"), "FF1-SKYZ2E3A", wrapper.NewWebSocketDialer(websocket.DefaultDialer),
		fastRandomizer{}, wrapper.NewClock(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	remote := <-accepted
	defer func() { _ = remote.Close() }()
	for i := 0; i < 2; i++ {
		_ = remote.SetReadDeadline(time.Now().Add(2 * time.Second))
		kind, data, err := remote.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage || string(data) != "ping" {
			t.Fatalf("frame %d: %d %q", i, kind, data)
		}
		if err := remote.WriteMessage(websocket.TextMessage, []byte("pong")); err != nil {
			t.Fatal(err)
		}
	}
	if !r.IsConnected() {
		t.Fatal("a text pong broke the session")
	}
}
