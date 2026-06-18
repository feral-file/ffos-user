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

	// WRITE_WAIT bounds how long any single websocket write may block. gorilla's
	// WriteMessage/WriteJSON hold no internal timeout, so without a deadline a
	// backpressured peer can park a write — and the connection mutex it holds —
	// indefinitely, wedging ping(), Close(), reconnect, and any handler that is
	// trying to reply (e.g. a storm of gate rejections). A deadline turns that
	// unbounded stall into a bounded error that the read loop converts into a
	// reconnect. The connection is single-writer (serialized by the mutex), so a
	// per-write deadline is safe.
	WRITE_WAIT = 5 * time.Second

	// MAX_INFLIGHT_HANDLERS caps concurrent message-handler goroutines so a
	// relayer command storm cannot spawn unbounded goroutines and exhaust
	// device memory. Per-command rate limiting still happens downstream in the
	// command router; this is a coarse crash-prevention backstop for the
	// dispatch fan-out itself.
	MAX_INFLIGHT_HANDLERS = 256

	// MAX_INFLIGHT_SHED_RESPONSES caps the goroutines emitting "rate_limited"
	// replies for shed commands. The replies are written off the read loop
	// (Send takes the connection lock; its write is deadline-bounded but still
	// holds the lock for up to WRITE_WAIT), so under the very storm we are
	// shedding a slow/backpressured socket must not be able to wedge reads, pong
	// handling, and pings behind one blocking write. When all slots are busy the
	// reply is dropped: a best-effort courtesy reply is worth less than a
	// responsive read loop, and a genuinely dead socket is torn down by the
	// keepalive path instead.
	MAX_INFLIGHT_SHED_RESPONSES = 16

	// MAX_INFLIGHT_CONTROL_HANDLERS caps concurrent handler goroutines for
	// control-plane (messageID == "system") traffic. That discriminator comes
	// straight off the decoded inbound envelope, so it is attacker-controllable:
	// a hostile or malformed relayer-side storm could label every payload as
	// "system" to escape the command dispatch budget (dispatchSem) entirely,
	// spawning unbounded goroutines and hammering the mediator's topic-state
	// save path. Control-plane traffic keeps its own lane so command pressure
	// never sheds it, but that lane MUST still be bounded. The bound is small
	// because legitimate control-plane messages (topic-state updates) are rare;
	// excess is dropped rather than queued, since system messages carry no
	// caller awaiting a reply by messageID.
	MAX_INFLIGHT_CONTROL_HANDLERS = 8
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
	Type      string  `json:"type,omitempty"`
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
	dispatchSem  chan struct{}
	controlSem   chan struct{}
	shedSem      chan struct{}

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
		endpoint:    endpoint,
		apiKey:      apiKey,
		dialer:      dialer,
		randomizer:  randomizer,
		clock:       clock,
		done:        make(chan struct{}),
		os:          os,
		json:        json,
		logger:      logger,
		handlers:    []Handler{},
		dispatchSem: make(chan struct{}, MAX_INFLIGHT_HANDLERS),
		controlSem:  make(chan struct{}, MAX_INFLIGHT_CONTROL_HANDLERS),
		shedSem:     make(chan struct{}, MAX_INFLIGHT_SHED_RESPONSES),
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

	// Initialize the ping shutdown channel under lock and capture it locally.
	// The ping goroutine must not read r.pingDoneChan directly: Close() and
	// reconnect() reassign the field under lock, and an unsynchronized read in
	// the goroutine's select would race with those writes (and could observe a
	// nil channel mid-reassignment). The local copy keeps the goroutine reading
	// a stable channel for its lifetime.
	r.Lock()
	if r.pingDoneChan == nil {
		r.pingDoneChan = make(chan struct{})
	}
	pingDone := r.pingDoneChan
	r.Unlock()

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
			case <-pingDone:
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
					if r.shouldStop(ctx) {
						r.logger.Info("Relayer read loop stopped after connection shutdown", zap.Error(err))
						return
					}

					r.logger.Warn("Relayer read failed; reconnecting",
						zap.Error(err),
						zap.Bool("abnormal_closure", isAbnormalClosure(err)),
					)
					err := r.reconnect(ctx)
					if err != nil {
						if r.shouldStop(ctx) {
							r.logger.Info("Skipping relayer reconnect failure during shutdown", zap.Error(err))
							return
						}
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
					zap.String("type", payload.Type),
					zap.String("messageID", payload.MessageID),
					zap.String("command", derefString(payload.Message.Command)),
					zap.String("topicID", derefString(payload.Message.TopicID)),
					zap.Int("message_length", len(msg)),
				)

				// Application pong is the relayer keepalive response: refresh the
				// deadline, then stop before command handlers see the control frame.
				if payload.Type == "pong" {
					r.logger.Info("Received application pong from relayer")
					if err := conn.SetReadDeadline(time.Time{}); err != nil {
						r.logger.Error("Failed to clear read deadline after pong", zap.Error(err))
					}
					continue
				}

				r.dispatchMessage(ctx, payload)
			}
		}
	}()
}

