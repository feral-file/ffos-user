package offlinecache_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeWSConn is a minimal in-memory stand-in for wrapper.WebSocketConn: a
// pair of channels play the role of a fake CDP peer so cdpsession's read
// pump and Send path can be exercised deterministically without a real
// websocket server.
type fakeWSConn struct {
	mu       sync.Mutex
	inbound  chan []byte // messages the "peer" sends to the session (ReadMessage)
	outbound chan []byte // messages the session sends to the "peer" (WriteMessage)
	closed   bool
}

func newFakeWSConn() *fakeWSConn {
	return &fakeWSConn{
		inbound:  make(chan []byte, 16),
		outbound: make(chan []byte, 16),
	}
}

func (c *fakeWSConn) WriteJSON(v interface{}) error { return nil }

func (c *fakeWSConn) ReadMessage() (int, []byte, error) {
	msg, ok := <-c.inbound
	if !ok {
		return 0, nil, errors.New("fake conn closed")
	}
	return 1, msg, nil
}

func (c *fakeWSConn) WriteMessage(messageType int, data []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("fake conn closed")
	}
	c.outbound <- data
	return nil
}

func (c *fakeWSConn) WriteControl(messageType int, data []byte, deadline time.Time) error { return nil }
func (c *fakeWSConn) SetPongHandler(h func(appData string) error)                         {}
func (c *fakeWSConn) SetReadDeadline(t time.Time) error                                   { return nil }
func (c *fakeWSConn) SetWriteDeadline(t time.Time) error                                  { return nil }

func (c *fakeWSConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil
	}
	c.closed = true
	close(c.inbound)
	return nil
}

// pushReply delivers a raw CDP JSON-RPC reply/event from the fake peer. It
// is guarded by the same mutex as Close so a test goroutine racing a
// session Close (which closes c.inbound) cannot send-on-closed-channel
// panic or trip the race detector on the unsynchronized close/send pair.
func (c *fakeWSConn) pushReply(data []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.inbound <- data
}

func (c *fakeWSConn) nextOutbound(t *testing.T) map[string]interface{} {
	t.Helper()
	select {
	case data := <-c.outbound:
		var msg map[string]interface{}
		require.NoError(t, json.Unmarshal(data, &msg))
		return msg
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for outbound CDP message")
		return nil
	}
}

var _ wrapper.WebSocketConn = (*fakeWSConn)(nil)

// concurrencyTrackingConn is a wrapper.WebSocketConn stand-in that does NOT
// serialize its own WriteMessage calls (unlike fakeWSConn, whose internal
// mutex would mask a missing write-lock in cdpSession itself). It flags
// concurrent entry into WriteMessage so a test can prove cdpSession.Send
// serializes writes on the caller's behalf, matching a real
// gorilla/websocket.Conn's single-writer requirement.
type concurrencyTrackingConn struct {
	inboundOnce sync.Once
	inbound     chan []byte
	writing     int32
	violated    int32
}

func newConcurrencyTrackingConn() *concurrencyTrackingConn {
	return &concurrencyTrackingConn{inbound: make(chan []byte)}
}

func (c *concurrencyTrackingConn) WriteJSON(v interface{}) error { return nil }

func (c *concurrencyTrackingConn) ReadMessage() (int, []byte, error) {
	msg, ok := <-c.inbound
	if !ok {
		return 0, nil, errors.New("concurrency tracking conn closed")
	}
	return 1, msg, nil
}

func (c *concurrencyTrackingConn) WriteMessage(messageType int, data []byte) error {
	if !atomic.CompareAndSwapInt32(&c.writing, 0, 1) {
		atomic.StoreInt32(&c.violated, 1)
		return nil
	}
	// Hold the "in a write" window open briefly so concurrent callers
	// racing cdpSession.Send are likely to overlap here if Send does not
	// serialize its own writes.
	time.Sleep(2 * time.Millisecond)
	atomic.StoreInt32(&c.writing, 0)
	return nil
}

func (c *concurrencyTrackingConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return nil
}
func (c *concurrencyTrackingConn) SetPongHandler(h func(appData string) error) {}
func (c *concurrencyTrackingConn) SetReadDeadline(t time.Time) error           { return nil }
func (c *concurrencyTrackingConn) SetWriteDeadline(t time.Time) error          { return nil }

func (c *concurrencyTrackingConn) Close() error {
	c.inboundOnce.Do(func() { close(c.inbound) })
	return nil
}

var _ wrapper.WebSocketConn = (*concurrencyTrackingConn)(nil)

