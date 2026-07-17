package cdp

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

var (
	ErrAlreadyInitialized          = errors.New("already initialized")
	ErrClientClosed                = errors.New("CDP client is closed")
	ErrCDPConnectionNotInitialized = errors.New("CDP connection is not initialized")
	ErrNoPageTargetFound           = errors.New("no page target found in Chromium instance")
	ErrMultiplePageTargetsFound    = errors.New("multiple page targets found in Chromium instance")
)

type RemoteError struct {
	Method      string
	Description string
	Unsupported bool
}

func (e *RemoteError) Error() string {
	if e.Method == "" {
		return fmt.Sprintf("CDP error: %s", e.Description)
	}
	return fmt.Sprintf("CDP error: %s: %s", e.Method, e.Description)
}

const (
	// CDP Methods
	METHOD_EVALUATE = "Runtime.evaluate"

	// CDP Types
	TYPE_STRING = "string"
	TYPE_OBJECT = "object"

	// CDP Subtypes
	SUBTYPE_ERROR = "error"
)

//go:generate mockgen -source=cdp.go -destination=../mocks/cdp.go -package=mocks -mock_names=CDP=MockCDP
type CDP interface {
	// Init performs a single blocking connect attempt. It is the low-level primitive
	// exercised by the discovery/dial unit tests; production startup uses Start.
	Init(ctx context.Context) error
	// Start launches the background connect/reconnect supervisor and returns immediately.
	// CDP availability must never gate daemon startup on a headless device, so callers
	// invoke this instead of Init and do not treat CDP absence as fatal. onConnect (may be
	// nil) runs after every successful (re)connect so callers can re-sync page state.
	Start(ctx context.Context, onConnect func())
	Send(method string, params map[string]interface{}) (interface{}, error)
	NoLogSend(method string, params map[string]interface{}) (interface{}, error)
	PageNavigationURL(ctx context.Context) (string, error)
	Close()
	Initialized() bool
}

type cdp struct {
	mu sync.Mutex

	// Wrappers to be injected
	dialer     wrapper.WebSocketDialer
	io         wrapper.IO
	json       wrapper.JSON
	httpClient wrapper.HTTPClient

	// Internal state
	conn     wrapper.WebSocketConn
	reqID    int
	endpoint string
	doneChan chan struct{}

	// reconnectCh signals the background connect loop that the active connection died and
	// must be re-established. It is buffered (depth 1) so concurrent Send failures collapse
	// into a single reconnect and never block the send path.
	reconnectCh chan struct{}
	// onConnect runs after every successful (re)connect from the connect loop. Set by Start.
	onConnect func()
	// retryInterval paces the connect loop's retry/reconnect cadence. Defaults to
	// connectRetryInterval; overridable in tests to keep the state machine fast.
	retryInterval time.Duration
	// sendTimeout bounds one send's write+read while holding c.mu. Defaults to
	// sendRequestTimeout; overridable in tests to exercise the wedged-socket path fast.
	sendTimeout time.Duration

	// Logger
	logger *zap.Logger
}

// New creates a new CDP client
func New(
	endpoint string,
	dialer wrapper.WebSocketDialer,
	io wrapper.IO,
	json wrapper.JSON,
	httpClient wrapper.HTTPClient,
	logger *zap.Logger,
) CDP {
	return &cdp{
		dialer:        dialer,
		io:            io,
		json:          json,
		httpClient:    httpClient,
		endpoint:      endpoint,
		reqID:         0,
		doneChan:      make(chan struct{}),
		reconnectCh:   make(chan struct{}, 1),
		retryInterval: connectRetryInterval,
		sendTimeout:   sendRequestTimeout,
		logger:        logger,
	}
}

