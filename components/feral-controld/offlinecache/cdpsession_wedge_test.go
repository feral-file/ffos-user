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

// blockedWriteConn is a wrapper.WebSocketConn stand-in whose WriteMessage
// blocks until the write deadline most recently set via SetWriteDeadline
// elapses, then fails — mirroring what a real net.Conn does when the
// peer stops reading and the OS send buffer fills. This is a distinct
// hazard from wedgedConn above (whose WriteMessage returns immediately
// but no reply ever arrives): here the write CALL ITSELF blocks, which
// would hold cdpSession's writeMu forever — wedging every future Send on
// the session, not just this one — unless Send sets a write deadline
// before calling WriteMessage. If SetWriteDeadline is never called,
// deadline stays its zero value and this conn blocks on <-c.closed
// (unbounded), exactly like an unbounded blocking write syscall.
type blockedWriteConn struct {
	mu        sync.Mutex
	deadline  time.Time
	closeOnce sync.Once
	closed    chan struct{}
}

func newBlockedWriteConn() *blockedWriteConn {
	return &blockedWriteConn{closed: make(chan struct{})}
}

func (c *blockedWriteConn) WriteJSON(v interface{}) error { return nil }

func (c *blockedWriteConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.deadline = t
	return nil
}

func (c *blockedWriteConn) WriteMessage(messageType int, data []byte) error {
	c.mu.Lock()
	deadline := c.deadline
	c.mu.Unlock()
	if deadline.IsZero() {
		<-c.closed
		return errors.New("blocked write conn closed without ever receiving a write deadline")
	}
	if wait := time.Until(deadline); wait > 0 {
		select {
		case <-time.After(wait):
		case <-c.closed:
			return errors.New("blocked write conn closed")
		}
	}
	return errors.New("i/o timeout")
}

func (c *blockedWriteConn) ReadMessage() (int, []byte, error) {
	<-c.closed
	return 0, nil, errors.New("blocked write conn closed")
}
func (c *blockedWriteConn) WriteControl(messageType int, data []byte, deadline time.Time) error {
	return nil
}
func (c *blockedWriteConn) SetPongHandler(h func(appData string) error) {}
func (c *blockedWriteConn) SetReadDeadline(t time.Time) error           { return nil }
func (c *blockedWriteConn) Close() error {
	c.closeOnce.Do(func() { close(c.closed) })
	return nil
}

var _ wrapper.WebSocketConn = (*blockedWriteConn)(nil)

// TestCDPSession_Send_BlockedWriteUnblocksViaWriteDeadline is the
// regression test for the write-side wedge hazard: unlike
// TestCDPSession_Send_WedgedTargetUnblocksViaInternalCeiling (where the
// write succeeds instantly but no reply arrives), this pins that Send
// itself sets a socket write deadline before calling WriteMessage, so a
// write that blocks at the syscall level — not merely a reply that never
// arrives — still cannot wedge writeMu (and therefore every future Send
// on this session) forever.
func TestCDPSession_Send_BlockedWriteUnblocksViaWriteDeadline(t *testing.T) {
	conn := newBlockedWriteConn()
	session := newCDPSessionWithTimeout(conn, wrapper.NewJSON(), zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)), 100*time.Millisecond)
	defer func() { _ = session.Close() }()

	start := time.Now()
	errCh := make(chan error, 1)
	go func() {
		_, err := session.Send(context.Background(), "Fetch.enable", map[string]interface{}{})
		errCh <- err
	}()

	select {
	case err := <-errCh:
		require.Error(t, err, "Send must fail once the blocked write's own deadline elapses, not hang forever")
		assert.Less(t, time.Since(start), 5*time.Second)
	case <-time.After(5 * time.Second):
		t.Fatal("Send blocked forever on a write that never honored a deadline — writeMu is now wedged for every future Send on this session too")
	}
}

// TestCDPSession_Send_BlockedWriteHonorsCallerTighterDeadlineNotInternalCeiling
// is the write-side counterpart to
// TestCDPSession_Send_CallerDeadlineShorterThanCeilingStillWins: that test
// proves a caller's tighter deadline wins on the REPLY-wait side
// (wedgedConn, whose write returns instantly). This one proves the same
// contract on the WRITE side itself, using a long internal sendTimeout
// (10s) alongside a short caller deadline (100ms) against
// blockedWriteConn, whose WriteMessage blocks until whatever deadline
// SetWriteDeadline last set. Before this test existed, Send always
// passed SetWriteDeadline a fresh time.Now().Add(s.sendTimeout) —
// ignoring the caller's own tighter ctx deadline entirely — so a
// blocked write would have silently stretched a 100ms caller deadline
// out to the full 10s internal ceiling; this pins that Send instead
// reuses ctx's own (already-narrowed-by-WithTimeout) deadline for the
// write, so the caller's shorter bound reaches the socket, not just the
// reply-wait select.
func TestCDPSession_Send_BlockedWriteHonorsCallerTighterDeadlineNotInternalCeiling(t *testing.T) {
	conn := newBlockedWriteConn()
	session := newCDPSessionWithTimeout(conn, wrapper.NewJSON(), zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)), 10*time.Second)
	defer func() { _ = session.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := session.Send(ctx, "Fetch.enable", map[string]interface{}{})
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second,
		"a blocked write must fail once the CALLER's short deadline elapses, not stretch out to the 10s internal ceiling")
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
