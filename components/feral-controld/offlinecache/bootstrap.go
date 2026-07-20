package offlinecache

import (
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

// Defaults for Options fields left unset by config.OfflineCacheConfig. Kept
// as named constants (rather than inlined in OptionsFromConfig) so
// main.go/tests/docs can reference the same values that ship as the
// zero-config behavior.
const (
	DefaultRootDir             = "/home/feralfile/.cache/offline-artworks"
	DefaultHeadlessBinaryPath  = "/usr/bin/chromium"
	DefaultHeadlessUserDataDir = "/home/feralfile/.cache/offline-artworks-headless-profile"
	// DefaultHeadlessDebugPort is distinct from the kiosk's 9222 so the
	// two Chromium processes (capture vs. player surface) never collide.
	DefaultHeadlessDebugPort    = 9223
	DefaultHeadlessIdleTeardown = 30 * time.Second
	DefaultStaticServerAddr     = "127.0.0.1:8082"
)

// Options bundles every offlinecache tunable, mirroring
// mintpairing.Options/OptionsFromConfig's shape: a package-owned, fully
// defaulted struct rather than passing a raw *config.OfflineCacheConfig
// around, so Bootstrap and its tests see one canonical set of values.
type Options struct {
	Enabled              bool
	RootDir              string
	MaxDiskBytes         int64
	CaptureWindowMs      int
	HeadlessBinaryPath   string
	HeadlessUserDataDir  string
	HeadlessDebugPort    int
	HeadlessIdleTeardown time.Duration
	StaticServerAddr     string
	MissPolicy           MissPolicy
	// KioskCDPEndpoint is the kiosk Chromium's DevTools HTTP endpoint
	// (e.g. "http://127.0.0.1:9222"). Always sourced from
	// config.CDPConfig.Endpoint at the call site — it is the same
	// physical kiosk cdp.CDP already connects to, never a second
	// configurable value that could drift out of sync with it.
	KioskCDPEndpoint string
}

// OptionsFromConfig fills Options from cfg (nil is treated as "feature
// disabled, use defaults for everything else" so callers can pass
// config.Get().OfflineCache directly without a nil check).
func OptionsFromConfig(cfg *config.OfflineCacheConfig, kioskCDPEndpoint string) Options {
	opts := Options{
		RootDir:              DefaultRootDir,
		HeadlessBinaryPath:   DefaultHeadlessBinaryPath,
		HeadlessUserDataDir:  DefaultHeadlessUserDataDir,
		HeadlessDebugPort:    DefaultHeadlessDebugPort,
		HeadlessIdleTeardown: DefaultHeadlessIdleTeardown,
		StaticServerAddr:     DefaultStaticServerAddr,
		MissPolicy:           MissPolicyFailClosed,
		KioskCDPEndpoint:     kioskCDPEndpoint,
	}
	if cfg == nil {
		return opts
	}

	opts.Enabled = cfg.Enabled
	if cfg.RootDir != "" {
		opts.RootDir = cfg.RootDir
	}
	opts.MaxDiskBytes = cfg.MaxDiskBytes
	if cfg.CaptureWindowMs > 0 {
		opts.CaptureWindowMs = cfg.CaptureWindowMs
	}
	if cfg.HeadlessBinaryPath != "" {
		opts.HeadlessBinaryPath = cfg.HeadlessBinaryPath
	}
	if cfg.HeadlessUserDataDir != "" {
		opts.HeadlessUserDataDir = cfg.HeadlessUserDataDir
	}
	if cfg.HeadlessDebugPort > 0 {
		opts.HeadlessDebugPort = cfg.HeadlessDebugPort
	}
	if cfg.HeadlessIdleTeardownSeconds > 0 {
		opts.HeadlessIdleTeardown = time.Duration(cfg.HeadlessIdleTeardownSeconds) * time.Second
	}
	if cfg.StaticServerAddr != "" {
		opts.StaticServerAddr = cfg.StaticServerAddr
	}
	if cfg.MissPolicy != "" {
		opts.MissPolicy = MissPolicy(cfg.MissPolicy)
	}
	return opts
}

// Runtime bundles Bootstrap's constructed components. Service and
// StaticServer have their own Start/Stop-shaped lifecycles the caller
// (main.go) must drive explicitly (Service.Start/Stop,
// StaticServer.ListenAndServe/Shutdown) — Bootstrap only wires the
// dependency graph, it does not start anything, matching every other
// component constructor in this codebase (systemd-services.mdc: startup
// must be explicit and observable, not hidden inside a constructor).
type Runtime struct {
	Service      Service
	KioskReplay  KioskReplay
	StaticServer StaticServer
}

// Bootstrap wires every offlinecache component together from Options. It
// is kept as one function (rather than spreading construction across
// main.go) so the dependency order — store, then downloader/capturer,
// then static server, then replayer (needs the static server), then
// service (needs capturer+store), then kiosk replay (needs the replayer)
// — is documented and testable in one place instead of re-derived at each
// call site.
func Bootstrap(
	opts Options,
	relayerClient relayer.Relayer,
	wsHandler ws.WS,
	httpClient wrapper.HTTPClient,
	dialer wrapper.WebSocketDialer,
	osWrapper wrapper.OS,
	ioWrapper wrapper.IO,
	jsonWrapper wrapper.JSON,
	execWrapper wrapper.Exec,
	clockWrapper wrapper.Clock,
	logger *zap.Logger,
) Runtime {
	store := NewStore(opts.RootDir, osWrapper, jsonWrapper, logger)
	classifier := NewClassifier(httpClient)
	downloader := NewDownloader(
		opts.HeadlessBinaryPath, opts.HeadlessUserDataDir, opts.HeadlessDebugPort,
		opts.HeadlessIdleTeardown, execWrapper, osWrapper, clockWrapper, httpClient, logger,
	)
	capturer := NewCapturer(downloader, dialer, httpClient, store, jsonWrapper, ioWrapper, clockWrapper, logger)
	staticServer := NewStaticServer(opts.StaticServerAddr, store, osWrapper, logger)
	replayer := NewReplayer(store, staticServer, opts.MissPolicy, jsonWrapper, logger)
	notifier := NewNotifier(relayerClient, wsHandler, logger)
	service := NewService(store, classifier, capturer, jsonWrapper, opts.CaptureWindowMs, opts.MaxDiskBytes, notifier, logger)
	kioskReplay := NewKioskReplay(replayer, store, opts.KioskCDPEndpoint, httpClient, dialer, jsonWrapper, ioWrapper, logger)

	return Runtime{Service: service, KioskReplay: kioskReplay, StaticServer: staticServer}
}
