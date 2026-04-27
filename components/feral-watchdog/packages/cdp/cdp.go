package cdp

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/packages/wrapper"
)

var (
	ErrAlreadyInitialized          = errors.New("already initialized")
	ErrCDPConnectionNotInitialized = errors.New("CDP connection is not initialized")
	ErrNoPageTargetFound           = errors.New("no page target found in Chromium instance")
	ErrMultiplePageTargetsFound    = errors.New("multiple page targets found in Chromium instance")
)

const (
	CDP_CRITICAL_CPU_TEMPERATURE_EVENT = "CriticalCPUTemperature"
	CDP_SERVICE_FAILED_EVENT           = "ServiceFailed"
	DISPLAY_FERALFILE_URL              = "http://127.0.0.1:8080/"

	// CDP Methods
	METHOD_EVALUATE = "Runtime.evaluate"
	METHOD_NAVIGATE = "Page.navigate"

	// CDP Types
	TYPE_STRING  = "string"
	TYPE_OBJECT  = "object"
	TYPE_BOOLEAN = "boolean"

	// CDP Subtypes
	SUBTYPE_ERROR = "error"
)

type Config struct {
	Endpoint string `json:"endpoint"`
}

//go:generate mockgen -source=cdp.go -destination=../mocks/mock_cdp.go -package=mocks -mock_names=ClientInterface=MockCDPClient
type ClientInterface interface {
	Init(ctx context.Context) error
	Send(method string, params map[string]interface{}) (interface{}, error)
	Close()
}

type Client struct {
	mu sync.Mutex

	// Wrappers to be injected
	dialer wrapper.WebSocketDialerInterface
	io     wrapper.IOInterface
	json   wrapper.JSONInterface
	http   wrapper.HTTPInterface

	// Internal state
	conn           wrapper.WebSocketConnInterface
	reqID          int
	endpoint       string
	isReconnecting bool
	doneChan       chan struct{}

	// Logger
	logger *zap.Logger
}

// NewClient creates a new CDP client with custom injected wrappers
func NewClient(
	config *Config,
	logger *zap.Logger,
	dialer wrapper.WebSocketDialerInterface,
	io wrapper.IOInterface,
	json wrapper.JSONInterface,
	http wrapper.HTTPInterface,
) *Client {
	return &Client{
		dialer:         dialer,
		io:             io,
		json:           json,
		http:           http,
		endpoint:       config.Endpoint,
		reqID:          0,
		logger:         logger,
		isReconnecting: false,
	}
}

// NewDefault creates a new CDP client with the default wrappers
func NewDefault(config *Config, logger *zap.Logger) *Client {
	return NewClient(
		config,
		logger,
		wrapper.NewWebSocketDialer(websocket.DefaultDialer),
		wrapper.NewIO(),
		wrapper.NewJSON(),
		wrapper.NewHTTP(),
	)
}

// Initialized returns true if the CDP connection is initialized
func (c *Client) Initialized() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.conn != nil
}

// Init fetches WS endpoint and dials Chromium
func (c *Client) Init(ctx context.Context) error {
	c.logger.Info("Initializing CDP", zap.String("endpoint", c.endpoint))

	// Ensure the relayer is not connected
	c.mu.Lock()
	if c.conn != nil {
		c.mu.Unlock()
		return ErrAlreadyInitialized
	}

	err := c.initLocked(ctx)
	c.mu.Unlock()
	return err
}

// initLocked performs the actual initialization logic (assumes lock is held)
func (c *Client) initLocked(ctx context.Context) error {
	// Fetch JSON with websocket debugger URL
	resp, err := c.http.Get(c.endpoint + "/json")
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
		return ErrNoPageTargetFound
	}

	if len(pageTargets) > 1 {
		return ErrMultiplePageTargetsFound
	}

	// Connect to the single page target
	target := pageTargets[0]
	conn, _, err := c.dialer.DialContext(ctx, target.WebSocketDebuggerURL, nil)
	if err != nil {
		return fmt.Errorf("cdp dial error: %w", err)
	}
	c.conn = conn
	c.doneChan = make(chan struct{})

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
func (c *Client) Send(method string, params map[string]interface{}) (interface{}, error) {
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
		return nil, err
	}

	c.mu.Lock()
	if err := c.conn.WriteMessage(websocket.TextMessage, data); err != nil {
		c.mu.Unlock()
		return nil, err
	}

	// Wait for response
	_, response, err := c.conn.ReadMessage()
	if err != nil {
		c.mu.Unlock()
		return nil, err
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
		return nil, err
	}

	result := resp.Result.Result

	// Check for uncaught errors
	if result.Type == TYPE_OBJECT &&
		result.Subtype != nil &&
		*result.Subtype == SUBTYPE_ERROR {
		return nil, errors.New(*result.Description)
	}

	// Check for response type mismatch
	switch result.Type {
	case TYPE_STRING:
		var v map[string]interface{}
		if err := c.json.Unmarshal([]byte(result.Value.(string)), &v); err != nil {
			return nil, err
		}
		return v, nil
	case TYPE_OBJECT:
		return result.Value, nil
	case TYPE_BOOLEAN:
		return result.Value, nil
	case "":
		return nil, nil
	default:
		return nil, errors.New("CDP response type mismatch: " + result.Type)
	}
}

