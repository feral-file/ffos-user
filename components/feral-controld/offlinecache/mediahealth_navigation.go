package offlinecache

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// NavigationSink receives the kiosk's artwork-frame navigations; the
// production sink is *mediahealth.Service.
type NavigationSink interface {
	ObserveNavigation(nav mediahealth.Navigation)
}

// KioskNavigationObserver watches the kiosk page's network over its OWN
// event-driven CDP session and reports the status of every artwork-frame
// document load to the media-health service (#392, "see the iframe's real
// HTTP status").
//
// Why a separate session: the daemon's synchronous cdp client cannot
// receive events, and the replay session that could is dialed only when
// the offline cache is enabled (opt-in), while media health is on by
// default. So, like the uarewrite interceptor, this owns one more
// websocket to the kiosk, dialed on every primary-CDP reconnect
// (AttachOnReconnect) because a kiosk restart mints a new target.
//
// Strictly passive, by design: Network.enable only — no Fetch
// interception, no Target.setAutoAttach, nothing that can pause a frame
// or delay playback. The kiosk's root target reports the DOCUMENT
// request of every cross-origin iframe the player mounts (the navigation
// is issued on the parent's behalf; verified on the office FF1,
// 2026-11-06), so the root session alone is enough. Only Document-type
// events are looked at; the rest of the page's traffic is dropped on the
// read pump without allocation beyond the decoded envelope.
//
// Handlers run on the session's read pump and must not block (see
// CDPSession.On): each observation is queued to one worker goroutine,
// which keeps per-source ordering and never stalls the pump on a store
// write. The queue is bounded; under a burst the newest observation is
// dropped with a debug log, never the pump.
type KioskNavigationObserver struct {
	endpoint   string
	httpClient wrapper.HTTPClient
	dialer     wrapper.WebSocketDialer
	json       wrapper.JSON
	io         wrapper.IO
	logger     *zap.Logger
	sink       NavigationSink
	dial       dialFunc

	mu      sync.Mutex
	session CDPSession
	// requests maps a Document request's requestId to the URL the frame
	// asked for (the first one: a redirect re-sends the same requestId
	// with the next URL, and the original is the one that names the wall
	// source). Per session; bounded (requestsCap).
	requests map[string]string

	queue     chan mediahealth.Navigation
	done      chan struct{}
	closeOnce sync.Once
	workOnce  sync.Once
}

// dialFunc mirrors DialPageSession's signature (a seam for tests).
type dialFunc func(ctx context.Context, endpoint string, httpClient wrapper.HTTPClient, dialer wrapper.WebSocketDialer, jsonWrapper wrapper.JSON, ioWrapper wrapper.IO, logger *zap.Logger) (CDPSession, error)

const (
	// requestsCap bounds the in-flight Document request map: a page has a
	// handful of frames at a time; the cap only guards against a leak when
	// a response or failure event is never delivered for a request.
	requestsCap = 256
	// navigationQueueCap bounds observations waiting for the worker.
	navigationQueueCap = 64
)

// NewKioskNavigationObserver builds an observer for the kiosk at endpoint
// (its DevTools HTTP endpoint, e.g. "http://127.0.0.1:9222").
func NewKioskNavigationObserver(
	endpoint string,
	httpClient wrapper.HTTPClient,
	dialer wrapper.WebSocketDialer,
	jsonWrapper wrapper.JSON,
	ioWrapper wrapper.IO,
	sink NavigationSink,
	logger *zap.Logger,
) *KioskNavigationObserver {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &KioskNavigationObserver{
		endpoint:   endpoint,
		httpClient: httpClient,
		dialer:     dialer,
		json:       jsonWrapper,
		io:         ioWrapper,
		logger:     logger,
		sink:       sink,
		dial:       DialPageSession,
		queue:      make(chan mediahealth.Navigation, navigationQueueCap),
		done:       make(chan struct{}),
	}
}

