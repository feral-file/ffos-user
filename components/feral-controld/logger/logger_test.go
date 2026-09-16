package logger

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zapcore"
)

func floatPtr(v float64) *float64 { return &v }

func TestStreamingConfigNormalized(t *testing.T) {
	t.Parallel()

	defaults := (*StreamingConfig)(nil).normalized()
	require.NotNil(t, defaults.SampleRate)
	assert.Equal(t, 1.0, *defaults.SampleRate)
	assert.Equal(t, DefaultStreamEndpoint, defaults.Endpoint)
	assert.Equal(t, int(DefaultIdleTimeout/time.Second), defaults.IdleTimeoutSeconds)
	assert.Equal(t, int(DefaultMaxBatchDuration/time.Second), defaults.MaxBatchDurationSeconds)

	high := (&StreamingConfig{SampleRate: floatPtr(2)}).normalized()
	assert.Equal(t, 1.0, *high.SampleRate)
	low := (&StreamingConfig{SampleRate: floatPtr(-1)}).normalized()
	assert.Equal(t, 0.0, *low.SampleRate)
	assert.Equal(t, DefaultStreamEndpoint, StreamEndpoint(nil))
	assert.Equal(t, "https://logs.example.test", StreamEndpoint(&StreamingConfig{Endpoint: "https://logs.example.test"}))
	assert.False(t, StreamDeliveryEnabled(nil))
	assert.False(t, StreamDeliveryEnabled(&StreamingConfig{SampleRate: floatPtr(1)}))
	assert.True(t, StreamDeliveryEnabled(&StreamingConfig{APIKey: " test-token ", SampleRate: floatPtr(1)}))
	assert.Equal(t, "test-token", StreamAPIKey(&StreamingConfig{APIKey: " test-token "}))
	assert.False(t, StreamDeliveryEnabled(&StreamingConfig{SampleRate: floatPtr(0)}))
}

func TestStreamWriterGroupsByIdleTimeout(t *testing.T) {
	requests := make(chan []streamRecord, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := decodeStreamRecords(t, r)
		requests <- records
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	w := newTestWriter(server.URL, 20*time.Millisecond, time.Second)
	go w.run()
	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "one"}
	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "two"}

	first := receiveRequest(t, requests)
	require.Len(t, first, 2)
	firstID := first[0].Context["session_id"]
	assert.NotEmpty(t, firstID)
	assert.Equal(t, firstID, first[1].Context["session_id"])

	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "warn", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "three"}
	second := receiveRequest(t, requests)
	require.Len(t, second, 1)
	assert.NotEqual(t, firstID, second[0].Context["session_id"])
	require.NoError(t, w.Close())
}

func TestStreamWriterCutsContinuousSessionAtMaximumDuration(t *testing.T) {
	requests := make(chan []streamRecord, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := decodeStreamRecords(t, r)
		requests <- records
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w := newTestWriter(server.URL, time.Second, 25*time.Millisecond)
	go w.run()
	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "one"}
	first := receiveRequest(t, requests)
	require.Len(t, first, 1)
	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "two"}
	second := receiveRequest(t, requests)
	require.Len(t, second, 1)
	assert.NotEqual(t, first[0].Context["session_id"], second[0].Context["session_id"])
	require.NoError(t, w.Close())
}

func TestStreamWriterSamplesOncePerSession(t *testing.T) {
	requests := make(chan []streamRecord, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "application/x-ndjson", r.Header.Get("Content-Type"))
		assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
		records := decodeStreamRecords(t, r)
		requests <- records
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()

	w := newTestWriter(server.URL, 20*time.Millisecond, time.Second)
	w.sampleRate = 0.5
	decisions := make(chan float64, 2)
	decisions <- 0.9 // Drop the complete first session.
	decisions <- 0.1 // Upload the complete second session.
	w.random = func() float64 { return <-decisions }
	go w.run()

	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "not sampled"}
	time.Sleep(40 * time.Millisecond)
	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "sampled"}

	got := receiveRequest(t, requests)
	require.Len(t, got, 1)
	assert.Equal(t, "sampled", got[0].Message)
	require.NoError(t, w.Close())
}

func TestStreamWriterCancelsSlowUploadAtShutdownBudget(t *testing.T) {
	requestStarted := make(chan struct{})
	releaseRequest := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(requestStarted)
		<-releaseRequest
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	w := newTestWriter(server.URL, 5*time.Millisecond, time.Minute)
	w.closeBudget = 20 * time.Millisecond
	go w.run()
	w.records <- streamRecord{Timestamp: time.Now().Format(time.RFC3339Nano), Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "final"}
	select {
	case <-requestStarted:
	case <-time.After(time.Second):
		t.Fatal("slow upload request did not start")
	}

	err := w.Close()
	require.ErrorContains(t, err, "shutdown budget")
	select {
	case <-w.uploadCtx.Done():
	default:
		t.Fatal("upload context was not canceled")
	}
	close(releaseRequest)
}