// connectRetryInterval paces the background connect loop. A headless device may leave
// Chromium's DevTools endpoint (127.0.0.1:9222) absent for minutes or hours (no monitor)
// and it can drop and return across kiosk restarts, so we retry at a steady, modest
// cadence: short enough to reconnect promptly once a display appears, long enough not to
// hammer /json or flood logs while the box is intentionally headless.
const connectRetryInterval = 5 * time.Second

// sendRequestTimeout bounds the write+read round-trip a single send performs while
// holding c.mu. The hold MUST stay bounded: a kiosk restart can leave the socket
// writable but never-replying, and an unbounded ReadMessage would then wedge the send
// goroutine on the mutex forever — reconnect never signals (it needs a send failure),
// and Initialized/Close/teardownConn all deadlock behind the same lock. Generous
// relative to real CDP replies (evaluate calls answer in milliseconds) so it only
// fires on a genuinely dead target, where poisoning the conn is what we want: the
// deadline error signals the connect loop to tear down and re-dial.
const sendRequestTimeout = 15 * time.Second

// Initialized returns true if the CDP connection is initialized
func (c *cdp) Initialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Init fetches WS endpoint and dials Chromium in a single blocking attempt. It is the
// low-level primitive: production startup uses Start (background supervisor) instead so
// CDP absence never gates the daemon. Init owns a watcher goroutine that closes the
// connection on ctx cancellation.
func (c *cdp) Init(ctx context.Context) error {
	c.logger.Info("Initializing CDP", zap.String("endpoint", c.endpoint))

	if err := ctx.Err(); err != nil {
		return err
	}

	// Hold c.mu across discovery+dial so concurrent Init callers cannot each open a second
	// connection (see TestClient_Init_Async).
	c.mu.Lock()
	// Close is terminal (it is the stop signal for the connect supervisor), so a closed
	// client must not be re-initializable — its watcher would exit instantly and the
	// connection would leak past shutdown.
	select {
	case <-c.doneChan:
		c.mu.Unlock()
		return ErrClientClosed
	default:
	}
	if c.conn != nil {
		c.mu.Unlock()
		return ErrAlreadyInitialized
	}
	conn, err := c.dialPageTarget(ctx)
	if err != nil {
		c.mu.Unlock()
		return err
	}
	c.conn = conn
	c.mu.Unlock()

	c.logger.Info("Connected to CDP")

	// Start goroutine to handle context cancellation
	go func() {
		for {
			select {
			case <-ctx.Done():
				c.Close()
				return
			case <-c.doneChan:
				return
			}
		}
	}()

	return nil
}

// dialPageTarget performs one CDP discovery+dial attempt: fetch /json, require exactly one
// page target (the single kiosk page), and dial its websocket debugger URL. It touches no
// shared connection state and does not hold c.mu, so callers own storing the returned conn.
// Discovery failures are logged at debug because on a headless device the endpoint is
// expected to be absent and the connect loop retries on an interval — higher log levels
// here would flood logs/Sentry. The ws debugger URL changes across Chromium restarts, so
// this re-fetches /json on every call rather than caching a target.
func (c *cdp) dialPageTarget(ctx context.Context) (wrapper.WebSocketConn, error) {
	// Bind the /json fetch to ctx (plus a cap) so it respects cancellation; raw http.Get
	// ignores the request context and can block for the shared client's default timeout.
	fetchCtx, fetchCancel := context.WithTimeout(ctx, initDebugTargetsFetchTimeout)
	defer fetchCancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, c.endpoint+"/json", nil)
	if err != nil {
		return nil, fmt.Errorf("failed to build debug targets request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch debug targets: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Failed to close response body", zap.Error(err))
		}
	}()

	body, err := c.io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read targets: %w", err)
	}

	var targets []struct {
		Type                 string `json:"type"`
		Title                string `json:"title"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := c.json.Unmarshal(body, &targets); err != nil {
		return nil, fmt.Errorf("invalid targets format: %w", err)
	}

	var pageTargets []struct {
		Type                 string `json:"type"`
		Title                string `json:"title"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	for _, t := range targets {
		if t.Type == "page" {
			pageTargets = append(pageTargets, t)
		}
	}
	c.logger.Debug("Filtered CDP page targets",
		zap.Int("target_count", len(targets)),
		zap.Int("page_target_count", len(pageTargets)))

	if len(pageTargets) == 0 {
		return nil, ErrNoPageTargetFound
	}
	if len(pageTargets) > 1 {
		return nil, ErrMultiplePageTargetsFound
	}

	target := pageTargets[0]
	conn, _, err := c.dialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp dial error: %w", err)
	}
	c.logger.Debug("Connected to CDP", zap.String("websocket_debugger_url", target.WebSocketDebuggerURL))
	return conn, nil
}

