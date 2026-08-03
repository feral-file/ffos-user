package offlinecache

import (
	"fmt"
	"net"
	go_url "net/url"
	"runtime"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
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
	// ResourceGate configures the admission gate in front of the capture
	// worker (see admission.go). Enabled defaults ON via OptionsFromConfig:
	// the gate protects the kiosk Chromium from capture-induced pressure,
	// so it must be effective with zero configuration.
	ResourceGate ResourceGateOptions
	// HeadlessLimits caps the capture Chromium's CPU/memory via a
	// transient systemd scope (see HeadlessLimits in downloader.go). Also
	// defaults ON: it bounds the in-flight-capture window the admission
	// gate cannot cover.
	HeadlessLimits HeadlessLimits
	// HeadlessLimitsWarning is non-empty when OptionsFromConfig had to
	// correct an internally inconsistent HeadlessLimits, or found one it
	// deliberately does NOT correct but that will not apply as written
	// (see alignHeadlessLimits, which covers both cases). Bootstrap logs
	// it; carried as data so the mapping stays a pure, testable function.
	HeadlessLimitsWarning string
	// ResourceGateWarning is non-empty when the resource-gate config
	// carries a setting that no longer does anything. Kept as its own
	// named field rather than folded into a generic slice for the same
	// reason HeadlessLimitsWarning is: the name says which subsystem is
	// complaining. A third one is the signal to generalize.
	ResourceGateWarning string
}

// ResourceGateOptions is the fully-defaulted admission-gate slice of
// Options, mirroring the parent struct's convention.
type ResourceGateOptions struct {
	Enabled bool
	Policy  AdmissionPolicy
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
		ResourceGate: ResourceGateOptions{
			Enabled: true,
			Policy: AdmissionPolicy{
				SoftwareBlockMemoryPercent: DefaultSoftwareBlockMemoryPercent,
				SoftwareBlockCPUTempC:      DefaultSoftwareBlockCPUTempC,
				MediaBlockMemoryPercent:    DefaultMediaBlockMemoryPercent,
				MediaBlockCPUTempC:         DefaultMediaBlockCPUTempC,
				MetricsStaleAfter:          DefaultMetricsStaleAfter,
				MemorySafetyCeilingPercent: DefaultMemorySafetyCeilingPercent,
			},
		},
		HeadlessLimits: defaultHeadlessLimits(),
	}
	if cfg == nil {
		// Still finalize: the cross-setting couplings must hold on every
		// path out of this function, not only the configured one.
		return finalizeOptions(opts)
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
	if rg := cfg.ResourceGate; rg != nil {
		opts.ResourceGate.Enabled = !rg.Disabled
		if rg.SoftwareMaxMemoryPercent > 0 {
			opts.ResourceGate.Policy.SoftwareBlockMemoryPercent = rg.SoftwareMaxMemoryPercent
		}
		if rg.SoftwareMaxCPUTempC > 0 {
			opts.ResourceGate.Policy.SoftwareBlockCPUTempC = rg.SoftwareMaxCPUTempC
		}
		if rg.MediaMaxMemoryPercent > 0 {
			opts.ResourceGate.Policy.MediaBlockMemoryPercent = rg.MediaMaxMemoryPercent
		}
		if rg.MediaMaxCPUTempC > 0 {
			opts.ResourceGate.Policy.MediaBlockCPUTempC = rg.MediaMaxCPUTempC
		}
		if rg.MemorySafetyCeilingPercent > 0 {
			opts.ResourceGate.Policy.MemorySafetyCeilingPercent = rg.MemorySafetyCeilingPercent
		}
		if rg.MetricsStaleAfterSeconds > 0 {
			opts.ResourceGate.Policy.MetricsStaleAfter = time.Duration(rg.MetricsStaleAfterSeconds) * time.Second
		}
		if rg.MaxDeferSeconds > 0 {
			// Honoring this silently is exactly the class of quiet
			// inertness this package now refuses to ship: the operator
			// asked for a deferral deadline and there is no longer any
			// such thing (see dequeueAdmitted — a deferred download waits
			// for the device instead of failing on a timer).
			opts.ResourceGateWarning = fmt.Sprintf(
				"offlineCache.resourceGate.maxDeferSeconds (%d) is no longer honored: a deferred download now waits for the device to recover instead of failing on a deadline, so downloads are never dropped from the queue except by an explicit clear",
				rg.MaxDeferSeconds)
		}
	}
	if hl := cfg.HeadlessLimits; hl != nil {
		opts.HeadlessLimits.Enabled = !hl.Disabled
		if hl.CPUQuotaPercent > 0 {
			opts.HeadlessLimits.CPUQuotaPercent = hl.CPUQuotaPercent
		}
		if hl.AllowedCPUs != "" {
			opts.HeadlessLimits.AllowedCPUs = hl.AllowedCPUs
		}
		if hl.MemoryMaxBytes > 0 {
			opts.HeadlessLimits.MemoryMaxBytes = hl.MemoryMaxBytes
		}
	}
	return finalizeOptions(opts)
}

