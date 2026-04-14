package relayer

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"syscall"
	"time"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

var (
	// Errors
	ErrAlreadyConnected = fmt.Errorf("relayer is already connected")
	ErrNotConnected     = fmt.Errorf("relayer is not connected")
)

const (
	MESSAGE_ID_SYSTEM = "system"
	PING_INTERVAL     = 15 * time.Second
	PONG_WAIT         = 3 * time.Second
)

type Message struct {
	Command *string        `json:"command,omitempty"`
	Request map[string]any `json:"request,omitempty"`
	TopicID *string        `json:"topicID,omitempty"`
}

type Response struct {
	Type      string `json:"type"`
	MessageID string `json:"messageID"`
	Message   any    `json:"message"`
}

type Payload struct {
	MessageID string  `json:"messageID"`
	Message   Message `json:"message"`
}

type Handler func(ctx context.Context, payload Payload) error

// Custom websocket error types
type PermanentError struct {
	Err error
}

func (e PermanentError) Error() string {
	return e.Err.Error()
}

type TransientError struct {
	Err error
}

func (e TransientError) Error() string {
	return e.Err.Error()
}

type BusyError struct {
	Err error
}

func (e BusyError) Error() string {
	return e.Err.Error()
}

// NotificationType represents the type of notification
type NotificationType string

const (
	NOTIFICATION_TYPE_PLAYER_STATUS NotificationType = "player_status"
	NOTIFICATION_TYPE_DEVICE_STATUS NotificationType = "device_status"
	NOTIFICATION_TYPE_DDC_STATUS    NotificationType = "ddc_status"
)

//go:generate mockgen -source=relayer.go -destination=../mocks/relayer.go -package=mocks -mock_names=Relayer=MockRelayer
type Relayer interface {
	IsConnected() bool
	Connect(ctx context.Context) error
	RetryableConnect(ctx context.Context) error
	Send(ctx context.Context, data interface{}) error
	OnRelayerMessage(handler Handler)
	RemoveRelayerMessage(handler Handler)
	Close()
}

// relayer handles connection to relay server
type relayer struct {
	sync.Mutex

	// Wrappers to be injected
	dialer     wrapper.WebSocketDialer
	randomizer wrapper.Randomizer
	clock      wrapper.Clock
	os         wrapper.OS
	json       wrapper.JSON

	// Internal state
	endpoint     string
	apiKey       string
	conn         wrapper.WebSocketConn
	done         chan struct{}
	pingDoneChan chan struct{}
	handlers     []Handler

	// Logger
	logger *zap.Logger
}

// New creates a new Relayer client
func New(
	endpoint string,
	apiKey string,
	dialer wrapper.WebSocketDialer,
	randomizer wrapper.Randomizer,
	clock wrapper.Clock,
	os wrapper.OS,
	json wrapper.JSON,
	logger *zap.Logger,
) Relayer {
	return &relayer{
		endpoint:   endpoint,
		apiKey:     apiKey,
		dialer:     dialer,
		randomizer: randomizer,
		clock:      clock,
		done:       make(chan struct{}),
		os:         os,
		json:       json,
		logger:     logger,
		handlers:   []Handler{},
	}
}

func (r *relayer) IsConnected() bool {
	r.Lock()
	defer r.Unlock()
	return r.conn != nil
}

