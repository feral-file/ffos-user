package cdp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap/zaptest"
)

// These tests exercise the background connect/reconnect supervisor (Start). They live in
// package cdp (not cdp_test) so they can shrink retryInterval to keep the state machine
// fast. They use local fakes rather than the generated mocks because the mocks package
// imports status, which imports cdp — importing mocks from an internal cdp test would form
// a cycle. The lower-level discovery/dial behavior is covered by the external cdp_test.go.

const fakeTargetsJSON = `[{"type":"page","title":"kiosk","webSocketDebuggerUrl":"ws://localhost:9222/devtools/page/1"}]`

// fakeHTTP serves /json discovery. When unavailable it errors like a refused connection,
// modeling a headless device whose Chromium DevTools endpoint is not up yet.
type fakeHTTP struct{ available atomic.Bool }

func (f *fakeHTTP) Do(*http.Request) (*http.Response, error) {
	if !f.available.Load() {
		return nil, errors.New("connection refused")
	}
	return &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(bytes.NewReader([]byte(fakeTargetsJSON))),
	}, nil
}
func (f *fakeHTTP) NewRequest(string, string, io.Reader) (*http.Request, error) { return nil, nil }
func (f *fakeHTTP) Get(string) (*http.Response, error)                          { return nil, nil }
func (f *fakeHTTP) Post(string, string, io.Reader) (*http.Response, error)      { return nil, nil }

// fakeIO and fakeJSON delegate to the standard library so discovery parses for real.
type fakeIO struct{}

func (fakeIO) ReadAll(r io.Reader) ([]byte, error) { return io.ReadAll(r) }

type fakeJSON struct{}

func (fakeJSON) Marshal(v any) ([]byte, error)            { return json.Marshal(v) }
func (fakeJSON) Unmarshal(d []byte, v any) error          { return json.Unmarshal(d, v) }
func (fakeJSON) NewDecoder(io.Reader) wrapper.JSONDecoder { return nil }
func (fakeJSON) NewEncoder(io.Writer) wrapper.JSONEncoder { return nil }

// fakeConn is a websocket connection whose write result and dial count are test-controlled.
type fakeConn struct {
	mu       sync.Mutex
	writeErr error
	closed   int
}

func (c *fakeConn) setWriteErr(err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.writeErr = err
}
func (c *fakeConn) WriteMessage(int, []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeErr
}
func (c *fakeConn) ReadMessage() (int, []byte, error) { return 0, nil, errors.New("not used") }
func (c *fakeConn) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed++
	return nil
}
func (c *fakeConn) WriteJSON(interface{}) error               { return nil }
func (c *fakeConn) WriteControl(int, []byte, time.Time) error { return nil }
func (c *fakeConn) SetPongHandler(func(string) error)         {}
func (c *fakeConn) SetReadDeadline(time.Time) error           { return nil }
func (c *fakeConn) SetWriteDeadline(time.Time) error          { return nil }

type fakeDialer struct {
	conn  *fakeConn
	dials atomic.Int32
}

func (d *fakeDialer) DialContext(context.Context, string, http.Header) (wrapper.WebSocketConn, *http.Response, error) {
	d.dials.Add(1)
	return d.conn, nil, nil
}

func newTestClient(t *testing.T) (*cdp, *fakeHTTP, *fakeDialer, *fakeConn) {
	t.Helper()
	fh := &fakeHTTP{}
	fh.available.Store(true)
	fc := &fakeConn{}
	fd := &fakeDialer{conn: fc}
	c := New("http://localhost:9222", fd, fakeIO{}, fakeJSON{}, fh, zaptest.NewLogger(t)).(*cdp)
	// Keep the retry/reconnect cadence tiny so the loop converges within the test timeout.
	c.retryInterval = time.Millisecond
	return c, fh, fd, fc
}

// waitFor polls cond until it holds or the deadline elapses.
func waitFor(t *testing.T, cond func() bool, msg string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("condition not met within timeout: %s", msg)
}

// TestStart_ConnectsWhenAvailable: with CDP present from the start, the supervisor connects
// and invokes onConnect so callers can re-sync page state.
func TestStart_ConnectsWhenAvailable(t *testing.T) {
	c, _, _, _ := newTestClient(t)

	var onConnectCalls int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.Start(ctx, func() { atomic.AddInt32(&onConnectCalls, 1) })

	waitFor(t, c.Initialized, "CDP should connect when available")
	waitFor(t, func() bool { return atomic.LoadInt32(&onConnectCalls) >= 1 }, "onConnect should run after connect")

	cancel()
	waitFor(t, func() bool { return !c.Initialized() }, "CDP should tear down on shutdown")
}

// TestStart_RetriesUntilAvailable: CDP absent at boot (headless with no monitor) must not be
// fatal; the supervisor keeps retrying and connects once Chromium appears.
func TestStart_RetriesUntilAvailable(t *testing.T) {
	c, fh, _, _ := newTestClient(t)
	fh.available.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.Start(ctx, nil)

	// Let the loop spin on the unavailable endpoint, then confirm it neither connects nor
	// gets stuck, and that it connects once the endpoint appears.
	time.Sleep(20 * time.Millisecond)
	assert.False(t, c.Initialized(), "CDP must stay disconnected while endpoint is absent")

	fh.available.Store(true)
	waitFor(t, c.Initialized, "CDP should connect once the endpoint appears")
}

// TestStart_ReconnectsAfterDrop: a dropped socket (kiosk/Chromium restart) surfaces as a
// Send failure; the supervisor must tear down and re-dial so the client recovers.
func TestStart_ReconnectsAfterDrop(t *testing.T) {
	c, _, fd, fc := newTestClient(t)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.Start(ctx, nil)
	waitFor(t, c.Initialized, "CDP should connect initially")
	waitFor(t, func() bool { return fd.dials.Load() >= 1 }, "first dial should happen")

	// Simulate a dropped socket: the next Send write fails, which must signal a reconnect.
	fc.setWriteErr(errors.New("broken pipe"))
	_, err := c.Send(METHOD_EVALUATE, map[string]interface{}{"expression": "x"})
	assert.Error(t, err, "send on a dead socket should error")

	// Once the supervisor tears down and re-dials, let the fresh connection write cleanly.
	fc.setWriteErr(nil)
	waitFor(t, func() bool { return fd.dials.Load() >= 2 }, "supervisor should re-dial after a drop")
	waitFor(t, c.Initialized, "CDP should be reconnected after a drop")
}

// TestStart_StopsOnClose: Close must stop the supervisor even when no connection was ever
// established (the headless steady state) — it must not keep dialing or cache a connection
// that appears after shutdown.
func TestStart_StopsOnClose(t *testing.T) {
	c, fh, fd, _ := newTestClient(t)
	fh.available.Store(false)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c.Start(ctx, func() { t.Error("onConnect must not fire after Close") })

	// Let the loop spin on the absent endpoint, then shut down while still disconnected.
	time.Sleep(10 * time.Millisecond)
	c.Close()

	// The endpoint appearing after Close must not resurrect the supervisor. The loop may
	// legitimately finish one already-started iteration (timer vs doneChan select race), so
	// assert that dialing STABILIZES rather than demanding an exact count, and that a
	// post-Close dial can never surface as an initialized connection.
	fh.available.Store(true)
	waitFor(t, func() bool {
		before := fd.dials.Load()
		time.Sleep(10 * time.Millisecond)
		return fd.dials.Load() == before
	}, "supervisor should stop dialing after Close")
	assert.False(t, c.Initialized(), "CDP must stay disconnected after Close")
}
