package offlinecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// defaultSendTimeout bounds how long Send will wait for a reply beyond
// whatever deadline the caller's ctx already carries. Several production
// callers (replay's per-request goroutine in replay.go, playlist-
// refresher's periodic SyncPlaylist call, main.go's CDP onConnect hook)
// pass a long-lived daemon/background context with no deadline of its
// own, so without an internal ceiling, a kiosk DevTools socket that
// accepts the write but never replies (a nonresponsive/wedged target)
// would wedge Send, and therefore its caller, forever. Unlike
// cdp.CDP.Send (which serializes one write+read at a time behind a
// single mutex and can safely poison the whole connection via
// socket-level read/write deadlines), cdpSession multiplexes many
// concurrent in-flight calls over one connection keyed by request ID,
// so the reply-wait bound must be per-call via context rather than a
// shared socket read deadline — setting one here would spuriously fail
// every other in-flight call sharing the connection's read side, not
// just the stuck one. (This does not rule out a socket WRITE deadline:
// Send below also sets one immediately before its own WriteMessage
// call, which is safe precisely because writes are already fully
// serialized by writeMu — see that call site's doc.) 15s mirrors cdp.go's sendRequestTimeout:
// generous relative to real CDP replies (these answer in milliseconds)
// so it only fires on a genuinely dead/nonresponsive target.
const defaultSendTimeout = 15 * time.Second

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
// Flat-mode multi-target: one dialed connection can carry several CDP
// targets (the directly-connected top-level page PLUS any child targets
// auto-attached via Target.setAutoAttach{flatten:true} — e.g. a kiosk
// page's cross-origin, out-of-process iframes). Every such child target
// has its own opaque CDP sessionId; the top-level target this connection
// was dialed against is addressed with the empty sessionId. ForSession
// returns a view scoped to one target's sessionId so callers can Send
// commands to, and On-subscribe to events from, exactly that target —
// see kiosktargets.go for the attach/detach lifecycle that drives it.
//
//go:generate mockgen -source=cdpsession.go -destination=../mocks/offlinecache_cdpsession.go -package=mocks -mock_names=CDPSession=MockCDPSession
type CDPSession interface {
	// Send issues a CDP command against this session's target and blocks
	// for its matching response, or until ctx is done or the session
	// closes.
	Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error)
	// On registers a handler for every event with the given CDP method name
	// (e.g. "Fetch.requestPaused") delivered for THIS session's target.
	// Handlers run synchronously on the read
	// pump goroutine in registration order, so a handler must not block or
	// call Send and wait on its own reply inline — that would deadlock the
	// pump against the very reply it is waiting to read. Long-running work
	// triggered by an event should be handed off to another goroutine.
	On(method string, handler func(params json.RawMessage))
	// ForSession returns a CDPSession view scoped to the given flat-mode
	// child target sessionID, multiplexed over this same underlying
	// connection. sessionID == "" returns the top-level target (the
	// receiver itself). Send/On on the returned view stamp/filter that
	// sessionId so a command reaches exactly its target and an event
	// handler only fires for its own target's events. Closing a non-root
	// view unregisters only that target's handlers; it must NOT tear down
	// the shared connection (see flatSession.Close).
	ForSession(sessionID string) CDPSession
	// Close stops the read pump and closes the underlying connection.
	// Idempotent; safe to call multiple times or after the peer already
	// closed the connection. On a child-target view (ForSession with a
	// non-empty id) this is instead a per-target detach — see
	// flatSession.Close.
	Close() error
}

// ErrCDPTransport marks a Send failure in which the CONNECTION itself
// failed — the socket was already closed, the write deadline could not be
// set, or the write did not go out. A gorilla/websocket connection is
// unusable after any of those, so a session that returns this can never
// work again and its holder should retire it rather than keep sending
// into it (see replayer.retireIfDeadLocked).
//
// Deliberately NOT returned for the two failures that leave the socket
// perfectly healthy: a CDP error REPLY (the peer answered — see
// cdpRemoteError) and the caller's own context expiring (our deadline,
// not the connection's death). Classifying at the source like this,
// rather than letting consumers infer "dead" by exclusion, is what keeps
// a marshal bug or a rejected command from being mistaken for a broken
// connection.
var ErrCDPTransport = errors.New("offline cache: cdp session transport failure")

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

// handlerKey scopes an event handler to both a CDP method name AND the
// flat-mode target sessionId it belongs to, so a "Fetch.requestPaused"
// handler registered for one cross-origin iframe target never fires for a
// different target's event of the same method. The empty sessionId is the
// top-level page target this connection was dialed against.
type handlerKey struct {
	method    string
	sessionID string
}