// RetryableConnect attempts to connect to the Relayer server and listens for messages indefinitely
// This function blocks the current thread and should be called in a separate goroutine unless otherwise specified
func (r *relayer) RetryableConnect(ctx context.Context) error {
	var attempts int
	for {
		attempts++
		topicID := state.GetState().Relayer.TopicID
		r.logger.Info("Connecting to Relayer",
			zap.String("endpoint", r.endpoint),
			zap.Int("attempts", attempts),
			zap.String("topicID", topicID),
			zap.Bool("topic_ready", topicID != ""),
			zap.Bool("currently_connected", r.IsConnected()),
		)

		err := r.Connect(ctx)
		if err == nil {
			return nil
		}

		var permanentErr PermanentError
		var transientErr TransientError
		var busyErr BusyError
		switch {
		case errors.Is(err, ErrAlreadyConnected):
			r.logger.Info("Relayer connect skipped because connection already exists")
			return nil
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return err
		case errors.As(err, &permanentErr):
			r.logger.Error("Relayer connection failed permanently", zap.Error(err))
			return err
		case errors.As(err, &transientErr):
			r.logger.Warn("Relayer connection failed transiently, will retry",
				zap.Error(err),
				zap.Int("attempts", attempts),
			)
			// randomize sleep time between 1 and 5 seconds
			sleepTime := r.randomizer.Duration(1*time.Second, 5*time.Second)
			r.logger.Info("Sleeping before relayer retry", zap.Duration("sleep", sleepTime))
			r.clock.Sleep(sleepTime)
			continue
		case errors.As(err, &busyErr):
			r.logger.Warn("Relayer endpoint is busy, will retry",
				zap.Error(err),
				zap.Int("attempts", attempts),
			)
			// randomize sleep time between 10 and 60 seconds
			sleepTime := r.randomizer.Duration(10*time.Second, 60*time.Second)
			r.logger.Info("Sleeping before relayer retry", zap.Duration("sleep", sleepTime))
			r.clock.Sleep(sleepTime)
			continue
		default:
			// For unknown error, we retry several times before giving up and return error
			r.logger.Error("Unknown relayer connection error", zap.Error(err), zap.Int("attempts", attempts))
			if attempts > 10 {
				return err
			}
			// randomize sleep time between 10 and 60 seconds
			sleepTime := r.randomizer.Duration(10*time.Second, 60*time.Second)
			r.logger.Info("Sleeping before relayer retry", zap.Duration("sleep", sleepTime))
			r.clock.Sleep(sleepTime)
			continue
		}
	}
}

// Connect connects to the Relayer server and listens for messages
func (r *relayer) Connect(ctx context.Context) error {
	// Ensure the relayer is not connected
	r.Lock()
	if r.conn != nil {
		r.Unlock()
		r.logger.Info("Relayer connect skipped because conn is already initialized")
		return ErrAlreadyConnected
	}

	// Create URL with topicID if available
	connectURL := r.endpoint

	if r.apiKey != "" {
		connectURL += fmt.Sprintf("/api/connection?apiKey=%s", r.apiKey)
	}

	topicID := state.GetState().Relayer.TopicID
	r.logger.Info("Retrieved topic ID from state",
		zap.String("topicID", topicID),
		zap.Bool("isEmpty", topicID == ""),
		zap.Bool("isReady", state.GetState().Relayer.IsReady()))

	if topicID != "" {
		connectURL += fmt.Sprintf("&topicID=%s", topicID)
		r.logger.Debug("Added topic ID to connection URL", zap.String("connectURL", connectURL))
	} else {
		r.logger.Warn("Topic ID is empty, connecting without topic ID",
			zap.String("connectURL", connectURL),
			zap.String("stateFile", constants.STATE_FILE))
	}

	conn, resp, err := r.dialer.DialContext(ctx, connectURL, nil)
	if err != nil {
		r.Unlock()
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		r.logger.Warn("Relayer dial failed",
			zap.Error(err),
			zap.String("endpoint", r.endpoint),
			zap.String("topicID", topicID),
			zap.Int("status_code", statusCode),
		)
		return r.categorizeWebsocketError(err, resp)
	}

	r.conn = conn
	r.Unlock()
	cfRay := ""
	if resp != nil {
		cfRay = resp.Header.Get("cf-ray")
	}
	r.logger.Info("Relayer websocket dial succeeded",
		zap.String("endpoint", r.endpoint),
		zap.String("topicID", topicID),
		zap.String("cf_ray", cfRay),
	)

	// Set pong handler
	conn.SetPongHandler(func(_ string) error {
		r.logger.Info("Received pong from relayer")
		return conn.SetReadDeadline(time.Time{})
	})

	if r.pingDoneChan == nil {
		r.pingDoneChan = make(chan struct{})
	}

	// Start pinging
	ticker := r.clock.NewTicker(PING_INTERVAL)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-r.done:
				ticker.Stop()
				return
			case <-r.pingDoneChan:
				ticker.Stop()
				return
			case <-ticker.C():
				r.logger.Info("Relayer ping ticker fired")
				r.ping()
			}
		}
	}()

	// Handle background tasks
	r.background(ctx)

	r.logger.Info("Connected to Relayer", zap.String("reqID", cfRay))

	return nil
}

