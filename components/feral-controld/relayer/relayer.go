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

	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

var (
	// Errors
	ErrAlreadyConnected = fmt.Errorf("relayer is already connected")
	ErrNotConnected     = fmt.Errorf("relayer is not connected")

	// Constants
	ControldCmds = map[RelayerCmd]bool{
		CMD_CONNECT:              true,
		CMD_SHOW_PAIRING_QR_CODE: true,
		CMD_PROFILE:              true,
		CMD_KEYBOARD_EVENT:       true,
		CMD_MOUSE_DRAG_EVENT:     true,
		CMD_MOUSE_TAP_EVENT:      true,
		CMD_SCREEN_ROTATION:      true,
		CMD_SHUTDOWN:             true,
		CMD_REBOOT:               true,
		CMD_DEVICE_STATUS:        true,
		CMD_UPDATE_TO_LATEST:     true,
	}
)

const (
	MESSAGE_ID_SYSTEM = "system"
	PING_INTERVAL     = 15 * time.Second
	PONG_WAIT         = 3 * time.Second
)

type RelayerCmd string

const (
	CMD_CONNECT              RelayerCmd = "connect"
	CMD_SHOW_PAIRING_QR_CODE RelayerCmd = "showPairingQRCode"
	CMD_PROFILE              RelayerCmd = "deviceMetrics"
	CMD_KEYBOARD_EVENT       RelayerCmd = "sendKeyboardEvent"
	CMD_MOUSE_DRAG_EVENT     RelayerCmd = "dragGesture"
	CMD_MOUSE_TAP_EVENT      RelayerCmd = "tapGesture"
	RELAYER_CMD_SYS_METRICS  RelayerCmd = "deviceMetrics"
	CMD_SCREEN_ROTATION      RelayerCmd = "rotate"
	CMD_SHUTDOWN             RelayerCmd = "shutdown"
	CMD_REBOOT               RelayerCmd = "reboot"
	CMD_DEVICE_STATUS        RelayerCmd = "getDeviceStatus"
	CMD_UPDATE_TO_LATEST     RelayerCmd = "updateToLatestVersion"
)

func (c RelayerCmd) ControldCmds() bool {
	return ControldCmds[c]
}

type Payload struct {
	MessageID string `json:"messageID"`
	Message   struct {
		Command *RelayerCmd            `json:"command,omitempty"`
		Args    map[string]interface{} `json:"request,omitempty"`
		TopicID *string                `json:"topicID,omitempty"`
	} `json:"message"`
}

func (p Payload) JSON() ([]byte, error) {
	return json.Marshal(p)
}

func (p Payload) Arguments(key string) (interface{}, error) {
	v, ok := p.Message.Args[key]
	if !ok {
		return nil, fmt.Errorf("key %s not found", key)
	}
	return v, nil
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
)

// notificationPersistConfig maps notification types to their persist record counts
var notificationPersistConfig = map[NotificationType]int{
	NOTIFICATION_TYPE_PLAYER_STATUS: 1,
	NOTIFICATION_TYPE_DEVICE_STATUS: 1,
}

//go:generate mockgen -source=relayer.go -destination=../mocks/relayer.go -package=mocks -mock_names=Relayer=MockRelayer
type Relayer interface {
	IsConnected() bool
	Connect(ctx context.Context) error
	RetryableConnect(ctx context.Context) error
	Send(ctx context.Context, data interface{}) error
	OnRelayerMessage(handler Handler)
	RemoveRelayerMessage(handler Handler)
	Close()
	SendNotification(ctx context.Context, notificationType NotificationType, message interface{}) error
}