func TestStreamWriterUsesEmissionTimeForIdleBoundary(t *testing.T) {
	requests := make(chan []streamRecord, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := decodeStreamRecords(t, r)
		requests <- records
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w := newTestWriter(server.URL, 20*time.Millisecond, time.Second)
	go w.run()
	start := time.Now()
	w.records <- streamRecord{Timestamp: start.Format(time.RFC3339Nano), emittedAt: start, Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "one"}
	later := start.Add(25 * time.Millisecond)
	w.records <- streamRecord{Timestamp: later.Format(time.RFC3339Nano), emittedAt: later, Level: "info", Service: "feral-controld", Environment: "test", DeviceID: "FF1-1", Message: "two"}

	first := receiveRequest(t, requests)
	second := receiveRequest(t, requests)
	require.Len(t, first, 1)
	require.Len(t, second, 1)
	assert.NotEqual(t, first[0].Context["session_id"], second[0].Context["session_id"])
	require.NoError(t, w.Close())
}

func TestStreamWriterDoesNotRewindSessionTimeForLateRecords(t *testing.T) {
	requests := make(chan []streamRecord, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		records := decodeStreamRecords(t, r)
		requests <- records
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	w := newTestWriter(server.URL, 20*time.Millisecond, time.Second)
	go w.run()
	start := time.Now()
	w.records <- streamRecord{Timestamp: start.Format(time.RFC3339Nano), emittedAt: start, Message: "first"}
	w.records <- streamRecord{Timestamp: start.Add(-time.Minute).Format(time.RFC3339Nano), emittedAt: start.Add(-time.Minute), Message: "late older write"}
	w.records <- streamRecord{Timestamp: start.Add(10 * time.Millisecond).Format(time.RFC3339Nano), emittedAt: start.Add(10 * time.Millisecond), Message: "current"}

	got := receiveRequest(t, requests)
	require.Len(t, got, 3)
	assert.Equal(t, got[0].Context["session_id"], got[1].Context["session_id"])
	assert.Equal(t, got[0].Context["session_id"], got[2].Context["session_id"])
	require.NoError(t, w.Close())
}

func TestCloudflareCoreBuildsPublicSafeFF1Record(t *testing.T) {
	w := &streamWriter{environment: "production", deviceID: "FF1-ABC", records: make(chan streamRecord, 1)}
	core := &cloudflareCore{writer: w, level: zapcore.DebugLevel}
	entry := zapcore.Entry{Time: time.Unix(1, 2), Level: zapcore.ErrorLevel, Message: "failed apiKey=message-secret", LoggerName: "test"}
	require.NoError(t, core.Write(entry, []zapcore.Field{
		{Key: "api_key", Type: zapcore.StringType, String: "field-secret"},
		{Key: "ssid", Type: zapcore.StringType, String: "home-network"},
		{Key: "attempt", Type: zapcore.Int64Type, Integer: 3},
	}))
	record := <-w.records
	assert.Equal(t, "feral-controld", record.Service)
	assert.Equal(t, "FF1-ABC", record.DeviceID)
	assert.Equal(t, "error", record.Level)
	assert.Equal(t, "failed [REDACTED_CREDENTIAL]", record.Message)
	assert.Nil(t, record.Structured)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "field-secret")
	assert.NotContains(t, string(encoded), "home-network")
}

func TestCloudflareCoreExcludesCommandPayloadAndSanitizesURLs(t *testing.T) {
	w := &streamWriter{environment: "production", deviceID: "FF1-ABC", records: make(chan streamRecord, 1)}
	core := &cloudflareCore{writer: w, level: zapcore.InfoLevel}
	//nolint:gosec // Intentional fake credentials exercise public-log sanitization.
	entry := zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "fetch https://user:pass@example.com/art?token=message-secret"}
	require.NoError(t, core.Write(entry, []zapcore.Field{
		{Key: "command", Type: zapcore.ByteStringType, Interface: []byte(`{"type":"uploadLogs","arguments":{"apiKey":"field-secret","title":"support"}}`)},
	}))
	record := <-w.records
	assert.Equal(t, "fetch https://example.com/art", record.Message)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "field-secret")
	assert.NotContains(t, string(encoded), "message-secret")
}

