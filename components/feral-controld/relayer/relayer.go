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
	"sync/atomic"
	"syscall"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/netmetrics"
	"github.com/feral-file/ffos-user/components/feral-controld/relaywire"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/gorilla/websocket"
	"go.uber.org/zap"
)

var (
	// Errors
	ErrAlreadyConnected = fmt.Errorf("relayer is already connected")
	ErrNotConnected     = fmt.Errorf("relayer is not connected")
	// ErrLegacyRetired is returned by the legacy relayer while it is latched
	// retired: its last dial failed permanently (the host answers 404/410 or
	// no longer resolves) or no endpoint is configured. See legacyRetiredGate.
	ErrLegacyRetired = errors.New("legacy relayer retired or not configured")
)

// A latched-retired legacy relayer is still dialed on an escalating probe:
// first after retiredProbeMin, doubling to retiredProbeMax, reset by a
// successful dial. The short first steps keep a transient permanent-looking
// answer (a Cloudflare 403/404 mid-deploy, a negatively cached NXDOMAIN) from
// costing a legacy-only device more than a few minutes of remote control; the
// cap keeps a truly retired host to one dial an hour. An offline→online edge
// also allows one dial (AllowRetiredProbe).
const (
	retiredProbeMin = time.Minute
	retiredProbeMax = time.Hour
)

const (
	MESSAGE_ID_SYSTEM = "system"
	PING_INTERVAL     = 15 * time.Second
	PONG_WAIT         = 3 * time.Second

	// MAX_MESSAGE_BYTES caps one inbound relayer frame; mirrors
	// hub.MAX_REQUEST_BODY_BYTES (not imported: relayer must not depend on hub).
	MAX_MESSAGE_BYTES = 4 << 20
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
	// RawRequest is the `request` token verbatim, for the same reason
	// commands.Command.RawArguments exists: an inline DP-1 playlist must be
	// signature-verified from the bytes the caller sent, not from a
	// re-marshal whose HTML escaping can inflate it past the verifier's size
	// bound (feral-file/ffos-user#307). Set by UnmarshalJSON only.
	RawRequest json.RawMessage `json:"-"`
	// Ambiguous: see commands.Command.Ambiguous.
	Ambiguous bool `json:"-"`
}

type messageWire struct {
	Command *string        `json:"command,omitempty"`
	Request map[string]any `json:"request,omitempty"`
	TopicID *string        `json:"topicID,omitempty"`
}

// UnmarshalJSON decodes the message and retains the `request` token verbatim.
func (m *Message) UnmarshalJSON(data []byte) error {
	var w messageWire
	if err := json.Unmarshal(data, &w); err != nil {
		return err
	}
	var tokens map[string]json.RawMessage
	if err := json.Unmarshal(data, &tokens); err != nil {
		return err
	}
	m.Command, m.Request, m.TopicID = w.Command, w.Request, w.TopicID
	m.Ambiguous = commands.AmbiguousTopLevel(data)
	m.RawRequest = nil
	if raw, ok := tokens["request"]; ok && len(raw) > 0 && string(raw) != "null" {
		m.RawRequest = raw
	}
	return nil
}

