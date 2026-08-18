package hub

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/netmetrics"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

const (
	HUB_ADDRESS         = "0.0.0.0:1111"
	READ_HEADER_TIMEOUT = 10 * time.Second
	READ_TIMEOUT        = 30 * time.Second
	WRITE_TIMEOUT       = 30 * time.Second
	IDLE_TIMEOUT        = 60 * time.Second
)

//go:generate mockgen -source=hub.go -destination=../mocks/hub.go -package=mocks -mock_names=Hub=MockHub
type Hub interface {
	Start()
	Stop() error
}

type hub struct {
	ctx            context.Context
	logger         *zap.Logger
	server         wrapper.HTTPServer
	wsHandler      ws.WS
	cmdHandler     commandrouter.Handler
	statusProvider StatusProvider
	capturer       screenshot.Capturer
	json           wrapper.JSON
	reqSlots       chan struct{}

	// contactObserver, when set, is invoked once per request on the counted
	// control-plane routes (cast, status, status_v2) from a NON-loopback
	// source. It feeds the provisioning escape policy's "a human's app is
	// talking to this device" deferral signal (docs/network-recovery-ux.md
	// §4.1). The exclusions are load-bearing, not hygiene: /metrics is scraped
	// by local feral-vmagent every 60s over loopback and any loopback poller
	// of a counted endpoint would otherwise pin the deferral permanently; the
	// long-lived /api/notification WebSocket's persistence is not fresh
	// evidence of a human; the catch-all is anything's stray traffic. Set at
	// wiring time before Start (same plain-field ordering contract as the
	// executor's probes); invoked on request goroutines, so the observer must
	// be internally synchronized and non-blocking.
	contactObserver func()
}

// SetContactObserver wires the control-plane contact signal (see
// contactObserver). Call before Start.
func (h *hub) SetContactObserver(fn func()) {
	h.contactObserver = fn
}

func New(
	ctx context.Context,
	wsHandler ws.WS,
	cmdHandler commandrouter.Handler,
	statusProvider StatusProvider,
	capturer screenshot.Capturer,
	server wrapper.HTTPServer,
	json wrapper.JSON,
	logger *zap.Logger,
) Hub {
	if server == nil {
		httpServer := &http.Server{
			Addr:              HUB_ADDRESS,
			Handler:           http.NewServeMux(),
			ReadHeaderTimeout: READ_HEADER_TIMEOUT,
			ReadTimeout:       READ_TIMEOUT,
			WriteTimeout:      WRITE_TIMEOUT,
			IdleTimeout:       IDLE_TIMEOUT,
		}
		server = wrapper.NewHTTPServer(httpServer)
	}
	h := &hub{
		ctx:            ctx,
		wsHandler:      wsHandler,
		cmdHandler:     cmdHandler,
		statusProvider: statusProvider,
		capturer:       capturer,
		json:           json,
		server:         server,
		logger:         logger,
		reqSlots:       make(chan struct{}, MAX_INFLIGHT_REQUESTS),
	}
	h.routes()
	return h
}

