package logger

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	MAX_FIELD_LENGTH = 256

	DefaultStreamEndpoint   = "https://677c668ad85c407fb1f0b68f63f41f21.ingest.cloudflare.com"
	DefaultEnvironment      = "production"
	DefaultIdleTimeout      = 5 * time.Second
	DefaultMaxBatchDuration = time.Minute
	maxRecordsPerRequest    = 500
	maxRequestBytes         = 4 << 20
	streamQueueCapacity     = 2048
	shutdownFlushBudget     = 1500 * time.Millisecond
)

var (
	remoteURLPattern       = regexp.MustCompile(`(?i)(?:https?|wss?)://[^\s"'<>]+`)
	credentialStartPattern = regexp.MustCompile(`(?i)["']?\b(?:password|secret|token|access[_-]?token|refresh[_-]?token|api[_-]?key|client[_-]?secret|private[_-]?key|authorization|cookie|dsn)\b["']?\s*[:=]`)
)

// StreamingConfig controls FF1 Cloudflare log delivery. Sampling is decided
// once per inactivity-bounded session: 1 sends every session, while a fraction
// sends that proportion of complete sessions. Zero explicitly disables upload.
type StreamingConfig struct {
	Endpoint                string   `json:"endpoint,omitempty"`
	Environment             string   `json:"environment,omitempty"`
	SampleRate              *float64 `json:"sampleRate,omitempty"`
	IdleTimeoutSeconds      int      `json:"idleTimeoutSeconds,omitempty"`
	MaxBatchDurationSeconds int      `json:"maxBatchDurationSeconds,omitempty"`
}

// StreamEndpoint returns the effective upload endpoint shared by daemon and
// player log delivery.
func StreamEndpoint(config *StreamingConfig) string {
	return config.normalized().Endpoint
}

func (c *StreamingConfig) normalized() StreamingConfig {
	if c == nil {
		one := 1.0
		return StreamingConfig{
			Endpoint:                DefaultStreamEndpoint,
			Environment:             DefaultEnvironment,
			SampleRate:              &one,
			IdleTimeoutSeconds:      int(DefaultIdleTimeout / time.Second),
			MaxBatchDurationSeconds: int(DefaultMaxBatchDuration / time.Second),
		}
	}
	out := *c
	if strings.TrimSpace(out.Endpoint) == "" {
		out.Endpoint = DefaultStreamEndpoint
	}
	if strings.TrimSpace(out.Environment) == "" {
		out.Environment = DefaultEnvironment
	}
	if out.SampleRate == nil {
		one := 1.0
		out.SampleRate = &one
	} else if *out.SampleRate < 0 {
		zero := 0.0
		out.SampleRate = &zero
	} else if *out.SampleRate > 1 {
		one := 1.0
		out.SampleRate = &one
	}
	if out.IdleTimeoutSeconds <= 0 {
		out.IdleTimeoutSeconds = int(DefaultIdleTimeout / time.Second)
	}
	if out.MaxBatchDurationSeconds <= 0 {
		out.MaxBatchDurationSeconds = int(DefaultMaxBatchDuration / time.Second)
	}
	return out
}

// New creates the local stdout/stderr logger. Cloudflare delivery is attached
// after the device configuration has loaded via AddCloudflare.
func New(debug bool) (*zap.Logger, error) {
	var config zap.Config
	if debug {
		config = zap.NewDevelopmentConfig()
		config.EncoderConfig.EncodeLevel = zapcore.CapitalColorLevelEncoder
	} else {
		config = zap.NewProductionConfig()
	}
	config.EncoderConfig.StacktraceKey = ""
	config.EncoderConfig.TimeKey = "timestamp"
	config.EncoderConfig.EncodeTime = zapcore.RFC3339NanoTimeEncoder
	return config.Build()
}

type streamRecord struct {
	Timestamp   string         `json:"timestamp"`
	Level       string         `json:"level"`
	Service     string         `json:"service"`
	Environment string         `json:"environment"`
	DeviceID    string         `json:"device_id"`
	Message     string         `json:"message"`
	Logger      string         `json:"logger,omitempty"`
	Structured  map[string]any `json:"structured,omitempty"`
	Context     map[string]any `json:"context,omitempty"`
	emittedAt   time.Time
}

type streamWriter struct {
	endpoint    string
	environment string
	deviceID    string
	sampleRate  float64
	idleTimeout time.Duration
	maxDuration time.Duration
	closeBudget time.Duration
	httpClient  *http.Client
	random      func() float64
	records     chan streamRecord
	uploads     chan []streamRecord
	closeOnce   sync.Once
	done        chan struct{}
	workerDone  chan struct{}
	uploadCtx   context.Context
	cancel      context.CancelFunc
}

