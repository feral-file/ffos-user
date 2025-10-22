package logger

import (
	"strconv"
	"strings"
	"sync"
	"time"

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

// SentryCore is a custom zapcore.Core that sends logs to Sentry
type SentryCore struct {
	zapcore.Core
	sentryConfig *SentryConfig
}

// NewSentryCore creates a new SentryCore
func NewSentryCore(core zapcore.Core, sentryConfig *SentryConfig) *SentryCore {
	return &SentryCore{
		Core:         core,
		sentryConfig: sentryConfig,
	}
}

// Write intercepts log entries and sends them to Sentry based on level
func (s *SentryCore) Write(entry zapcore.Entry, fields []zapcore.Field) error {
	// First write to the original core
	if err := s.Core.Write(entry, fields); err != nil {
		return err
	}

	// Skip Sentry if not enabled
	if s.sentryConfig == nil || !s.sentryConfig.IsEnabled() {
		return nil
	}

	// Create Sentry event based on log level
	switch entry.Level {
	case zapcore.InfoLevel:
		// Add breadcrumb for info logs
		sentry.AddBreadcrumb(&sentry.Breadcrumb{
			Message:   entry.Message,
			Level:     sentry.LevelInfo,
			Timestamp: entry.Time,
			Data:      s.FieldsToMap(fields),
		})

	case zapcore.WarnLevel:
		// Send warning event
		sentry.WithScope(func(scope *sentry.Scope) {
			scope.SetLevel(sentry.LevelWarning)
			scope.SetContext("fields", s.FieldsToMap(fields))
			scope.SetTag("logger", entry.LoggerName)
			sentry.CaptureMessage(entry.Message)
		})

	case zapcore.ErrorLevel:
		// Send error event
		sentry.WithScope(func(scope *sentry.Scope) {
			scope.SetLevel(sentry.LevelError)
			scope.SetContext("fields", s.FieldsToMap(fields))
			scope.SetTag("logger", entry.LoggerName)

			// Check if there's an error field and capture it as an exception
			errorField := s.FindErrorField(fields)
			if errorField != nil {
				sentry.CaptureException(errorField)
			} else {
				sentry.CaptureMessage(entry.Message)
			}
		})

	case zapcore.FatalLevel, zapcore.PanicLevel:
		// Send crash event for fatal/panic logs
		sentry.WithScope(func(scope *sentry.Scope) {
			scope.SetLevel(sentry.LevelFatal)
			scope.SetContext("fields", s.FieldsToMap(fields))
			scope.SetTag("logger", entry.LoggerName)
			scope.SetTag("crash", "true")

			// Check if there's an error field and capture it as an exception
			errorField := s.FindErrorField(fields)
			if errorField != nil {
				sentry.CaptureException(errorField)
			} else {
				sentry.CaptureMessage(entry.Message)
			}
		})

		// Flush immediately for fatal/panic events
		sentry.Flush(2 * time.Second)
	}

	return nil
}

// fieldsToMap converts zap fields to a map for Sentry context
func (s *SentryCore) FieldsToMap(fields []zapcore.Field) map[string]interface{} {
	result := make(map[string]interface{})
	for _, field := range fields {
		switch field.Type {
		case zapcore.StringType:
			result[field.Key] = field.String
		case zapcore.Int64Type, zapcore.Int32Type, zapcore.Int16Type, zapcore.Int8Type:
			result[field.Key] = field.Integer
		case zapcore.Uint64Type, zapcore.Uint32Type, zapcore.Uint16Type, zapcore.Uint8Type:
			result[field.Key] = field.Integer
		case zapcore.Float64Type, zapcore.Float32Type:
			result[field.Key] = field.Interface
		case zapcore.BoolType:
			result[field.Key] = field.Integer == 1
		case zapcore.DurationType:
			result[field.Key] = time.Duration(field.Integer).String()
		case zapcore.TimeType:
			result[field.Key] = time.Unix(0, field.Integer).Format(time.RFC3339)
		case zapcore.ErrorType:
			if field.Interface != nil {
				result[field.Key] = field.Interface.(error).Error()
			}
		default:
			result[field.Key] = field.Interface
		}
	}
	return result
}

// findErrorField looks for an error field in the zap fields
func (s *SentryCore) FindErrorField(fields []zapcore.Field) error {
	for _, field := range fields {
		if field.Type == zapcore.ErrorType && field.Interface != nil {
			if err, ok := field.Interface.(error); ok {
				return err
			}
		}
	}
	return nil
}

//go:generate mockgen -source=logger.go -destination=../mocks/logger.go -package=mocks -mock_names=LoggerManager=MockLoggerManager
type LoggerManager interface {
	New(debug bool) (*zap.Logger, error)
	NewWithSentry(debug bool, sentryConfig *SentryConfig) (*zap.Logger, error)
	NewDefault() (*zap.Logger, error)
	InitSentry(sentryConfig *SentryConfig) error
	SetGlobalTag(key, value string)
	FlushSentry(timeout time.Duration)
}

type defaultLoggerManager struct {
	loggerLock sync.Mutex
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

// NewWithSentry creates a logger with Sentry integration
func (m *defaultLoggerManager) NewWithSentry(debug bool, sentryConfig *SentryConfig) (*zap.Logger, error) {
	// Create the logger
	core, err := m.New(debug)
	if err != nil {
		return nil, err
	}

	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()

	// Wrap with Sentry core
	sentryCore := NewSentryCore(core.Core(), sentryConfig)

	// Create logger with the Sentry-integrated core
	logger := zap.New(sentryCore)

	return logger, nil
}

func (m *defaultLoggerManager) NewDefault() (*zap.Logger, error) {
	return m.New(true)
}

// InitSentry initializes Sentry with the provided configuration
func (m *defaultLoggerManager) InitSentry(sentryConfig *SentryConfig) error {
	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()

	if !sentryConfig.IsEnabled() {
		return nil // Skip Sentry initialization if DSN is empty or not configured
	}

	err := sentry.Init(sentry.ClientOptions{
		Dsn:              sentryConfig.DSN,
		Debug:            sentryConfig.GetDebug(),
		SampleRate:       sentryConfig.GetSampleRate(),
		Environment:      sentryConfig.Environment,
		Release:          sentryConfig.Release,
		SendDefaultPII:   true,
		AttachStacktrace: true,
	})

	return err
}

// SetGlobalTag sets a tag in the global Sentry scope
// This ensures all Sentry events include the tag for better filtering and debugging
func (m *defaultLoggerManager) SetGlobalTag(key, value string) {
	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()

	if key == "" || value == "" {
		return
	}

	sentry.ConfigureScope(func(scope *sentry.Scope) {
		scope.SetTag(key, value)
	})
}

// FlushSentry flushes any pending Sentry events
func (m *defaultLoggerManager) FlushSentry(timeout time.Duration) {
	m.loggerLock.Lock()
	defer m.loggerLock.Unlock()

	sentry.Flush(timeout)
}

// Global instance for backward compatibility
var globalLoggerManager LoggerManager = NewLoggerManager()

// Backward compatible functions
func New(debug bool) (*zap.Logger, error) {
	return globalLoggerManager.New(debug)
}

// NewWithSentry creates a logger with Sentry integration
func NewWithSentry(debug bool, sentryConfig *SentryConfig) (*zap.Logger, error) {
	return globalLoggerManager.NewWithSentry(debug, sentryConfig)
}

func NewDefault() (*zap.Logger, error) {
	return globalLoggerManager.NewDefault()
}

// InitSentry initializes Sentry with the provided configuration
func InitSentry(sentryConfig *SentryConfig) error {
	return globalLoggerManager.InitSentry(sentryConfig)
}

// SetGlobalTag sets a tag in the global Sentry scope
// This ensures all Sentry events include the tag for better filtering and debugging
func SetGlobalTag(key, value string) {
	globalLoggerManager.SetGlobalTag(key, value)
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

