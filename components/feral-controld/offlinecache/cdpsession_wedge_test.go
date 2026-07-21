package offlinecache

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// wedgedConn is a wrapper.WebSocketConn stand-in whose peer accepts every
// write but never sends a reply — the "socket is writable but the target
// never responds" scenario defaultSendTimeout's doc describes (a
// nonresponsive/wedged kiosk DevTools target). ReadMessage blocks until
// Close, exactly like a real conn sitting on an open-but-silent socket.
type wedgedConn struct {
	closeOnce sync.Once
	closed    chan struct{}
}

func newWedgedConn() *wedgedConn {
	return &wedgedConn{closed: make(chan struct{})}
}

func (c *wedgedConn) WriteJSON(v interface{}) error { return nil }
func (c *wedgedConn) WriteMessage(messageType int, data []byte) error {
	return nil // Accepted, silently. No reply is ever produced.
}
func (c *wedgedConn) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, errors.New("wedged conn closed")
}
func (c *wedgedConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return nil
}
func (c *wedgedConn) SetPongHandler(h func(appData string) error) {}
func (c *wedgedConn) SetReadDeadline(t time.Time) error           { return nil }
func (c *wedgedConn) SetWriteDeadline(t time.Time) error          { return nil }
func (c *wedgedConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

var _ wrapper.WebSocketConn = (*wedgedConn)(nil)

// TestCDPSession_Send_WedgedTargetUnblocksViaInternalCeiling is the
// regression test pinning that the event-driven CDP session must not
// hang indefinitely on a nonresponsive kiosk DevTools socket: Send must
// return an error once its internal ceiling elapses even when
// the caller passes a context with no deadline of its own (mirroring
// replay.go's context.Background() and playlist-refresher/main.go's
// long-lived daemon context) and the peer's write succeeds but no reply
// ever arrives. newCDPSessionWithTimeout pins the ceiling short so this
// does not need to wait out the real 15s production default.
func TestCDPSession_Send_WedgedTargetUnblocksViaInternalCeiling(t *testing.T) {
	conn := newWedgedConn()
	session := newCDPSessionWithTimeout(conn, wrapper.NewJSON(), zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)), 100*time.Millisecond)
	defer func() { _ = session.Close() }()

	start := time.Now()
	_, err := session.Send(context.Background(), "Fetch.enable", map[string]interface{}{})
	require.Error(t, err, "Send against a wedged target must fail, not block forever")
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 5*time.Second,
		"Send must return once its internal ceiling elapses, not hang indefinitely")
}

// TestCDPSession_Send_CallerDeadlineShorterThanCeilingStillWins pins that
// the internal ceiling only adds an upper bound; it must never override a
// caller-supplied deadline that is already tighter (context.WithTimeout
// on top of an existing deadline always keeps the sooner of the two).
func TestCDPSession_Send_CallerDeadlineShorterThanCeilingStillWins(t *testing.T) {
	conn := newWedgedConn()
	session := newCDPSessionWithTimeout(conn, wrapper.NewJSON(), zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)), 10*time.Second)
	defer func() { _ = session.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := session.Send(ctx, "Fetch.enable", map[string]interface{}{})
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Less(t, time.Since(start), 2*time.Second,
		"the caller's own, tighter deadline must not be overridden by the longer internal ceiling")
}
