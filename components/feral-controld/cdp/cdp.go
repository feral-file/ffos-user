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
	Init(ctx context.Context) error
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
	clock      wrapper.Clock

	// Internal state
	conn       wrapper.WebSocketConn
	reqID      int
	endpoint   string
	doneChan   chan struct{}
	supervised bool // true once Init has started the connection supervisor

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
	clock wrapper.Clock,
	logger *zap.Logger,
) CDP {
	return &cdp{
		dialer:     dialer,
		io:         io,
		json:       json,
		httpClient: httpClient,
		clock:      clock,
		endpoint:   endpoint,
		reqID:      0,
		doneChan:   make(chan struct{}),
		logger:     logger,
	}
}

// Initialized returns true if the CDP connection is initialized
func (c *cdp) Initialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Init fetches WS endpoint and dials Chromium
func (c *cdp) Init(ctx context.Context) error {
	c.logger.Info("Initializing CDP", zap.String("endpoint", c.endpoint))

	if err := ctx.Err(); err != nil {
		return err
	}

	// Hold the lock across discovery and dial so concurrent Init calls
	// serialize: exactly one connects, the rest get ErrAlreadyInitialized.
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.conn != nil {
		return ErrAlreadyInitialized
	}

	if err := c.connectLocked(ctx, initDebugTargetsFetchTimeout); err != nil {
		return err
	}

	// Handle context cancellation and redial after send() tears down a dead
	// socket. A Chromium restart drops the DevTools websocket; without the
	// redial, every command fails until controld itself is restarted.
	c.supervised = true
	go c.superviseConnection(ctx)

	return nil
}

// connectLocked discovers the single page target, dials it, and installs the
// connection. Callers must hold c.mu.
func (c *cdp) connectLocked(ctx context.Context, fetchTimeout time.Duration) error {
	// Fetch JSON with websocket debugger URL. Use a request bound to ctx (plus a cap) so
	// this step respects cancellation; raw http.Get ignores the request context and can
	// block for the shared client's default timeout.
	fetchCtx, fetchCancel := context.WithTimeout(ctx, fetchTimeout)
	defer fetchCancel()
	req, err := http.NewRequestWithContext(fetchCtx, http.MethodGet, c.endpoint+"/json", nil)
	if err != nil {
		return fmt.Errorf("failed to build debug targets request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to fetch debug targets: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Failed to close response body", zap.Error(err))
		}
	}()

	body, err := c.io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("failed to read targets: %w", err)
	}

	var targets []struct {
		Type                 string `json:"type"`
		Title                string `json:"title"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := c.json.Unmarshal(body, &targets); err != nil {
		return fmt.Errorf("invalid targets format: %w", err)
	}
	c.logger.Info("Fetched CDP targets", zap.Int("target_count", len(targets)))

	// Collect all page targets
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
	c.logger.Info("Filtered CDP page targets", zap.Int("page_target_count", len(pageTargets)))

	if len(pageTargets) == 0 {
		c.logger.Error("No CDP page targets available")
		return ErrNoPageTargetFound
	}

	if len(pageTargets) > 1 {
		c.logger.Error("Multiple CDP page targets available", zap.Int("page_target_count", len(pageTargets)))
		return ErrMultiplePageTargetsFound
	}

	// Connect to the single page target
	target := pageTargets[0]
	c.logger.Info("Selected CDP page target", zap.String("websocket_debugger_url", target.WebSocketDebuggerURL))
	conn, _, err := c.dialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		c.logger.Error("CDP dial failed", zap.String("websocket_debugger_url", target.WebSocketDebuggerURL), zap.Error(err))
		return fmt.Errorf("cdp dial error: %w", err)
	}
	c.conn = conn

	c.logger.Info("Connected to CDP", zap.String("url", target.WebSocketDebuggerURL))
	return nil
}

// superviseConnection closes the client on context cancellation and redials
// whenever the connection has been torn down by a transport failure. It runs
// for the lifetime of the client; Close stops it.
func (c *cdp) superviseConnection(ctx context.Context) {
	ticker := c.clock.NewTicker(reconnectInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			c.Close()
			return
		case <-c.doneChan:
			return
		case <-ticker.C():
			c.mu.Lock()
			closed := false
			select {
			case <-c.doneChan:
				closed = true
			default:
			}
			if closed {
				c.mu.Unlock()
				return
			}
			if c.conn != nil {
				c.mu.Unlock()
				continue
			}

			err := c.connectLocked(ctx, reconnectTargetsFetchTimeout)
			c.mu.Unlock()
			if err != nil {
				c.logger.Warn("CDP reconnect attempt failed", zap.Error(err))
				continue
			}
			c.logger.Info("CDP reconnected")
		}
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

// reconnectInterval paces redial attempts after the connection dies. Chromium
// needs a few seconds after a restart to expose a new page target, so faster
// polling only burns discovery requests.
const reconnectInterval = 5 * time.Second

// reconnectTargetsFetchTimeout bounds each reconnect discovery probe so a hung
// devtools endpoint cannot pin the supervisor for the shared client's timeout.
const reconnectTargetsFetchTimeout = 5 * time.Second

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
	if c.conn == nil {
		c.mu.Unlock()
		return nil, ErrCDPConnectionNotInitialized
	}
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		// gorilla/websocket connections are permanently broken after a
		// write error; tear down so the supervisor redials.
		c.teardownConnLocked("write error")
		c.mu.Unlock()
		return nil, fmt.Errorf("CDP write error: %w", err)
	}

	// Wait for response
	_, response, err := c.conn.ReadMessage()
	if err != nil {
		// Read errors are equally terminal for the connection.
		c.teardownConnLocked("read error")
		c.mu.Unlock()
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

// teardownConnLocked closes and clears a broken connection so Initialized()
// reports false and the supervisor redials. Callers must hold c.mu.
func (c *cdp) teardownConnLocked(reason string) {
	if c.conn == nil {
		return
	}
	if err := c.conn.Close(); err != nil {
		c.logger.Warn("Failed to close broken CDP connection", zap.Error(err))
	}
	c.conn = nil
	c.logger.Warn("CDP connection torn down, awaiting reconnect", zap.String("reason", reason))
}

// Close closes the CDP connection
func (c *cdp) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil && !c.supervised {
		// Never initialized — Close before Init is a no-op.
		return
	}

	// Always stop the supervisor, even when the connection is already torn
	// down — otherwise a shutdown while disconnected leaks the goroutine and
	// it may redial after Close.
	select {
	case <-c.doneChan:
		// Already closed
		return
	default:
		close(c.doneChan)
	}

	if c.conn == nil {
		// No live connection to close
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