// AttachOnReconnect dials a fresh session to the kiosk, installs the
// Document handlers and enables the Network domain. Call it from the
// primary CDP client's onConnect hook; a previous session, if any, is
// closed first. Non-fatal to the daemon: a kiosk that cannot be observed
// costs observations, never playback.
func (o *KioskNavigationObserver) AttachOnReconnect(ctx context.Context) error {
	o.workOnce.Do(func() { go o.work() })
	select {
	case <-o.done:
		return fmt.Errorf("media health navigation observer: closed")
	default:
	}
	session, err := o.dial(ctx, o.endpoint, o.httpClient, o.dialer, o.json, o.io, o.logger)
	if err != nil {
		return fmt.Errorf("media health navigation observer: dial kiosk CDP session: %w", err)
	}
	o.install(session)
	if _, err := session.Send(ctx, "Network.enable", map[string]interface{}{
		// No payload buffering: the observer reads statuses, never bodies.
		"maxTotalBufferSize":    0,
		"maxResourceBufferSize": 0,
	}); err != nil {
		if cerr := session.Close(); cerr != nil {
			o.logger.Debug("media health navigation observer: closing unarmed session", zap.Error(cerr))
		}
		return fmt.Errorf("media health navigation observer: Network.enable on kiosk: %w", err)
	}
	o.mu.Lock()
	select {
	case <-o.done:
		// Closed while dialing (see uarewrite.Interceptor for the same
		// race): do not install a session nobody will close.
		o.mu.Unlock()
		if cerr := session.Close(); cerr != nil {
			o.logger.Debug("media health navigation observer: closing session dialed into shutdown", zap.Error(cerr))
		}
		return fmt.Errorf("media health navigation observer: closed")
	default:
	}
	previous := o.session
	o.session = session
	o.requests = make(map[string]string)
	o.mu.Unlock()
	if previous != nil {
		if err := previous.Close(); err != nil {
			o.logger.Debug("media health navigation observer: closing superseded session", zap.Error(err))
		}
	}
	o.logger.Info("media health: kiosk navigation observer attached")
	return nil
}

// Close releases the session and stops the worker.
func (o *KioskNavigationObserver) Close() error {
	o.closeOnce.Do(func() { close(o.done) })
	o.mu.Lock()
	session := o.session
	o.session = nil
	o.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.Close()
}

// install binds the three handlers to session. Each checks that session
// is still the installed one before acting, so an event from a superseded
// connection cannot be attributed to the live one.
func (o *KioskNavigationObserver) install(session CDPSession) {
	session.On("Network.requestWillBeSent", func(params json.RawMessage) { o.onRequest(session, params) })
	session.On("Network.responseReceived", func(params json.RawMessage) { o.onResponse(session, params) })
	session.On("Network.loadingFailed", func(params json.RawMessage) { o.onFailed(session, params) })
}

// The subsets of the three Network events this observer reads.
type networkRequestEvent struct {
	RequestID string `json:"requestId"`
	Type      string `json:"type"`
	Request   struct {
		URL string `json:"url"`
	} `json:"request"`
}

type networkResponseEvent struct {
	RequestID string `json:"requestId"`
	Type      string `json:"type"`
	Response  struct {
		Status int `json:"status"`
	} `json:"response"`
}

type networkFailedEvent struct {
	RequestID string `json:"requestId"`
	Type      string `json:"type"`
	ErrorText string `json:"errorText"`
	Canceled  bool   `json:"canceled"`
}

func (o *KioskNavigationObserver) onRequest(session CDPSession, params json.RawMessage) {
	var evt networkRequestEvent
	if err := o.json.Unmarshal(params, &evt); err != nil || evt.Type != "Document" || evt.RequestID == "" {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.session != session {
		return
	}
	if _, seen := o.requests[evt.RequestID]; seen {
		// A redirect hop: keep the URL the frame originally asked for.
		return
	}
	if len(o.requests) >= requestsCap {
		// Leaked entries (no response, no failure) would otherwise grow
		// forever; starting over costs at most the attribution of the
		// requests in flight right now.
		o.requests = make(map[string]string)
	}
	o.requests[evt.RequestID] = evt.Request.URL
}

func (o *KioskNavigationObserver) onResponse(session CDPSession, params json.RawMessage) {
	var evt networkResponseEvent
	if err := o.json.Unmarshal(params, &evt); err != nil || evt.Type != "Document" {
		return
	}
	if u, ok := o.take(session, evt.RequestID); ok {
		o.enqueue(mediahealth.Navigation{URL: u, Status: evt.Response.Status})
	}
}

func (o *KioskNavigationObserver) onFailed(session CDPSession, params json.RawMessage) {
	var evt networkFailedEvent
	if err := o.json.Unmarshal(params, &evt); err != nil || evt.Type != "Document" {
		return
	}
	if u, ok := o.take(session, evt.RequestID); ok {
		o.enqueue(mediahealth.Navigation{URL: u, Error: evt.ErrorText, Canceled: evt.Canceled})
	}
}

// take returns and forgets the URL recorded for requestID on session.
func (o *KioskNavigationObserver) take(session CDPSession, requestID string) (string, bool) {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.session != session {
		return "", false
	}
	u, ok := o.requests[requestID]
	if ok {
		delete(o.requests, requestID)
	}
	return u, ok
}

func (o *KioskNavigationObserver) enqueue(nav mediahealth.Navigation) {
	select {
	case o.queue <- nav:
	default:
		o.logger.Debug("media health navigation observer: queue full; observation dropped",
			zap.Int("status", nav.Status), zap.String("error", nav.Error))
	}
}

// work delivers queued observations to the sink, in order, off the pump.
func (o *KioskNavigationObserver) work() {
	for {
		select {
		case <-o.done:
			return
		case nav := <-o.queue:
			o.sink.ObserveNavigation(nav)
		}
	}
}
