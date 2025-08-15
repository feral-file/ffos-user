package logger_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Feral-File/ffos-user/components/feral-connectd/logger"
	"github.com/Feral-File/ffos-user/components/feral-connectd/mocks"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"
)

type testSetup struct {
	ctrl              *gomock.Controller
	mockLoggerManager *mocks.MockLoggerManager
	realManager       logger.LoggerManager
	testLogger        *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	testLogger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	mockLoggerManager := mocks.NewMockLoggerManager(ctrl)
	realManager := logger.NewLoggerManager()

	return &testSetup{
		ctrl:              ctrl,
		mockLoggerManager: mockLoggerManager,
		realManager:       realManager,
		testLogger:        testLogger,
	}
}

func (ts *testSetup) teardown() {
	logger.ResetForTesting()
	ts.ctrl.Finish()
}

// Test SentryCore functionality
func TestSentryCore_Write(t *testing.T) {
	// Create an observed core to capture logs
	observedCore, logs := observer.New(zapcore.InfoLevel)

	// Create a mock Sentry config (disabled)
	sentryConfig := &logger.SentryConfig{
		DSN: "", // Empty DSN means disabled
	}

	// Create Sentry core with the observed core
	sentryCore := logger.NewSentryCore(observedCore, sentryConfig)

	// Create logger with the Sentry core
	testLogger := zap.New(sentryCore)

	// Test different log levels
	testLogger.Info("This is an info message", zap.String("key", "value"))
	testLogger.Warn("This is a warning message", zap.Int("number", 42))
	testLogger.Error("This is an error message", zap.Error(errors.New("test error")))

	// Verify logs were written to the observed core
	entries := logs.All()
	assert.Len(t, entries, 3, "Expected 3 log entries")

	// Verify log levels
	expectedLevels := []zapcore.Level{zapcore.InfoLevel, zapcore.WarnLevel, zapcore.ErrorLevel}
	for i, entry := range entries {
		assert.Equal(t, expectedLevels[i], entry.Level, "Log level mismatch at index %d", i)
	}
}

func TestSentryCore_FieldsToMap(t *testing.T) {
	sentryCore := &logger.SentryCore{}

	fields := []zapcore.Field{
		zap.String("string_field", "test"),
		zap.Int("int_field", 123),
		zap.Bool("bool_field", true),
		zap.Duration("duration_field", time.Second),
		zap.Error(errors.New("test error")),
	}

	result := sentryCore.FieldsToMap(fields)

	// Verify all field types are converted correctly
	assert.Equal(t, "test", result["string_field"])
	assert.Equal(t, int64(123), result["int_field"])
	assert.Equal(t, true, result["bool_field"])
	assert.Equal(t, "1s", result["duration_field"])
	assert.Equal(t, "test error", result["error"])
}

func TestSentryCore_FindErrorField(t *testing.T) {
	tests := []struct {
		name        string
		fields      []zapcore.Field
		expectError bool
		errorMsg    string
	}{
		{
			name: "error field found",
			fields: []zapcore.Field{
				zap.String("string_field", "test"),
				zap.Error(errors.New("test error")),
				zap.Int("int_field", 123),
			},
			expectError: true,
			errorMsg:    "test error",
		},
		{
			name: "no error field",
			fields: []zapcore.Field{
				zap.String("string_field", "test"),
				zap.Int("int_field", 123),
			},
			expectError: false,
		},
		{
			name:        "empty fields",
			fields:      []zapcore.Field{},
			expectError: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sentryCore := &logger.SentryCore{}
			foundError := sentryCore.FindErrorField(tt.fields)

			if tt.expectError {
				assert.NotNil(t, foundError, "Expected to find an error field")
				assert.Equal(t, tt.errorMsg, foundError.Error())
			} else {
				assert.Nil(t, foundError, "Expected no error field")
			}
		})
	}
}