type Response struct {
	Type               string `json:"type"`
	MessageID          string `json:"messageID,omitempty"`
	NotificationType   string `json:"notification_type,omitempty"`
	PersistRecordCount int    `json:"persist_record_count,omitempty"`
	Message            any    `json:"message"`
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
	NOTIFICATION_TYPE_PLAYER_STATUS                 NotificationType = "player_status"
	NOTIFICATION_TYPE_DEVICE_STATUS                 NotificationType = "device_status"
	NOTIFICATION_TYPE_DDC_STATUS                    NotificationType = "ddc_status"
	NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST NotificationType = "mint_pairing_approval_request"
	NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME NotificationType = "mint_pairing_approval_outcome"
	// NOTIFICATION_TYPE_OFFLINE_CACHE_STATUS is emitted per item-state
	// transition by offlinecache.Service (see offlinecache/notifier.go),
	// mirroring the relayer+hub-WS dual-send path status.go uses.
	NOTIFICATION_TYPE_OFFLINE_CACHE_STATUS NotificationType = "offline_cache_status"
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
	endpoint         string
	deviceID         string
	session          *deviceSession
	peerDisconnected func(Peer)
	apiKey           string
	conn             wrapper.WebSocketConn
	done             chan struct{}
	pingDoneChan     chan struct{}
	handlers         []Handler
	dispatchSem      chan struct{}
	controlSem       chan struct{}
	shedSem          chan struct{}

	// v2Attempts counts v2 dials since the last stable session and
	// v2ConnectedAt is when the current one started (see retryV2).
	v2Attempts    int
	v2ConnectedAt time.Time

	// connObserver, when set, is told about connection lifecycle transitions:
	// (true, 0) after a successful dial, (false, 0) on teardown, and
	// (false, code) when the read loop saw a close frame with a code. It feeds
	// the netlog flight recorder; the observer must be internally synchronized
	// and non-blocking (the recorder's enqueue is). Set once at wiring time
	// BEFORE any Connect (same plain-field ordering contract as the hub's
	// contactObserver); nil is a no-op.
	connObserver func(connected bool, closeCode int)

	// Legacy-retirement latch (legacy instance only; guarded by the mutex,
	// except retiredFlag). Every permanent dial error survives a restart
	// (404/410, NXDOMAIN, a bad URL), so redialing on every heartbeat only
	// spends CPU and log volume, and exiting the daemon fixes nothing.
	// retiredAt is the zero time until the first gate check after latching;
	// probeIn is the current probe interval; probeArmed allows one dial on the
	// next check (an offline→online edge). retiredFlag mirrors retired for
	// readers that must not wait on the mutex, which Connect holds across a
	// dial (the /metrics scrape).
	retired     bool
	retiredAt   time.Time
	probeIn     time.Duration
	probeArmed  bool
	retiredFlag atomic.Bool

	// Logger
	logger *zap.Logger
}

// SetConnectionObserver wires the lifecycle observer (see connObserver).
// Call before the first Connect.
func (r *relayer) SetConnectionObserver(fn func(connected bool, closeCode int)) {
	r.connObserver = fn
}

// AllowRetiredProbe lets a latched-retired legacy relayer dial once on its next
// connect attempt. The mediator calls it on an offline→online edge: a captive
// portal or filtering resolver also answers with a permanent DNS error, and
// without this a device behind one would stay off the legacy relayer for up to
// the current probe interval after the network recovers.
func (r *relayer) AllowRetiredProbe() {
	r.Lock()
	defer r.Unlock()
	if r.retired {
		r.probeArmed = true
	}
}

// Retired reports whether the legacy relayer is latched retired (see
// legacyRetiredGate). It feeds the relayer_legacy_retired gauge.
func (r *relayer) Retired() bool {
	return r.retiredFlag.Load()
}

// legacyRetiredGate decides whether the legacy relayer may dial now. It
// returns ErrLegacyRetired when no endpoint is configured, or while latched
// and neither the escalating probe nor an armed edge probe is due.
func (r *relayer) legacyRetiredGate() error {
	r.Lock()
	defer r.Unlock()
	if r.endpoint == "" {
		r.setRetiredLocked(true)
		return ErrLegacyRetired
	}
	if !r.retired {
		return nil
	}
	now := r.clock.Now()
	if r.probeArmed {
		r.probeArmed = false
		r.retiredAt = now
		return nil
	}
	if !r.retiredAt.IsZero() && now.Sub(r.retiredAt) >= r.probeIn {
		r.retiredAt = now
		r.probeIn = min(2*r.probeIn, retiredProbeMax)
		return nil
	}
	if r.retiredAt.IsZero() {
		r.retiredAt = now
	}
	return ErrLegacyRetired
}

// setRetired latches or clears the legacy-retirement latch.
func (r *relayer) setRetired(retired bool) {
	r.Lock()
	defer r.Unlock()
	r.setRetiredLocked(retired)
}

