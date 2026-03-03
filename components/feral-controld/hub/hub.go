package hub

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
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
	ctx        context.Context
	logger     *zap.Logger
	server     wrapper.HTTPServer
	wsHandler  ws.WS
	cmdHandler commandrouter.Handler
	json       wrapper.JSON
	readConfig func() ([]byte, error)
}

func New(
	ctx context.Context,
	wsHandler ws.WS,
	cmdHandler commandrouter.Handler,
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
		ctx:        ctx,
		wsHandler:  wsHandler,
		cmdHandler: cmdHandler,
		json:       json,
		server:     server,
		logger:     logger,
		readConfig: defaultReadFF1Config,
	}
	h.routes()
	return h
}

func (h *hub) routes() {
	handler := h.server.Handler()
	mux, ok := handler.(*http.ServeMux)
	if !ok {
		panic("Expected ServeMux handler, got different type")
	}

	mux.HandleFunc("/api/cast", h.handleCast)
	mux.HandleFunc("/api/version", h.handleVersion)
	mux.HandleFunc("/api/info", h.handleInfo)
	mux.HandleFunc("/api/status", h.handleStatus)
	mux.HandleFunc("/api/notification", h.handleNotification)
}

func defaultReadFF1Config() ([]byte, error) {
	return os.ReadFile(constant.FF1_CONFIG_FILE)
}

// Start starts the HTTP server
func (h *hub) Start() {
	h.logger.Info("Starting HTTP server", zap.String("addr", HUB_ADDRESS))

	// Start server in a goroutine
	go func() {
		if err := h.server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			h.logger.Error("HTTP server error", zap.Error(err))
			// FIXME should restart the server instead of stopping it
			if e := h.Stop(); e != nil {
				h.logger.Error("Failed to stop HTTP server", zap.Error(e))
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

	var payload commands.Command
	if err := h.json.NewDecoder(r.Body).Decode(&payload); err != nil {
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

// handleVersion handles GET /api/version endpoint
func (h *hub) handleVersion(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	payload, err := h.readFF1VersionPayload()
	if err != nil {
		h.logger.Error("Failed to read FF1 version", zap.Error(err))
		http.Error(w, "Failed to read FF1 config", http.StatusInternalServerError)
		return
	}

	err = h.respondJSON(w, http.StatusOK, map[string]any{"version": payload})
	if err != nil {
		h.logger.Warn("Failed to respond with JSON", zap.Error(err))
		return
	}
}

// handleInfo handles GET /api/info endpoint
func (h *hub) handleInfo(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	payload, err := h.readFF1VersionPayload()
	if err != nil {
		h.logger.Error("Failed to read FF1 version", zap.Error(err))
		http.Error(w, "Failed to read FF1 config", http.StatusInternalServerError)
		return
	}

	response := map[string]any{
		"ff1Version": payload,
		"version":    payload,
	}

	err = h.respondJSON(w, http.StatusOK, response)
	if err != nil {
		h.logger.Warn("Failed to respond with JSON", zap.Error(err))
		return
	}
}

// handleStatus handles GET /api/status endpoint
func (h *hub) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	payload, err := h.readFF1VersionPayload()
	if err != nil {
		h.logger.Error("Failed to read FF1 version", zap.Error(err))
		http.Error(w, "Failed to read FF1 config", http.StatusInternalServerError)
		return
	}

	response := map[string]any{
		"device":           map[string]string{"version": payload},
		"installedVersion": payload,
		"ff1Version":       payload,
		"osVersion":        payload,
	}

	err = h.respondJSON(w, http.StatusOK, response)
	if err != nil {
		h.logger.Warn("Failed to respond with JSON", zap.Error(err))
		return
	}
}

func (h *hub) readFF1VersionPayload() (string, error) {
	configJSON, err := h.readConfig()
	if err != nil {
		return "", err
	}

	var payload struct {
		Version string `json:"version"`
	}

	if err := json.Unmarshal(configJSON, &payload); err != nil {
		return "", err
	}

	return payload.Version, nil
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
