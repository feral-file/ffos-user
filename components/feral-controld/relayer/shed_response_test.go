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
// It is concurrency-safe because shed replies are now written from a separate
// writer goroutine (see shedResponseAsync), so the read-loop test and the
// writer race on the recorded writes under -race otherwise.
type fakeShedConn struct {
	mu     sync.Mutex
	writes [][]byte
	// onWrite, when non-nil, is signaled (non-blocking) after each write so a
	// test can wait for an asynchronous shed reply to land.
	onWrite chan struct{}
	// block, when non-nil, holds WriteMessage until the channel is closed/sent,
	// simulating a slow or backpressured socket.
	block chan struct{}
}

func (f *fakeShedConn) WriteJSON(interface{}) error { return nil }
func (f *fakeShedConn) ReadMessage() (int, []byte, error) {
	return 0, nil, nil
}
func (f *fakeShedConn) WriteMessage(_ int, data []byte) error {
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
func (f *fakeShedConn) Close() error                              { return nil }

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

// A shed command must receive a legible rate_limited RPC response mirroring the
// command router's shape, so callers see a rejection instead of a silent
// timeout (feral-file/ffos-user#208).
func TestSendShedResponse_RepliesToCommand(t *testing.T) {
	conn := &fakeShedConn{}
	r := &relayer{
		json:   wrapper.NewJSON(),
		conn:   conn,
		logger: zaptest.NewLogger(t),
	}

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
	r := &relayer{
		json:   wrapper.NewJSON(),
		conn:   conn,
		logger: zaptest.NewLogger(t),
	}

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
	r := &relayer{
		json:        wrapper.NewJSON(),
		conn:        conn,
		logger:      zaptest.NewLogger(t),
		done:        make(chan struct{}),
		dispatchSem: make(chan struct{}, 1),
		shedSem:     make(chan struct{}, 1),
		handlers: []Handler{
			func(context.Context, Payload) error {
				handlerCalls.Add(1)
				return nil
			},
		},
	}

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

// The shed reply must be emitted off the read loop: a slow/backpressured socket
// (the very condition we shed under) must not be able to wedge dispatch behind
// a single blocking write.
func TestDispatchMessage_ShedReplyDoesNotBlockReadLoop(t *testing.T) {
	release := make(chan struct{})
	conn := &fakeShedConn{block: release}
	r := &relayer{
		json:        wrapper.NewJSON(),
		conn:        conn,
		logger:      zaptest.NewLogger(t),
		done:        make(chan struct{}),
		dispatchSem: make(chan struct{}, 1),
		shedSem:     make(chan struct{}, 1),
		handlers:    []Handler{func(context.Context, Payload) error { return nil }},
	}

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