type cdpSession struct {
	conn   wrapper.WebSocketConn
	json   wrapper.JSON
	logger *zap.Logger

	// sendTimeout is defaultSendTimeout in production; overridable only
	// by newCDPSessionWithTimeout (test-only) to exercise the
	// wedged-target path deterministically and fast, mirroring
	// cdp.go's cdp.sendTimeout field/send_wedge_test.go convention.
	sendTimeout time.Duration

	nextID int64 // atomic, next outbound request id

	// writeMu serializes conn.WriteMessage calls. gorilla/websocket's
	// Conn is not safe for concurrent writes from multiple goroutines
	// (only Close/WriteControl are); replay.go deliberately spawns one
	// goroutine per Fetch.requestPaused event (see its On doc), so
	// multiple in-flight Sends racing on the same session's write side
	// is an expected, not a rare, occurrence and must be serialized here
	// rather than left to the caller.
	writeMu sync.Mutex

	mu      sync.Mutex
	pending map[int64]*pendingCall
	// handlers is keyed by (method, sessionId): a single connection
	// multiplexes events for the top-level target (empty sessionId) and
	// every auto-attached child target (its own sessionId), so routing
	// must key on both — see handlerKey and dispatchEvent.
	handlers map[handlerKey][]func(params json.RawMessage)
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
	return newCDPSessionWithTimeout(conn, jsonWrapper, logger, defaultSendTimeout)
}

// newCDPSessionWithTimeout is NewCDPSession's actual constructor, with an
// injectable sendTimeout so tests can pin the wedged-target ceiling
// behavior without waiting out the real production timeout.
func newCDPSessionWithTimeout(conn wrapper.WebSocketConn, jsonWrapper wrapper.JSON, logger *zap.Logger, sendTimeout time.Duration) CDPSession {
	s := &cdpSession{
		conn:        conn,
		json:        jsonWrapper,
		logger:      logger,
		sendTimeout: sendTimeout,
		pending:     make(map[int64]*pendingCall),
		handlers:    make(map[handlerKey][]func(params json.RawMessage)),
		doneChan:    make(chan struct{}),
	}
	go s.readPump()
	return s
}

type cdpOutbound struct {
	ID     int64                  `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params"`
	// SessionID routes this command to a flat-mode child target. Omitted
	// (via omitempty) for the top-level target, keeping every existing
	// root-only call byte-identical on the wire to before flat-mode
	// support existed.
	SessionID string `json:"sessionId,omitempty"`
}

