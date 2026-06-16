package relayer

import (
	"context"
	"encoding/json"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeShedConn is a minimal WebSocketConn that records WriteMessage payloads.
// A hand-rolled fake avoids importing the generated mocks package, which would
// create an import cycle in this in-package (white-box) test.
//
// It is concurrency-safe because shed replies are written from a separate
// writer goroutine (see shedResponseAsync), so the read-loop test and the
// writer race on the recorded writes under -race otherwise.
type fakeShedConn struct {
	mu     sync.Mutex
	writes [][]byte
	// onWrite, when non-nil, is signaled (non-blocking) after each write so a
	// test can wait for an asynchronous shed reply to land.
	onWrite chan struct{}
	// writeStarted, when non-nil, is signaled (non-blocking) on entry to
	// WriteMessage so a test can observe that a write is in flight (and thus
	// holding the connection mutex) before it completes.
	writeStarted chan struct{}
	// block, when non-nil, holds WriteMessage until the channel is closed/sent,
	// simulating a slow or backpressured socket.
	block chan struct{}
	// writeDeadlineSet records whether SetWriteDeadline was called, and the last
	// deadline passed, so tests can assert writes are deadline-bounded.
	writeDeadlineSet  bool
	lastWriteDeadline time.Time
}

func (f *fakeShedConn) WriteJSON(interface{}) error { return nil }
func (f *fakeShedConn) ReadMessage() (int, []byte, error) {
	return 0, nil, nil
}
func (f *fakeShedConn) WriteMessage(_ int, data []byte) error {
	if f.writeStarted != nil {
		select {
		case f.writeStarted <- struct{}{}:
		default:
		}
	}
	if f.block != nil {
		<-f.block
	}
	f.mu.Lock()
	f.writes = append(f.writes, append([]byte(nil), data...))
	f.mu.Unlock()
	if f.onWrite != nil {
		select {
		case f.onWrite <- struct{}{}:
		default:
		}
	}
	return nil
}
func (f *fakeShedConn) WriteControl(int, []byte, time.Time) error { return nil }
func (f *fakeShedConn) SetPongHandler(func(string) error)         {}
func (f *fakeShedConn) SetReadDeadline(time.Time) error           { return nil }
func (f *fakeShedConn) SetWriteDeadline(t time.Time) error {
	f.mu.Lock()
	f.writeDeadlineSet = true
	f.lastWriteDeadline = t
	f.mu.Unlock()
	return nil
}
func (f *fakeShedConn) Close() error { return nil }

func (f *fakeShedConn) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func (f *fakeShedConn) writeAt(i int) []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writes[i]
}

func (f *fakeShedConn) deadlineWasSet() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.writeDeadlineSet
}

// newShedRelayer builds a white-box relayer wired only with what the shed/
// dispatch paths need. A real clock is required because Send now stamps a write
// deadline via clock.Now().
func newShedRelayer(t *testing.T, conn wrapper.WebSocketConn, handlers ...Handler) *relayer {
	return &relayer{
		json:        wrapper.NewJSON(),
		clock:       wrapper.NewClock(),
		conn:        conn,
		logger:      zaptest.NewLogger(t),
		done:        make(chan struct{}),
		dispatchSem: make(chan struct{}, 1),
		shedSem:     make(chan struct{}, MAX_INFLIGHT_SHED_RESPONSES),
		handlers:    handlers,
	}
}

// A shed command must receive a legible rate_limited RPC response mirroring the
// command router's shape, so callers see a rejection instead of a silent
// timeout (feral-file/ffos-user#208).
func TestSendShedResponse_RepliesToCommand(t *testing.T) {
	conn := &fakeShedConn{}
	r := newShedRelayer(t, conn)

	command := "displayPlaylist"
	r.sendShedResponse(context.Background(), Payload{
		MessageID: "msg-1",
		Message:   Message{Command: &command},
	})

	require.Equal(t, 1, conn.writeCount(), "exactly one shed response should be written")

	var resp Response
	require.NoError(t, json.Unmarshal(conn.writeAt(0), &resp))
	assert.Equal(t, "RPC", resp.Type)
	assert.Equal(t, "msg-1", resp.MessageID)

	body, ok := resp.Message.(map[string]interface{})
	require.True(t, ok, "shed response body must be a structured map")
	assert.Equal(t, "rate_limited", body["error"])
	assert.Equal(t, "displayPlaylist", body["command"])
}