// Start launches the background connect/reconnect supervisor. It returns immediately so
// CDP availability never gates daemon startup (DBus, relayer, systemd READY). The loop owns
// the connection for the process lifetime and exits only on ctx cancellation.
func (c *cdp) Start(ctx context.Context, onConnect func()) {
	c.onConnect = onConnect
	go c.connectLoop(ctx)
}

// connectLoop establishes and re-establishes the CDP connection until ctx is canceled.
// It retries while Chromium is absent (headless boot), and reconnects after a drop
// (kiosk/Chromium restart). Because the client is synchronous request/response, a dropped
// socket only surfaces as a Send write/read error, which signals reconnectCh; the loop then
// tears down and re-dials. The loop is the sole owner of teardown-on-drop so the send path
// stays lock-simple.
func (c *cdp) connectLoop(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		// Stop before dialing when Close already happened — avoids a pointless dial whose
		// result would be discarded by the shutdown check below anyway.
		select {
		case <-c.doneChan:
			return
		default:
		}

		conn, err := c.dialPageTarget(ctx)
		if err != nil {
			// Absence is the expected steady state on a headless device; keep at debug so
			// it does not flood logs/Sentry while intentionally without a display.
			c.logger.Debug("CDP not connected, will retry", zap.Error(err))
			if !c.sleep(ctx, c.retryInterval) {
				return
			}
			continue
		}

		// Close closes doneChan while holding c.mu, so checking it under the same lock
		// makes "cache the fresh conn" and "shutdown already happened" mutually exclusive:
		// a Close that raced the dial wins, and we must not cache the connection or fire
		// onConnect after shutdown.
		c.mu.Lock()
		select {
		case <-c.doneChan:
			c.mu.Unlock()
			if closeErr := conn.Close(); closeErr != nil {
				c.logger.Warn("Failed to close CDP connection after shutdown race", zap.Error(closeErr))
			}
			return
		default:
			c.conn = conn
		}
		c.mu.Unlock()
		c.logger.Info("CDP connected")

		// Re-sync web-app-facing state after every (re)connect so the daemon converges to
		// the state a fresh boot would produce (Chromium reloads its page across restarts).
		if c.onConnect != nil {
			c.onConnect()
		}

		select {
		case <-ctx.Done():
			c.teardownConn()
			return
		case <-c.doneChan:
			// Close() already tore the connection down; just stop supervising.
			return
		case <-c.reconnectCh:
			c.logger.Info("CDP connection dropped, reconnecting")
			c.teardownConn()
			if !c.sleep(ctx, c.retryInterval) {
				return
			}
		}
	}
}

// sleep waits for d, ctx cancellation, or Close. Returns false when the wait was
// interrupted so callers exit the connect loop promptly on shutdown.
func (c *cdp) sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-c.doneChan:
		return false
	case <-t.C:
		return true
	}
}

// teardownConn closes and clears the active connection. Idempotent so it is safe to call
// from both the connect loop and Close during shutdown.
func (c *cdp) teardownConn() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn == nil {
		return
	}
	if err := c.conn.Close(); err != nil {
		c.logger.Warn("Failed to close CDP connection", zap.Error(err))
	}
	c.conn = nil
}