func (r *relayer) setRetiredLocked(retired bool) {
	if retired && !r.retired {
		r.retiredAt = time.Time{}
		r.probeIn = retiredProbeMin
	}
	r.retired = retired
	r.retiredFlag.Store(retired)
	if !retired {
		r.probeArmed = false
	}
}

// observeConn forwards one lifecycle transition to the observer, if wired.
func (r *relayer) observeConn(connected bool, closeCode int) {
	if r.connObserver != nil {
		r.connObserver(connected, closeCode)
	}
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
	if r.deviceID != "" {
		return r.retryV2(ctx)
	}
	if err := r.legacyRetiredGate(); err != nil {
		return err
	}
	var attempts int
	for {
		attempts++
		topicID := state.ClaimSnapshot().TopicID
		r.logger.Info("Connecting to Relayer",
			zap.String("endpoint", r.endpoint),
			zap.Int("attempts", attempts),
			zap.String("topic_fp", helper.TopicFingerprint(topicID)),
			zap.Bool("topic_ready", topicID != ""),
			zap.Bool("currently_connected", r.IsConnected()),
		)

		err := r.Connect(ctx)
		if err == nil {
			r.setRetired(false)
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
			// Latch: the next attempts return ErrLegacyRetired without dialing
			// until a probe is due (see legacyRetiredGate).
			r.setRetired(true)
			r.logger.Warn("Legacy relayer failed permanently; latched retired", zap.Error(err))
			return fmt.Errorf("%w: %w", ErrLegacyRetired, err)
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
	// A config without a legacy endpoint means "no legacy relayer": never dial
	// an empty URL (it would be retried as an unknown error forever).
	if r.deviceID == "" && r.endpoint == "" {
		r.setRetired(true)
		return ErrLegacyRetired
	}
	// Ensure the relayer is not connected
	r.Lock()
	if r.conn != nil {
		r.Unlock()
		r.logger.Info("Relayer connect skipped because conn is already initialized")
		return ErrAlreadyConnected
	}

	// The v2 carrier is addressed by the device ID and never reads the claim:
	// no topic goes into its URL or its logs.
	var connectURL, topicID string
	var headers http.Header
	if r.deviceID != "" {
		var err error
		connectURL, err = relaywire.ConnectionURL(r.endpoint, r.deviceID)
		if err != nil {
			r.Unlock()
			return PermanentError{Err: err}
		}
		headers = http.Header{"Sec-WebSocket-Protocol": []string{relaywire.Subprotocol}}
	} else {
		// Create URL with topicID if available
		connectURL = r.endpoint

		if r.apiKey != "" {
			connectURL += fmt.Sprintf("/api/connection?apiKey=%s", r.apiKey)
		}

		// Single snapshot: topicID and isReady must come from the SAME write, not
		// two separate GetState() reads that could straddle a concurrent
		// clearPersistedClaim()/topic assignment.
		claim := state.ClaimSnapshot()
		topicID = claim.TopicID
		r.logger.Info("Retrieved topic ID from state",
			zap.String("topic_fp", helper.TopicFingerprint(topicID)),
			zap.Bool("isEmpty", topicID == ""),
			zap.Bool("isReady", claim.TopicReady))

		if topicID != "" {
			connectURL += fmt.Sprintf("&topicID=%s", topicID)
			r.logger.Debug("Added topic ID to connection URL", zap.String("connectURL", connectURL))
		} else {
			r.logger.Warn("Topic ID is empty, connecting without topic ID",
				zap.String("connectURL", connectURL),
				zap.String("stateFile", constants.STATE_FILE))
		}
	}
	conn, resp, err := r.dialer.DialContext(ctx, connectURL, headers)
	if err != nil {
		r.Unlock()
		statusCode := 0
		if resp != nil {
			statusCode = resp.StatusCode
		}
		fields := []zap.Field{zap.Error(err), zap.String("endpoint", r.endpoint), zap.Int("status_code", statusCode)}
		if r.deviceID == "" {
			fields = append(fields, zap.String("topic_fp", helper.TopicFingerprint(topicID)))
		}
		r.logger.Warn("Relayer dial failed", fields...)
		return r.categorizeWebsocketError(err, resp)
	}

	// Bound every inbound frame BEFORE it is decoded. The relayer is the
	// second ingress for displayPlaylist (the LAN hub is the first, capped by
	// hub.MAX_REQUEST_BODY_BYTES; URL fetches by dp1.MaxPlaylistBodyBytes),
	// and an inline playlist's cost downstream — JSON decode into a map, JCS
	// canonicalization per signature entry — scales with its size, so an
	// unbounded frame from a remote controller or a compromised relayer is
	// an unbounded memory and CPU spend. Same limit as the hub so both paths
	// admit the same worst-case document. gorilla returns ErrReadLimit on an
	// oversized frame and the read loop reconnects, which is the right
	// outcome for a peer that sends one.
	conn.SetReadLimit(MAX_MESSAGE_BYTES)
	r.conn = conn
	if r.deviceID != "" {
		conn.SetReadLimit(relaywire.MaxFrameBytes)
		r.session = newDeviceSession(conn)
		r.session.onDisconnect = r.peerDisconnected
		r.v2ConnectedAt = time.Now()
	}
	// Stage-0/1 observability (docs/wan-outage-observability.md): connection
	// state is exported event-driven from the three lifecycle sites (here,
	// closeConn, and the read-loop close-frame capture). Published UNDER the
	// relayer lock, matching closeConn's already-locked publication — an
	// unlocked publish here could interleave with a concurrent Close() so
	// that the final exported state says "connected" while r.conn is nil,
	// corrupting the last record sealed into the ring at shutdown.
	if r.deviceID == "" {
		netmetrics.SetRelayerConnected(true)
	} else {
		netmetrics.SetRelayV2Connected(true)
	}
	r.observeConn(true, 0)
	r.Unlock()
	cfRay := ""
	if resp != nil {
		cfRay = resp.Header.Get("cf-ray")
	}
	dialFields := []zap.Field{zap.String("endpoint", r.endpoint), zap.String("cf_ray", cfRay)}
	if r.deviceID == "" {
		dialFields = append(dialFields, zap.String("topic_fp", helper.TopicFingerprint(topicID)))
	}
	r.logger.Info("Relayer websocket dial succeeded", dialFields...)

	// Set pong handler
	conn.SetPongHandler(func(_ string) error {
		return conn.SetReadDeadline(time.Time{})
	})

	// Initialize the ping shutdown channel under lock and capture it locally.
	// The ping goroutine must not read r.pingDoneChan directly: Close() and
	// reconnect() reassign the field under lock, and an unsynchronized read in
	// the goroutine's select would race with those writes (and could observe a
	// nil channel mid-reassignment). The local copy keeps the goroutine reading
	// a stable channel for its lifetime.
	r.Lock()
	// Close() retires r.done permanently for the goroutines of the PREVIOUS
	// session; a Connect after Close (the factory-reset-failed recovery path)
	// must start a FRESH generation, or the new read/ping goroutines would
	// observe the closed channel and exit immediately — a dialed socket with
	// no reader. Each goroutine captures ITS generation locally: a stale
	// goroutine outliving a Close/Connect cycle must keep gating on the
	// channel it was born under, never the field (which is both a data race
	// and the wrong generation).
	select {
	case <-r.done:
		r.done = make(chan struct{})
	default:
	}
	done := r.done
	if r.pingDoneChan == nil {
		r.pingDoneChan = make(chan struct{})
	}
	pingDone := r.pingDoneChan
	session := r.session
	r.Unlock()
	if session != nil {
		go r.v2Keepalive(ctx, done, pingDone, session)
	}

	// Start pinging
	ticker := r.clock.NewTicker(PING_INTERVAL)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-done:
				ticker.Stop()
				return
			case <-pingDone:
				ticker.Stop()
				return
			case <-ticker.C():
				r.ping()
			}
		}
	}()

	// Handle background tasks
	r.background(ctx, done)

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