func (r *relayer) reconnect(ctx context.Context) error {
	r.logger.Info("Reconnecting to Relayer")

	// Close the connection
	r.Lock()
	r.closeConn()

	if r.pingDoneChan != nil {
		close(r.pingDoneChan)
		r.pingDoneChan = nil
	}
	r.Unlock()

	// Retry to connect
	return r.RetryableConnect(ctx)
}

func (r *relayer) OnRelayerMessage(f Handler) {
	r.Lock()
	defer r.Unlock()
	r.handlers = append(r.handlers, f)
}

func (r *relayer) RemoveRelayerMessage(f Handler) {
	r.Lock()
	defer r.Unlock()

	for i, handler := range r.handlers {
		if fmt.Sprintf("%p", handler) == fmt.Sprintf("%p", f) {
			r.handlers = append(r.handlers[:i], r.handlers[i+1:]...)
			break
		}
	}
}

func (r *relayer) background(ctx context.Context) {
	go func() {
		r.logger.Info("Relayer background goroutine started")
		for {
			select {
			case <-ctx.Done():
				r.logger.Info("Closing WebSocket connection due to context cancellation")
				r.Close()
				return
			case <-r.done:
				// Exit if closed manually
				r.logger.Info("Context handler exiting due to manual close")
				return
			default:
				r.Lock()
				if r.conn == nil {
					r.Unlock()
					r.logger.Info("Relayer background exiting because connection is nil")
					return
				}

				conn := r.conn
				r.Unlock()
				_, msg, err := conn.ReadMessage()
				if err != nil {
					r.logger.Error("Failed to read message. Will attempt to reconnect shortly", zap.Error(err))
					err := r.reconnect(ctx)
					if err != nil {
						// Stop the program and let the systemd restart it
						r.logger.Error("Failed to reconnect to Relayer, the controld will be restarted by systemd shortly", zap.Error(err))
						r.os.Exit(1)
					}
					return
				}

				logMsg := helper.TruncateBytes(msg, logger.MAX_FIELD_LENGTH)
				var payload Payload
				if err := json.Unmarshal(msg, &payload); err != nil {
					r.logger.Error("Invalid JSON received",
						zap.ByteString("message", logMsg),
						zap.Int("message_length", len(msg)),
					)
					continue
				}

				r.logger.Info("Received message",
					zap.ByteString("message", logMsg),
					zap.String("messageID", payload.MessageID),
					zap.String("command", derefString(payload.Message.Command)),
					zap.String("topicID", derefString(payload.Message.TopicID)),
					zap.Int("message_length", len(msg)),
				)

				// Forward payload to handlers
				for _, handler := range r.handlers {
					p := payload
					h := handler

					// Run the handler in a separate goroutine to avoid blocking the main thread
					go func(ctx context.Context, payload Payload, handler Handler) {
						select {
						case <-ctx.Done():
							return
						case <-r.done:
							return
						default:
							if err := handler(ctx, payload); err != nil {
								r.logger.Error("Failed to handle message",
									zap.Error(err),
									zap.String("messageID", payload.MessageID),
									zap.String("command", derefString(payload.Message.Command)),
								)
							}
							return
						}
					}(ctx, p, h)
				}
			}
		}
	}()
}

// Send sends a message to the Relayer server
func (r *relayer) Send(ctx context.Context, data interface{}) error {
	r.Lock()
	defer r.Unlock()

	if r.conn == nil {
		r.logger.Warn("Attempted to send message while relayer is disconnected", zap.String("payload_type", fmt.Sprintf("%T", data)))
		return ErrNotConnected
	}

	// Marshal data to JSON
	jsonData, err := r.json.Marshal(data)
	if err != nil {
		r.logger.Error("Failed to marshal relayer payload", zap.String("payload_type", fmt.Sprintf("%T", data)), zap.Error(err))
		return fmt.Errorf("failed to marshal data: %w", err)
	}

	r.logger.Info("Sending message to Relayer",
		zap.ByteString("message", helper.TruncateBytes(jsonData, logger.MAX_FIELD_LENGTH)),
		zap.String("payload_type", fmt.Sprintf("%T", data)),
		zap.Int("message_length", len(jsonData)),
	)

	return r.conn.WriteMessage(websocket.TextMessage, jsonData)
}

