package hub

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
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
		forwarded = decodePlayerLogRecords(t, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	h := &hub{
		statusProvider: fixedStatusProvider{info: StatusInfo{DeviceID: " FF1-TEST "}},
		logEndpoint:    upstream.URL,
		logAPIKey:      "test-token",
		logEnvironment: "trusted-test",
		logSampleRate:  1,
		logHTTPClient:  upstream.Client(),
	}
	//nolint:gosec // Intentional fake credentials exercise public-log sanitization.
	body := `[{"timestamp":"2026-09-15T01:02:03.000Z","level":"error","environment":"browser-secret","message":"failed https://user:pass@example.com/art?token=private","context":{"session_id":"Bearer browser-secret"}}]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, playerOrigin, w.Header().Get("Access-Control-Allow-Origin"))
	require.Len(t, forwarded, 1)
	assert.Equal(t, "player", forwarded[0].Service)
	assert.Equal(t, "FF1-TEST", forwarded[0].DeviceID)
	assert.Equal(t, "trusted-test", forwarded[0].Environment)
	assert.Equal(t, "failed https://example.com/art", forwarded[0].Message)
	assert.Equal(t, map[string]string{"session_id": derivePlayerSessionID("FF1-TEST", "Bearer browser-secret")}, forwarded[0].Context)
	assert.NotContains(t, forwarded[0].Context["session_id"], "secret")
}

func TestHandlePlayerLogsRedactsCredentialBearingMessages(t *testing.T) {
	var forwarded []playerLogRecord
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded = decodePlayerLogRecords(t, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	h := &hub{
		statusProvider: fixedStatusProvider{info: StatusInfo{DeviceID: "FF1-TEST"}},
		logEndpoint:    upstream.URL,
		logAPIKey:      "test-token",
		logEnvironment: "trusted-test",
		logSampleRate:  1,
		logHTTPClient:  upstream.Client(),
	}
	//nolint:gosec // Intentional fake credentials exercise the proxy boundary.
	body := `[
		{"timestamp":"2026-09-15T01:02:03Z","level":"error","environment":"production","message":"Authorization: Bearer secret-token","context":{"session_id":"session-1"}},
		{"timestamp":"2026-09-15T01:02:04Z","level":"error","environment":"production","message":"Authorization Bearer whitespace-secret","context":{"session_id":"session-1"}},
		{"timestamp":"2026-09-15T01:02:05Z","level":"error","environment":"production","message":"payload {\"apiKey\":\"secret\"}","context":{"session_id":"session-1"}},
		{"timestamp":"2026-09-15T01:02:06Z","level":"error","environment":"production","message":"connect wss://user:secret@example.com/socket?token=query-secret","context":{"session_id":"session-1"}},
		{"timestamp":"2026-09-15T01:02:07Z","level":"error","environment":"production","message":"Bearer standalone-secret","context":{"session_id":"session-1"}},
		{"timestamp":"2026-09-15T01:02:08Z","level":"error","environment":"production","message":"request Basic embedded-secret","context":{"session_id":"session-1"}}
	]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	req.Header.Set("Access-Control-Request-Method", http.MethodPost)
	req.Header.Set("Access-Control-Request-Headers", "content-type")
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	require.Len(t, forwarded, 6)
	for _, record := range forwarded {
		assert.NotContains(t, record.Message, "secret")
		assert.NotContains(t, record.Message, "user:")
	}
	assert.Equal(t, "[REDACTED_CREDENTIAL]", forwarded[0].Message)
	assert.Equal(t, "[REDACTED_CREDENTIAL]", forwarded[1].Message)
	assert.Equal(t, "payload { [REDACTED_CREDENTIAL]", forwarded[2].Message)
	assert.Equal(t, "connect wss://example.com/socket", forwarded[3].Message)
	assert.Equal(t, "[REDACTED_CREDENTIAL]", forwarded[4].Message)
	assert.Equal(t, "request [REDACTED_CREDENTIAL]", forwarded[5].Message)
}