// routes registers every hub endpoint through the shared middleware. Each route
// MUST be wrapped by withMiddleware — it is the single chokepoint for the
// in-flight storm cap, request logging, and the future LAN authorization check
// (issue #3471). Do not register a bare handler here.
func (h *hub) routes() {
	handler := h.server.Handler()
	mux, ok := handler.(*http.ServeMux)
	if !ok {
		panic("Expected ServeMux handler, got different type")
	}

	// One /metrics route, several producer-owned registries: playback counters
	// (status) and the stage-0 network gauges (netmetrics) each stay with the
	// package that writes them, merged only at the serving edge here. Both are
	// cache-only — a scrape never triggers probe work (the §4.7 rule extended
	// to metrics; see docs/wan-outage-observability.md).
	metrics := promhttp.HandlerFor(prometheus.Gatherers{
		status.PlaybackMetricsGatherer(),
		netmetrics.Gatherer(),
	}, promhttp.HandlerOpts{})

	mux.HandleFunc("/api/cast", h.withMiddleware("cast", h.handleCast))
	mux.HandleFunc("/api/notification", h.withMiddleware("notification", h.handleNotification))
	mux.HandleFunc("/api/status", h.withMiddleware("status", h.handleStatus))
	mux.HandleFunc("/api/v2/status", h.withMiddleware("status_v2", h.handleStatusV2))
	mux.HandleFunc("/api/screenshot", h.withMiddleware("screenshot", h.handleScreenshot))
	mux.HandleFunc("/metrics", h.withMiddleware("metrics", metrics.ServeHTTP))

	// Chokepoint completeness: without this, the ServeMux serves unmatched
	// paths its own bare 404 — bypassing the storm cap, request logging, and
	// the future LAN-auth seam entirely. "/" is ServeMux's catch-all, so
	// registering it through the same middleware guarantees every request
	// that resolves to a handler passes withMiddleware, matched route or not.
	// (ServeMux's own path-cleaning 301s still happen before dispatch — an
	// inherent net/http behavior that consumes no handler resources.)
	mux.HandleFunc("/", h.withMiddleware("unmatched", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
}

// Listener retry backoff bounds. Vars rather than consts so tests can compress
// the schedule.
var (
	listenRetryBase = time.Second
	listenRetryMax  = 30 * time.Second
)

// Start starts the HTTP server. The listener goroutine retries ListenAndServe
// with capped exponential backoff rather than giving up: this hub is the
// BLE-replacement LAN recovery channel, so a transient bind failure at startup
// (e.g. a lingering :1111 holder) must not silently disable it until an
// unrelated daemon restart. Retrying ends when the server reports
// ErrServerClosed (Stop ran) or the hub context is canceled.
func (h *hub) Start() {
	h.logger.Info("Starting HTTP server", zap.String("addr", HUB_ADDRESS))

	// Start server in a goroutine
	go func() {
		backoff := listenRetryBase
		for {
			err := h.server.ListenAndServe()
			if err == nil || errors.Is(err, http.ErrServerClosed) {
				return
			}
			h.logger.Error("HTTP server error; retrying listener",
				zap.Error(err), zap.Duration("backoff", backoff))
			select {
			case <-h.ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff *= 2; backoff > listenRetryMax {
				backoff = listenRetryMax
			}
		}
	}()

	// Start another goroutine to handle context cancellation
	go func() {
		<-h.ctx.Done()
		err := h.Stop()
		if err != nil {
			h.logger.Error("Failed to stop HTTP server", zap.Error(err))
		}
	}()
}

// handleCast handles POST /api/cast endpoint
func (h *hub) handleCast(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Post method is required", http.StatusMethodNotAllowed)
		return
	}

	// Concurrency is bounded upstream by the shared middleware (reqSlots); the
	// per-command token-bucket gate runs below inside cmdHandler.Process.
	var payload commands.Command
	if err := h.json.NewDecoder(r.Body).Decode(&payload); err != nil {
		// The middleware's MaxBytesReader makes an oversized body surface here
		// as *http.MaxBytesError: report it as 413 (the caller sent too much),
		// not 400 (the caller sent garbage).
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			h.logger.Warn("Cast payload exceeds body limit", zap.Int64("limit", maxErr.Limit))
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		h.logger.Error("Failed to decode cast payload", zap.Error(err))
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}

	payloadJSON, _ := payload.JSON()
	h.logger.Info("Received cast request", zap.ByteString("payload", helper.TruncateBytes(payloadJSON, logger.MAX_FIELD_LENGTH)))

	if payload.Type == "" {
		http.Error(w, "Command type is required", http.StatusBadRequest)
		return
	}

	result, err := h.cmdHandler.Process(h.ctx, payload)
	if err != nil {
		if commandrouter.IsRateLimited(err) {
			h.logger.Warn("Cast request rejected by storm protection", zap.Error(err))
			http.Error(w, "Too many commands, slow down", http.StatusTooManyRequests)
			return
		}
		h.logger.Error("Failed to process cast request", zap.Error(err))
		http.Error(w, "Failed to process cast request", http.StatusInternalServerError)
		return
	}
	if result == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	err = h.respondJSON(w, http.StatusOK, result)
	if err != nil {
		h.logger.Warn("Failed to respond with JSON", zap.Error(err))
		return
	}
}

// handleScreenshot captures Chromium's rendered page. It observes the
// renderer, not the compositor, HDMI path, or physical panel. width and height
// are optional bounding-box dimensions; Chromium scales the full viewport
// uniformly so the result is never cropped or stretched.
func (h *hub) handleScreenshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "GET method is required", http.StatusMethodNotAllowed)
		return
	}
	// This endpoint is intended for native LAN clients. Reject browser-originated
	// requests so a remote page cannot read the FF1 display through DNS rebinding
	// or a direct private-network request while Hub authentication is unfinished.
	if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
		http.Error(w, "Browser origins are not allowed", http.StatusForbidden)
		return
	}

	bounds, err := screenshotBounds(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if h.capturer == nil {
		http.Error(w, "Screenshot capture is unavailable", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), screenshot.CaptureTimeout)
	defer cancel()

	image, err := h.capturer.Capture(ctx, bounds)
	if err != nil {
		statusCode := screenshotErrorStatus(err)
		h.logger.Warn("Failed to capture screenshot", zap.Int("status", statusCode), zap.Error(err))
		if statusCode == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "1")
		}
		http.Error(w, http.StatusText(statusCode), statusCode)
		return
	}

	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Length", strconv.Itoa(len(image.Data)))
	w.Header().Set("X-FF1-Capture-Source", "chromium-cdp")
	w.Header().Set("X-FF1-Screenshot-Width", strconv.Itoa(image.Width))
	w.Header().Set("X-FF1-Screenshot-Height", strconv.Itoa(image.Height))
	if image.SHA256 != "" {
		w.Header().Set("X-FF1-Capture-SHA256", image.SHA256)
	}
	if !image.CapturedAt.IsZero() {
		w.Header().Set("X-FF1-Captured-At", image.CapturedAt.Format(time.RFC3339Nano))
	}
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(image.Data); err != nil {
		h.logger.Warn("Failed to write screenshot response", zap.Error(err))
	}
}