// dispatchMessage routes a single decoded payload to the registered handlers.
//
// Backpressure is accounted PER PAYLOAD, not per handler: one dispatch slot
// covers the whole fan-out and a saturated command is shed exactly once. This
// matters because more than one handler can be registered at a time — the
// mediator is permanent, but dbus.GetRelayerTopicID temporarily registers a
// control-plane listener. Per-handler accounting would otherwise shed the same
// command once per listener (duplicate rate_limited replies for one messageID)
// and make effective command capacity depend on how many listeners happen to
// be installed.
//
// Control-plane messages (topic/system state) are rare and must NOT be shed by
// command pressure, so they run in a separate, smaller bounded lane (controlSem)
// instead of the command budget. The separate lane is itself capped because the
// "system" discriminator is attacker-controllable; see MAX_INFLIGHT_CONTROL_HANDLERS.
// Either way the handlers run off the read loop so a slow handler cannot stall reads.
//
// Extracted from the read loop so the saturation/shed path is unit-testable
// without the full connection machinery.
func (r *relayer) dispatchMessage(ctx context.Context, payload Payload) {
	// Snapshot handlers under the lock: OnRelayerMessage/RemoveRelayerMessage
	// mutate the slice concurrently (e.g. the temporary D-Bus listener), so the
	// read loop must not iterate it directly.
	r.Lock()
	handlers := append([]Handler(nil), r.handlers...)
	r.Unlock()
	if len(handlers) == 0 {
		return
	}

	// Control-plane traffic runs in its own bounded lane so command pressure
	// never sheds it — but the lane is still bounded, because messageID is
	// attacker-controllable (it is read straight off the decoded envelope). A
	// storm of payloads labeled "system" must not escape every budget and spawn
	// unbounded goroutines / repeatedly hit the mediator's topic-state save path.
	if payload.MessageID == MESSAGE_ID_SYSTEM {
		select {
		case r.controlSem <- struct{}{}:
			go r.runHandlers(ctx, payload, handlers, r.controlSem)
		default:
			// Control lane saturated: drop. System messages carry no caller
			// awaiting a reply by messageID (sendShedResponse skips them), so a
			// legible rejection is neither expected nor possible — dropping is
			// the correct backstop against a spoofed control-plane storm.
			r.logger.Warn("Relayer control-plane lane saturated, dropping system message",
				zap.String("topicID", derefString(payload.Message.TopicID)),
			)
		}
		return
	}

	select {
	case r.dispatchSem <- struct{}{}:
		go r.runHandlers(ctx, payload, handlers, r.dispatchSem)
	default:
		// Saturated: shed this command once, but reply legibly so the caller
		// sees a rate-limit rejection instead of a silent timeout
		// (feral-file/ffos-user#208). The reply is emitted off the read loop
		// (shedResponseAsync) so a blocked write cannot wedge it.
		r.logger.Warn("Relayer dispatch saturated, shedding command",
			zap.String("messageID", payload.MessageID),
			zap.String("command", derefString(payload.Message.Command)),
		)
		r.shedResponseAsync(ctx, payload)
	}
}

// runHandlers invokes every registered handler for one payload, off the read
// loop. When sem is non-nil it owns a single slot in that semaphore (dispatchSem
// for commands, controlSem for control-plane) for the whole fan-out and releases
// it when all handlers return.
func (r *relayer) runHandlers(ctx context.Context, payload Payload, handlers []Handler, sem chan struct{}) {
	if sem != nil {
		defer func() { <-sem }()
	}
	select {
	case <-ctx.Done():
		return
	case <-r.done:
		return
	default:
	}
	for _, handler := range handlers {
		if err := handler(ctx, payload); err != nil {
			r.logger.Error("Failed to handle message",
				zap.Error(err),
				zap.String("messageID", payload.MessageID),
				zap.String("command", derefString(payload.Message.Command)),
			)
		}
	}
}

