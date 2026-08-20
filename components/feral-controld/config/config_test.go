package config_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type testSetup struct {
	ctrl     *gomock.Controller
	ctx      context.Context
	mockOS   *mocks.MockOS
	mockJSON *mocks.MockJSON
	mockExec *mocks.MockExec
	cm       config.ConfigManager
	logger   *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockOS := mocks.NewMockOS(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockExec := mocks.NewMockExec(ctrl)
	cm := config.NewConfigManagerWithDeps(mockOS, mockJSON, mockExec)

	return &testSetup{
		ctrl:     ctrl,
		ctx:      ctx,
		mockOS:   mockOS,
		mockJSON: mockJSON,
		mockExec: mockExec,
		cm:       cm,
		logger:   logger,
	}
}

func (ts *testSetup) teardown() {
	config.ResetForTesting()
	ts.ctrl.Finish()
}

// setupMACExpectations sets up mock expectations for MAC info fetching.
// This mocks the actual nmcli and ethtool commands that getMACInfo calls.
func (ts *testSetup) setupMACExpectations() {
	// Mock nmcli command to get network devices
	nmcliCmd := mocks.NewMockExecCmd(ts.ctrl)
	nmcliCmd.EXPECT().Output().Return([]byte("enp1s0:ethernet\nwlp2s0:wifi"), nil).Times(1)
	ts.mockExec.EXPECT().CommandContext(gomock.Any(), "nmcli", "-t", "-f", "DEVICE,TYPE", "device").Return(nmcliCmd).Times(1)

	// Mock ethtool commands to get MAC addresses for each device
	ethtoolCmd1 := mocks.NewMockExecCmd(ts.ctrl)
	ethtoolCmd1.EXPECT().Output().Return([]byte("Permanent address: aa:bb:cc:dd:ee:ff"), nil).Times(1)
	ts.mockExec.EXPECT().CommandContext(gomock.Any(), "ethtool", "-P", "enp1s0").Return(ethtoolCmd1).Times(1)

	ethtoolCmd2 := mocks.NewMockExecCmd(ts.ctrl)
	ethtoolCmd2.EXPECT().Output().Return([]byte("Permanent address: 11:22:33:44:55:66"), nil).Times(1)
	ts.mockExec.EXPECT().CommandContext(gomock.Any(), "ethtool", "-P", "wlp2s0").Return(ethtoolCmd2).Times(1)
}

// Test ConfigManager interface

func TestConfigManager_Load_Success_ExistingFile(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	configData := `{
		"cdp": {
			"endpoint": "http://localhost:9222"
		},
		"relayer": {
			"endpoint": "wss://relay.feralfile.com",
			"apiKey": "test-api-key"
		},
		"sentry": {
			"dsn": "https://test@sentry.io/123",
			"environment": "test"
		}
	}`

	// Setup expectations
	ts.mockOS.EXPECT().
		ReadFile(constants.CONFIG_FILE).
		Return([]byte(configData), nil).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(configData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			cfg := v.(*config.Config)
			cfg.CDPConfig = &config.CDPConfig{
				Endpoint: "http://localhost:9222",
			}
			cfg.RelayerConfig = &config.RelayerConfig{
				Endpoint: "wss://relay.feralfile.com",
				APIKey:   "test-api-key",
			}
			cfg.SentryConfig = &logger.SentryConfig{
				DSN:         "https://test@sentry.io/123",
				Environment: "test",
			}
			return nil
		}).
		Times(1)

	// Setup MAC address expectations
	ts.setupMACExpectations()

	// Execute
	result, err := ts.cm.Load(ts.logger)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "http://localhost:9222", result.CDPConfig.Endpoint)
	assert.Equal(t, "wss://relay.feralfile.com", result.RelayerConfig.Endpoint)
	assert.Equal(t, "test-api-key", result.RelayerConfig.APIKey)
	assert.Equal(t, "https://test@sentry.io/123", result.SentryConfig.DSN)
	assert.Equal(t, "test", result.SentryConfig.Environment)

	// Verify MAC info is populated as a map
	assert.NotNil(t, result.MACInfo)
	assert.Equal(t, "aa:bb:cc:dd:ee:ff", result.MACInfo["enp1s0"])
	assert.Equal(t, "11:22:33:44:55:66", result.MACInfo["wlp2s0"])
}

