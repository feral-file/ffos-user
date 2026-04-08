package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

type castEnvelope struct {
	Command string          `json:"command"`
	Request json.RawMessage `json:"request"`
}

type displayPlaylistRequest struct {
	DP1Call     json.RawMessage `json:"dp1_call,omitempty"`
	Playlist    json.RawMessage `json:"playlist,omitempty"`
	PlaylistURL string          `json:"playlistUrl,omitempty"`
	Intent      json.RawMessage `json:"intent,omitempty"`
}

type cdpClient struct {
	endpoint string
	nextID   int64
}

func newCDPClient(endpoint string) *cdpClient {
	return &cdpClient{endpoint: strings.TrimRight(endpoint, "/")}
}

func (c *cdpClient) evaluate(ctx context.Context, expression string) (map[string]any, error) {
	wsURL, err := c.pageWebSocketURL(ctx)
	if err != nil {
		return nil, err
	}

	conn, _, err := websocket.DefaultDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, fmt.Errorf("cdp dial: %w", err)
	}
	defer conn.Close()

	id := atomic.AddInt64(&c.nextID, 1)
	msg := map[string]any{
		"id":     id,
		"method": "Runtime.evaluate",
		"params": map[string]any{
			"expression": expression,
		},
	}
	if err := conn.WriteJSON(msg); err != nil {
		return nil, fmt.Errorf("cdp write: %w", err)
	}

	for {
		var resp struct {
			ID     int64 `json:"id"`
			Result struct {
				Result struct {
					Type    string `json:"type"`
					Subtype string `json:"subtype,omitempty"`
					Value   any    `json:"value"`
				} `json:"result"`
			} `json:"result"`
		}
		if err := conn.ReadJSON(&resp); err != nil {
			return nil, fmt.Errorf("cdp read: %w", err)
		}
		if resp.ID != id {
			continue
		}
		switch resp.Result.Result.Type {
		case "":
			return nil, nil
		case "string":
			var out map[string]any
			if err := json.Unmarshal([]byte(resp.Result.Result.Value.(string)), &out); err != nil {
				return nil, fmt.Errorf("cdp decode: %w", err)
			}
			return out, nil
		case "object":
			if m, ok := resp.Result.Result.Value.(map[string]any); ok {
				return m, nil
			}
			raw, err := json.Marshal(resp.Result.Result.Value)
			if err != nil {
				return nil, fmt.Errorf("cdp marshal object: %w", err)
			}
			var out map[string]any
			if err := json.Unmarshal(raw, &out); err != nil {
				return nil, fmt.Errorf("cdp decode object: %w", err)
			}
			return out, nil
		default:
			return nil, fmt.Errorf("unexpected cdp result type: %s", resp.Result.Result.Type)
		}
	}
}

func (c *cdpClient) pageWebSocketURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/json", nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch targets: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read targets: %w", err)
	}
	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		return "", fmt.Errorf("decode targets: %w", err)
	}
	for _, t := range targets {
		if t.Type == "page" && t.WebSocketDebuggerURL != "" {
			return t.WebSocketDebuggerURL, nil
		}
	}
	return "", fmt.Errorf("no page websocket target found")
}