func TestCloudflareCoreExcludesRoutineHubPolls(t *testing.T) {
	w := &streamWriter{environment: "production", deviceID: "FF1-ABC", records: make(chan streamRecord, 1)}
	core := &cloudflareCore{writer: w, level: zapcore.InfoLevel}
	entry := zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "Hub request served"}

	for _, route := range []string{"metrics", "status", "status_v2"} {
		require.NoError(t, core.Write(entry, []zapcore.Field{{
			Key: "route", Type: zapcore.StringType, String: route,
		}}))
		assert.Empty(t, w.records, "route %s should stay out of the remote stream", route)
	}

	require.NoError(t, core.Write(entry, []zapcore.Field{{
		Key: "route", Type: zapcore.StringType, String: "cast",
	}}))
	assert.Equal(t, "Hub request served", (<-w.records).Message)
}

func TestCloudflareCoreExcludesRecurringRelayerRetries(t *testing.T) {
	w := &streamWriter{environment: "production", deviceID: "FF1-ABC", records: make(chan streamRecord, 1)}
	core := &cloudflareCore{writer: w, level: zapcore.InfoLevel}

	for _, message := range []string{
		"Connecting to Relayer",
		"Relayer connection failed transiently, will retry",
		"Sleeping before relayer retry",
		"Relayer endpoint is busy, will retry",
		"Unknown relayer connection error",
		"Relayer dial failed",
	} {
		require.NoError(t, core.Write(zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: message}, nil))
		assert.Empty(t, w.records, "%q should stay out of the remote stream", message)
	}

	require.NoError(t, core.Write(zapcore.Entry{Time: time.Now(), Level: zapcore.InfoLevel, Message: "Connected to Relayer"}, nil))
	assert.Equal(t, "Connected to Relayer", (<-w.records).Message)
}

func TestSanitizePublicMessageRedactsCredentialForms(t *testing.T) {
	//nolint:gosec // Intentional fake credentials exercise public-log sanitization.
	tests := map[string]string{
		"authorization header":     `Authorization: Bearer secret-token`,
		"authorization whitespace": `Authorization Bearer top-secret`,
		"quoted JSON key":          `payload {"apiKey":"secret"}`,
		"websocket userinfo":       `connect wss://user:secret@example.com/socket?token=query-secret`,
	}
	for name, input := range tests {
		t.Run(name, func(t *testing.T) {
			got := SanitizePublicMessage(input)
			assert.NotContains(t, got, "secret")
			assert.NotContains(t, got, "user:")
			assert.NotContains(t, got, "token=query")
		})
	}
	assert.Equal(t, "[REDACTED_CREDENTIAL]", SanitizePublicMessage(tests["authorization header"]))
	assert.Equal(t, "[REDACTED_CREDENTIAL]", SanitizePublicMessage(tests["authorization whitespace"]))
	assert.Equal(t, "payload { [REDACTED_CREDENTIAL]", SanitizePublicMessage(tests["quoted JSON key"]))
	assert.Equal(t, "connect wss://example.com/socket", SanitizePublicMessage(tests["websocket userinfo"]))
}

func newTestWriter(endpoint string, idle, maximum time.Duration) *streamWriter {
	uploadCtx, cancel := context.WithCancel(context.Background())
	return &streamWriter{
		endpoint: endpoint, apiKey: "test-token", environment: "test", deviceID: "FF1-1",
		sampleRate: 1, idleTimeout: idle, maxDuration: maximum,
		closeBudget: shutdownFlushBudget,
		httpClient:  &http.Client{Timeout: time.Second}, random: func() float64 { return 0 },
		records: make(chan streamRecord, 16), done: make(chan struct{}), workerDone: make(chan struct{}),
		uploads:   make(chan []streamRecord, 16),
		uploadCtx: uploadCtx, cancel: cancel,
	}
}

func decodeStreamRecords(t *testing.T, r *http.Request) []streamRecord {
	t.Helper()
	assert.Equal(t, "application/x-ndjson", r.Header.Get("Content-Type"))
	assert.Equal(t, "Bearer test-token", r.Header.Get("Authorization"))
	decoder := json.NewDecoder(r.Body)
	var records []streamRecord
	for {
		var record streamRecord
		err := decoder.Decode(&record)
		if err == io.EOF {
			return records
		}
		require.NoError(t, err)
		records = append(records, record)
	}
}

func receiveRequest(t *testing.T, requests <-chan []streamRecord) []streamRecord {
	t.Helper()
	select {
	case records := <-requests:
		return records
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for log upload")
		return nil
	}
}