// AddCloudflare tees every enabled zap entry to the FF1 Pipeline without
// putting network I/O on the caller's logging path. Close flushes the final
// partial session during shutdown.
func AddCloudflare(base *zap.Logger, config *StreamingConfig, deviceID string, debug bool) (*zap.Logger, io.Closer, error) {
	if base == nil {
		return nil, nil, fmt.Errorf("base logger is nil")
	}
	cfg := config.normalized()
	if *cfg.SampleRate == 0 {
		return base, nopCloser{}, nil
	}
	if strings.TrimSpace(deviceID) == "" {
		return base, nil, fmt.Errorf("device id is empty")
	}

	uploadCtx, cancel := context.WithCancel(context.Background())
	writer := &streamWriter{
		endpoint:    cfg.Endpoint,
		environment: cfg.Environment,
		deviceID:    deviceID,
		sampleRate:  *cfg.SampleRate,
		idleTimeout: time.Duration(cfg.IdleTimeoutSeconds) * time.Second,
		maxDuration: time.Duration(cfg.MaxBatchDurationSeconds) * time.Second,
		closeBudget: shutdownFlushBudget,
		httpClient:  &http.Client{Timeout: 10 * time.Second},
		random:      rand.Float64,
		records:     make(chan streamRecord, streamQueueCapacity),
		uploads:     make(chan []streamRecord, 32),
		done:        make(chan struct{}),
		workerDone:  make(chan struct{}),
		uploadCtx:   uploadCtx,
		cancel:      cancel,
	}
	go writer.run()

	minimumLevel := zapcore.InfoLevel
	if debug {
		minimumLevel = zapcore.DebugLevel
	}
	core := &cloudflareCore{writer: writer, level: zap.LevelEnablerFunc(func(level zapcore.Level) bool {
		return level >= minimumLevel
	})}
	return base.WithOptions(zap.WrapCore(func(existing zapcore.Core) zapcore.Core {
		return zapcore.NewTee(existing, core)
	})), writer, nil
}

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

type cloudflareCore struct {
	writer *streamWriter
	level  zapcore.LevelEnabler
}

func (c *cloudflareCore) Enabled(level zapcore.Level) bool { return c.level.Enabled(level) }

func (c *cloudflareCore) With(fields []zapcore.Field) zapcore.Core {
	// Arbitrary structured fields stay exclusively in the local core.
	return c
}

func (c *cloudflareCore) Check(entry zapcore.Entry, checked *zapcore.CheckedEntry) *zapcore.CheckedEntry {
	if c.Enabled(entry.Level) {
		return checked.AddCore(entry, c)
	}
	return checked
}

func (c *cloudflareCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	if isRoutineHubPoll(entry.Message, fields) {
		return nil
	}
	record := streamRecord{
		Timestamp:   entry.Time.UTC().Format(time.RFC3339Nano),
		Level:       normalizeLevel(entry.Level),
		Service:     "feral-controld",
		Environment: c.writer.environment,
		DeviceID:    c.writer.deviceID,
		Message:     SanitizePublicMessage(entry.Message),
		Logger:      entry.LoggerName,
		emittedAt:   entry.Time,
	}
	// The public FF1 stream deliberately receives no arbitrary zap fields.
	// Existing fields include signed URLs, SSIDs, MAC addresses, command
	// payloads, and credentials; key-name redaction cannot make that open set
	// safe. Keep them in the local log and stream the stable message only.
	_ = fields
	select {
	case c.writer.records <- record:
	default:
		// A bounded queue protects the daemon during network outages and log
		// storms. Do not emit another log here or create a recursive flood.
	}
	return nil
}

func isRoutineHubPoll(message string, fields []zapcore.Field) bool {
	if message != "Hub request served" {
		return false
	}
	for _, field := range fields {
		if field.Key != "route" || field.Type != zapcore.StringType {
			continue
		}
		switch field.String {
		case "metrics", "status", "status_v2":
			return true
		default:
			return false
		}
	}
	return false
}

func (c *cloudflareCore) Sync() error { return nil }

func normalizeLevel(level zapcore.Level) string {
	switch level {
	case zapcore.DPanicLevel, zapcore.PanicLevel, zapcore.FatalLevel:
		return "fatal"
	case zapcore.WarnLevel:
		return "warn"
	case zapcore.ErrorLevel:
		return "error"
	case zapcore.DebugLevel:
		return "debug"
	default:
		return "info"
	}
}

func (w *streamWriter) Close() error {
	budget := w.closeBudget
	if budget <= 0 {
		budget = shutdownFlushBudget
	}
	w.closeOnce.Do(func() { close(w.done) })
	select {
	case <-w.workerDone:
		w.cancel()
		return nil
	case <-time.After(budget):
		w.cancel()
		select {
		case <-w.workerDone:
		case <-time.After(100 * time.Millisecond):
		}
		return fmt.Errorf("cloudflare log flush exceeded %s shutdown budget", budget)
	}
}