// Test LoggerManager interface with real implementation
func TestLoggerManager_RealImplementation(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (*zap.Logger, error)
	}{
		{
			name: "New with debug mode",
			setupFunc: func(ts *testSetup) (*zap.Logger, error) {
				return ts.realManager.New(true)
			},
		},
		{
			name: "New with production mode",
			setupFunc: func(ts *testSetup) (*zap.Logger, error) {
				return ts.realManager.New(false)
			},
		},
		{
			name: "NewDefault",
			setupFunc: func(ts *testSetup) (*zap.Logger, error) {
				return ts.realManager.NewDefault()
			},
		},
		{
			name: "NewWithSentry disabled",
			setupFunc: func(ts *testSetup) (*zap.Logger, error) {
				sentryConfig := &logger.SentryConfig{DSN: ""}
				return ts.realManager.NewWithSentry(true, sentryConfig)
			},
		},
		{
			name: "NewWithSentry with config",
			setupFunc: func(ts *testSetup) (*zap.Logger, error) {
				sentryConfig := &logger.SentryConfig{
					DSN:         "https://test@sentry.io/123",
					Environment: "test",
					Debug:       "false",
					SampleRate:  "0.5",
				}
				return ts.realManager.NewWithSentry(false, sentryConfig)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			result, err := tt.setupFunc(ts)
			assert.NoError(t, err)
			assert.NotNil(t, result)
		})
	}
}

func TestLoggerManager_SentryOperations(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) error
	}{
		{
			name: "InitSentry with disabled config",
			setupFunc: func(ts *testSetup) error {
				sentryConfig := &logger.SentryConfig{DSN: ""}
				return ts.realManager.InitSentry(sentryConfig)
			},
		},
		{
			name: "InitSentry with nil config",
			setupFunc: func(ts *testSetup) error {
				return ts.realManager.InitSentry(nil)
			},
		},
		{
			name: "SetGlobalTopicID with valid ID",
			setupFunc: func(ts *testSetup) error {
				ts.realManager.SetGlobalTopicID("test-topic-123")
				return nil
			},
		},
		{
			name: "SetGlobalTopicID with empty ID",
			setupFunc: func(ts *testSetup) error {
				ts.realManager.SetGlobalTopicID("")
				return nil
			},
		},
		{
			name: "FlushSentry",
			setupFunc: func(ts *testSetup) error {
				ts.realManager.FlushSentry(1 * time.Second)
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			err := tt.setupFunc(ts)
			assert.NoError(t, err)
		})
	}
}

// Test package-level functions (backward compatibility)
func TestPackageLevelFunctions_RealImplementation(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func() error
	}{
		{
			name: "New function",
			setupFunc: func() error {
				l, err := logger.New(true)
				if err != nil {
					return err
				}
				if l == nil {
					return errors.New("logger is nil")
				}
				return nil
			},
		},
		{
			name: "NewDefault function",
			setupFunc: func() error {
				l, err := logger.NewDefault()
				if err != nil {
					return err
				}
				if l == nil {
					return errors.New("logger is nil")
				}
				return nil
			},
		},
		{
			name: "NewWithSentry function",
			setupFunc: func() error {
				sentryConfig := &logger.SentryConfig{DSN: ""}
				l, err := logger.NewWithSentry(true, sentryConfig)
				if err != nil {
					return err
				}
				if l == nil {
					return errors.New("logger is nil")
				}
				return nil
			},
		},
		{
			name: "InitSentry function",
			setupFunc: func() error {
				sentryConfig := &logger.SentryConfig{DSN: ""}
				return logger.InitSentry(sentryConfig)
			},
		},
		{
			name: "SetGlobalTopicID function",
			setupFunc: func() error {
				logger.SetGlobalTopicID("test-topic")
				return nil
			},
		},
		{
			name: "FlushSentry function",
			setupFunc: func() error {
				logger.FlushSentry(1 * time.Second)
				return nil
			},
		},
		{
			name: "NewRelayerMessageTracer function",
			setupFunc: func() error {
				testLogger := zaptest.NewLogger(&testing.T{})
				tracer := logger.NewRelayerMessageTracer(testLogger)
				if tracer == nil {
					return errors.New("tracer is nil")
				}
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Reset to ensure clean state
			logger.ResetForTesting()

			err := tt.setupFunc()
			assert.NoError(t, err)
		})
	}
}

