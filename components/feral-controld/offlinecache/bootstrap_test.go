package offlinecache_test

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func TestOptionsFromConfig_NilConfigUsesDefaults(t *testing.T) {
	opts := offlinecache.OptionsFromConfig(nil, "http://127.0.0.1:9222")

	assert.False(t, opts.Enabled)
	assert.Equal(t, offlinecache.DefaultRootDir, opts.RootDir)
	assert.Equal(t, offlinecache.DefaultHeadlessBinaryPath, opts.HeadlessBinaryPath)
	assert.Equal(t, offlinecache.DefaultHeadlessUserDataDir, opts.HeadlessUserDataDir)
	assert.Equal(t, offlinecache.DefaultHeadlessDebugPort, opts.HeadlessDebugPort)
	assert.Equal(t, offlinecache.DefaultHeadlessIdleTeardown, opts.HeadlessIdleTeardown)
	assert.Equal(t, offlinecache.DefaultStaticServerAddr, opts.StaticServerAddr)
	assert.Equal(t, offlinecache.MissPolicyFailClosed, opts.MissPolicy)
	assert.Equal(t, "http://127.0.0.1:9222", opts.KioskCDPEndpoint)
	assert.Zero(t, opts.MaxDiskBytes)
	assert.Zero(t, opts.CaptureWindowMs)
}

func TestOptionsFromConfig_EmptyConfigUsesDefaultsButRespectsEnabled(t *testing.T) {
	opts := offlinecache.OptionsFromConfig(&config.OfflineCacheConfig{Enabled: true}, "http://127.0.0.1:9222")

	assert.True(t, opts.Enabled)
	assert.Equal(t, offlinecache.DefaultRootDir, opts.RootDir)
	assert.Equal(t, offlinecache.MissPolicyFailClosed, opts.MissPolicy)
}

func TestOptionsFromConfig_OverridesEveryField(t *testing.T) {
	cfg := &config.OfflineCacheConfig{
		Enabled:                     true,
		RootDir:                     "/custom/root",
		MaxDiskBytes:                123456,
		CaptureWindowMs:             5000,
		HeadlessBinaryPath:          "/custom/chromium",
		HeadlessUserDataDir:         "/custom/profile",
		HeadlessDebugPort:           9333,
		HeadlessIdleTeardownSeconds: 45,
		StaticServerAddr:            "127.0.0.1:9999",
		MissPolicy:                  "pass_through",
	}

	opts := offlinecache.OptionsFromConfig(cfg, "http://127.0.0.1:9222")

	assert.True(t, opts.Enabled)
	assert.Equal(t, "/custom/root", opts.RootDir)
	assert.EqualValues(t, 123456, opts.MaxDiskBytes)
	assert.Equal(t, 5000, opts.CaptureWindowMs)
	assert.Equal(t, "/custom/chromium", opts.HeadlessBinaryPath)
	assert.Equal(t, "/custom/profile", opts.HeadlessUserDataDir)
	assert.Equal(t, 9333, opts.HeadlessDebugPort)
	assert.Equal(t, 45*time.Second, opts.HeadlessIdleTeardown)
	assert.Equal(t, "127.0.0.1:9999", opts.StaticServerAddr)
	assert.Equal(t, offlinecache.MissPolicy("pass_through"), opts.MissPolicy)
}

func TestBootstrap_WiresEveryComponent(t *testing.T) {
	dir := t.TempDir()
	opts := offlinecache.OptionsFromConfig(&config.OfflineCacheConfig{
		Enabled:          true,
		RootDir:          dir,
		StaticServerAddr: "127.0.0.1:0",
	}, "http://127.0.0.1:9222")

	rt := offlinecache.Bootstrap(
		opts, nil, nil,
		wrapper.NewHTTPClient(), wrapper.NewWebSocketDialer(nil),
		wrapper.NewOS(), wrapper.NewIO(), wrapper.NewJSON(), wrapper.NewExec(), wrapper.NewClock(),
		zaptest.NewLogger(t),
	)

	require.NotNil(t, rt.Service)
	require.NotNil(t, rt.KioskReplay)
	require.NotNil(t, rt.StaticServer)

	// Bootstrap must not have started anything (no I/O at construction
	// time) — Start-ing the service should still work against a freshly
	// wired store.
	require.NoError(t, rt.Service.Start(t.Context()))
	rt.Service.Stop()
}