func (w *streamWriter) run() {
	defer close(w.workerDone)
	deliveryDone := make(chan struct{})
	go func() {
		defer close(deliveryDone)
		for records := range w.uploads {
			w.upload(w.uploadCtx, records)
		}
	}()

	var batch []streamRecord
	var sessionID string
	var sampled bool
	var sessionStartedAt, lastRecordAt time.Time
	var idleTimer, maximumTimer *time.Timer
	var idleC, maximumC <-chan time.Time

	stopTimers := func() {
		if idleTimer != nil {
			idleTimer.Stop()
		}
		if maximumTimer != nil {
			maximumTimer.Stop()
		}
		idleC, maximumC = nil, nil
	}
	flushChunk := func(endSession bool) {
		if sampled && len(batch) > 0 {
			ready := append([]streamRecord(nil), batch...)
			select {
			case w.uploads <- ready:
			default:
				// Remote backpressure must not distort session boundaries or
				// block daemon work. The bounded upload queue is the final fuse.
			}
		}
		batch = nil
		if endSession {
			stopTimers()
			sessionID = ""
			sessionStartedAt = time.Time{}
			lastRecordAt = time.Time{}
		}
	}
	add := func(record streamRecord) {
		emittedAt := record.emittedAt
		if emittedAt.IsZero() {
			emittedAt, _ = time.Parse(time.RFC3339Nano, record.Timestamp)
		}
		if emittedAt.IsZero() {
			emittedAt = time.Now()
		}
		if sessionID != "" && (emittedAt.Sub(lastRecordAt) >= w.idleTimeout || emittedAt.Sub(sessionStartedAt) >= w.maxDuration) {
			flushChunk(true)
		}
		if sessionID == "" {
			sessionID = uuid.NewString()
			sampled = w.random() < w.sampleRate
			sessionStartedAt = emittedAt
			maximumTimer = time.NewTimer(w.maxDuration)
			maximumC = maximumTimer.C
		}
		lastRecordAt = emittedAt
		record.Context = map[string]any{"session_id": sessionID}
		if sampled {
			batch = append(batch, record)
			if len(batch) >= maxRecordsPerRequest {
				flushChunk(false)
			}
		}
		if idleTimer == nil {
			idleTimer = time.NewTimer(w.idleTimeout)
		} else {
			if !idleTimer.Stop() {
				select {
				case <-idleTimer.C:
				default:
				}
			}
			idleTimer.Reset(w.idleTimeout)
		}
		idleC = idleTimer.C
	}

	for {
		select {
		case record := <-w.records:
			add(record)
		case <-idleC:
			flushChunk(true)
		case <-maximumC:
			flushChunk(true)
		case <-w.done:
			for {
				select {
				case record := <-w.records:
					add(record)
				default:
					flushChunk(true)
					close(w.uploads)
					<-deliveryDone
					return
				}
			}
		}
	}
}

func (w *streamWriter) upload(ctx context.Context, records []streamRecord) {
	payload, err := json.Marshal(records)
	if err != nil {
		return
	}
	if len(payload) > maxRequestBytes {
		if len(records) == 1 {
			return
		}
		middle := len(records) / 2
		w.upload(ctx, records[:middle])
		w.upload(ctx, records[middle:])
		return
	}
	for attempt := 0; attempt < 3; attempt++ {
		if ctx.Err() != nil {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, w.endpoint, bytes.NewReader(payload))
		if err != nil {
			return
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := w.httpClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 300 {
				return
			}
			if resp.StatusCode < 500 && resp.StatusCode != http.StatusRequestTimeout && resp.StatusCode != http.StatusTooManyRequests {
				return
			}
		}
		if attempt < 2 {
			timer := time.NewTimer(time.Duration(attempt+1) * 250 * time.Millisecond)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
		}
	}
	fmt.Fprintln(os.Stderr, "Cloudflare log upload failed after retries")
}

// SanitizePublicMessage removes credentials and private URL components from a
// free-form message before it crosses the device boundary.
func SanitizePublicMessage(message string) string {
	message = remoteURLPattern.ReplaceAllStringFunc(message, func(raw string) string {
		trailing := ""
		for len(raw) > 0 && strings.ContainsRune(".,);]", rune(raw[len(raw)-1])) {
			trailing = raw[len(raw)-1:] + trailing
			raw = raw[:len(raw)-1]
		}
		parsed, err := url.Parse(raw)
		if err != nil || parsed.Host == "" {
			return "[REDACTED_URL]" + trailing
		}
		parsed.User = nil
		parsed.RawQuery = ""
		parsed.Fragment = ""
		return parsed.String() + trailing
	})
	if location := credentialStartPattern.FindStringIndex(message); location != nil {
		prefix := strings.TrimSpace(message[:location[0]])
		if prefix == "" {
			return "[REDACTED_CREDENTIAL]"
		}
		return prefix + " [REDACTED_CREDENTIAL]"
	}
	return message
}