// shedResponseAsync emits a shed "rate_limited" reply without blocking the
// caller (the read loop). Send takes the connection lock and its write, though
// now deadline-bounded, can still hold that lock for up to WRITE_WAIT, so
// writing inline under a storm could stall reads, pong handling, and pings
// behind one slow write. Hand the reply to a bounded set of writer goroutines
// instead, and drop it when they are all busy: a dropped best-effort reply is
// preferable to a wedged read loop, and a genuinely dead socket is detected and
// reconnected by the keepalive path.
// Writes stay serialized for gorilla's single-writer requirement because every
// writer goroutine, like ping and Send, takes the connection lock.
func (r *relayer) shedResponseAsync(ctx context.Context, payload Payload) {
	select {
	case r.shedSem <- struct{}{}:
		go func(payload Payload) {
			defer func() { <-r.shedSem }()
			r.sendShedResponse(ctx, payload)
		}(payload)
	default:
		r.logger.Warn("Relayer shed-response writers saturated, dropping rate-limited reply",
			zap.String("messageID", payload.MessageID),
			zap.String("command", derefString(payload.Message.Command)),
		)
	}
}

// sendShedResponse replies to a command that was shed under dispatch
// saturation with the same structured "rate_limited" RPC body the command
// router uses, so callers see a legible rejection instead of a silent timeout.
// Only RPC command messages carry a caller awaiting a response by messageID;
// messages without one (or control-plane traffic) are skipped.
func (r *relayer) sendShedResponse(ctx context.Context, payload Payload) {
	if payload.MessageID == "" || payload.MessageID == MESSAGE_ID_SYSTEM {
		return
	}

	resp := Response{
		Type:      "RPC",
		MessageID: payload.MessageID,
		Message: map[string]any{
			"error":   "rate_limited",
			"command": derefString(payload.Message.Command),
			"message": "device is shedding a command storm; retry shortly",
		},
	}
	if err := r.Send(ctx, resp); err != nil {
		r.logger.Warn("Failed to send shed rate-limited response",
			zap.Error(err),
			zap.String("messageID", payload.MessageID),
		)
	}
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

	// Bound the write so a backpressured peer cannot hold the connection mutex
	// (and therefore block ping/Close/reconnect, or pin a caller's dispatch
	// slot) indefinitely. A failed deadline set still lets the write proceed —
	// it is only a best-effort safeguard, not a hard precondition.
	if err := r.conn.SetWriteDeadline(r.clock.Now().Add(WRITE_WAIT)); err != nil {
		r.logger.Warn("Failed to set write deadline before send", zap.Error(err))
	}

	return r.conn.WriteMessage(websocket.TextMessage, jsonData)
}

// ping sends both transport and application keepalive frames so older and newer
// relayer builds can keep the connection alive during rollout.
func (r *relayer) ping() {
	r.Lock()
	defer r.Unlock()
	if r.conn == nil {
		r.logger.Info("Skipping relayer ping because connection is nil")
		return
	}

	r.logger.Info("Sending relayer ping")
	deadline := r.clock.Now().Add(PONG_WAIT)

	if err := r.conn.SetReadDeadline(deadline); err != nil {
		r.logger.Error("Failed to set read deadline before transport ping", zap.Error(err))
	}

	if err := r.conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
		r.logger.Error("Failed to send transport ping", zap.Error(err))
	}

	// WriteControl takes its own deadline, but WriteJSON relies on the
	// connection write deadline; bound it so a stuck application-ping write
	// cannot hold the mutex (and starve Close/reconnect) indefinitely.
	if err := r.conn.SetWriteDeadline(r.clock.Now().Add(WRITE_WAIT)); err != nil {
		r.logger.Error("Failed to set write deadline before application ping", zap.Error(err))
	}

	if err := r.conn.WriteJSON(map[string]string{"type": "ping"}); err != nil {
		r.logger.Error("Failed to send application ping", zap.Error(err))
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

// shouldStop is checked after blocking socket calls return so shutdown-driven
// close errors do not get mistaken for remote disconnects that need reconnecting.
func (r *relayer) shouldStop(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return true
	default:
	}

	select {
	case <-r.done:
		return true
	default:
		return false
	}
}

func isAbnormalClosure(err error) bool {
	var closeErr *websocket.CloseError
	return errors.As(err, &closeErr) && closeErr.Code == websocket.CloseAbnormalClosure
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