// Test mocking functionality
func TestLoggerManager_Mocking_Success(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		testFunc  func(*testSetup) error
	}{
		{
			name: "Mock New function",
			setupFunc: func(ts *testSetup) {
				ts.mockLoggerManager.EXPECT().
					New(true).
					Return(ts.testLogger, nil).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				l, err := logger.New(true)
				if err != nil {
					return err
				}
				if l != ts.testLogger {
					return errors.New("expected mocked logger")
				}
				return nil
			},
		},
		{
			name: "Mock NewDefault function",
			setupFunc: func(ts *testSetup) {
				ts.mockLoggerManager.EXPECT().
					NewDefault().
					Return(ts.testLogger, nil).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				l, err := logger.NewDefault()
				if err != nil {
					return err
				}
				if l != ts.testLogger {
					return errors.New("expected mocked logger")
				}
				return nil
			},
		},
		{
			name: "Mock NewWithSentry function",
			setupFunc: func(ts *testSetup) {
				sentryConfig := &logger.SentryConfig{DSN: "test"}
				ts.mockLoggerManager.EXPECT().
					NewWithSentry(false, sentryConfig).
					Return(ts.testLogger, nil).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				sentryConfig := &logger.SentryConfig{DSN: "test"}
				l, err := logger.NewWithSentry(false, sentryConfig)
				if err != nil {
					return err
				}
				if l != ts.testLogger {
					return errors.New("expected mocked logger")
				}
				return nil
			},
		},
		{
			name: "Mock InitSentry function",
			setupFunc: func(ts *testSetup) {
				sentryConfig := &logger.SentryConfig{DSN: "test"}
				ts.mockLoggerManager.EXPECT().
					InitSentry(sentryConfig).
					Return(nil).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				sentryConfig := &logger.SentryConfig{DSN: "test"}
				return logger.InitSentry(sentryConfig)
			},
		},
		{
			name: "Mock SetGlobalTopicID function",
			setupFunc: func(ts *testSetup) {
				ts.mockLoggerManager.EXPECT().
					SetGlobalTopicID("test-topic").
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				logger.SetGlobalTopicID("test-topic")
				return nil
			},
		},
		{
			name: "Mock FlushSentry function",
			setupFunc: func(ts *testSetup) {
				ts.mockLoggerManager.EXPECT().
					FlushSentry(2 * time.Second).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				logger.FlushSentry(2 * time.Second)
				return nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Inject mock
			logger.InjectLoggerManagerForTesting(ts.mockLoggerManager)

			// Setup expectations
			tt.setupFunc(ts)

			// Execute test
			err := tt.testFunc(ts)
			assert.NoError(t, err)
		})
	}
}

