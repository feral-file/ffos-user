package hub

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
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
	json           wrapper.JSON
	reqSlots       chan struct{}
}

func New(
	ctx context.Context,
	wsHandler ws.WS,
	cmdHandler commandrouter.Handler,
	statusProvider StatusProvider,
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

	metrics := promhttp.HandlerFor(status.PlaybackMetricsGatherer(), promhttp.HandlerOpts{})

	mux.HandleFunc("/api/cast", h.withMiddleware("cast", h.handleCast))
	mux.HandleFunc("/api/notification", h.withMiddleware("notification", h.handleNotification))
	mux.HandleFunc("/api/status", h.withMiddleware("status", h.handleStatus))
	mux.HandleFunc("/api/v2/status", h.withMiddleware("status_v2", h.handleStatusV2))
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