// background runs the read loop for ONE connection generation: done is the
// generation channel captured at Connect time (see the generation note there).
func (r *relayer) background(ctx context.Context, done chan struct{}) {
	go func() {
		r.logger.Info("Relayer background goroutine started")
		for {
			select {
			case <-ctx.Done():
				r.logger.Info("Closing WebSocket connection due to context cancellation")
				r.Close()
				return
			case <-done:
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
				session := r.session
				r.Unlock()
				kind, msg, err := conn.ReadMessage()
				if err != nil {
					// The close code only exists here, at the read that saw the
					// close frame — record it before any teardown path runs.
					// Correlated server-side with net_internet_reachable it
					// separates "middlebox killed the WSS" from WAN loss
					// (docs/wan-outage-observability.md stage 0).
					var closeErr *websocket.CloseError
					if errors.As(err, &closeErr) {
						if r.deviceID == "" {
							netmetrics.RecordRelayerCloseCode(closeErr.Code)
						}
						r.observeConn(false, closeErr.Code)
					}
					if r.shouldStop(ctx, done) {
						r.logger.Info("Relayer read loop stopped after connection shutdown", zap.Error(err))
						return
					}

					r.logger.Warn("Relayer read failed; reconnecting",
						zap.Error(err),
						zap.Bool("abnormal_closure", isAbnormalClosure(err)),
					)
					err := r.reconnect(ctx)
					if err != nil {
						if r.shouldStop(ctx, done) {
							r.logger.Info("Skipping relayer reconnect failure during shutdown", zap.Error(err))
							return
						}
						if r.deviceID != "" {
							r.logger.Warn("Optional v2 reconnect stopped", zap.Error(err))
							return
						}
						// Never exit the daemon: v2 and the LAN hub must keep
						// running. A permanent failure is latched (ErrLegacyRetired)
						// and anything else is redialed by the mediator's reconcile
						// heartbeat, which owns legacy reconnection.
						r.logger.Warn("Legacy relayer reconnect stopped; the mediator reconcile owns the next attempt", zap.Error(err))
					}
					return
				}

				messageCtx := ctx
				if session != nil && kind == websocket.TextMessage && string(msg) == "pong" {
					// The relay's auto-response to the §10 text ping: liveness only.
					if err := conn.SetReadDeadline(time.Time{}); err != nil {
						r.logger.Debug("Failed to clear read deadline after v2 pong", zap.Error(err))
					}
					continue
				}
				if session != nil {
					var source string
					msg, source, err = session.receive(kind, msg)
					if err != nil || msg == nil {
						continue
					}
					messageCtx = context.WithValue(ctx, relaySourceKey{}, relaySource{session, source})
				}

				logMsg := helper.TruncateBytes(msg, logger.MAX_FIELD_LENGTH)
				if session != nil {
					// v2 bodies are a peer's opaque payloads (sealed
					// envelopes, or whatever a stranger on the public
					// channel sent): never log them.
					logMsg = []byte("[v2 payload redacted]")
				}
				var payload Payload
				if err := json.Unmarshal(msg, &payload); err != nil {
					logInvalid := r.logger.Error
					if session != nil {
						logInvalid = r.logger.Debug // any stranger on the channel can send this
					}
					logInvalid("Invalid JSON received",
						zap.ByteString("message", logMsg),
						zap.Int("message_length", len(msg)),
					)
					continue
				}

				// Application pong is the relayer keepalive response: refresh the
				// deadline, then stop before command handlers see the control frame.
				// Keepalive success is routine — only failures deserve loud logs.
				if payload.Type == "pong" {
					if err := conn.SetReadDeadline(time.Time{}); err != nil {
						r.logger.Error("Failed to clear read deadline after pong", zap.Error(err))
					}
					continue
				}

				// A system frame carries the topic, a bearer credential:
				// never log its raw body.
				if payload.MessageID == MESSAGE_ID_SYSTEM {
					logMsg = []byte("[system frame redacted]")
				}
				if session != nil {
					r.logger.Debug("Received v2 message",
						zap.String("messageID", payload.MessageID),
						zap.String("command", derefString(payload.Message.Command)),
						zap.Int("message_length", len(msg)),
					)
				} else {
					r.logger.Info("Received message",
						zap.ByteString("message", logMsg),
						zap.String("type", payload.Type),
						zap.String("messageID", payload.MessageID),
						zap.String("command", derefString(payload.Message.Command)),
						zap.String("topic_fp", helper.TopicFingerprint(derefString(payload.Message.TopicID))),
						zap.Int("message_length", len(msg)),
					)
				}

				r.dispatchMessage(messageCtx, payload, done)
			}
		}
	}()
}

