package cdp

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-connectd/wrapper"
)

var (
	ErrAlreadyInitialized          = errors.New("already initialized")
	ErrCDPConnectionNotInitialized = errors.New("CDP connection is not initialized")
	ErrNoPageTargetFound           = errors.New("no page target found in Chromium instance")
	ErrMultiplePageTargetsFound    = errors.New("multiple page targets found in Chromium instance")
)

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
	Close()
	Initialized() bool
}

type cdp struct {
	mu sync.Mutex

	// Wrappers to be injected
	dialer wrapper.WebSocketDialer
	io     wrapper.IO
	json   wrapper.JSON
	http   wrapper.HTTP

	// Internal state
	conn     wrapper.WebSocketConn
	reqID    int
	endpoint string
	doneChan chan struct{}

	// Logger
	logger *zap.Logger
}

// New creates a new CDP client
func New(
	endpoint string,
	dialer wrapper.WebSocketDialer,
	io wrapper.IO,
	json wrapper.JSON,
	http wrapper.HTTP,
	logger *zap.Logger,
) CDP {
	return &cdp{
		dialer:   dialer,
		io:       io,
		json:     json,
		http:     http,
		endpoint: endpoint,
		reqID:    0,
		doneChan: make(chan struct{}),
		logger:   logger,
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

	// Ensure the relayer is not connected
	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return ErrAlreadyInitialized
	}

	// Fetch JSON with websocket debugger URL
	resp, err := c.http.Get(c.endpoint + "/json")
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("failed to fetch debug targets: %w", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			c.logger.Warn("Failed to close response body", zap.Error(err))
		}
	}()

	body, err := c.io.ReadAll(resp.Body)
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("failed to read targets: %w", err)
	}

	var targets []struct {
		Type                 string `json:"type"`
		Title                string `json:"title"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := c.json.Unmarshal(body, &targets); err != nil {
		c.mu.Unlock()
		return fmt.Errorf("invalid targets format: %w", err)
	}

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

	if len(pageTargets) == 0 {
		c.mu.Unlock()
		return ErrNoPageTargetFound
	}

	if len(pageTargets) > 1 {
		c.mu.Unlock()
		return ErrMultiplePageTargetsFound
	}

	// Connect to the single page target
	target := pageTargets[0]
	conn, _, err := c.dialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		c.mu.Unlock()
		return fmt.Errorf("cdp dial error: %w", err)
	}
	c.conn = conn
	c.mu.Unlock()

	c.logger.Info("Connected to CDP", zap.String("url", target.WebSocketDebuggerURL))

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

// Send sends a raw CDP JSON-RPC message and waits for response
func (c *cdp) Send(method string, params map[string]interface{}) (interface{}, error) {
	c.logger.Info("Sending CDP request", zap.String("method", method), zap.Any("params", params))

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
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("CDP write error: %w", err)
	}

	// Wait for response
	_, response, err := c.conn.ReadMessage()
	if err != nil {
		c.mu.Unlock()
		return nil, fmt.Errorf("failed to read CDP response: %w", err)
	}
	c.mu.Unlock()

	c.logger.Debug("Received CDP response",
		zap.String("method", method),
		zap.String("response", string(response)))

	var resp struct {
		ID     int `json:"id"`
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

	result := resp.Result.Result

	// Check for uncaught errors
	if result.Type == TYPE_OBJECT &&
		result.Subtype != nil &&
		*result.Subtype == SUBTYPE_ERROR {
		return nil, fmt.Errorf("CDP error: %v", *result.Description)
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

// Close closes the CDP connection
func (c *cdp) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.conn == nil {
		// Already closed
		return
	}

	c.logger.Info("Closing CDP connection")

	select {
	case <-c.doneChan:
		// Already closed
		return
	default:
		close(c.doneChan)
	}

	err := c.conn.Close()
	if err != nil {
		c.logger.Warn("Failed to close CDP connection", zap.Error(err))
	}

	c.conn = nil
	c.logger.Info("CDP connection closed")
}
