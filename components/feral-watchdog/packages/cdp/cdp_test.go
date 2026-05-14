package cdp

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/packages/wrapper"
)

func TestSendServiceFailedEventReconnectsWhenStartupInitWasSkipped(t *testing.T) {
	conn := &fakeWebSocketConn{
		readMessage: []byte(`{"id":1,"result":{"result":{"type":""}}}`),
	}
	dialer := &fakeWebSocketDialer{conn: conn}
	httpClient := &fakeHTTPClient{
		body: `[{"type":"page","webSocketDebuggerUrl":"ws://debugger.test/page"}]`,
	}

	client := NewClient(
		&Config{Endpoint: "http://127.0.0.1:9222"},
		zap.NewNop(),
		dialer,
		wrapper.NewIO(),
		wrapper.NewJSON(),
		httpClient,
	)
	t.Cleanup(client.Close)

	if err := client.SendServiceFailedEvent(context.Background()); err != nil {
		t.Fatalf("SendServiceFailedEvent returned error: %v", err)
	}

	if httpClient.gotURL != "http://127.0.0.1:9222/json" {
		t.Fatalf("unexpected CDP target URL: got=%q", httpClient.gotURL)
	}
	if dialer.gotURL != "ws://debugger.test/page" {
		t.Fatalf("unexpected websocket URL: got=%q", dialer.gotURL)
	}

	writes := conn.Writes()
	if len(writes) != 1 {
		t.Fatalf("expected one CDP write, got %d", len(writes))
	}
	if !strings.Contains(writes[0], `"method":"Runtime.evaluate"`) {
		t.Fatalf("expected Runtime.evaluate request, got %s", writes[0])
	}
	if !strings.Contains(writes[0], `window.handleWatchdogEvent(\"ServiceFailed\")`) {
		t.Fatalf("expected service failed watchdog event, got %s", writes[0])
	}
}

type fakeHTTPClient struct {
	body   string
	err    error
	gotURL string
}

func (f *fakeHTTPClient) Get(url string) (*http.Response, error) {
	f.gotURL = url
	if f.err != nil {
		return nil, f.err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(strings.NewReader(f.body)),
		Header:     make(http.Header),
	}, nil
}

type fakeWebSocketDialer struct {
	conn   wrapper.WebSocketConnInterface
	err    error
	gotURL string
}

func (f *fakeWebSocketDialer) DialContext(_ context.Context, stringURL string, requestHeader http.Header) (wrapper.WebSocketConnInterface, *http.Response, error) {
	f.gotURL = stringURL
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.conn, &http.Response{StatusCode: http.StatusSwitchingProtocols, Header: requestHeader}, nil
}

type fakeWebSocketConn struct {
	mu          sync.Mutex
	writes      []string
	readMessage []byte
	readErr     error
	closeErr    error
}

func (f *fakeWebSocketConn) WriteJSON(interface{}) error { return nil }

func (f *fakeWebSocketConn) ReadMessage() (int, []byte, error) {
	if f.readErr != nil {
		return websocket.TextMessage, nil, f.readErr
	}
	return websocket.TextMessage, f.readMessage, nil
}

func (f *fakeWebSocketConn) WriteMessage(_ int, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writes = append(f.writes, string(data))
	return nil
}

func (f *fakeWebSocketConn) WriteControl(int, []byte, time.Time) error { return nil }

func (f *fakeWebSocketConn) SetPongHandler(func(string) error) {}

func (f *fakeWebSocketConn) SetReadDeadline(time.Time) error { return nil }

func (f *fakeWebSocketConn) Close() error { return f.closeErr }

func (f *fakeWebSocketConn) Writes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	writes := make([]string, len(f.writes))
	copy(writes, f.writes)
	return writes
}
