package relayer

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/relaywire"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// slowRandomizer makes every redial wait long enough that only WakeV2 can
// end it inside the test.
type slowRandomizer struct{}

func (slowRandomizer) Intn(int) int { return 0 }
func (slowRandomizer) Duration(time.Duration, time.Duration) time.Duration {
	return time.Hour
}

// v2Server accepts v2 upgrades while up is true and refuses them with the
// relay's 429 otherwise.
func v2Server(t *testing.T, up *atomic.Bool) (string, <-chan *websocket.Conn) {
	t.Helper()
	accepted := make(chan *websocket.Conn, 4)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !up.Load() {
			http.Error(w, "capacity", http.StatusTooManyRequests)
			return
		}
		upg := websocket.Upgrader{Subprotocols: []string{relaywire.Subprotocol}}
		if conn, err := upg.Upgrade(w, req, nil); err == nil {
			accepted <- conn
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http"), accepted
}

func newTestV2(endpoint string) *relayer {
	return NewV2(endpoint, "FF1-SKYZ2E3A", wrapper.NewWebSocketDialer(websocket.DefaultDialer),
		slowRandomizer{}, wrapper.NewClock(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop()).(*relayer)
}

func v2AttemptsOf(r *relayer) int {
	r.v2Mu.Lock()
	defer r.v2Mu.Unlock()
	return r.v2Attempts
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal(what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// An online edge ends a long redial wait at once and starts the backoff over.
func TestWakeV2EndsTheRedialWait(t *testing.T) {
	var up atomic.Bool
	endpoint, accepted := v2Server(t, &up)
	r := newTestV2(endpoint)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.Close()

	result := make(chan error, 1)
	go func() { result <- r.RetryableConnect(ctx) }()
	// The first dial is refused (429), so the loop is now in an hour-long wait.
	waitFor(t, "retry loop never reached its wait", func() bool { return v2AttemptsOf(r) >= 2 })
	up.Store(true)
	r.WakeV2()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("redial after wake: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("WakeV2 did not end the redial wait")
	}
	remote := <-accepted
	defer func() { _ = remote.Close() }()
	if !r.IsConnected() {
		t.Fatal("not connected after the woken dial")
	}
}

// feral-file#3551 G3 bench F2: the online edge for the new network arrives
// while the socket dialed on the old one still looks connected; when that
// socket dies the redial must wait the floor, not the backoff grown while the
// device was offline (~155 s on the bench). A wake while connected leaves no
// token behind, so the floor wait is not skipped either.
func TestWakeV2WhileConnectedAppliesWhenTheSocketDies(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	endpoint, accepted := v2Server(t, &up)
	r := newTestV2(endpoint)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.Close()
	if err := r.Connect(ctx); err != nil {
		t.Fatal(err)
	}
	remote := <-accepted
	r.v2Mu.Lock()
	r.v2Attempts = 7 // grown while offline, plus the dial that opened this session
	r.v2Mu.Unlock()

	r.WakeV2()
	if got := v2AttemptsOf(r); got != 1 {
		t.Fatalf("wake while connected left v2Attempts %d, want 1", got)
	}
	if len(r.v2Wake) != 0 {
		t.Fatal("WakeV2 left a token while connected")
	}

	_ = remote.Close() // the path is gone: the device reads EOF
	// retryV2 counted the floor wait (1 → 2) and is now waiting on it; on the
	// old code it would be waiting on attempt 8.
	waitFor(t, "redial did not settle on the floor wait", func() bool {
		r.Lock()
		closed := r.conn == nil
		r.Unlock()
		return closed && v2AttemptsOf(r) == 2
	})
	if got := v2RetryDelay(1); got != v2RetryFloor {
		t.Fatalf("attempt 1 waits %v, want the floor", got)
	}
}

// A token left by a wake that raced a dial is drained once the dial succeeds,
// so it cannot skip the wait after that session ends.
func TestV2DrainsWakeTokenAfterConnect(t *testing.T) {
	var up atomic.Bool
	up.Store(true)
	endpoint, accepted := v2Server(t, &up)
	r := newTestV2(endpoint)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer r.Close()
	r.v2Wake <- struct{}{} // as if WakeV2 ran while the dial below was in flight
	if err := r.RetryableConnect(ctx); err != nil {
		t.Fatal(err)
	}
	remote := <-accepted
	defer func() { _ = remote.Close() }()
	if len(r.v2Wake) != 0 {
		t.Fatal("stale wake token survived a successful dial")
	}
}

// The legacy instance has no v2 loop to wake.
func TestWakeV2IsANoOpOnLegacy(t *testing.T) {
	r := New("wss://legacy.test", "", wrapper.NewWebSocketDialer(websocket.DefaultDialer), slowRandomizer{},
		wrapper.NewClock(), wrapper.NewOS(), wrapper.NewJSON(), zap.NewNop()).(*relayer)
	r.WakeV2()
	if r.v2Wake != nil || v2AttemptsOf(r) != 0 {
		t.Fatal("WakeV2 touched the legacy instance")
	}
}
