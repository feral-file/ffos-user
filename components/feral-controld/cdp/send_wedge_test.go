package cdp

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// wedgeHTTP serves /json discovery pointing at a test-owned websocket URL, so the client
// dials a REAL gorilla connection — the deadline behavior under test lives in the real
// conn, which the fakes/mocks cannot model.
type wedgeHTTP struct{ targetsJSON string }

func (f *wedgeHTTP) Do(*http.Request) (*http.Response, error) {
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(f.targetsJSON))),
	}, nil
}
func (f *wedgeHTTP) NewRequest(string, string, io.Reader) (*http.Request, error) { return nil, nil }
func (f *wedgeHTTP) Get(string) (*http.Response, error)                          { return nil, nil }
func (f *wedgeHTTP) Post(string, string, io.Reader) (*http.Response, error)      { return nil, nil }

// TestSend_WedgedSocketUnblocksAndSignalsDrop pins the PR #218 review fix: a socket that
// stays writable but never replies (the post-kiosk-restart zombie) must not wedge send
// forever while holding the client mutex. send must return an error once the round-trip
// deadline expires, signal the connect loop to reconnect, and leave Close able to
// complete instead of deadlocking behind the held lock.
func TestSend_WedgedSocketUnblocksAndSignalsDrop(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var srvConns sync.WaitGroup
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		srvConns.Add(1)
		defer srvConns.Done()
		defer func() { _ = conn.Close() }()
		// Swallow client messages without ever replying: writable, silent.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer srv.Close()
	defer srvConns.Wait()

	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http")
	c := &cdp{
		dialer: wrapper.NewWebSocketDialer(websocket.DefaultDialer),
		io:     fakeIO{},
		json:   fakeJSON{},
		httpClient: &wedgeHTTP{
			targetsJSON: fmt.Sprintf(
				`[{"type":"page","title":"kiosk","webSocketDebuggerUrl":"%s"}]`, wsURL),
		},
		endpoint:      srv.URL,
		doneChan:      make(chan struct{}),
		reconnectCh:   make(chan struct{}, 1),
		retryInterval: 10 * time.Millisecond,
		sendTimeout:   200 * time.Millisecond,
		logger:        zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)),
	}
	require.NoError(t, c.Init(context.Background()))

	start := time.Now()
	_, err := c.send("Runtime.evaluate", map[string]interface{}{"expression": "1"})
	require.Error(t, err, "send against a silent socket must fail, not block")
	require.Less(t, time.Since(start), 5*time.Second,
		"send must return within the round-trip deadline, not hang")

	select {
	case <-c.reconnectCh:
		// The failed send woke the connect loop — reconnection is possible again.
	default:
		t.Fatal("expected the deadline failure to signal a reconnect")
	}

	closeDone := make(chan struct{})
	go func() {
		c.Close()
		close(closeDone)
	}()
	select {
	case <-closeDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Close deadlocked behind the send mutex")
	}
}