// TestCDPSession_Send_SerializesConcurrentWrites reproduces the concurrency
// pattern replay.go relies on: it spawns one goroutine per
// Fetch.requestPaused event (see replay.go's onRequestPaused doc), and
// each of those goroutines can call session.Send on the same session
// while other requests are still in flight. cdpSession.Send must
// serialize the underlying WriteMessage calls itself.
func TestCDPSession_Send_SerializesConcurrentWrites(t *testing.T) {
	conn := newConcurrencyTrackingConn()
	session := offlinecache.NewCDPSession(conn, wrapper.NewJSON(), zaptest.NewLogger(t))
	defer func() { _ = session.Close() }()

	const concurrentSends = 20
	var wg sync.WaitGroup
	wg.Add(concurrentSends)
	for i := 0; i < concurrentSends; i++ {
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			// No reply is ever pushed, so every Send resolves via its
			// own context timeout; only the write-serialization
			// behavior is under test here.
			_, _ = session.Send(ctx, "Network.enable", nil)
		}()
	}
	wg.Wait()

	assert.Zero(t, atomic.LoadInt32(&conn.violated),
		"cdpSession.Send must serialize WriteMessage calls across concurrent callers")
}

func TestCDPSession_Send_MatchesReplyByID(t *testing.T) {
	conn := newFakeWSConn()
	session := offlinecache.NewCDPSession(conn, wrapper.NewJSON(), zaptest.NewLogger(t))
	defer func() { _ = session.Close() }()

	resultCh := make(chan struct {
		result json.RawMessage
		err    error
	}, 1)
	go func() {
		result, err := session.Send(context.Background(), "Network.enable", map[string]interface{}{})
		resultCh <- struct {
			result json.RawMessage
			err    error
		}{result, err}
	}()

	sent := conn.nextOutbound(t)
	assert.Equal(t, "Network.enable", sent["method"])
	id := int64(sent["id"].(float64))

	reply, err := json.Marshal(map[string]interface{}{
		"id":     id,
		"result": map[string]interface{}{"ok": true},
	})
	require.NoError(t, err)
	conn.pushReply(reply)

	select {
	case res := <-resultCh:
		require.NoError(t, res.err)
		assert.JSONEq(t, `{"ok":true}`, string(res.result))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Send to resolve")
	}
}

func TestCDPSession_Send_RemoteError(t *testing.T) {
	conn := newFakeWSConn()
	session := offlinecache.NewCDPSession(conn, wrapper.NewJSON(), zaptest.NewLogger(t))
	defer func() { _ = session.Close() }()

	errCh := make(chan error, 1)
	go func() {
		_, err := session.Send(context.Background(), "Bogus.method", nil)
		errCh <- err
	}()

	sent := conn.nextOutbound(t)
	id := int64(sent["id"].(float64))

	reply, err := json.Marshal(map[string]interface{}{
		"id":    id,
		"error": map[string]interface{}{"code": -32601, "message": "method not found"},
	})
	require.NoError(t, err)
	conn.pushReply(reply)

	select {
	case gotErr := <-errCh:
		require.Error(t, gotErr)
		assert.Contains(t, gotErr.Error(), "method not found")
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Send error")
	}
}

func TestCDPSession_Send_ContextCanceled(t *testing.T) {
	conn := newFakeWSConn()
	session := offlinecache.NewCDPSession(conn, wrapper.NewJSON(), zaptest.NewLogger(t))
	defer func() { _ = session.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := session.Send(ctx, "Network.enable", nil)
		errCh <- err
	}()

	conn.nextOutbound(t) // drain the write so Send is blocked awaiting a reply
	cancel()

	select {
	case err := <-errCh:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled Send to return")
	}
}

func TestCDPSession_On_DispatchesEventsByMethod(t *testing.T) {
	conn := newFakeWSConn()
	session := offlinecache.NewCDPSession(conn, wrapper.NewJSON(), zaptest.NewLogger(t))
	defer func() { _ = session.Close() }()

	received := make(chan json.RawMessage, 1)
	session.On("Fetch.requestPaused", func(params json.RawMessage) {
		received <- params
	})

	event, err := json.Marshal(map[string]interface{}{
		"method": "Fetch.requestPaused",
		"params": map[string]interface{}{"requestId": "abc"},
	})
	require.NoError(t, err)
	conn.pushReply(event)

	select {
	case params := <-received:
		assert.JSONEq(t, `{"requestId":"abc"}`, string(params))
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event dispatch")
	}
}

func TestCDPSession_Close_FailsPendingCalls(t *testing.T) {
	conn := newFakeWSConn()
	session := offlinecache.NewCDPSession(conn, wrapper.NewJSON(), zaptest.NewLogger(t))

	errCh := make(chan error, 1)
	go func() {
		_, err := session.Send(context.Background(), "Network.enable", nil)
		errCh <- err
	}()

	conn.nextOutbound(t)
	require.NoError(t, session.Close())

	select {
	case err := <-errCh:
		assert.Error(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for pending Send to fail after Close")
	}

	// A second Close must be a harmless no-op.
	assert.NoError(t, session.Close())
}

func TestCDPSession_Send_AfterClose(t *testing.T) {
	conn := newFakeWSConn()
	session := offlinecache.NewCDPSession(conn, wrapper.NewJSON(), zaptest.NewLogger(t))
	require.NoError(t, session.Close())

	// Give the read pump a moment to observe the closed connection and mark
	// the session closed.
	assert.Eventually(t, func() bool {
		_, err := session.Send(context.Background(), "Network.enable", nil)
		return err != nil
	}, 2*time.Second, 10*time.Millisecond)
}
