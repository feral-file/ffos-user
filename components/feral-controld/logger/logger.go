package logger

import (
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/feral-file/zapsentry"
	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

// SentryConfig contains Sentry-specific configuration
type SentryConfig struct {
	DSN         string `json:"dsn"`
	Debug       string `json:"debug"`       // Will be converted to bool
	SampleRate  string `json:"sample_rate"` // Will be converted to float64
	Environment string `json:"environment"`
	Release     string `json:"release"`
	Repository  string `json:"repository"` // Git repository for commit linking
}

// GetDebug converts the string debug value to bool
func (sc *SentryConfig) GetDebug() bool {
	if sc.Debug == "" {
		return false
	}
	debug, err := strconv.ParseBool(strings.ToLower(sc.Debug))
	if err != nil {
		return false
	}
	return debug
}

// GetSampleRate converts the string sample_rate value to float64
func (sc *SentryConfig) GetSampleRate() float64 {
	if sc.SampleRate == "" {
		return 1.0 // Default sample rate
	}
	rate, err := strconv.ParseFloat(sc.SampleRate, 64)
	if err != nil {
		return 1.0 // Default sample rate
	}
	return rate
}

// IsEnabled checks if Sentry is enabled (DSN is not empty)
func (sc *SentryConfig) IsEnabled() bool {
	return sc != nil && strings.TrimSpace(sc.DSN) != ""
}

//go:generate mockgen -source=logger.go -destination=../mocks/logger.go -package=mocks -mock_names=LoggerManager=MockLoggerManager
type LoggerManager interface {
	New(debug bool) (*zap.Logger, error)
	SetGlobalTopicID(topicID string)
	AddSentry(logger *zap.Logger, config SentryConfig) (*zap.Logger, error)
	FlushSentry(timeout time.Duration)
}

type defaultLoggerManager struct {
	loggerLock   sync.Mutex
	sentryClient *sentry.Client
}

func NewLoggerManager() LoggerManager {
	return &defaultLoggerManager{}
}

func (m *defaultLoggerManager) New(debug bool) (*zap.Logger, error) {
	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()

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

	// Create the logger with the custom core
	logger, err := config.Build()
	if err != nil {
		return nil, err
	}

	return logger, nil
}

// AddSentry integrates Sentry into the provided logger
func (m *defaultLoggerManager) AddSentry(logger *zap.Logger, config SentryConfig) (*zap.Logger, error) {
	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()

	sc, err := sentry.NewClient(sentry.ClientOptions{
		Dsn:              config.DSN,
		Debug:            config.GetDebug(),
		SampleRate:       config.GetSampleRate(),
		Environment:      config.Environment,
		Release:          config.Release,
		SendDefaultPII:   true,
		AttachStacktrace: true,
	})
	if err != nil {
		return nil, err
	}
	m.sentryClient = sc

	cfg := zapsentry.Configuration{
		Level:             zapcore.ErrorLevel, // when to send message to sentry
		EnableBreadcrumbs: false,
		Tags:              map[string]string{},
	}

	core, err := zapsentry.NewCore(cfg, zapsentry.NewSentryClientFromClient(m.sentryClient))
	if err != nil {
		return nil, err
	}

	return zapsentry.AttachCoreToLogger(core, logger), nil
}

// FlushSentry flushes any pending Sentry events
func (m *defaultLoggerManager) FlushSentry(timeout time.Duration) {
	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()
	if m.sentryClient == nil {
		return
	}
	m.sentryClient.Flush(timeout)
}

// SetGlobalTopicID sets the topic ID in the global Sentry scope
// This ensures all Sentry events include the topic ID for better filtering and debugging
func (m *defaultLoggerManager) SetGlobalTopicID(topicID string) {
	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()

	if topicID == "" {
		return
	}
	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag("topic_id", topicID)
		scope.SetContext("relayer", map[string]interface{}{
			"topic_id": topicID,
		})
	})
}

// Global instance for backward compatibility
var globalLoggerManager LoggerManager = NewLoggerManager()

// Backward compatible functions
func New(debug bool) (*zap.Logger, error) {
	return globalLoggerManager.New(debug)
}

// SetGlobalTopicID sets the topic ID in the global Sentry scope
// This ensures all Sentry events include the topic ID for better filtering and debugging
func SetGlobalTopicID(topicID string) {
	globalLoggerManager.SetGlobalTopicID(topicID)
}

// AddSentry integrates Sentry into the provided logger
func AddSentry(logger *zap.Logger, config SentryConfig) (*zap.Logger, error) {
	return globalLoggerManager.AddSentry(logger, config)
}

// FlushSentry flushes any pending Sentry events
func FlushSentry(timeout time.Duration) {
	globalLoggerManager.FlushSentry(timeout)
}

// For testing - inject a mock logger manager
func InjectLoggerManagerForTesting(lm LoggerManager) {
	globalLoggerManager = lm
}

// Reset for testing
func ResetForTesting() {
	globalLoggerManager = NewLoggerManager()
}
