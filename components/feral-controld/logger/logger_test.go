package logger_test

import (
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
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
			name:     "GetDebug with TRUE (case insensitive)",
			config:   &logger.SentryConfig{Debug: "TRUE"},
			testFunc: func(sc *logger.SentryConfig) interface{} { return sc.GetDebug() },
			expected: true,
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

// Test AddSentry functionality
func TestLoggerManager_AddSentry(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test logger
	testLogger, err := ts.realManager.New(false)
	assert.NoError(t, err)
	assert.NotNil(t, testLogger)

	// Create a test SentryConfig
	sentryConfig := logger.SentryConfig{
		DSN:         "https://test@sentry.io/123",
		Debug:       "true",
		SampleRate:  "0.5",
		Environment: "test",
		Release:     "1.0.0",
		Repository:  "test-repo",
	}

	// Test AddSentry
	enhancedLogger, err := ts.realManager.AddSentry(testLogger, sentryConfig)
	assert.NoError(t, err)
	assert.NotNil(t, enhancedLogger)
	assert.NotEqual(t, testLogger, enhancedLogger) // Should be a different logger instance
}

// Test AddSentry with invalid DSN
func TestLoggerManager_AddSentry_InvalidDSN(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test logger
	testLogger, err := ts.realManager.New(false)
	assert.NoError(t, err)
	assert.NotNil(t, testLogger)

	// Create a test SentryConfig with invalid DSN
	sentryConfig := logger.SentryConfig{
		DSN: "invalid-dsn",
	}

	// Test AddSentry with invalid DSN - should return error
	enhancedLogger, err := ts.realManager.AddSentry(testLogger, sentryConfig)
	assert.Error(t, err)
	assert.Nil(t, enhancedLogger)
}

// Test FlushSentry functionality
func TestLoggerManager_FlushSentry(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test FlushSentry - should not panic
	ts.realManager.FlushSentry(1 * time.Second)
	ts.realManager.FlushSentry(0) // Test with zero timeout
}

// Test SetGlobalTopicID functionality
func TestLoggerManager_SetGlobalTopicID(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test SetGlobalTopicID with valid topic ID
	ts.realManager.SetGlobalTopicID("test-topic-123")

	// Test SetGlobalTopicID with empty topic ID (should not panic)
	ts.realManager.SetGlobalTopicID("")

	// Test SetGlobalTopicID with whitespace topic ID (should not panic)
	ts.realManager.SetGlobalTopicID("   ")
}

// Test package-level functions (backward compatibility)
func TestPackageLevelFunctions_RealImplementation(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func() error
	}{
		{
			name: "New function with debug",
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
			name: "New function with production",
			setupFunc: func() error {
				l, err := logger.New(false)
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
			name: "AddSentry function",
			setupFunc: func() error {
				l, err := logger.New(false)
				if err != nil {
					return err
				}
				sentryConfig := logger.SentryConfig{
					DSN:         "https://test@sentry.io/123",
					Debug:       "true",
					SampleRate:  "0.5",
					Environment: "test",
					Release:     "1.0.0",
					Repository:  "test-repo",
				}
				enhancedLogger, err := logger.AddSentry(l, sentryConfig)
				if err != nil {
					return err
				}
				if enhancedLogger == nil {
					return errors.New("enhanced logger is nil")
				}
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
			name: "SetGlobalTopicID function",
			setupFunc: func() error {
				logger.SetGlobalTopicID("test-topic")
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
			name: "Mock New function with production mode",
			setupFunc: func(ts *testSetup) {
				ts.mockLoggerManager.EXPECT().
					New(false).
					Return(ts.testLogger, nil).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				l, err := logger.New(false)
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
			name: "Mock AddSentry function",
			setupFunc: func(ts *testSetup) {
				sentryConfig := logger.SentryConfig{
					DSN:         "https://test@sentry.io/123",
					Debug:       "true",
					SampleRate:  "0.5",
					Environment: "test",
					Release:     "1.0.0",
					Repository:  "test-repo",
				}
				ts.mockLoggerManager.EXPECT().
					AddSentry(ts.testLogger, sentryConfig).
					Return(ts.testLogger, nil).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				sentryConfig := logger.SentryConfig{
					DSN:         "https://test@sentry.io/123",
					Debug:       "true",
					SampleRate:  "0.5",
					Environment: "test",
					Release:     "1.0.0",
					Repository:  "test-repo",
				}
				enhancedLogger, err := logger.AddSentry(ts.testLogger, sentryConfig)
				if err != nil {
					return err
				}
				if enhancedLogger != ts.testLogger {
					return errors.New("expected mocked enhanced logger")
				}
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
			name: "AddSentry function returns error",
			setupFunc: func(ts *testSetup) {
				sentryConfig := logger.SentryConfig{
					DSN:         "https://test@sentry.io/123",
					Debug:       "true",
					SampleRate:  "0.5",
					Environment: "test",
					Release:     "1.0.0",
					Repository:  "test-repo",
				}
				ts.mockLoggerManager.EXPECT().
					AddSentry(ts.testLogger, sentryConfig).
					Return(nil, errors.New("sentry integration failed")).
					Times(1)
			},
			testFunc: func(ts *testSetup) error {
				sentryConfig := logger.SentryConfig{
					DSN:         "https://test@sentry.io/123",
					Debug:       "true",
					SampleRate:  "0.5",
					Environment: "test",
					Release:     "1.0.0",
					Repository:  "test-repo",
				}
				enhancedLogger, err := logger.AddSentry(ts.testLogger, sentryConfig)
				if err == nil {
					return errors.New("expected error")
				}
				if enhancedLogger != nil {
					return errors.New("expected nil enhanced logger")
				}
				return err
			},
			wantErr: "sentry integration failed",
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

	t.Run("concurrent AddSentry operations", func(t *testing.T) {
		// Create a base logger first
		baseLogger, err := ts.realManager.New(false)
		assert.NoError(t, err)
		assert.NotNil(t, baseLogger)

		done := make(chan bool, numGoroutines)

		for i := range numGoroutines {
			go func(id int) {
				sentryConfig := logger.SentryConfig{
					DSN:         "https://test@sentry.io/123",
					Debug:       "true",
					SampleRate:  "0.5",
					Environment: "test",
					Release:     "1.0.0",
					Repository:  "test-repo",
				}
				enhancedLogger, err := ts.realManager.AddSentry(baseLogger, sentryConfig)
				if err != nil {
					t.Errorf("AddSentry failed: %v", err)
				}
				if enhancedLogger == nil {
					t.Error("Enhanced logger is nil")
				}
				done <- true
			}(i)
		}

		// Wait for all goroutines to complete
		for range numGoroutines {
			<-done
		}
	})

	t.Run("concurrent FlushSentry operations", func(t *testing.T) {
		done := make(chan bool, numGoroutines)

		for i := range numGoroutines {
			go func(id int) {
				ts.realManager.FlushSentry(time.Duration(id) * time.Millisecond)
				done <- true
			}(i)
		}

		// Wait for all goroutines to complete
		for range numGoroutines {
			<-done
		}
	})

	t.Run("concurrent SetGlobalTopicID operations", func(t *testing.T) {
		done := make(chan bool, numGoroutines)

		for i := range numGoroutines {
			go func(id int) {
				ts.realManager.SetGlobalTopicID("test-topic-" + strconv.Itoa(id))
				done <- true
			}(i)
		}

		// Wait for all goroutines to complete
		for range numGoroutines {
			<-done
		}
	})
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

// Test logger configuration
func TestLoggerManager_LoggerConfiguration(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	t.Run("debug logger configuration", func(t *testing.T) {
		logger, err := ts.realManager.New(true)
		assert.NoError(t, err)
		assert.NotNil(t, logger)

		// Test that debug logger works
		logger.Info("Debug logger test")
		logger.Debug("Debug message")
		logger.Warn("Warning message")
		logger.Error("Error message")
	})

	t.Run("production logger configuration", func(t *testing.T) {
		logger, err := ts.realManager.New(false)
		assert.NoError(t, err)
		assert.NotNil(t, logger)

		// Test that production logger works
		logger.Info("Production logger test")
		logger.Warn("Warning message")
		logger.Error("Error message")
	})
}

// Test Sentry integration edge cases
func TestLoggerManager_SentryIntegration_EdgeCases(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test that AddSentry works with a valid sentry config
	t.Run("AddSentry with valid sentry config", func(t *testing.T) {
		testLogger, err := ts.realManager.New(false)
		assert.NoError(t, err)
		assert.NotNil(t, testLogger)

		sentryConfig := logger.SentryConfig{
			DSN:         "https://test@sentry.io/123",
			Debug:       "true",
			SampleRate:  "0.5",
			Environment: "test",
			Release:     "1.0.0",
			Repository:  "test-repo",
		}
		enhancedLogger, err := ts.realManager.AddSentry(testLogger, sentryConfig)
		assert.NoError(t, err)
		assert.NotNil(t, enhancedLogger)
		assert.NotEqual(t, testLogger, enhancedLogger)
	})

	// Test AddSentry with empty DSN
	t.Run("AddSentry with empty DSN", func(t *testing.T) {
		testLogger, err := ts.realManager.New(false)
		assert.NoError(t, err)
		assert.NotNil(t, testLogger)

		sentryConfig := logger.SentryConfig{
			DSN: "",
		}
		enhancedLogger, err := ts.realManager.AddSentry(testLogger, sentryConfig)
		// Empty DSN is actually accepted by Sentry (uses noop transport)
		assert.NoError(t, err)
		assert.NotNil(t, enhancedLogger)
	})

	// Test AddSentry with various debug values
	t.Run("AddSentry with different debug values", func(t *testing.T) {
		testLogger, err := ts.realManager.New(false)
		assert.NoError(t, err)
		assert.NotNil(t, testLogger)

		testCases := []struct {
			name   string
			debug  string
			expect bool
		}{
			{"true", "true", true},
			{"false", "false", false},
			{"TRUE", "TRUE", true},
			{"FALSE", "FALSE", false},
			{"empty", "", false},
			{"invalid", "invalid", false},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				sentryConfig := logger.SentryConfig{
					DSN:   "https://test@sentry.io/123",
					Debug: tc.debug,
				}
				assert.Equal(t, tc.expect, sentryConfig.GetDebug())
			})
		}
	})

	// Test AddSentry with various sample rates
	t.Run("AddSentry with different sample rates", func(t *testing.T) {
		testLogger, err := ts.realManager.New(false)
		assert.NoError(t, err)
		assert.NotNil(t, testLogger)

		testCases := []struct {
			name     string
			rate     string
			expected float64
		}{
			{"0.5", "0.5", 0.5},
			{"1.0", "1.0", 1.0},
			{"0.0", "0.0", 0.0},
			{"empty", "", 1.0},
			{"invalid", "invalid", 1.0},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				sentryConfig := logger.SentryConfig{
					DSN:        "https://test@sentry.io/123",
					SampleRate: tc.rate,
				}
				assert.Equal(t, tc.expected, sentryConfig.GetSampleRate())
			})
		}
	})
}

// Test multiple logger manager instances
func TestLoggerManager_MultipleInstances(t *testing.T) {
	// Create multiple instances
	manager1 := logger.NewLoggerManager()
	manager2 := logger.NewLoggerManager()

	// Test that they are independent
	logger1, err1 := manager1.New(true)
	assert.NoError(t, err1)
	assert.NotNil(t, logger1)

	logger2, err2 := manager2.New(false)
	assert.NoError(t, err2)
	assert.NotNil(t, logger2)

	// Test that they can be used concurrently
	done := make(chan bool, 2)

	go func() {
		manager1.FlushSentry(1 * time.Second)
		done <- true
	}()

	go func() {
		manager2.FlushSentry(1 * time.Second)
		done <- true
	}()

	// Wait for both to complete
	<-done
	<-done
}

// Test SentryConfig edge cases
func TestSentryConfig_EdgeCases(t *testing.T) {
	t.Run("IsEnabled with various DSN formats", func(t *testing.T) {
		testCases := []struct {
			name     string
			dsn      string
			expected bool
		}{
			{"valid HTTPS DSN", "https://test@sentry.io/123", true},
			{"valid HTTP DSN", "http://test@sentry.io/123", true},
			{"empty DSN", "", false},
			{"whitespace DSN", "   ", false},
			{"tab DSN", "\t", false},
			{"newline DSN", "\n", false},
			{"mixed whitespace DSN", " \t\n ", false},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				config := &logger.SentryConfig{DSN: tc.dsn}
				assert.Equal(t, tc.expected, config.IsEnabled())
			})
		}
	})

	t.Run("GetDebug with case variations", func(t *testing.T) {
		testCases := []struct {
			name     string
			debug    string
			expected bool
		}{
			{"lowercase true", "true", true},
			{"uppercase TRUE", "TRUE", true},
			{"mixed case TrUe", "TrUe", true},
			{"lowercase false", "false", false},
			{"uppercase FALSE", "FALSE", false},
			{"mixed case FaLsE", "FaLsE", false},
			{"empty", "", false},
			{"invalid", "invalid", false},
			{"1", "1", true},  // strconv.ParseBool accepts "1" as true
			{"0", "0", false}, // strconv.ParseBool accepts "0" as false
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				config := &logger.SentryConfig{Debug: tc.debug}
				assert.Equal(t, tc.expected, config.GetDebug())
			})
		}
	})

	t.Run("GetSampleRate with various values", func(t *testing.T) {
		testCases := []struct {
			name     string
			rate     string
			expected float64
		}{
			{"0.0", "0.0", 0.0},
			{"0.5", "0.5", 0.5},
			{"1.0", "1.0", 1.0},
			{"1", "1", 1.0},
			{"0", "0", 0.0},
			{"empty", "", 1.0},
			{"invalid", "invalid", 1.0},
			{"negative", "-0.5", -0.5},
			{"greater than 1", "1.5", 1.5},
		}

		for _, tc := range testCases {
			t.Run(tc.name, func(t *testing.T) {
				config := &logger.SentryConfig{SampleRate: tc.rate}
				assert.Equal(t, tc.expected, config.GetSampleRate())
			})
		}
	})
}
