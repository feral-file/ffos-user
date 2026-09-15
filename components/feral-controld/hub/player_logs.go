package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/logger"
)

const (
	playerOrigin       = "http://127.0.0.1:8080"
	maxPlayerLogCount  = 500
	maxPlayerMessage   = 2048
	maxPlayerFieldSize = 64
)

type playerLogInput struct {
	Timestamp   string            `json:"timestamp"`
	Level       string            `json:"level"`
	Environment string            `json:"environment"`
	Message     string            `json:"message"`
	Context     map[string]string `json:"context"`
}

type playerLogRecord struct {
	Timestamp   string            `json:"timestamp"`
	Level       string            `json:"level"`
	Service     string            `json:"service"`
	Environment string            `json:"environment"`
	DeviceID    string            `json:"device_id"`
	Message     string            `json:"message"`
	Context     map[string]string `json:"context"`
}

// handlePlayerLogs is the narrow browser-to-pipeline bridge for the on-device
// player. The browser cannot safely own device identity or Cloudflare CORS
// configuration, so controld validates the local batch, attaches both trusted
// dimensions, and performs the public network request server-side.
func (h *hub) handlePlayerLogs(w http.ResponseWriter, r *http.Request) {
	if !isStrictLoopbackAddr(r.RemoteAddr) || r.Header.Get("Origin") != playerOrigin {
		http.Error(w, "Forbidden", http.StatusForbidden)
		return
	}
	w.Header().Set("Access-Control-Allow-Origin", playerOrigin)
	w.Header().Set("Vary", "Origin")

	if r.Method == http.MethodOptions {
		w.Header().Set("Access-Control-Allow-Methods", http.MethodPost)
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	var input []playerLogInput
	if err := decoder.Decode(&input); err != nil {
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			http.Error(w, "Request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
		return
	}
	if len(input) == 0 || len(input) > maxPlayerLogCount {
		http.Error(w, "Log batch must contain 1 to 500 records", http.StatusBadRequest)
		return
	}

	var deviceID string
	if h.statusProvider != nil {
		deviceID = strings.TrimSpace(h.statusProvider.Status(r.Context()).DeviceID)
	}
	if deviceID == "" {
		http.Error(w, "Device identity unavailable", http.StatusServiceUnavailable)
		return
	}

	records := make([]playerLogRecord, 0, len(input))
	for _, record := range input {
		if !validPlayerLog(record) {
			http.Error(w, "Invalid log record", http.StatusBadRequest)
			return
		}
		records = append(records, playerLogRecord{
			Timestamp:   record.Timestamp,
			Level:       record.Level,
			Service:     "player",
			Environment: strings.TrimSpace(record.Environment),
			DeviceID:    deviceID,
			Message:     logger.SanitizePublicMessage(record.Message),
			Context:     map[string]string{"session_id": record.Context["session_id"]},
		})
	}

	payload, err := json.Marshal(records)
	if err != nil {
		http.Error(w, "Failed to encode logs", http.StatusInternalServerError)
		return
	}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, h.logEndpoint, bytes.NewReader(payload))
	if err != nil {
		http.Error(w, "Log pipeline unavailable", http.StatusBadGateway)
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.logHTTPClient.Do(req)
	if err != nil {
		http.Error(w, "Log pipeline unavailable", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		w.WriteHeader(resp.StatusCode)
		return
	}
	w.WriteHeader(http.StatusAccepted)
}

// isStrictLoopbackAddr fails closed because this check guards device identity
// attribution. The middleware's similarly named helper intentionally treats
// malformed addresses as loopback for a different, recovery-safety purpose.
func isStrictLoopbackAddr(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validPlayerLog(record playerLogInput) bool {
	if _, err := time.Parse(time.RFC3339Nano, record.Timestamp); err != nil {
		return false
	}
	environment := strings.TrimSpace(record.Environment)
	if !validPlayerLevel(record.Level) || len(environment) == 0 || len(environment) > maxPlayerFieldSize {
		return false
	}
	if strings.TrimSpace(record.Message) == "" || len(record.Message) > maxPlayerMessage {
		return false
	}
	sessionID := strings.TrimSpace(record.Context["session_id"])
	return sessionID != "" && len(sessionID) <= maxPlayerFieldSize && len(record.Context) == 1
}

func validPlayerLevel(level string) bool {
	switch level {
	case "trace", "debug", "info", "warn", "error", "fatal":
		return true
	default:
		return false
	}
}