// ping sends a ping to keep the connection alive
func (r *relayer) ping() {
	r.Lock()
	defer r.Unlock()
	if r.conn == nil {
		r.logger.Info("Skipping relayer ping because connection is nil")
		return
	}

	r.logger.Info("Sending relayer ping")
	if err := r.conn.WriteMessage(websocket.PingMessage, []byte("ping")); err != nil {
		r.logger.Error("Failed to send ping", zap.Error(err))
		return
	} else {
		err = r.conn.SetReadDeadline(r.clock.Now().Add(PONG_WAIT))
		if err != nil {
			r.logger.Error("Failed to set read deadline", zap.Error(err))
		}
	}
}

// Close closes the Relayer connection
func (r *relayer) Close() {
	r.Lock()
	defer r.Unlock()

	r.logger.Info("Closing Relayer connection")

	select {
	case <-r.done:
		// Already closed
	default:
		close(r.done)
	}

	if r.pingDoneChan != nil {
		select {
		case <-r.pingDoneChan:
			// Already closed
		default:
			close(r.pingDoneChan)
		}
		r.pingDoneChan = nil
	}

	r.closeConn()
}

func (r *relayer) closeConn() {
	if r.conn == nil {
		r.logger.Info("closeConn called with nil connection")
		return
	}

	deadline := r.clock.Now().Add(2 * time.Second)
	err := r.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
		deadline,
	)
	if err != nil {
		r.logger.Warn("Failed to write close message", zap.Error(err))
	}

	err = r.conn.Close()
	if err != nil {
		r.logger.Warn("Failed to close Relayer connection", zap.Error(err))
	}

	r.conn = nil
	r.logger.Info("Relayer connection closed")
}

func (r *relayer) categorizeWebsocketError(err error, resp *http.Response) error {
	// Extract error types for analysis
	var urlErr *url.Error
	var netErr net.Error
	var dnsErr *net.DNSError
	var tlsErr *tls.RecordHeaderError

	errors.As(err, &urlErr)
	errors.As(err, &netErr)
	errors.As(err, &dnsErr)
	errors.As(err, &tlsErr)

	// 1. Busy errors (retryable server issues)
	if errors.Is(err, syscall.ECONNREFUSED) {
		return BusyError{Err: err}
	}

	if errors.Is(err, websocket.ErrBadHandshake) {
		statusCode := resp.StatusCode
		if statusCode >= 500 || statusCode == http.StatusTooManyRequests {
			return BusyError{Err: err}
		}
	}

	// 2. Permanent errors (configuration/unsupported issues)
	if errors.Is(err, websocket.ErrBadHandshake) {
		return PermanentError{Err: err}
	}

	if urlErr != nil {
		urlErrStr := urlErr.Error()
		if strings.Contains(urlErrStr, "unsupported protocol scheme") ||
			strings.Contains(urlErrStr, "bad request uri") ||
			strings.Contains(urlErrStr, "invalid control character in URL") {
			return PermanentError{Err: err}
		}
	}

	if dnsErr != nil && !dnsErr.Temporary() && !dnsErr.Timeout() {
		return PermanentError{Err: err}
	}

	if errors.Is(err, syscall.EPROTONOSUPPORT) ||
		errors.Is(err, syscall.EADDRNOTAVAIL) {
		return PermanentError{Err: err}
	}

	// 3. Transient errors (network issues that might resolve)
	if (netErr != nil && netErr.Timeout()) ||
		(dnsErr != nil && dnsErr.Temporary()) ||
		errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.EHOSTUNREACH) ||
		errors.Is(err, syscall.ENETUNREACH) ||
		(tlsErr != nil) {
		return TransientError{Err: err}
	}

	// 4. Fallback to unknown error
	return err
}

func derefString(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