// finalizeOptions applies the couplings BETWEEN sections, after each has
// been resolved from config. Kept as the single exit point of
// OptionsFromConfig so a future early return cannot silently skip them
// and leave the resource gate and the headless cap independently tuned.
func finalizeOptions(opts Options) Options {
	opts.HeadlessLimits, opts.HeadlessLimitsWarning = alignHeadlessLimits(opts.HeadlessLimits)
	// Couple the admission gate to the cap the capture actually runs
	// under, so the two are never independently-tuned numbers that only
	// happen to agree — see AdmissionPolicy.SoftwareReserveBytes. A
	// disabled cap leaves the reserve at 0, which drops the derived term
	// and leaves the static threshold in charge.
	if opts.HeadlessLimits.Enabled {
		opts.ResourceGate.Policy.SoftwareReserveBytes = opts.HeadlessLimits.MemoryMaxBytes
	}
	return opts
}

// alignHeadlessLimits enforces the one internal consistency rule the two
// CPU properties have: CPUQuota can never exceed what AllowedCPUs is able
// to deliver. `AllowedCPUs=0-1` with `CPUQuota=300%` is not a 3-CPU
// budget — the cpuset physically caps it at 200%, so the quota silently
// stops being a limit at all and the operator's intent ("cap CPU at
// 300%") is not what runs. Clamping makes the effective value match what
// the kernel will actually enforce, and returns a warning string the
// caller logs (this function stays pure so OptionsFromConfig's tests can
// assert the mapping).
func alignHeadlessLimits(l HeadlessLimits) (HeadlessLimits, string) {
	if !l.Enabled {
		return l, ""
	}
	allowed := countAllowedCPUs(l.AllowedCPUs)
	// A spec that is present but unparseable is a CONFIGURED pin that will
	// silently apply nowhere — the same class of quiet inertness this whole
	// area exists to make loud. It is easy to hit by accident because
	// systemd's AllowedCPUs= accepts whitespace-separated forms that
	// taskset -c rejects, so an operator can copy a legal systemd value
	// straight into config and lose the pin without a word. Reported here
	// rather than left to a Bool field on a later Info line.
	if allowed <= 0 && strings.TrimSpace(l.AllowedCPUs) != "" {
		return l, fmt.Sprintf(
			"offlineCache.headlessLimits.allowedCpus %q is not a CPU list taskset can apply (systemd accepts whitespace-separated forms that taskset -c rejects); the capture chromium will run WITHOUT a cpu pin",
			l.AllowedCPUs)
	}
	if l.CPUQuotaPercent <= 0 || allowed <= 0 {
		return l, "" // no pin: the quota alone governs
	}
	if maxQuota := allowed * 100; l.CPUQuotaPercent > maxQuota {
		warning := fmt.Sprintf(
			"offlineCache.headlessLimits.cpuQuotaPercent (%d%%) exceeds what allowedCpus %q can deliver (%d CPUs = %d%%); clamped to %d%%",
			l.CPUQuotaPercent, l.AllowedCPUs, allowed, maxQuota, maxQuota)
		l.CPUQuotaPercent = maxQuota
		return l, warning
	}
	return l, ""
}