// relayer handles connection to relay server
type relayer struct {
	sync.Mutex

	// Wrappers to be injected
	dialer     wrapper.WebSocketDialer
	randomizer wrapper.Randomizer
	clock      wrapper.Clock
	os         wrapper.OS

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
		r.logger.Info("Connecting to Relayer", zap.String("endpoint", r.endpoint), zap.Int("attempts", attempts))

		err := r.Connect(ctx)
		if err == nil {
			return nil
		}

		var permanentErr PermanentError
		var transientErr TransientError
		var busyErr BusyError
		switch {
		case errors.Is(err, ErrAlreadyConnected):
			return nil
		case errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded):
			return err
		case errors.As(err, &permanentErr):
			return err
		case errors.As(err, &transientErr):
			// randomize sleep time between 1 and 5 seconds
			sleepTime := r.randomizer.Duration(1*time.Second, 5*time.Second)
			r.clock.Sleep(sleepTime)
			continue
		case errors.As(err, &busyErr):
			// randomize sleep time between 10 and 60 seconds
			sleepTime := r.randomizer.Duration(10*time.Second, 60*time.Second)
			r.clock.Sleep(sleepTime)
			continue
		default:
			// For unknown error, we retry several times before giving up and return error
			r.logger.Error("Unknown relayer connection error", zap.Error(err))
			if attempts > 10 {
				return err
			}
			// randomize sleep time between 10 and 60 seconds
			sleepTime := r.randomizer.Duration(10*time.Second, 60*time.Second)
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
		return ErrAlreadyConnected
	}

	// Create URL with topicID if available
	connectURL := r.endpoint

	if r.apiKey != "" {
		connectURL += fmt.Sprintf("/api/connection?apiKey=%s", r.apiKey)
	}

	topicID := state.GetState().Relayer.TopicID
	r.logger.Debug("Retrieved topic ID from state",
		zap.String("topicID", topicID),
		zap.Bool("isEmpty", topicID == ""),
		zap.Bool("isReady", state.GetState().Relayer.IsReady()))

	if topicID != "" {
		connectURL += fmt.Sprintf("&topicID=%s", topicID)
		r.logger.Debug("Added topic ID to connection URL", zap.String("connectURL", connectURL))
	} else {
		r.logger.Warn("Topic ID is empty, connecting without topic ID",
			zap.String("connectURL", connectURL),
			zap.String("stateFile", "/home/feralfile/.state/controld.state"))
	}

	conn, resp, err := r.dialer.DialContext(ctx, connectURL, nil)
	if err != nil {
		r.Unlock()
		return r.categorizeWebsocketError(err, resp)
	}

	r.conn = conn
	r.Unlock()

	// Set pong handler
	conn.SetPongHandler(func(_ string) error {
		r.logger.Debug("Received pong")
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
			case <-ticker.C:
				r.ping()
			}
		}
	}()

	// Handle background tasks
	r.background(ctx)

	r.logger.Info("Connected to Relayer", zap.String("reqID", resp.Header.Get("cf-ray")))

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
				r.logger.Debug("Closing WebSocket connection due to context cancellation")
				r.Close()
				return
			case <-r.done:
				// Exit if closed manually
				r.logger.Debug("Context handler exiting due to manual close")
				return
			default:
				r.Lock()
				if r.conn == nil {
					r.Unlock()
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

				r.logger.Info("Received message", zap.ByteString("message", msg))

				// Unmarshal payload
				var payload Payload
				if err := json.Unmarshal(msg, &payload); err != nil {
					r.logger.Error("Invalid JSON received", zap.ByteString("message", msg))
					continue
				}

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
								r.logger.Error("Failed to handle message", zap.Error(err))
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
		return ErrNotConnected
	}

	r.logger.Info("Sending message to Relayer", zap.Any("data", data))

	return r.conn.WriteJSON(data)
}

// ping sends a ping to keep the connection alive
func (r *relayer) ping() {
	r.Lock()
	defer r.Unlock()
	if r.conn == nil {
		return
	}

	r.logger.Debug("Sending ping")
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

func (r *relayer) SendNotification(ctx context.Context, notificationType NotificationType, message interface{}) error {
	r.logger.Debug("Attempting to send notification",
		zap.String("type", string(notificationType)),
		zap.Bool("relayer_connected", r.IsConnected()))

	if !r.IsConnected() {
		r.logger.Warn("Relayer not connected, skipping notification",
			zap.String("type", string(notificationType)))
		return nil
	}

	notification := map[string]interface{}{
		"type":              "notification",
		"notification_type": string(notificationType),
		"message":           message,
	}

	// Get persist record count from the configuration map
	if persistRecordCount, exists := notificationPersistConfig[notificationType]; exists {
		notification["persist_record_count"] = persistRecordCount
		r.logger.Debug("Sending notification",
			zap.String("type", string(notificationType)),
			zap.Int("persist_count", persistRecordCount),
			zap.Any("message", message))
	} else {
		r.logger.Debug("Sending notification without persist config",
			zap.String("type", string(notificationType)),
			zap.Any("message", message))
	}

	return r.conn.WriteJSON(notification)
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