type cdpInbound struct {
	ID     int64           `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	Result json.RawMessage `json:"result"`
	Error  *cdpRemoteError `json:"error"`
	// SessionID tags which flat-mode target this event/response came
	// from; empty for the top-level target. Only events are routed by it
	// (responses match on ID alone, since ids are unique per connection
	// regardless of session — see resolveCall).
	SessionID string `json:"sessionId,omitempty"`
}

// Send issues a command against the top-level target (empty sessionId).
func (s *cdpSession) Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	return s.sendForSession(ctx, "", method, params)
}

// sendForSession is Send's implementation, additionally stamping a
// flat-mode sessionId on the outbound envelope when non-empty so the
// command reaches a specific child target. The id-allocation, write
// serialization, pending-map, and timeout logic are all session-agnostic:
// CDP request ids are unique per CONNECTION (not per session), so
// multiplexing several targets over one socket needs no extra
// concurrency control here beyond the existing writeMu/pending machinery.
func (s *cdpSession) sendForSession(ctx context.Context, sessionID, method string, params map[string]interface{}) (json.RawMessage, error) {
	// Always impose the internal ceiling on top of whatever the caller's
	// ctx already carries — context.WithTimeout keeps the sooner of the
	// two deadlines, so a caller with its own tighter deadline is
	// unaffected, and a caller with none (context.Background(), or a
	// long-lived daemon-lifetime ctx) still cannot block this call
	// forever. See defaultSendTimeout's doc for why this must be a
	// per-call ctx bound rather than a per-connection socket deadline.
	ctx, cancel := context.WithTimeout(ctx, s.sendTimeout)
	defer cancel()

	id := atomic.AddInt64(&s.nextID, 1)

	call := &pendingCall{done: make(chan struct{})}
	s.mu.Lock()
	if s.closed {
		err := s.closeErr
		s.mu.Unlock()
		return nil, fmt.Errorf("%w: cdp session closed: %w", ErrCDPTransport, closedOrUnknown(err))
	}
	s.pending[id] = call
	s.mu.Unlock()

	cleanup := func() {
		s.mu.Lock()
		delete(s.pending, id)
		s.mu.Unlock()
	}

	data, err := s.json.Marshal(cdpOutbound{ID: id, Method: method, Params: params, SessionID: sessionID})
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("offline cache: marshal cdp request %s: %w", method, err)
	}

	s.writeMu.Lock()
	// A write deadline bounds only the raw WriteMessage call below, not
	// any reply — it is independent of, and does not conflict with,
	// this session's earlier design choice to bound the reply wait via
	// ctx rather than a socket read deadline (see defaultSendTimeout's
	// doc: a shared read deadline would spuriously fail every other
	// pending call on this connection, but SetWriteDeadline only ever
	// affects the next write, and writes are already fully serialized
	// by writeMu). Without this, a write that blocks because the peer
	// stopped reading (TCP send buffer full) would hold writeMu
	// forever, wedging every future Send on this session too, not just
	// the one whose write is stuck.
	//
	// Deliberately reuse ctx's OWN deadline here rather than a fresh
	// time.Now().Add(s.sendTimeout): ctx is already
	// context.WithTimeout(callerCtx, s.sendTimeout) above, so its
	// deadline is already the EARLIER of the caller's own (possibly
	// tighter) deadline and this call's internal ceiling. Send cannot
	// reach the ctx.Done() select case below until WriteMessage itself
	// returns, so if the write deadline ignored a tighter caller
	// deadline and used the full internal ceiling instead, a blocked
	// write would silently stretch that caller's shorter deadline out
	// to the full 15s ceiling — defeating the exact "caller's tighter
	// deadline wins" contract this ctx construction promises.
	deadline, _ := ctx.Deadline() // ok is always true: WithTimeout above guarantees a deadline.
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		s.writeMu.Unlock()
		cleanup()
		return nil, fmt.Errorf("%w: set cdp write deadline for %s: %w", ErrCDPTransport, method, err)
	}
	err = s.conn.WriteMessage(websocket.TextMessage, data)
	s.writeMu.Unlock()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("%w: write cdp request %s: %w", ErrCDPTransport, method, err)
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
		return nil, fmt.Errorf("%w: cdp session closed while awaiting %s", ErrCDPTransport, method)
	}
}

func closedOrUnknown(err error) error {
	if err != nil {
		return err
	}
	return fmt.Errorf("session closed")
}

// On registers a handler for the top-level target (empty sessionId).
func (s *cdpSession) On(method string, handler func(params json.RawMessage)) {
	s.onForSession("", method, handler)
}

func (s *cdpSession) onForSession(sessionID, method string, handler func(params json.RawMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := handlerKey{method: method, sessionID: sessionID}
	s.handlers[key] = append(s.handlers[key], handler)
}

// removeHandlers drops every handler registered for a child target's
// sessionId. Called when that target detaches (see flatSession.Close):
// without it, a long-lived kiosk connection that sees many iframe
// navigations would accumulate dead per-target handler entries
// unboundedly, since each freshly-attached iframe gets a brand-new
// sessionId. The empty (top-level) sessionId is never removed this way —
// its handlers live for the whole connection.
func (s *cdpSession) removeHandlers(sessionID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for key := range s.handlers {
		if key.sessionID == sessionID {
			delete(s.handlers, key)
		}
	}
}

// ForSession returns a view scoped to sessionID. The empty id is the
// top-level target, which is this connection itself.
func (s *cdpSession) ForSession(sessionID string) CDPSession {
	if sessionID == "" {
		return s
	}
	return &flatSession{parent: s, sessionID: sessionID}
}

func (s *cdpSession) Close() error {
	var err error
	s.closeOnce.Do(func() {
		err = s.conn.Close()
	})
	return err
}

// flatSession is a CDPSession view scoped to one flat-mode child target,
// sharing the parent connection's read pump, write serialization, and
// pending-call machinery. It exists so replay.go can keep threading a
// single CDPSession per intercepted target (one per attached iframe)
// through its per-request fulfill/continue/fail helpers unchanged — each
// target just gets its own flatSession whose Send/On carry that target's
// sessionId automatically.
type flatSession struct {
	parent    *cdpSession
	sessionID string
}

func (f *flatSession) Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	return f.parent.sendForSession(ctx, f.sessionID, method, params)
}

func (f *flatSession) On(method string, handler func(params json.RawMessage)) {
	f.parent.onForSession(f.sessionID, method, handler)
}

func (f *flatSession) ForSession(sessionID string) CDPSession {
	return f.parent.ForSession(sessionID)
}

// Close on a child view is a per-target DETACH, not a connection
// teardown: it only unregisters this target's event handlers. It must
// never close the parent socket — several child targets and the
// top-level page all share that one connection, so closing it here would
// silently kill interception for every OTHER attached target too. The
// child target's actual end is signaled out of band by
// Target.detachedFromTarget (see kiosktargets.go); this just stops us
// dispatching any late in-flight events for it.
func (f *flatSession) Close() error {
	f.parent.removeHandlers(f.sessionID)
	return nil
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
			s.dispatchEvent(msg.Method, msg.SessionID, msg.Params)
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

func (s *cdpSession) dispatchEvent(method, sessionID string, params json.RawMessage) {
	s.mu.Lock()
	handlers := append([]func(json.RawMessage){}, s.handlers[handlerKey{method: method, sessionID: sessionID}]...)
	s.mu.Unlock()
	for _, h := range handlers {
		h(params)
	}
}