func TestHandlePlayerLogsSamplesWholeSessionsAcrossBatches(t *testing.T) {
	forwarded := make(chan []playerLogRecord, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded <- decodePlayerLogRecords(t, r)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()

	keepSession, dropSession := "", ""
	for i := 0; keepSession == "" || dropSession == ""; i++ {
		candidate := fmt.Sprintf("session-%d", i)
		derived := derivePlayerSessionID("FF1-TEST", candidate)
		if samplePlayerSession("FF1-TEST", derived, 0.5) {
			keepSession = candidate
		} else {
			dropSession = candidate
		}
	}
	h := &hub{
		statusProvider:    fixedStatusProvider{info: StatusInfo{DeviceID: "FF1-TEST"}},
		logEndpoint:       upstream.URL,
		logAPIKey:         "test-token",
		logEnvironment:    "trusted-test",
		logSampleRate:     0.5,
		logSessionSampler: samplePlayerSession,
		logHTTPClient:     upstream.Client(),
	}

	send := func(sessionID, message string) int {
		body := fmt.Sprintf(`[{"timestamp":"2026-09-15T01:02:03Z","level":"info","environment":"production","message":%q,"context":{"session_id":%q}}]`, message, sessionID)
		req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:12345"
		req.Header.Set("Origin", playerOrigin)
		w := httptest.NewRecorder()
		h.handlePlayerLogs(w, req)
		return w.Code
	}

	assert.Equal(t, http.StatusAccepted, send(dropSession, "drop one"))
	assert.Equal(t, http.StatusAccepted, send(dropSession, "drop two"))
	assert.Empty(t, forwarded, "a dropped session must stay dropped across batches")
	assert.Equal(t, http.StatusAccepted, send(keepSession, "keep one"))
	assert.Equal(t, http.StatusAccepted, send(keepSession, "keep two"))
	assert.Equal(t, "keep one", (<-forwarded)[0].Message)
	assert.Equal(t, "keep two", (<-forwarded)[0].Message)
	assert.True(t, samplePlayerSession("FF1-TEST", derivePlayerSessionID("FF1-TEST", keepSession), 0.5))
	assert.False(t, samplePlayerSession("FF1-TEST", derivePlayerSessionID("FF1-TEST", dropSession), 0.5))
}

func TestHandlePlayerLogsKeepsDebugAndTraceLocal(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	h := &hub{
		statusProvider: fixedStatusProvider{info: StatusInfo{DeviceID: "FF1-TEST"}},
		logEndpoint:    upstream.URL,
		logAPIKey:      "test-token",
		logEnvironment: "trusted-test",
		logSampleRate:  1,
		logHTTPClient:  upstream.Client(),
	}
	body := `[
		{"timestamp":"2026-09-15T01:02:03Z","level":"trace","environment":"browser-secret","message":"trace diagnostic","context":{"session_id":"session-1"}},
		{"timestamp":"2026-09-15T01:02:04Z","level":"debug","environment":"browser-secret","message":"debug diagnostic","context":{"session_id":"session-1"}}
	]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.False(t, called)
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
	assert.Equal(t, "Content-Type", w.Header().Get("Access-Control-Allow-Headers"))

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
		logAPIKey:      "test-token",
		logEnvironment: "trusted-test",
		logSampleRate:  1,
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

func TestHandlePlayerLogsRejectsOversizedBodyBeforeJSONAllocation(t *testing.T) {
	body := `[{"timestamp":"2026-09-15T01:02:03Z","level":"info","environment":"production","message":"` +
		strings.Repeat("x", maxPlayerLogBodyBytes) +
		`","context":{"session_id":"session-1"}}]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	(&hub{}).handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

func TestHandlePlayerLogsAcceptsWithoutForwardingWhenDisabled(t *testing.T) {
	called := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	h := &hub{
		statusProvider:      statusOnlyProvider{info: StatusInfo{DeviceID: "untrusted"}},
		logEndpoint:         upstream.URL,
		logHTTPClient:       upstream.Client(),
		logDeliveryDisabled: true,
	}
	body := `[{"timestamp":"2026-09-15T01:02:03Z","level":"info","environment":"production","message":"hello","context":{"session_id":"session-1"}}]`
	req := httptest.NewRequest(http.MethodPost, "/api/logs", strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:12345"
	req.Header.Set("Origin", playerOrigin)
	w := httptest.NewRecorder()

	h.handlePlayerLogs(w, req)

	assert.Equal(t, http.StatusAccepted, w.Code)
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
		logAPIKey:      "test-token",
		logEnvironment: "trusted-test",
		logSampleRate:  1,
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
		logAPIKey:      "test-token",
		logEnvironment: "trusted-test",
		logSampleRate:  1,
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

func decodePlayerLogRecords(t *testing.T, r *http.Request) []playerLogRecord {
	t.Helper()
	assert.Equal(t, "application/x-ndjson", r.Header.Get("Content-Type"))
	assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
	decoder := json.NewDecoder(r.Body)
	var records []playerLogRecord
	for {
		var record playerLogRecord
		err := decoder.Decode(&record)
		if err == io.EOF {
			return records
		}
		require.NoError(t, err)
		records = append(records, record)
	}
}
