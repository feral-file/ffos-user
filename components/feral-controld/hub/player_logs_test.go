package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type fixedStatusProvider struct{ info StatusInfo }

func (p fixedStatusProvider) Status(_ context.Context) StatusInfo { return p.info }

func (p fixedStatusProvider) FF1DeviceID() string { return p.info.DeviceID }

type statusOnlyProvider struct{ info StatusInfo }

func (p statusOnlyProvider) Status(_ context.Context) StatusInfo { return p.info }

func TestHandlePlayerLogsEnrichesAndForwardsSafeRecords(t *testing.T) {
	var forwarded []playerLogRecord
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, http.MethodPost, r.Method)
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		require.NoError(t, json.NewDecoder(r.Body).Decode(&forwarded))
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	h := &hub{
		statusProvider: fixedStatusProvider{info: StatusInfo{DeviceID: " FF1-TEST "}},
		logEndpoint:    upstream.URL,
		logHTTPClient:  upstream.Client(),
	}
	//nolint:gosec // Intentional fake credentials exercise public-log sanitization.
	body := `[{"timestamp":"2026-09-15T01:02:03.000Z","level":"error","environment":"production","message":"failed https://user:pass@example.com/art?token=private","context":{"session_id":"session-1"}}]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	require.Len(t, forwarded, 1)
	assert.Equal(t, "player", forwarded[0].Service)
	assert.Equal(t, "FF1-TEST", forwarded[0].DeviceID)
	assert.Equal(t, "failed https://example.com/art", forwarded[0].Message)
	assert.Equal(t, map[string]string{"session_id": "session-1"}, forwarded[0].Context)
}

func TestHandlePlayerLogsAllowsOnlyPlayerPreflightOnLoopback(t *testing.T) {
	h := &hub{}
	req := httptest.NewRequest(http.MethodOptions, "/api/logs", nil)
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, playerOrigin, w.Header().Get("Access-Control-Allow-Origin"))
	assert.Equal(t, http.MethodPost, w.Header().Get("Access-Control-Allow-Methods"))

	remote := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(`[]`))
	remote.RemoteAddr = "192.0.2.1:12345"
	remote.Header.Set("Origin", playerOrigin)
	remoteResponse := httptest.NewRecorder()
	h.handlePlayerLogs(remoteResponse, remote)
	assert.Equal(t, http.StatusForbidden, remoteResponse.Code)
}

func TestHandlePlayerLogsRejectsInvalidRecordWithoutCallingUpstream(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	h := &hub{
		statusProvider: fixedStatusProvider{info: StatusInfo{DeviceID: "FF1-TEST"}},
		logEndpoint:    upstream.URL,
		logHTTPClient:  upstream.Client(),
	}
	body := `[{"timestamp":"not-a-time","level":"info","environment":"production","message":"hello","context":{"session_id":"session-1"}}]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.False(t, called)
}

func TestHandlePlayerLogsPropagatesUpstreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer upstream.Close()
	h := &hub{
		statusProvider: fixedStatusProvider{info: StatusInfo{DeviceID: "FF1-TEST"}},
		logEndpoint:    upstream.URL,
		logHTTPClient:  upstream.Client(),
	}
	body := `[{"timestamp":"2026-09-15T01:02:03Z","level":"info","environment":"production","message":"hello","context":{"session_id":"session-1"}}]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

func TestHandlePlayerLogsRejectsStatusControllerIDFallback(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	h := &hub{
		statusProvider: statusOnlyProvider{info: StatusInfo{DeviceID: "phone-1"}},
		logEndpoint:    upstream.URL,
		logHTTPClient:  upstream.Client(),
	}
	body := `[{"timestamp":"2026-09-15T01:02:03Z","level":"info","environment":"production","message":"hello","context":{"session_id":"session-1"}}]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	assert.False(t, called)
}