// maxPlausibleCPUID bounds a CPU id in an AllowedCPUs/taskset list. Well
// above any machine this ships on, and it keeps a hostile or fat-fingered
// "0-9999999999" from materializing a set large enough to matter.
const maxPlausibleCPUID = 4095

// countAllowedCPUs reports how many DISTINCT CPUs a systemd AllowedCPUs /
// `taskset -c` list selects, or 0 when the spec is one taskset cannot
// apply. Callers treat 0 as "no pin" (see alignHeadlessLimits and
// captureWrapperArgv).
//
// DISTINCT, not the sum of segment widths. taskset accepts overlapping and
// repeated segments and simply unions them — verified: `0-3,2-5` pins six
// CPUs (0-5), not eight, and `0,0,1` pins two, not three. Summing widths
// overstates what the pin can deliver, which would let alignHeadlessLimits
// admit a CPUQuota above it; the quota then silently stops being a limit,
// which is the exact failure that clamp exists to prevent.
//
// Empty segments are MALFORMED, not skippable: `taskset -c "0-3,,4"` is
// rejected outright, so counting it as five would hand the spawn a spec
// that cannot exec — and the whole wrapper would then fall back, costing
// the pin. Returning 0 routes it to the configured-but-unusable warning
// instead.
func countAllowedCPUs(spec string) int {
	if strings.TrimSpace(spec) == "" {
		return 0
	}
	cpus := make(map[int]struct{})
	for part := range strings.SplitSeq(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			return 0
		}
		lo, hi, isRange := strings.Cut(part, "-")
		start, err := strconv.Atoi(strings.TrimSpace(lo))
		if err != nil || start < 0 {
			return 0
		}
		end := start
		if isRange {
			if end, err = strconv.Atoi(strings.TrimSpace(hi)); err != nil || end < start {
				return 0
			}
		}
		if end > maxPlausibleCPUID {
			return 0
		}
		for id := start; id <= end; id++ {
			cpus[id] = struct{}{}
		}
	}
	return len(cpus)
}