// signalDrop wakes the connect loop to re-establish the connection after a failed
// write/read. Non-blocking: the buffered reconnectCh collapses concurrent failures into a
// single reconnect. A no-op when Start was never called (e.g. the Init-only unit tests).
func (c *cdp) signalDrop() {
	select {
	case c.reconnectCh <- struct{}{}:
	default:
	}
}

// initDebugTargetsFetchTimeout matches the shared HTTP client round-trip limit so
// bootstrap /json discovery stays aligned with prior Get behavior while the request
// remains context-driven (see Init). A shorter cap caused false Init failures when
// Chromium answered /json between that cap and the client timeout (cold boot / recovery).
const initDebugTargetsFetchTimeout = wrapper.HTTPClientTimeout

// pageNavigationURLFetchTimeout bounds the best-effort /json probe. Without this,
// a hung Chromium devtools endpoint could block until the shared HTTP client's
// 30s timeout, stalling DeviceStatus polling and downstream notifications.
const pageNavigationURLFetchTimeout = 5 * time.Second

func (c *cdp) PageNavigationURL(ctx context.Context) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(ctx, pageNavigationURLFetchTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/json", nil)
	if err != nil {
		return "", fmt.Errorf("failed to build debug targets request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("failed to fetch debug targets: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Failed to close response body", zap.Error(err))
		}
	}()

	body, err := c.io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read targets: %w", err)
	}

	var targets []struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}
	if err := c.json.Unmarshal(body, &targets); err != nil {
		return "", fmt.Errorf("invalid targets format: %w", err)
	}
	c.logger.Debug("Fetched CDP targets for navigation URL", zap.Int("target_count", len(targets)))

	pageTargets := make([]string, 0, len(targets))
	for _, t := range targets {
		if t.Type == "page" {
			pageTargets = append(pageTargets, t.URL)
		}
	}
	c.logger.Debug("Filtered CDP page targets for navigation URL", zap.Int("page_target_count", len(pageTargets)))

	if len(pageTargets) == 0 {
		return "", ErrNoPageTargetFound
	}

	if len(pageTargets) > 1 {
		return "", ErrMultiplePageTargetsFound
	}

	return pageTargets[0], nil
}

// Send sends a raw CDP JSON-RPC message with logging
func (c *cdp) Send(method string, params map[string]interface{}) (interface{}, error) {
	logParams, _ := helper.TruncateMap(params, logger.MAX_FIELD_LENGTH)
	c.logger.Info("Sending CDP request", zap.String("method", method), zap.ByteString("params", logParams))

	return c.send(method, params)
}

// NoLogSend sends a raw CDP JSON-RPC message without logging
func (c *cdp) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	return c.send(method, params)
}

