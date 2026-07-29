package offlinecache

import (
	go_url "net/url"
	"strconv"
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
	// DefaultMaxDiskBytes bounds the store's total size when
	// offlineCache.maxDiskBytes is left unset (0) in config. Store and
	// Service's own maxDiskBytes/maxBytes parameters treat <=0 as
	// "unlimited" — a reasonable seam-level contract for direct
	// construction (tests, other future callers) — but OptionsFromConfig
	// deliberately does not let that meaning reach an operator's config
	// file: this feature exists to cache potentially gigabyte-scale
	// software-artwork assets (see docs/offline-artwork-capture.md's
	// 1.1GB video case) on disk-constrained embedded devices, so a
	// config that merely omits maxDiskBytes must not silently mean
	// "fill the disk." 10 GiB comfortably holds a full DP-1 playlist's
	// worth (up to 1024 items per the DP-1 spec) of video-heavy
	// artworks while still bounding runaway growth — operators on
	// smaller devices can still tighten this via offlineCache.maxDiskBytes.
	DefaultMaxDiskBytes = 10 << 30 // 10 GiB
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
		MaxDiskBytes:         DefaultMaxDiskBytes,
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
	if cfg.MaxDiskBytes > 0 {
		opts.MaxDiskBytes = cfg.MaxDiskBytes
	}
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
	Service     Service
	KioskReplay KioskReplay
	// ScopeLost is exposed for the same reason Notifier below is: main.go
	// needs a method the narrower dependency it otherwise holds does not
	// carry. It is deliberately the one-method ScopeLostRegistrar rather
	// than the whole Replayer — main.go has no business calling
	// Attach/EnableForPlaylist/Disable. Wiring it can only happen once
	// PlaylistRefresher exists, and that is constructed after this
	// Bootstrap call since it depends on KioskReplay.
	ScopeLost    ScopeLostRegistrar
	StaticServer StaticServer
	// Notifier is exposed so main.go can Close its background WS-
	// delivery worker at shutdown (see Notifier.Close's doc) — the
	// Service itself only holds it through the narrower ProgressObserver
	// interface (OnItemStateChanged only), which has no Close method, so
	// Service cannot own this shutdown step itself.
	Notifier *Notifier
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
	headlessDebugPort := safeHeadlessDebugPort(opts.HeadlessDebugPort, opts.KioskCDPEndpoint, logger)
	downloader := NewDownloader(
		opts.HeadlessBinaryPath, opts.HeadlessUserDataDir, headlessDebugPort,
		opts.HeadlessIdleTeardown, execWrapper, osWrapper, clockWrapper, httpClient, logger,
	)

	// bodyClient is deliberately NOT the daemon-wide httpClient above.
	// wrapper.NewHTTPClient carries a 30s http.Client.Timeout, and that
	// timeout covers the ENTIRE request including the response body —
	// so every artwork asset that takes more than 30 seconds to
	// transfer failed, unconditionally, on both download paths. That is
	// flatly incompatible with what this subsystem exists to do: the
	// store's budget is measured in GiB (DefaultMaxDiskBytes), and
	// staticserver.go only earns its keep on blobs over 200 MB — a size
	// no device uplink moves in 30 seconds, which made that entire
	// replay path unreachable in practice rather than merely slow.
	//
	// The two consumers below are the only ones that stream artwork
	// bodies. Everything else here (classifier's HEAD/GET probe,
	// downloader's and kioskReplay's localhost CDP calls) is a small,
	// fast request that SHOULD keep the daemon default: a stuck local
	// call must not hang forever just because large downloads need
	// room. Both body paths bound themselves explicitly instead — see
	// mediaDownloadTimeout and captureFinalizeWindowDefault — which is
	// the contract NewHTTPClientWithoutTimeout's own doc requires of
	// every caller.
	bodyClient := wrapper.NewHTTPClientWithoutTimeout()

	capturer := NewCapturer(downloader, dialer, bodyClient, store, jsonWrapper, ioWrapper, clockWrapper, opts.MaxDiskBytes, logger)
	// mediaCapturer needs no Downloader/dialer — it downloads a
	// non-software item's single-file source directly over HTTP, never
	// spinning up the headless Chromium capturer/downloader owns (see
	// mediacapture.go's package doc).
	mediaCapturer := NewMediaCapturer(bodyClient, store, clockWrapper, opts.MaxDiskBytes, logger)
	staticServer := NewStaticServer(opts.StaticServerAddr, store, osWrapper, logger)
	replayer := NewReplayer(store, staticServer, opts.MissPolicy, jsonWrapper, logger)
	// The concrete replayer implements the scope-lost setter, but Replayer
	// deliberately does not declare it (see ScopeLostRegistrar). Asserted
	// once here rather than exposing a wider interface from Bootstrap; a
	// failed assertion leaves Runtime.ScopeLost nil, which main.go's guard
	// treats as "no prompt recovery wired", not as a startup failure.
	scopeLost, _ := replayer.(ScopeLostRegistrar)
	notifier := NewNotifier(relayerClient, wsHandler, logger)
	service := NewService(store, classifier, capturer, mediaCapturer, jsonWrapper, opts.CaptureWindowMs, opts.MaxDiskBytes, notifier, logger)
	kioskReplay := NewKioskReplay(replayer, store, opts.KioskCDPEndpoint, httpClient, dialer, jsonWrapper, ioWrapper, clockWrapper, logger)

	return Runtime{
		Service: service, KioskReplay: kioskReplay, ScopeLost: scopeLost,
		StaticServer: staticServer, Notifier: notifier,
	}
}

