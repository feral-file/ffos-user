package logger

import (
	"context"
	"encoding/json"
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
}

func TestStreamWriterGroupsByIdleTimeout(t *testing.T) {
	requests := make(chan []streamRecord, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var records []streamRecord
		require.NoError(t, json.NewDecoder(r.Body).Decode(&records))
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
		var records []streamRecord
		require.NoError(t, json.NewDecoder(r.Body).Decode(&records))
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
		assert.Equal(t, "application/json", r.Header.Get("Content-Type"))
		var records []streamRecord
		require.NoError(t, json.NewDecoder(r.Body).Decode(&records))
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
		var records []streamRecord
		require.NoError(t, json.NewDecoder(r.Body).Decode(&records))
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
	assert.Equal(t, "failed apiKey=[REDACTED]", record.Message)
	assert.Nil(t, record.Structured)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), "field-secret")
	assert.NotContains(t, string(encoded), "home-network")
}

func TestCloudflareCoreExcludesCommandPayloadAndSanitizesURLs(t *testing.T) {
	w := &streamWriter{environment: "production", deviceID: "FF1-ABC", records: make(chan streamRecord, 1)}
	core := &cloudflareCore{writer: w, level: zapcore.InfoLevel}
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

func newTestWriter(endpoint string, idle, maximum time.Duration) *streamWriter {
	uploadCtx, cancel := context.WithCancel(context.Background())
	return &streamWriter{
		endpoint: endpoint, environment: "test", deviceID: "FF1-1",
		sampleRate: 1, idleTimeout: idle, maxDuration: maximum,
		closeBudget: shutdownFlushBudget,
		httpClient:  &http.Client{Timeout: time.Second}, random: func() float64 { return 0 },
		records: make(chan streamRecord, 16), done: make(chan struct{}), workerDone: make(chan struct{}),
		uploads:   make(chan []streamRecord, 16),
		uploadCtx: uploadCtx, cancel: cancel,
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