func (c *cdp) send(method string, params map[string]interface{}) (interface{}, error) {
	c.mu.Lock()
	if c.conn == nil {
		c.mu.Unlock()
		return nil, ErrCDPConnectionNotInitialized
	}

	c.reqID++
	reqID := c.reqID
	c.mu.Unlock()

	msg := map[string]interface{}{
		"id":     reqID,
		"method": method,
		"params": params,
	}

	data, err := c.json.Marshal(msg)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal CDP message: %w", err)
	}

	c.mu.Lock()
	// Re-check under the re-acquired lock: between the ID allocation above and here,
	// a failed send on another goroutine can have woken the connect loop, whose
	// teardownConn nils c.conn. Writing through the stale copy would panic.
	if c.conn == nil {
		c.mu.Unlock()
		return nil, ErrCDPConnectionNotInitialized
	}
	// One deadline covers the whole write+read round-trip so the c.mu hold is bounded
	// (see sendRequestTimeout). A socket that is writable but never replies — the
	// post-kiosk-restart zombie — must surface as an error here, because a send failure
	// is the only signal that wakes the connect loop to tear down and re-dial.
	deadline := time.Now().Add(c.sendTimeout)
	if err := c.conn.SetWriteDeadline(deadline); err != nil {
		c.mu.Unlock()
		c.signalDrop()
		return nil, fmt.Errorf("failed to set CDP write deadline: %w", err)
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.mu.Unlock()
		// A write failure means the socket is dead (Chromium gone/restarting). Wake the
		// connect loop to tear down and re-establish so later sends fail clean rather than
		// hammering a dead conn.
		c.signalDrop()
		return nil, fmt.Errorf("CDP write error: %w", err)
	}

	// Wait for response. An expired read deadline poisons the gorilla conn (all later
	// reads fail), which is intended: the drop signal below has the connect loop replace
	// the connection entirely.
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		c.mu.Unlock()
		c.signalDrop()
		return nil, fmt.Errorf("failed to set CDP read deadline: %w", err)
	}
	_, response, err := c.conn.ReadMessage()
	if err != nil {
		c.mu.Unlock()
		c.signalDrop()
		return nil, fmt.Errorf("failed to read CDP response: %w", err)
	}
	c.mu.Unlock()

	c.logger.Debug("Received CDP response",
		zap.String("method", method),
		zap.String("response", string(response)))

	var resp struct {
		ID    int `json:"id"`
		Error *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
			Data    any    `json:"data"`
		} `json:"error"`
		Result struct {
			Result struct {
				Type        string      `json:"type"`
				Subtype     *string     `json:"subtype"`
				ClassName   *string     `json:"className"`
				Description *string     `json:"description"`
				Value       interface{} `json:"value"`
			} `json:"result"`
		} `json:"result"`
	}
	if err := c.json.Unmarshal(response, &resp); err != nil {
		return nil, fmt.Errorf("failed to parse CDP response: %w", err)
	}

	if resp.Error != nil {
		return nil, &RemoteError{
			Method:      method,
			Description: resp.Error.Message,
			Unsupported: isUnsupportedRemoteMethodError(method, resp.Error.Code),
		}
	}

	result := resp.Result.Result

	// Check for uncaught errors
	if result.Type == TYPE_OBJECT &&
		result.Subtype != nil &&
		*result.Subtype == SUBTYPE_ERROR {
		description := "remote error"
		switch {
		case result.Description != nil && strings.TrimSpace(*result.Description) != "":
			description = *result.Description
		case result.ClassName != nil && strings.TrimSpace(*result.ClassName) != "":
			description = *result.ClassName
		}
		return nil, &RemoteError{
			Method:      method,
			Description: description,
			Unsupported: false,
		}
	}

	// Check for response type mismatch
	switch result.Type {
	case TYPE_STRING:
		var v map[string]interface{}
		if err := c.json.Unmarshal([]byte(result.Value.(string)), &v); err != nil {
			return nil, fmt.Errorf("CDP unmarshal error: %w", err)
		}
		return v, nil
	case TYPE_OBJECT:
		return result.Value, nil
	case "":
		return nil, nil
	default:
		return nil, fmt.Errorf("CDP response type mismatch: %s", result.Type)
	}
}

func isUnsupportedRemoteMethodError(method string, code int) bool {
	return method == "Input.synthesizePinchGesture" && code == -32601
}

// Close closes the CDP connection
func (c *cdp) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// doneChan is the stop signal for the connect supervisor, so it must close even
	// when no connection was ever established (the headless steady state) — otherwise
	// the loop keeps dialing, and could cache a connection, after Close.
	select {
	case <-c.doneChan:
		// Already closed
		return
	default:
		close(c.doneChan)
	}

	if c.conn == nil {
		return
	}

	c.logger.Info("Closing CDP connection")

	err := c.conn.Close()
	if err != nil {
		c.logger.Warn("Failed to close CDP connection", zap.Error(err))
	}

	c.conn = nil
	c.logger.Info("CDP connection closed")
}
