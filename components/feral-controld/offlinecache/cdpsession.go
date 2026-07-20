package offlinecache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// CDPSession is a minimal event-driven Chrome DevTools Protocol client: it
// supports both blocking request/response calls AND delivery of unsolicited
// events (Network.*, Fetch.*, Target.*) to registered handlers via a
// background read pump. The daemon's existing synchronous client
// (components/feral-controld/cdp) cannot receive events at all — see its
// mutex-held, one-write-then-one-read Send — which is why offline
// capture/replay, both driven primarily by CDP events rather than RPC
// replies, need this separate session type. It intentionally does not
// reuse cdp.CDP's reconnect supervisor: capture/replay sessions are
// short-lived (one per download job, one per displayed cached item) and
// own their own dial/teardown lifecycle in downloader.go/replay.go.
//
//go:generate mockgen -source=cdpsession.go -destination=../mocks/offlinecache_cdpsession.go -package=mocks -mock_names=CDPSession=MockCDPSession
type CDPSession interface {
	// Send issues a CDP command and blocks for its matching response, or
	// until ctx is done or the session closes.
	Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error)
	// On registers a handler for every event with the given CDP method name
	// (e.g. "Fetch.requestPaused"). Handlers run synchronously on the read
	// pump goroutine in registration order, so a handler must not block or
	// call Send and wait on its own reply inline — that would deadlock the
	// pump against the very reply it is waiting to read. Long-running work
	// triggered by an event should be handed off to another goroutine.
	On(method string, handler func(params json.RawMessage))
	// Close stops the read pump and closes the underlying connection.
	// Idempotent; safe to call multiple times or after the peer already
	// closed the connection.
	Close() error
}

// cdpRemoteError is a CDP JSON-RPC error reply (distinct from
// cdp.RemoteError in the synchronous client since the wire shapes are
// intentionally decoupled between the two clients).
type cdpRemoteError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *cdpRemoteError) Error() string {
	return fmt.Sprintf("cdp error %d: %s", e.Code, e.Message)
}

type pendingCall struct {
	result json.RawMessage
	err    error
	done   chan struct{}
}

type cdpSession struct {
	conn   wrapper.WebSocketConn
	json   wrapper.JSON
	logger *zap.Logger

	nextID int64 // atomic, next outbound request id

	// writeMu serializes conn.WriteMessage calls. gorilla/websocket's
	// Conn is not safe for concurrent writes from multiple goroutines
	// (only Close/WriteControl are); replay.go deliberately spawns one
	// goroutine per Fetch.requestPaused event (see its On doc), so
	// multiple in-flight Sends racing on the same session's write side
	// is an expected, not a rare, occurrence and must be serialized here
	// rather than left to the caller.
	writeMu sync.Mutex

	mu       sync.Mutex
	pending  map[int64]*pendingCall
	handlers map[string][]func(params json.RawMessage)
	closed   bool
	closeErr error

	closeOnce sync.Once
	doneChan  chan struct{}
}

// NewCDPSession wraps an already-dialed CDP websocket connection and starts
// its background read pump immediately. Callers own dialing: capture dials
// the headless downloader's target (downloader.go), replay dials the kiosk
// page target (replay.go) — the discovery/target-selection logic differs
// enough between the two that sharing a dialer here would not simplify
// either caller.
func NewCDPSession(conn wrapper.WebSocketConn, jsonWrapper wrapper.JSON, logger *zap.Logger) CDPSession {
	s := &cdpSession{
		conn:     conn,
		json:     jsonWrapper,
		logger:   logger,
		pending:  make(map[int64]*pendingCall),
		handlers: make(map[string][]func(params json.RawMessage)),
		doneChan: make(chan struct{}),
	}
	go s.readPump()
	return s
}

type cdpOutbound struct {
	ID     int64                  `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params"`
}

type cdpInbound struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *cdpRemoteError `json:"error"`
}

func (s *cdpSession) Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	id := atomic.AddInt64(&s.nextID, 1)

	call := &pendingCall{done: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		return nil, fmt.Errorf("offline cache: cdp session closed: %w", closedOrUnknown(err))
	}
	s.pending[id] = call
	s.mu.Unlock()

	cleanup := func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}

	data, err := s.json.Marshal(cdpOutbound{ID: id, Method: method, Params: params})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("offline cache: marshal cdp request %s: %w", method, err)
	}

	s.writeMu.Lock()
	err = s.conn.WriteMessage(websocket.TextMessage, data)
	s.writeMu.Unlock()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("offline cache: write cdp request %s: %w", method, err)
	}

	select {
	case <-call.done:
		cleanup()
		return call.result, call.err
	case <-ctx.Done():
		cleanup()
		return nil, ctx.Err()
	case <-s.doneChan:
		cleanup()
		return nil, fmt.Errorf("offline cache: cdp session closed while awaiting %s", method)
	}
}

func closedOrUnknown(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("session closed")
}

func (s *cdpSession) On(method string, handler func(params json.RawMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = append(s.handlers[method], handler)
}

func (s *cdpSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.conn.Close()
	})
	return err
}

// readPump owns the connection's read side for the session's lifetime. It
// is the sole writer of s.closed/s.pending-teardown, so Send and On never
// need to reason about a concurrent partial shutdown.
func (s *cdpSession) readPump() {
	defer s.shutdown()

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			s.mu.Lock()
			s.closeErr = err
			s.mu.Unlock()
			return
		}

		var msg cdpInbound
		if err := s.json.Unmarshal(data, &msg); err != nil {
			s.logger.Warn("offline cache: failed to parse CDP message", zap.Error(err))
			continue
		}

		if msg.Method != "" {
			s.dispatchEvent(msg.Method, msg.Params)
			continue
		}

		s.resolveCall(msg.ID, msg.Result, msg.Error)
	}
}

func (s *cdpSession) shutdown() {
	s.mu.Lock()
	s.closed = true
	pending := s.pending
	s.pending = nil
	s.mu.Unlock()

	for _, call := range pending {
		call.err = fmt.Errorf("offline cache: cdp session closed")
		close(call.done)
	}
	close(s.doneChan)
}

func (s *cdpSession) resolveCall(id int64, result json.RawMessage, cdpErr *cdpRemoteError) {
	s.mu.Lock()
	call, ok := s.pending[id]
	if ok {
		delete(s.pending, id)
	}
	s.mu.Unlock()
	if !ok {
		// Unsolicited or already-timed-out reply; nothing to deliver it to.
		return
	}
	call.result = result
	if cdpErr != nil {
		call.err = cdpErr
	}
	close(call.done)
}

func (s *cdpSession) dispatchEvent(method string, params json.RawMessage) {
	s.mu.Lock()
	handlers := append([]func(json.RawMessage){}, s.handlers[method]...)
	s.mu.Unlock()
	for _, h := range handlers {
		h(params)
	}
}