func screenshotBounds(r *http.Request) (screenshot.Bounds, error) {
	query := r.URL.Query()
	for key := range query {
		if key != "width" && key != "height" {
			return screenshot.Bounds{}, fmt.Errorf("unsupported query parameter %q", key)
		}
		if len(query[key]) != 1 {
			return screenshot.Bounds{}, fmt.Errorf("query parameter %q must appear once", key)
		}
	}

	width, err := screenshotDimension(query.Get("width"), "width")
	if err != nil {
		return screenshot.Bounds{}, err
	}
	height, err := screenshotDimension(query.Get("height"), "height")
	if err != nil {
		return screenshot.Bounds{}, err
	}
	return screenshot.Bounds{Width: width, Height: height}, nil
}

func screenshotDimension(raw, name string) (int, error) {
	if raw == "" {
		return 0, nil
	}
	dimension, err := strconv.Atoi(raw)
	if err != nil || dimension <= 0 || dimension > screenshot.MaxDimension {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", name, screenshot.MaxDimension)
	}
	return dimension, nil
}

func screenshotErrorStatus(err error) int {
	switch {
	case errors.Is(err, screenshot.ErrBusy):
		return http.StatusTooManyRequests
	case errors.Is(err, screenshot.ErrUnavailable):
		return http.StatusServiceUnavailable
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusGatewayTimeout
	}
	var timeoutErr net.Error
	if errors.As(err, &timeoutErr) && timeoutErr.Timeout() {
		return http.StatusGatewayTimeout
	}
	return http.StatusBadGateway
}

// handleNotification handles GET /api/notification endpoint and upgrades to WebSocket
func (h *hub) handleNotification(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	connID, err := h.wsHandler.NewConnection(w, r)
	if err != nil {
		h.logger.Error("Failed to establish websocket connection", zap.Error(err))
		http.Error(w, "Failed to upgrade to websocket", http.StatusInternalServerError)
		return
	}

	h.logger.Info("WebSocket connection established", zap.String("connID", connID), zap.String("remote_addr", r.RemoteAddr))
}

// respondJSON responds with a JSON body
func (h *hub) respondJSON(w http.ResponseWriter, code int, body any) error {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	return h.json.NewEncoder(w).Encode(body)
}

// Stop gracefully shuts down the server
func (h *hub) Stop() error {
	h.logger.Info("Stopping server")

	// Close all websocket connections
	h.wsHandler.Close()

	// Shutdown HTTP server
	return h.server.Shutdown(context.Background())
}