func TestConfigManager_Load_Success_AlreadyLoaded(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	configData := `{
		"cdp": {
			"endpoint": "http://localhost:9222"
		}
	}`

	// First load - should read from file
	ts.mockOS.EXPECT().
		ReadFile(constants.CONFIG_FILE).
		Return([]byte(configData), nil).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(configData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			cfg := v.(*config.Config)
			cfg.CDPConfig = &config.CDPConfig{
				Endpoint: "http://localhost:9222",
			}
			cfg.RelayerConfig = &config.RelayerConfig{}
			cfg.SentryConfig = &logger.SentryConfig{}
			return nil
		}).
		Times(1)

	// Setup MAC address expectations
	ts.setupMACExpectations()

	// First load
	result1, err1 := ts.cm.Load(ts.logger)
	assert.NoError(t, err1)

	// Second load - should return cached config without file operations
	result2, err2 := ts.cm.Load(ts.logger)
	assert.NoError(t, err2)
	assert.Equal(t, result1, result2)
}

func TestConfigManager_Load_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		{
			name: "config file not found",
			setupFunc: func(ts *testSetup) {
				notFoundErr := &os.PathError{Op: "open", Path: constants.CONFIG_FILE, Err: os.ErrNotExist}

				ts.mockOS.EXPECT().
					ReadFile(constants.CONFIG_FILE).
					Return(nil, notFoundErr).
					Times(1)

				ts.mockOS.EXPECT().
					IsNotExist(notFoundErr).
					Return(true).
					Times(1)
			},
			wantErr: "config file not found",
		},
		{
			name: "read file error",
			setupFunc: func(ts *testSetup) {
				readErr := fmt.Errorf("permission denied")

				ts.mockOS.EXPECT().
					ReadFile(gomock.Any()).
					Return(nil, readErr).
					Times(1)

				ts.mockOS.EXPECT().
					IsNotExist(readErr).
					Return(false).
					Times(1)
			},
			wantErr: "failed to read config file",
		},
		{
			name: "JSON unmarshal error",
			setupFunc: func(ts *testSetup) {
				invalidJSON := `{"invalid": json}`

				ts.mockOS.EXPECT().
					ReadFile(gomock.Any()).
					Return([]byte(invalidJSON), nil).
					Times(1)

				ts.mockOS.EXPECT().
					IsNotExist(nil).
					Return(false).
					Times(1)

				ts.mockJSON.EXPECT().
					Unmarshal([]byte(invalidJSON), gomock.Any()).
					Return(fmt.Errorf("invalid character 'j' looking for beginning of value")).
					Times(1)
			},
			wantErr: "failed to parse config file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			tt.setupFunc(ts)

			result, err := ts.cm.Load(ts.logger)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Nil(t, result)
		})
	}
}

func TestConfigManager_Get_InitialCall(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	cm := config.NewConfigManager()
	result := cm.Get()

	assert.NotNil(t, result)
	assert.NotNil(t, result.CDPConfig)
	assert.NotNil(t, result.RelayerConfig)
	assert.NotNil(t, result.SentryConfig)
	assert.Empty(t, result.CDPConfig.Endpoint)
	assert.Empty(t, result.RelayerConfig.Endpoint)
	assert.Empty(t, result.RelayerConfig.APIKey)
	assert.Empty(t, result.SentryConfig.DSN)
	assert.Empty(t, result.SentryConfig.Environment)
}