func TestLoggerManager_Mocking_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		testFunc  func(*testSetup) error
		wantErr   string
	}{
		{
			name: "New function returns error",
			setupFunc: func(ts *testSetup) {
				ts.mockLoggerManager.EXPECT().
					New(false).
					Return(nil, errors.New("logger creation failed")).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				l, err := logger.New(false)
				if err == nil {
					return errors.New("expected error")
				}
				if l != nil {
					return errors.New("expected nil logger")
				}
				return err
			},
			wantErr: "logger creation failed",
		},
		{
			name: "NewDefault function returns error",
			setupFunc: func(ts *testSetup) {
				ts.mockLoggerManager.EXPECT().
					NewDefault().
					Return(nil, errors.New("default logger failed")).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				l, err := logger.NewDefault()
				if err == nil {
					return errors.New("expected error")
				}
				if l != nil {
					return errors.New("expected nil logger")
				}
				return err
			},
			wantErr: "default logger failed",
		},
		{
			name: "NewWithSentry function returns error",
			setupFunc: func(ts *testSetup) {
				sentryConfig := &logger.SentryConfig{DSN: "invalid"}
				ts.mockLoggerManager.EXPECT().
					NewWithSentry(true, sentryConfig).
					Return(nil, errors.New("sentry logger failed")).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				sentryConfig := &logger.SentryConfig{DSN: "invalid"}
				l, err := logger.NewWithSentry(true, sentryConfig)
				if err == nil {
					return errors.New("expected error")
				}
				if l != nil {
					return errors.New("expected nil logger")
				}
				return err
			},
			wantErr: "sentry logger failed",
		},
		{
			name: "InitSentry function returns error",
			setupFunc: func(ts *testSetup) {
				sentryConfig := &logger.SentryConfig{DSN: "invalid"}
				ts.mockLoggerManager.EXPECT().
					InitSentry(sentryConfig).
					Return(errors.New("sentry init failed")).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				sentryConfig := &logger.SentryConfig{DSN: "invalid"}
				return logger.InitSentry(sentryConfig)
			},
			wantErr: "sentry init failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			// Inject mock
			logger.InjectLoggerManagerForTesting(ts.mockLoggerManager)

			// Setup expectations
			tt.setupFunc(ts)

			// Execute test
			err := tt.testFunc(ts)
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// Test concurrent access patterns
func TestLoggerManager_ConcurrentAccess(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	const numGoroutines = 20

	t.Run("concurrent New calls", func(t *testing.T) {
		results := make(chan *zap.Logger, numGoroutines)
		errors := make(chan error, numGoroutines)

		for range numGoroutines {
			go func() {
				result, err := ts.realManager.New(true)
				results <- result
				errors <- err
			}()
		}

		// Collect results
		for range numGoroutines {
			result := <-results
			err := <-errors
			assert.NoError(t, err)
			assert.NotNil(t, result)
		}
	})

	t.Run("concurrent Sentry operations", func(t *testing.T) {
		done := make(chan bool, numGoroutines)

		for i := range numGoroutines {
			go func(id int) {
				// Test different operations concurrently
				switch id % 3 {
				case 0:
					ts.realManager.SetGlobalTopicID("concurrent-topic")
				case 1:
					ts.realManager.FlushSentry(1 * time.Millisecond)
				case 2:
					_ = ts.realManager.InitSentry(&logger.SentryConfig{DSN: ""})
				}
				done <- true
			}(i)
		}

		// Wait for all goroutines to complete
		for range numGoroutines {
			<-done
		}
	})
}

// Test SentryConfig methods
func TestSentryConfig_Methods(t *testing.T) {
	tests := []struct {
		name     string
		config   *logger.SentryConfig
		testFunc func(*logger.SentryConfig) interface{}
		expected interface{}
	}{
		{
			name:     "GetDebug with true",
			config:   &logger.SentryConfig{Debug: "true"},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetDebug() },
			expected: true,
		},
		{
			name:     "GetDebug with false",
			config:   &logger.SentryConfig{Debug: "false"},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetDebug() },
			expected: false,
		},
		{
			name:     "GetDebug with empty string",
			config:   &logger.SentryConfig{Debug: ""},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetDebug() },
			expected: false,
		},
		{
			name:     "GetDebug with invalid value",
			config:   &logger.SentryConfig{Debug: "invalid"},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetDebug() },
			expected: false,
		},
		{
			name:     "GetSampleRate with valid value",
			config:   &logger.SentryConfig{SampleRate: "0.5"},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetSampleRate() },
			expected: 0.5,
		},
		{
			name:     "GetSampleRate with empty string",
			config:   &logger.SentryConfig{SampleRate: ""},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetSampleRate() },
			expected: 1.0,
		},
		{
			name:     "GetSampleRate with invalid value",
			config:   &logger.SentryConfig{SampleRate: "invalid"},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetSampleRate() },
			expected: 1.0,
		},
		{
			name:     "IsEnabled with valid DSN",
			config:   &logger.SentryConfig{DSN: "https://test@sentry.io/123"},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.IsEnabled() },
			expected: true,
		},
		{
			name:     "IsEnabled with empty DSN",
			config:   &logger.SentryConfig{DSN: ""},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.IsEnabled() },
			expected: false,
		},
		{
			name:     "IsEnabled with whitespace DSN",
			config:   &logger.SentryConfig{DSN: "   "},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.IsEnabled() },
			expected: false,
		},
		{
			name:     "IsEnabled with nil config",
			config:   nil,
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.IsEnabled() },
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.testFunc(tt.config)
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Test injection and reset functionality
func TestLoggerManager_InjectionAndReset(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test injection
	logger.InjectLoggerManagerForTesting(ts.mockLoggerManager)

	// Setup expectation
	ts.mockLoggerManager.EXPECT().
		New(true).
		Return(ts.testLogger, nil).
		Times(1)

	// Test that injected mock is used
	result, err := logger.New(true)
	assert.NoError(t, err)
	assert.Equal(t, ts.testLogger, result)

	// Test reset
	logger.ResetForTesting()

	// Test that real implementation is used after reset
	result2, err2 := logger.New(true)
	assert.NoError(t, err2)
	assert.NotNil(t, result2)
	assert.NotEqual(t, ts.testLogger, result2) // Should be different from mocked logger
}