// Control-plane (system) and response-less messages have no caller awaiting a
// reply by messageID, so they must not trigger a shed response.
func TestSendShedResponse_SkipsSystemAndEmpty(t *testing.T) {
	conn := &fakeShedConn{}
	r := newShedRelayer(t, conn)

	r.sendShedResponse(context.Background(), Payload{MessageID: MESSAGE_ID_SYSTEM})
	r.sendShedResponse(context.Background(), Payload{MessageID: ""})

	assert.Zero(t, conn.writeCount(), "no response should be sent for system/empty message IDs")
}

// When the dispatch semaphore is saturated, a command must be shed: the
// registered handler must NOT run, and the caller must receive a legible
// rate_limited RPC reply (feral-file/ffos-user#208).
func TestDispatchMessage_ShedsSaturatedCommand(t *testing.T) {
	conn := &fakeShedConn{onWrite: make(chan struct{}, 1)}
	var handlerCalls atomic.Int32
	r := newShedRelayer(t, conn, func(context.Context, Payload) error {
		handlerCalls.Add(1)
		return nil
	})

	// Pre-saturate the only dispatch slot so the incoming command is shed.
	r.dispatchSem <- struct{}{}

	command := "displayPlaylist"
	r.dispatchMessage(context.Background(), Payload{
		MessageID: "msg-1",
		Message:   Message{Command: &command},
	})

	// The shed reply is written off the read loop, so wait for it to land.
	select {
	case <-conn.onWrite:
	case <-time.After(time.Second):
		t.Fatal("expected a shed rate_limited reply to be written")
	}

	assert.Equal(t, int32(0), handlerCalls.Load(), "handler must not run for a shed command")
	require.Equal(t, 1, conn.writeCount())

	var resp Response
	require.NoError(t, json.Unmarshal(conn.writeAt(0), &resp))
	assert.Equal(t, "RPC", resp.Type)
	assert.Equal(t, "msg-1", resp.MessageID)
	body, ok := resp.Message.(map[string]interface{})
	require.True(t, ok, "shed response body must be a structured map")
	assert.Equal(t, "rate_limited", body["error"])
	assert.Equal(t, "displayPlaylist", body["command"])
}

// A saturated command must be shed exactly ONCE per payload, regardless of how
// many handlers are registered. Production can have more than one handler (the
// mediator plus a temporary D-Bus topic-ID listener); per-handler shedding
// would emit duplicate rate_limited replies for one messageID and couple
// command capacity to listener count.
func TestDispatchMessage_ShedsOncePerPayloadAcrossHandlers(t *testing.T) {
	conn := &fakeShedConn{onWrite: make(chan struct{}, 4)}
	var h1, h2 atomic.Int32
	r := newShedRelayer(t, conn,
		func(context.Context, Payload) error { h1.Add(1); return nil },
		func(context.Context, Payload) error { h2.Add(1); return nil },
	)

	// Pre-saturate the only dispatch slot so the incoming command is shed.
	r.dispatchSem <- struct{}{}

	command := "displayPlaylist"
	r.dispatchMessage(context.Background(), Payload{
		MessageID: "msg-1",
		Message:   Message{Command: &command},
	})

	// Exactly one reply lands...
	select {
	case <-conn.onWrite:
	case <-time.After(time.Second):
		t.Fatal("expected one shed rate_limited reply")
	}
	// ...and no second reply is produced for the additional handler.
	select {
	case <-conn.onWrite:
		t.Fatal("shed reply must be emitted once per payload, not once per handler")
	case <-time.After(100 * time.Millisecond):
	}

	assert.Equal(t, 1, conn.writeCount(), "one payload sheds one reply")
	assert.Equal(t, int32(0), h1.Load(), "no handler runs for a shed command")
	assert.Equal(t, int32(0), h2.Load(), "no handler runs for a shed command")
}

// The shed reply must be emitted off the read loop: a slow/backpressured socket
// (the very condition we shed under) must not be able to wedge dispatch behind
// a single blocking write.
func TestDispatchMessage_ShedReplyDoesNotBlockReadLoop(t *testing.T) {
	release := make(chan struct{})
	conn := &fakeShedConn{block: release}
	r := newShedRelayer(t, conn, func(context.Context, Payload) error { return nil })

	// Saturate the dispatch slot so the command takes the shed path, whose write
	// is now stuck on the blocked socket.
	r.dispatchSem <- struct{}{}

	command := "displayPlaylist"
	returned := make(chan struct{})
	go func() {
		r.dispatchMessage(context.Background(), Payload{
			MessageID: "msg-1",
			Message:   Message{Command: &command},
		})
		close(returned)
	}()

	select {
	case <-returned:
		// Good: dispatch returned even though the shed write is blocked.
	case <-time.After(time.Second):
		t.Fatal("dispatchMessage blocked on a slow shed-response write")
	}

	// Release the parked writer goroutine so the test leaves nothing wedged.
	close(release)
}