func TestConfigManager_Get_AfterLoad(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	configData := `{
		"cdp": {
			"endpoint": "http://localhost:9222"
		},
		"relayer": {
			"endpoint": "wss://relay.feralfile.com"
		}
	}`

	// Setup successful load
	ts.mockOS.EXPECT().
		ReadFile(gomock.Any()).
		Return([]byte(configData), nil).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(configData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			cfg := v.(*config.Config)
			cfg.CDPConfig = &config.CDPConfig{
				Endpoint: "http://localhost:9222",
			}
			cfg.RelayerConfig = &config.RelayerConfig{
				Endpoint: "wss://relay.feralfile.com",
			}
			cfg.SentryConfig = &logger.SentryConfig{}
			return nil
		}).
		Times(1)

	// Setup MAC address expectations
	ts.setupMACExpectations()

	// Load config
	loadedConfig, err := ts.cm.Load(ts.logger)
	assert.NoError(t, err)

	// Get should return the same config
	result := ts.cm.Get()
	assert.Equal(t, loadedConfig, result)
	assert.Equal(t, "http://localhost:9222", result.CDPConfig.Endpoint)
	assert.Equal(t, "wss://relay.feralfile.com", result.RelayerConfig.Endpoint)
}

// Test concurrent access patterns with ConfigManager

func TestConfigManager_ConcurrentGet(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test concurrent Get calls
	const numGoroutines = 20
	results := make(chan *config.Config, numGoroutines)

	for range numGoroutines {
		go func() {
			result := ts.cm.Get()
			results <- result
		}()
	}

	// Collect results
	var configs []*config.Config
	for range numGoroutines {
		result := <-results
		assert.NotNil(t, result)
		assert.NotNil(t, result.CDPConfig)
		assert.NotNil(t, result.RelayerConfig)
		assert.NotNil(t, result.SentryConfig)
		assert.Empty(t, result.CDPConfig.Endpoint)
		assert.Empty(t, result.RelayerConfig.Endpoint)
		configs = append(configs, result)
	}

	// Verify all results are identical (due to mutex protection)
	firstConfig := configs[0]
	for i, cfg := range configs {
		assert.Equal(t, firstConfig.CDPConfig.Endpoint, cfg.CDPConfig.Endpoint,
			"concurrent Get %d: CDP endpoint mismatch", i)
		assert.Equal(t, firstConfig.RelayerConfig.Endpoint, cfg.RelayerConfig.Endpoint,
			"concurrent Get %d: relayer endpoint mismatch", i)
	}
}

func TestConfigManager_ConcurrentLoad(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	configData := `{
		"cdp": {
			"endpoint": "http://concurrent-test:9222"
		},
		"relayer": {
			"endpoint": "wss://concurrent-relay.test.com"
		}
	}`

	const numGoroutines = 5

	// Expect ReadFile to succeed only once (first caller wins, others get cached result)
	ts.mockOS.EXPECT().
		ReadFile(constants.CONFIG_FILE).
		Return([]byte(configData), nil).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(configData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			cfg := v.(*config.Config)
			cfg.CDPConfig = &config.CDPConfig{
				Endpoint: "http://concurrent-test:9222",
			}
			cfg.RelayerConfig = &config.RelayerConfig{
				Endpoint: "wss://concurrent-relay.test.com",
			}
			cfg.SentryConfig = &logger.SentryConfig{}
			return nil
		}).
		Times(1)

	// Setup MAC address expectations
	ts.setupMACExpectations()

	// Execute concurrent loads
	results := make(chan *config.Config, numGoroutines)
	errors := make(chan error, numGoroutines)

	for range numGoroutines {
		go func() {
			result, err := ts.cm.Load(ts.logger)
			results <- result
			errors <- err
		}()
	}

	// Collect results
	var loadedConfigs []*config.Config
	for range numGoroutines {
		result := <-results
		err := <-errors
		assert.NoError(t, err, "expected no error from concurrent load")
		assert.NotNil(t, result, "expected non-nil config from concurrent load")
		loadedConfigs = append(loadedConfigs, result)
	}

	// Verify all results are identical (due to mutex protection and caching)
	firstConfig := loadedConfigs[0]
	for i, loadedConfig := range loadedConfigs {
		assert.Equal(t, firstConfig.CDPConfig.Endpoint, loadedConfig.CDPConfig.Endpoint,
			"concurrent load %d: CDP endpoint mismatch", i)
		assert.Equal(t, firstConfig.RelayerConfig.Endpoint, loadedConfig.RelayerConfig.Endpoint,
			"concurrent load %d: relayer endpoint mismatch", i)
	}

	// Verify the expected values
	assert.Equal(t, "http://concurrent-test:9222", firstConfig.CDPConfig.Endpoint)
	assert.Equal(t, "wss://concurrent-relay.test.com", firstConfig.RelayerConfig.Endpoint)
}