func main() {
	addr := envOr("CAST_SHIM_ADDR", ":1111")
	cdpEndpoint := envOr("CAST_SHIM_CDP_ENDPOINT", "http://127.0.0.1:9222")

	mux := http.NewServeMux()
	client := newCDPClient(cdpEndpoint)

	mux.HandleFunc("/api/cast", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
			return
		}

		var env castEnvelope
		if err := json.NewDecoder(r.Body).Decode(&env); err != nil {
			http.Error(w, "Invalid JSON payload", http.StatusBadRequest)
			return
		}

		switch env.Command {
		case "getDeviceStatus":
			writeJSON(w, http.StatusOK, getDeviceStatus())
		case "displayPlaylist":
			result, err := handleDisplayPlaylist(r.Context(), client, env.Request)
			if err != nil {
				log.Printf("displayPlaylist failed: %v", err)
				http.Error(w, "Failed to process cast request", http.StatusInternalServerError)
				return
			}
			if result == nil {
				result = map[string]any{"message": map[string]any{"ok": true}}
			}
			writeJSON(w, http.StatusOK, result)
		default:
			http.Error(w, "Unsupported command", http.StatusNotImplemented)
		}
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("cast shim listening on %s (cdp=%s)", addr, cdpEndpoint)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func handleDisplayPlaylist(ctx context.Context, client *cdpClient, rawRequest json.RawMessage) (map[string]any, error) {
	var req displayPlaylistRequest
	if err := json.Unmarshal(rawRequest, &req); err != nil {
		return nil, fmt.Errorf("decode request: %w", err)
	}

	payloadRequest := map[string]any{}
	if len(bytes.TrimSpace(req.DP1Call)) > 0 && string(bytes.TrimSpace(req.DP1Call)) != "null" {
		var raw any
		if err := json.Unmarshal(req.DP1Call, &raw); err != nil {
			return nil, fmt.Errorf("decode dp1_call: %w", err)
		}
		payloadRequest["dp1_call"] = raw
	}
	if _, exists := payloadRequest["dp1_call"]; !exists && len(bytes.TrimSpace(req.Playlist)) > 0 && string(bytes.TrimSpace(req.Playlist)) != "null" {
		var raw any
		if err := json.Unmarshal(req.Playlist, &raw); err != nil {
			return nil, fmt.Errorf("decode playlist: %w", err)
		}
		payloadRequest["dp1_call"] = raw
	}
	if req.PlaylistURL != "" {
		payloadRequest["playlistUrl"] = req.PlaylistURL
	}
	if len(bytes.TrimSpace(req.Intent)) > 0 && string(bytes.TrimSpace(req.Intent)) != "null" {
		var intent any
		if err := json.Unmarshal(req.Intent, &intent); err != nil {
			return nil, fmt.Errorf("decode intent: %w", err)
		}
		payloadRequest["intent"] = intent
	} else {
		payloadRequest["intent"] = map[string]any{"action": "now_display"}
	}

	payload := map[string]any{
		"messageID": fmt.Sprintf("cast-shim-%d", time.Now().UnixNano()),
		"message": map[string]any{
			"command": "displayPlaylist",
			"request": payloadRequest,
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal payload: %w", err)
	}

	return client.evaluate(ctx, fmt.Sprintf("window.handleCDPRequest(%s)", payloadJSON))
}

func getDeviceStatus() map[string]any {
	return map[string]any{
		"screenRotation":   currentRotation(),
		"connectedWifi":    currentWifi(),
		"installedVersion": currentVersion(),
		"latestVersion":    currentVersion(),
	}
}

func currentRotation() string {
	data, err := os.ReadFile("/home/feralfile/.config/screen-orientation")
	if err != nil {
		return "landscape"
	}
	value := strings.TrimSpace(string(data))
	switch value {
	case "portrait", "landscape", "normal", "90", "180", "270":
		if value == "normal" || value == "180" {
			return "landscape"
		}
		if value == "90" || value == "270" {
			return "portrait"
		}
		return value
	default:
		return "landscape"
	}
}

func currentWifi() string {
	out, err := exec.Command("iwgetid", "-r").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

func currentVersion() string {
	cmds := [][]string{
		{"pacman", "-Q", "feral-controld"},
		{"pacman", "-Q", "feral-setupd"},
	}
	for _, args := range cmds {
		out, err := exec.Command(args[0], args[1:]...).Output()
		if err != nil {
			continue
		}
		parts := strings.Fields(strings.TrimSpace(string(out)))
		if len(parts) >= 2 {
			return parts[1]
		}
	}
	return "1.0.10"
}

func envOr(key, fallback string) string {
	value := strings.TrimSpace(os.Getenv(key))
	if value == "" {
		return fallback
	}
	return value
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