// safeHeadlessDebugPort defends against offlineCache.headlessDebugPort
// being accidentally configured to the same port as the kiosk's own CDP
// endpoint (kioskCDPEndpoint, always the live config.CDPConfig.Endpoint
// — see Options.KioskCDPEndpoint's doc). The two Chromium processes this
// package deliberately keeps separate (headless capture vs. the kiosk
// player surface — see Downloader's doc) are told apart ONLY by which
// port each one's DevTools endpoint answers on. If the two ports
// collided, Downloader.Acquire's readiness probe would succeed against
// the ALREADY-RUNNING kiosk endpoint instead of a freshly spawned
// headless process, and capture would then discover and navigate the
// kiosk's own live page target — corrupting whatever is currently on
// screen for the viewer. Ports are compared numerically (not as raw
// strings) so formatting differences in the endpoint text never mask a
// real collision. A colliding configured port is coerced to a port
// guaranteed distinct from the kiosk's — starting from
// DefaultHeadlessDebugPort but stepping past it if that default itself
// happens to equal the (unusually) configured kiosk port, since a fixed
// single fallback would otherwise just relocate the same hazard rather
// than closing it — and logged at Error (mirroring safeLoopbackAddr's
// staticServerAddr guard) rather than merely documented, since this is a
// startup-time daemon misconfiguration, not something a caller can react
// to at the point of use.
func safeHeadlessDebugPort(configured int, kioskCDPEndpoint string, logger *zap.Logger) int {
	kioskPort, ok := cdpEndpointPort(kioskCDPEndpoint)
	if !ok || configured != kioskPort {
		return configured
	}
	forced := DefaultHeadlessDebugPort
	for forced == kioskPort {
		forced++
	}
	logger.Error("offline cache: headlessDebugPort collides with the kiosk CDP endpoint's port, forcing a non-colliding headless port to prevent capture from attaching to the live kiosk Chromium",
		zap.Int("configured", configured), zap.String("kiosk_cdp_endpoint", kioskCDPEndpoint), zap.Int("forced", forced))
	return forced
}

// cdpEndpointPort extracts the numeric port from a CDP HTTP endpoint such
// as "http://127.0.0.1:9222", returning ok=false if endpoint does not
// parse or carries no explicit numeric port (safeHeadlessDebugPort then
// skips the collision check rather than guessing).
func cdpEndpointPort(endpoint string) (port int, ok bool) {
	u, err := go_url.Parse(endpoint)
	if err != nil {
		return 0, false
	}
	portStr := u.Port()
	if portStr == "" {
		return 0, false
	}
	port, err = strconv.Atoi(portStr)
	if err != nil {
		return 0, false
	}
	return port, true
}