// Test config package-level functions
func TestConfig_Load_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Use ConfigManager with mocked dependencies for testing global Load function
	cm := config.NewConfigManagerWithDeps(ts.mockOS, ts.mockJSON, ts.mockExec)
	config.InjectConfigManagerForTesting(cm)

	configData := `{
		"cdp": {
			"endpoint": "http://global-test:9222"
		}
	}`

	ts.mockOS.EXPECT().
		ReadFile(constants.CONFIG_FILE).
		Return([]byte(configData), nil).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(configData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			cfg := v.(*config.Config)
			cfg.CDPConfig = &config.CDPConfig{
				Endpoint: "http://global-test:9222",
			}
			cfg.RelayerConfig = &config.RelayerConfig{}
			cfg.SentryConfig = &logger.SentryConfig{}
			return nil
		}).
		Times(1)

	// Setup MAC address expectations
	ts.setupMACExpectations()

	result, err := config.Load(ts.logger)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "http://global-test:9222", result.CDPConfig.Endpoint)

	// Verify MAC info is populated as a map
	assert.NotNil(t, result.MACInfo)
	assert.Equal(t, "aa:bb:cc:dd:ee:ff", result.MACInfo["enp1s0"])
	assert.Equal(t, "11:22:33:44:55:66", result.MACInfo["wlp2s0"])
}

func TestConfig_Get_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result := config.Get()

	assert.NotNil(t, result)
	assert.NotNil(t, result.CDPConfig)
	assert.NotNil(t, result.RelayerConfig)
	assert.NotNil(t, result.SentryConfig)
	assert.Empty(t, result.CDPConfig.Endpoint)
	assert.Empty(t, result.RelayerConfig.Endpoint)
}

// TestProvisioningTuningPermissiveDecode pins the §3 config hazard rule: a
// syntactically valid but wrong-typed provisioning block logs and falls back
// to defaults WITHOUT failing the load (config.Load failing is fatal under
// Restart=always), while a well-formed block decodes and an absent one yields
// zeros (the machine's defaults).
func TestProvisioningTuningPermissiveDecode(t *testing.T) {
	t.Run("absent block yields zeros", func(t *testing.T) {
		c := &config.Config{}
		got := c.ProvisioningTuning(zap.NewNop())
		assert.Equal(t, config.ProvisioningTuning{}, got)
	})

	t.Run("well-formed block decodes", func(t *testing.T) {
		c := &config.Config{Provisioning: []byte(`{"setupIncompleteDisabled":true,"episodeApPhaseSeconds":120}`)}
		got := c.ProvisioningTuning(zap.NewNop())
		assert.True(t, got.SetupIncompleteDisabled)
		assert.Equal(t, 120, got.EpisodeApPhaseSeconds)
	})

	t.Run("wrong-typed block falls back to defaults without failing", func(t *testing.T) {
		c := &config.Config{Provisioning: []byte(`"not an object"`)}
		got := c.ProvisioningTuning(zap.NewNop())
		assert.Equal(t, config.ProvisioningTuning{}, got)
	})

	t.Run("wrong-typed block survives the whole-config parse", func(t *testing.T) {
		// The RawMessage field must swallow a wrong-typed (but syntactically
		// valid) block at Load time — the strictness stays scoped to existing
		// keys.
		var c config.Config
		err := json.Unmarshal([]byte(`{"provisioning": 42, "enableHub": true}`), &c)
		require.NoError(t, err, "a wrong-typed provisioning block must not fail the parse")
		assert.Equal(t, config.ProvisioningTuning{}, c.ProvisioningTuning(zap.NewNop()))
	})
}