// Send must stamp a write deadline before writing so a backpressured peer
// cannot hold the connection mutex (and thus block ping/Close/reconnect, or pin
// a caller's dispatch slot) indefinitely.
func TestSend_AppliesWriteDeadline(t *testing.T) {
	conn := &fakeShedConn{}
	r := newShedRelayer(t, conn)

	require.NoError(t, r.Send(context.Background(), map[string]string{"k": "v"}))

	assert.True(t, conn.deadlineWasSet(), "Send must set a write deadline before writing")
	assert.False(t, conn.lastWriteDeadline.IsZero(), "write deadline must be a real (future) time")
	require.Equal(t, 1, conn.writeCount())
}

// A handler that replies via Send (as the mediator does for gate rejections)
// runs while holding a dispatch slot. If its write blocks on a backpressured
// socket the slot stays held — but only until the write returns, which the
// write deadline bounds. This proves a storm of rejection replies cannot pin
// the dispatch backstop indefinitely.
func TestDispatchMessage_SlotReleasedAfterHandlerSendCompletes(t *testing.T) {
	release := make(chan struct{})
	conn := &fakeShedConn{block: release, writeStarted: make(chan struct{}, 1)}
	r := newShedRelayer(t, conn)
	// Handler mimics mediator.handleRelayerMessage's rejection reply: it Sends,
	// and that write blocks on the stuck socket while the slot is held.
	r.handlers = []Handler{
		func(ctx context.Context, p Payload) error {
			return r.Send(ctx, Response{Type: "RPC", MessageID: p.MessageID, Message: "reply"})
		},
	}

	command := "displayPlaylist"
	r.dispatchMessage(context.Background(), Payload{
		MessageID: "msg-1",
		Message:   Message{Command: &command},
	})

	// The handler's Send is in flight, holding the only dispatch slot (cap 1).
	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("handler send never started")
	}
	select {
	case r.dispatchSem <- struct{}{}:
		<-r.dispatchSem
		t.Fatal("dispatch slot must stay held while the handler's send is in flight")
	default:
	}

	// Once the write returns (bounded by WRITE_WAIT in production), the slot is
	// freed and the backstop recovers.
	close(release)
	require.Eventually(t, func() bool {
		select {
		case r.dispatchSem <- struct{}{}:
			<-r.dispatchSem
			return true
		default:
			return false
		}
	}, time.Second, 5*time.Millisecond, "dispatch slot must be released after the handler's send completes")
}

// Lifecycle progress: a write blocked in WriteMessage holds the connection
// mutex, so Close cannot complete until that write returns — but it MUST
// complete once it does. In production WRITE_WAIT guarantees the write returns
// within a bound (see TestSend_AppliesWriteDeadline), so Close/ping/reconnect
// make bounded progress instead of wedging forever.
func TestClose_ProceedsOnceBlockedWriteReleases(t *testing.T) {
	release := make(chan struct{})
	conn := &fakeShedConn{block: release, writeStarted: make(chan struct{}, 1)}
	r := newShedRelayer(t, conn)

	// Start a send that blocks inside WriteMessage while holding the mutex.
	sendReturned := make(chan struct{})
	go func() {
		_ = r.Send(context.Background(), map[string]string{"k": "v"})
		close(sendReturned)
	}()

	select {
	case <-conn.writeStarted:
	case <-time.After(time.Second):
		t.Fatal("write never started")
	}

	// Close blocks on the mutex held by the in-flight write...
	closeReturned := make(chan struct{})
	go func() {
		r.Close()
		close(closeReturned)
	}()
	select {
	case <-closeReturned:
		t.Fatal("Close returned while a write still held the connection mutex")
	case <-time.After(100 * time.Millisecond):
	}

	// ...and proceeds promptly once the write returns (the deadline guarantees
	// this happens within WRITE_WAIT in production).
	close(release)
	select {
	case <-closeReturned:
	case <-time.After(time.Second):
		t.Fatal("Close did not proceed after the blocked write released")
	}
	<-sendReturned
}