// defaultHeadlessLimits derives the CPU pin and the CPU quota TOGETHER so
// they are aligned by construction on whatever machine this runs on (see
// DefaultHeadlessCPUShareOfPinned for why a fixed quota cannot be).
//
// The pin is the first quarter of the machine's logical CPUs, at least
// two — SwiftShader on a single CPU would starve page load inside the
// capture windows and turn the cap into a fidelity regression. Pinning is
// what bounds worst-case package power draw no matter what the captured
// artwork's code does; a CPUQuota alone still lets short bursts light up
// every core's boost clocks, which is the spike that can brown out the
// whole device. On the 16-thread target this yields "0-3" @ 300%.
//
// That hazard is not theoretical: an FF1 hard-reset the instant a capture
// started, with the pin turning out to be applying nowhere. Whether the
// draw was the mechanism is unproven, but the pin is the defense either
// way — and it is therefore enforced by taskset rather than by the systemd
// AllowedCPUs= property, which is inert here (see captureWrapperArgv).
func defaultHeadlessLimits() HeadlessLimits {
	total := runtime.NumCPU()
	pinned := max(2, total/4)
	if total > 0 && pinned > total {
		pinned = total
	}
	// Floor at one full CPU: below that the quota, not the artwork, would
	// be what fails captures.
	quota := max(100, int(float64(pinned)*100*DefaultHeadlessCPUShareOfPinned))
	return HeadlessLimits{
		Enabled:         true,
		CPUQuotaPercent: quota,
		AllowedCPUs:     fmt.Sprintf("0-%d", pinned-1),
		MemoryMaxBytes:  DefaultHeadlessMemoryMaxBytes,
	}
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
	// Notifier is exposed so main.go can stop its background WS-delivery
	// worker at shutdown (via CloseWithin — see its doc) — the
	// Service itself only holds it through the narrower ProgressObserver
	// interface (OnItemStateChanged only), which has no Close method, so
	// Service cannot own this shutdown step itself.
	Notifier *Notifier
	// SysMetricsSink, when non-nil (the admission gate is enabled), is
	// main.go's hook for feeding monitord's raw sysmetrics payloads into
	// the gate — wired to mediator.SetSysMetricsObserver so the mediator
	// stays policy-free forwarding glue and never imports an offlinecache
	// type for it. The sink runs inline on the mediator's D-Bus
	// signal-dispatch goroutine: it must not block, and the gate's Observe
	// honors that by only decoding and storing.
	SysMetricsSink func(raw []byte)
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
	if opts.HeadlessLimitsWarning != "" {
		logger.Warn("offline cache: " + opts.HeadlessLimitsWarning)
	}
	if opts.ResourceGateWarning != "" {
		logger.Warn("offline cache: " + opts.ResourceGateWarning)
	}
	store := NewStore(opts.RootDir, osWrapper, jsonWrapper, logger)
	// net.DefaultResolver honors the system resolver configuration
	// (systemd-resolved on this device), so the guard's view of a name
	// matches what the capture paths will actually dial moments later —
	// a private resolver here would make the check answer a different
	// question than the one that matters. See ErrUnsafeSource.
	classifier := NewClassifier(httpClient, net.DefaultResolver)
	headlessDebugPort := safeHeadlessDebugPort(opts.HeadlessDebugPort, opts.KioskCDPEndpoint, logger)
	downloader := NewDownloader(
		opts.HeadlessBinaryPath, opts.HeadlessUserDataDir, headlessDebugPort,
		opts.HeadlessIdleTeardown, opts.HeadlessLimits, execWrapper, osWrapper, clockWrapper, httpClient, logger,
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
	mediaCapturer := NewMediaCapturer(bodyClient, store, jsonWrapper, clockWrapper, opts.MaxDiskBytes, logger)
	staticServer := NewStaticServer(opts.StaticServerAddr, store, osWrapper, logger)
	replayer := NewReplayer(store, staticServer, opts.MissPolicy, constants.WEBAPP_URL, jsonWrapper, logger)
	// The concrete replayer implements the scope-lost setter, but Replayer
	// deliberately does not declare it (see ScopeLostRegistrar). Asserted
	// once here rather than exposing a wider interface from Bootstrap; a
	// failed assertion leaves Runtime.ScopeLost nil, which main.go's guard
	// treats as "no prompt recovery wired", not as a startup failure.
	scopeLost, _ := replayer.(ScopeLostRegistrar)
	notifier := NewNotifier(relayerClient, wsHandler, logger)
	admissionOpts := AdmissionOptions{Clock: clockWrapper}
	var sysMetricsSink func(raw []byte)
	if opts.ResourceGate.Enabled {
		gate := NewAdmissionGate(opts.ResourceGate.Policy, clockWrapper, jsonWrapper, logger)
		admissionOpts.Controller = gate
		sysMetricsSink = gate.Observe
	}
	service := NewService(store, classifier, capturer, mediaCapturer, jsonWrapper, opts.CaptureWindowMs, opts.MaxDiskBytes, notifier, admissionOpts, logger)
	kioskReplay := NewKioskReplay(replayer, store, opts.KioskCDPEndpoint, httpClient, dialer, jsonWrapper, ioWrapper, clockWrapper, logger)

	return Runtime{
		Service: service, KioskReplay: kioskReplay, ScopeLost: scopeLost,
		StaticServer: staticServer, Notifier: notifier, SysMetricsSink: sysMetricsSink,
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