func TestGatewayUserAgentTuningDecodesHostsAndUserAgent(t *testing.T) {
	t.Parallel()

	var c config.Config
	raw := `{"gatewayUserAgent":{"userAgent":"feral-player/9.9","hosts":["ipfs.io","https://dweb.link"]}}`
	require.NoError(t, json.Unmarshal([]byte(raw), &c))

	g := c.GatewayUserAgentTuning(zaptest.NewLogger(t))
	require.NotNil(t, g)
	assert.Equal(t, "feral-player/9.9", g.UserAgent)
	assert.Equal(t, []string{"ipfs.io", "https://dweb.link"}, g.Hosts)
	assert.True(t, g.IsEnabled(), "a block without \"enabled\" stays ON")
}

// The whole reason gatewayUserAgent is held as RawMessage: a wrong-typed
// value inside an OPTIONAL block must not fail config.Load. Load failure is
// Fatal, and under Restart=always (the unit sets StartLimitIntervalSec=0 so
// it never latches dead) that is an unbounded crash loop which takes the LAN
// hub, captive portal, provisioning and claiming down with it — every remote
// recovery surface, over a typo in a block whose purpose is to be hand-edited.
func TestGatewayUserAgentMalformedBlockIsNotFatal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  string
	}{
		{name: "hosts is a string, not an array", raw: `{"gatewayUserAgent":{"hosts":"ipfs.io"}}`},
		{name: "enabled is a string, not a bool", raw: `{"gatewayUserAgent":{"enabled":"yes"}}`},
		{name: "userAgent is a number", raw: `{"gatewayUserAgent":{"userAgent":123}}`},
		{name: "whole block is a string", raw: `{"gatewayUserAgent":"off"}`},
		{name: "whole block is an array", raw: `{"gatewayUserAgent":["ipfs.io"]}`},
		{name: "hosts holds non-strings", raw: `{"gatewayUserAgent":{"hosts":[1,2,3]}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			var c config.Config
			// The top-level unmarshal is the step that used to die. It must
			// now survive every shape above.
			require.NoError(t, json.Unmarshal([]byte(tt.raw), &c),
				"a malformed optional block must not fail the whole config parse")

			// And the permissive decode must report "absent" rather than
			// returning a half-populated section, so callers fall back to
			// the built-in defaults.
			assert.Nil(t, c.GatewayUserAgentTuning(zaptest.NewLogger(t)))
		})
	}
}

// An absent block and a well-formed block must both keep working — the
// permissive path must not have made the normal path lossy.
func TestGatewayUserAgentTuningAbsentBlock(t *testing.T) {
	t.Parallel()

	var c config.Config
	require.NoError(t, json.Unmarshal([]byte(`{}`), &c))

	g := c.GatewayUserAgentTuning(zaptest.NewLogger(t))
	assert.Nil(t, g)
	assert.True(t, g.IsEnabled(), "an absent block leaves the rewrite ON")
}

// A nil logger must not panic: config decoding runs before the final logger
// exists on some paths.
func TestGatewayUserAgentTuningNilLogger(t *testing.T) {
	t.Parallel()

	var c config.Config
	require.NoError(t, json.Unmarshal([]byte(`{"gatewayUserAgent":{"hosts":"bad"}}`), &c))
	assert.NotPanics(t, func() { c.GatewayUserAgentTuning(nil) })
}