// dispatchMessage routes a single decoded payload to the registered handlers.
//
// Backpressure is accounted PER PAYLOAD, not per handler: one dispatch slot
// covers the whole fan-out and a saturated command is shed exactly once. This
// matters because more than one handler can be registered at a time — the
// mediator is permanent, but a transient control-plane listener can register
// alongside it. Per-handler accounting would otherwise shed the same
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
func (r *relayer) dispatchMessage(ctx context.Context, payload Payload, done chan struct{}) {
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
			go r.runHandlers(ctx, payload, handlers, r.controlSem, done)
		default:
			// Control lane saturated: drop. System messages carry no caller
			// awaiting a reply by messageID (sendShedResponse skips them), so a
			// legible rejection is neither expected nor possible — dropping is
			// the correct backstop against a spoofed control-plane storm.
			r.logger.Warn("Relayer control-plane lane saturated, dropping system message",
				zap.String("topic_fp", helper.TopicFingerprint(derefString(payload.Message.TopicID))),
			)
		}
		return
	}

	select {
	case r.dispatchSem <- struct{}{}:
		go r.runHandlers(ctx, payload, handlers, r.dispatchSem, done)
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
func (r *relayer) runHandlers(ctx context.Context, payload Payload, handlers []Handler, sem chan struct{}, done chan struct{}) {
	if sem != nil {
		defer func() { <-sem }()
	}
	select {
	case <-ctx.Done():
		return
	case <-done:
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
	if r.deviceID != "" {
		session := r.session
		r.Unlock()
		if session == nil {
			return ErrNotConnected
		}
		body, err := r.json.Marshal(data)
		if err != nil {
			return err
		}
		return session.send(ctx, body)
	}
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
		return
	}

	deadline := r.clock.Now().Add(PONG_WAIT)

	if err := r.conn.SetReadDeadline(deadline); err != nil {
		r.logger.Error("Failed to set read deadline before transport ping", zap.Error(err))
	}

	if err := r.conn.WriteControl(websocket.PingMessage, nil, deadline); err != nil {
		r.logger.Error("Failed to send transport ping", zap.Error(err))
	}

	if r.deviceID != "" {
		return
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
// Close retires the current session. NOTE: a Connect racing this call can
// start a fresh done generation (that is the point — factory-reset recovery),
// so done-close alone does not guarantee no goroutine survives; full shutdown
// relies on ctx cancellation, which every relayer goroutine also observes.
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
func (r *relayer) shouldStop(ctx context.Context, done chan struct{}) bool {
	select {
	case <-ctx.Done():
		return true
	default:
	}

	select {
	case <-done:
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
	if r.session != nil {
		r.session.close()
		r.session = nil
	}
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
	// Every real teardown funnels through here (read-error reconnects and
	// explicit Close alike), so this is the one site that can count
	// disconnects exactly once per connection.
	if r.deviceID == "" {
		netmetrics.SetRelayerConnected(false)
		netmetrics.RecordRelayerDisconnect()
	} else {
		netmetrics.SetRelayV2Connected(false)
	}
	r.observeConn(false, 0)
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