func (c *Client) IsReconnectionError(err error) bool {
	if err == nil {
		return false
	}

	// Check for websocket close errors
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		c.logger.Debug("Detected websocket close error",
			zap.Int("code", closeErr.Code),
			zap.String("text", closeErr.Text))
		return true
	}

	// Check for common network errors that indicate connection issues
	errStr := err.Error()
	reconnectionErrors := []string{
		"connection reset by peer",
		"broken pipe",
		"connection refused",
		"network is unreachable",
		"no route to host",
		"timeout",
		"use of closed network connection",
		"write: broken pipe",
		"write: connection reset by peer",
	}

	for _, reconnectionError := range reconnectionErrors {
		if strings.Contains(errStr, reconnectionError) {
			c.logger.Debug("Detected reconnection error", zap.String("error", errStr))
			return true
		}
	}

	return false
}

func (c *Client) Reconnect(ctx context.Context) error {
	if c.isReconnecting {
		return nil
	}

	c.logger.Info("Reconnecting to CDP")
	c.mu.Lock()
	c.isReconnecting = true
	defer func() {
		c.isReconnecting = false
		c.mu.Unlock()
	}()

	// Close the connection if it exists
	if c.conn != nil {
		c.logger.Info("Closing existing CDP connection")

		select {
		case <-c.doneChan:
			// Already closed
		default:
			close(c.doneChan)
		}

		err := c.conn.Close()
		if err != nil {
			c.logger.Warn("Failed to close CDP connection", zap.Error(err))
		}

		c.conn = nil
	}

	// Re-initialize the connection
	return c.initLocked(ctx)
}

// sendWatchdogEvent sends a watchdog event to the website via CDP
func (c *Client) sendWatchdogEvent(ctx context.Context, eventType string) error {
	expression := fmt.Sprintf("window.handleWatchdogEvent(%q)", eventType)

	params := map[string]interface{}{
		"expression": expression,
	}

	_, err := c.Send(METHOD_EVALUATE, params)
	if err != nil {
		if c.IsReconnectionError(err) {
			if reconnErr := c.Reconnect(ctx); reconnErr != nil {
				return fmt.Errorf("failed to reconnect: %w", reconnErr)
			}
			// Retry after reconnect
			_, err = c.Send(METHOD_EVALUATE, params)
			if err != nil {
				return fmt.Errorf("failed to send watchdog event: %w", err)
			}
		} else {
			return fmt.Errorf("failed to send watchdog event: %w", err)
		}
	}

	return nil
}

// SendCriticalCPUTemperatureNotification sends a critical CPU temperature notification
func (c *Client) SendCriticalCPUTemperatureNotification(ctx context.Context) error {
	err := c.sendWatchdogEvent(ctx, CDP_CRITICAL_CPU_TEMPERATURE_EVENT)
	if err != nil {
		return err
	}
	c.logger.Info("Critical CPU temperature notification sent successfully")
	return nil
}

// SendServiceFailedEvent sends service failed event to website via CDP
func (c *Client) SendServiceFailedEvent(ctx context.Context) error {
	err := c.sendWatchdogEvent(ctx, CDP_SERVICE_FAILED_EVENT)
	if err != nil {
		return err
	}
	c.logger.Info("Service failed event sent successfully via CDP")
	return nil
}

// Navigate navigates to the specified URL
func (c *Client) Navigate(ctx context.Context, url string) error {
	c.logger.Info("CDP: Navigating to", zap.String("url", url))
	params := map[string]interface{}{
		"url": url,
	}
	_, err := c.Send(METHOD_NAVIGATE, params)
	if err != nil {
		if c.IsReconnectionError(err) {
			if reconnErr := c.Reconnect(ctx); reconnErr != nil {
				return fmt.Errorf("failed to reconnect: %w", reconnErr)
			}
			// Retry navigation after reconnect
			_, err = c.Send(METHOD_NAVIGATE, params)
			if err != nil {
				return fmt.Errorf("failed to navigate to %s: %w", url, err)
			}
		} else {
			return fmt.Errorf("failed to navigate to %s: %w", url, err)
		}
	}
	return nil
}

// Close closes the CDP connection
func (c *Client) Close() {
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
