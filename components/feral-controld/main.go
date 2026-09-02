package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	go_os "os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	go_daemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/feral-file/godbus"
	"github.com/getsentry/sentry-go"
	dbus_v5 "github.com/godbus/dbus/v5"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/devicename"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mdns"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/netlog"
	"github.com/feral-file/ffos-user/components/feral-controld/netmetrics"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	oomrecovery "github.com/feral-file/ffos-user/components/feral-controld/oom-recovery"
	"github.com/feral-file/ffos-user/components/feral-controld/playersession"
	playlist_refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/uarewrite"
	"github.com/feral-file/ffos-user/components/feral-controld/watchdog"
	"github.com/feral-file/ffos-user/components/feral-controld/wifictl"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

const (
	SHUTDOWN_TIMEOUT = 2 * time.Second
)

var (
	debug = false
)

// boundGateForSourceProbe caps configured command capacity using the current
// displayPlaylist policy, rather than assuming DefaultGateConfig's weight.
// The prober also owns a process-wide slot budget; this cap keeps an enabled
// gate from admitting work that can only queue behind that hard bound.
func boundGateForSourceProbe(gateCfg commandrouter.GateConfig, logger *zap.Logger) commandrouter.GateConfig {
	if !gateCfg.Enabled || gateCfg.MaxConcurrent <= 0 {
		return gateCfg
	}

	castPolicy, ok := gateCfg.Policies[commands.CMD_DISPLAY_PLAYLIST]
	if !ok {
		castPolicy = gateCfg.Default
	}
	castWeight := castPolicy.Weight
	if castWeight < 1 {
		castWeight = 1
	}
	maxConcurrent := int64(offlinecache.MaxConcurrentSourceProbeCasts) * castWeight
	if gateCfg.MaxConcurrent <= maxConcurrent {
		return gateCfg
	}

	logger.Warn("command storm maxConcurrent capped by source-probe header budget",
		zap.Int64("configured", gateCfg.MaxConcurrent),
		zap.Int64("effective", maxConcurrent),
		zap.Int64("displayPlaylistWeight", castWeight),
	)
	gateCfg.MaxConcurrent = maxConcurrent
	return gateCfg
}

type app struct {
	// Basic components
	Ctx context.Context
	// Cancel cancels Ctx. Long-lived components built in initializeApp (the hub
	// shutdown watcher, the claim flow's claimCtx, the playlist refresher) hold
	// Ctx directly, so shutdown must cancel at this root — canceling a child
	// context derived later would leave those paths running on a live context.
	// Nil in the test app, which passes its own ctx.
	Cancel context.CancelFunc
	Logger *zap.Logger

	// Wrappers
	Clock      wrapper.Clock
	OS         wrapper.OS
	Signal     wrapper.Signal
	Daemon     wrapper.Daemon
	HTTPClient wrapper.HTTPClient
	IO         wrapper.IO
	JSON       wrapper.JSON
	Random     wrapper.Randomizer
	Exec       wrapper.Exec
	Math       wrapper.Math

	// Components
	CDP               cdp.CDP
	Relayer           relayer.Relayer
	DBus              dbus.DBus
	Mediator          mediator.Mediator
	OOMRecoverer      oomrecovery.Recoverer
	Executor          devicectl.Executor
	DeviceStatus      status.DeviceStatus
	StatusPoller      status.Poller
	Watchdog          watchdog.Watchdog
	PlaylistRefresher playlist_refresher.Refresher
	PlaylistScheduler playlistschedule.Scheduler
	MintPairing       mintpairing.Service
	// KioskReplay, OfflineCacheService, and OfflineCacheStaticServer may
	// all be nil (feature disabled via config.OfflineCacheConfig).
	KioskReplay              offlinecache.KioskReplay
	OfflineCacheService      offlinecache.Service
	OfflineCacheStaticServer offlinecache.StaticServer
	// OfflineCacheNotifier's shutdown (its background WS-delivery worker,
	// stopped via CloseWithin — see run's defer) is driven directly here
	// rather than through OfflineCacheService: Service only holds it via
	// the narrower ProgressObserver interface, which has no Close method —
	// see offlinecache.Runtime.Notifier's doc.
	OfflineCacheNotifier *offlinecache.Notifier
	// UARewrite rewrites the outgoing User-Agent for the configured
	// artwork origins (#296). Independent of the offline cache above: the
	// bug it fixes has nothing to do with caching, so it must run on a
	// device with offlineCache off. Nil only when explicitly disabled via
	// gatewayUserAgent.enabled=false, or when its policy failed to build.
	UARewrite   *uarewrite.Interceptor
	Hub         hub.Hub
	LinkChecker *status.LinkChecker
	// Netlog is the WAN-outage flight recorder; nil when disabled by config,
	// unavailable (ring open failed), or in the test app.
	Netlog *netlog.Recorder

	// StateLoadKnown records whether run()'s state.Load succeeded (false =
	// the persisted state was unreadable and quarantined). The provisioning
	// machine's boot claim seed reads it (see InitialClaimed): a corrupt state
	// file must read as claim-UNKNOWN (= claimed, the fail-safe), not as the
	// empty state's "unclaimed". Atomic because run() writes it after
	// initializeApp built the closure that reads it.
	StateLoadKnown *atomic.Bool

	// Provisioning is the setup-AP trigger state machine. run() starts it
	// unconditionally; left nil in the test app. Typed as an interface so tests
	// can inject an ordering spy.
	Provisioning provisioningRunner
	// SetupUI is the on-screen setup-narration surface driven by the provisioning
	// domain. run() re-pushes its last state (Resync) when CDP (re)connects so a
	// late-loading player catches up. Nil in the test app.
	SetupUI *setupui.Service
	// Session is the cross-cutting page-generation/readiness/navigation
	// authority (design doc §2). run() bumps its generation on every CDP
	// (re)connect (Session.OnConnect); every off-lane producer that used to
	// run its own ad-hoc CDP-reconnect resync (sleep invalidation, playlist
	// recompute, status force-refresh, setup narration resync, connectivity
	// re-seed) is registered as a reconciler on it instead, at composition
	// time in initializeApp. Nil in the test app.
	Session *playersession.Session
}

func main() {
	// Read from options
	flag.BoolVar(&debug, "debug", false, "Enable debug mode")
	flag.Parse()

	// Initialize basic logger first for config loading
	basicLogger, err := logger.New(debug)
	if err != nil {
		fmt.Fprintf(go_os.Stderr, "Failed to initialize logger: %s\n", err)
		go_os.Exit(1)
	}
	defer func() {
		_ = basicLogger.Sync()
	}()

	// Load configuration
	config, err := config.Load(basicLogger)
	if err != nil {
		basicLogger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Create the final logger (with Sentry if configured)
	finalLogger := basicLogger
	if config.SentryConfig.IsEnabled() {
		sentryLogger, err := logger.AddSentry(finalLogger, *config.SentryConfig)
		if err != nil {
			finalLogger.Error("Failed to create Sentry-integrated logger, falling back to basic logger", zap.Error(err))
		} else {
			finalLogger = sentryLogger
			finalLogger.Info("Sentry initialized successfully",
				zap.String("environment", config.SentryConfig.Environment),
				zap.String("release", config.SentryConfig.Release))
			defer logger.FlushSentry(2 * time.Second)
		}
	} else {
		finalLogger.Info("Sentry not configured, using basic logger")
	}

	// Initialize app
	app := initializeApp(
		finalLogger,
		config.CDPConfig.Endpoint,
		config.RelayerConfig.Endpoint,
		config.RelayerConfig.APIKey,
		config.MintPairingConfig,
		config.OfflineCache,
		config.GatewayUserAgentTuning(finalLogger),
		config.Netlog,
		provisioningTuningFromConfig(config.ProvisioningTuning(finalLogger), finalLogger),
		dbus.NAME,
		[]dbus_v5.MatchOption{
			dbus_v5.WithMatchPathNamespace(dbus_v5.ObjectPath("/com/feralfile")),
		})

	// Graceful shutdown cancels the app-lifetime context created in
	// initializeApp (see app.Cancel).
	ctx, cancel := app.Ctx, app.Cancel
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan go_os.Signal, 1)
	app.Signal.Notify(sigCh, go_os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		app.Logger.Info("Received signal, initiating shutdown...",
			zap.String("signal", sig.String()))
		cancel()

		app.Clock.Sleep(SHUTDOWN_TIMEOUT)
		app.Logger.Error("Shutdown timed out, forcing exit...",
			zap.Duration("timeout", SHUTDOWN_TIMEOUT))

		if config.SentryConfig.IsEnabled() {
			sentry.Flush(1 * time.Second)
		}

		app.OS.Exit(1)
	}()

	// Run the app
	err = app.run(ctx, config)
	if err != nil {
		app.Logger.Fatal("Failed to run app", zap.Error(err))
	}
}

func (app *app) run(ctx context.Context, conf *config.Config) error {
	// Load state. A load failure must NOT abort startup: controld is the sole
	// SoftAP/LAN-recovery owner, so returning here would crash-loop the daemon
	// (Restart=always with no start limit) and strand an offline device with
	// no setup AP and no LAN hub — the exact unrecoverable state this daemon
	// exists to prevent. Quarantine the unreadable file (best-effort rename,
	// preserving the bytes for diagnosis instead of overwriting them) and
	// continue on a fresh empty state: the worst case is the device presents
	// as unclaimed and re-pairs, which the claim flow handles.
	// The returned *State is intentionally unused below: every downstream
	// read of ConnectedDevice/Relayer.TopicID goes through
	// state.ClaimSnapshot() instead, which reads the SAME in-memory state
	// Load() just installed but under the state package's lock. Load()
	// itself still must run — it is what populates the global manager's
	// in-memory state from disk before that first ClaimSnapshot() call.
	if _, err := state.Load(app.Logger); err != nil {
		app.Logger.Error("Failed to load persisted state; quarantining it and continuing with empty state", zap.Error(err))
		if rerr := app.OS.Rename(constants.STATE_FILE, constants.STATE_FILE+".corrupt"); rerr != nil {
			app.Logger.Warn("Failed to quarantine state file", zap.Error(rerr))
		}
		state.GetState() // installs a fresh empty state
	} else if app.StateLoadKnown != nil {
		// A SUCCESSFUL load makes the claim reading known; the quarantine
		// branch above deliberately leaves it unknown, so the provisioning
		// machine's boot claim seed reads the empty state as
		// claim-UNKNOWN = claimed (constraint 8's fail-safe) rather than as
		// unclaimed — the one consumer for which "present as unclaimed and
		// re-pair" is NOT acceptable, because it would authorize an automatic
		// setup-AP raise over a possibly claimed exhibition frame.
		app.StateLoadKnown.Store(true)
	}

	// Set global topic ID in Sentry if available.
	if claim := state.ClaimSnapshot(); conf.SentryConfig.IsEnabled() && claim.TopicID != "" {
		logger.SetGlobalTopicID(claim.TopicID)
	}

	// Start watchdog
	app.Watchdog.Start(ctx)
	defer app.Watchdog.Stop()

	// Initialize DBus client. NON-FATAL: controld is the sole SoftAP/LAN
	// recovery owner, and D-Bus startup can fail transiently — the session bus
	// socket may not be up yet at boot, or the com.feralfile.controld name may
	// still be held by a dying predecessor during a restart race. Returning
	// here crash-looped the daemon before the hub or provisioning ever
	// started, leaving an offline device with NEITHER recovery surface.
	// Continue startup and retry in the background instead: every D-Bus
	// consumer degrades gracefully until a retry succeeds (Call errors →
	// provisioning's connUnknown discipline assumes offline and keeps
	// re-querying, so the setup AP still raises; handler registrations are
	// recorded by the Restartable wrapper and replayed onto the client that
	// finally starts).
	//
	// Restart safety lives in dbus.Restartable, not here: each retry builds a
	// FRESH underlying godbus client, started before publication, so retries
	// never race live Call/Stop and never accumulate signal registrations on
	// the shared bus (failed attempts are torn down with their connection).
	// See dbus/restartable.go for the godbus hazards this design closes.
	if err := app.DBus.Start(); err != nil {
		app.Logger.Error("DBus start failed; continuing startup and retrying in background", zap.Error(err))
		go func() {
			backoff := time.Second
			for {
				if serr := app.Clock.SleepContext(ctx, backoff); serr != nil {
					return
				}
				if serr := app.DBus.Start(); serr == nil {
					app.Logger.Info("DBus client started after retry")
					return
				} else { //nolint:revive // keep the retry outcome branches adjacent
					app.Logger.Warn("DBus start retry failed", zap.Error(serr), zap.Duration("backoff", backoff))
				}
				if backoff *= 2; backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
		}()
	}
	defer func() {
		_ = app.DBus.Stop()
	}()

	// Initialize Mediator
	app.Mediator.Start()
	defer app.Mediator.Stop()

	// Start the WAN-outage flight recorder early — an outage in progress at
	// boot is exactly the MoMA case it exists for — and independent of the
	// hub: the ring must fill even when nothing serves metrics.
	if app.Netlog != nil {
		app.Netlog.Start(ctx)
		defer app.Netlog.Stop()
	}

	// P2.5 startup ordering: the local recovery + setup surfaces (hub + mDNS, then
	// the provisioning domain) come up BEFORE the relayer/CDP init below and never
	// wait on them. The relayer connection is a best-effort, never-fatal step (see
	// the gate further down); bringing the LAN hub and the SoftAP setup path up
	// first means a device that cannot reach the relayer at all can still be
	// recovered over the LAN and can still raise its setup AP. Nothing here depends
	// on the relayer or CDP being up.

	// Start Hub if enabled. The hub listener stays bound unconditionally (it is
	// the BLE-replacement LAN recovery channel), while mDNS *discoverability* is
	// keyed on link state inside the mediator — see InitializeMDNS.
	if conf.HubEnabled() {
		app.Hub.Start()
		defer func() {
			if err := app.Hub.Stop(); err != nil {
				app.Logger.Warn("Failed to stop hub", zap.Error(err))
			}
		}()

		// Stage-0 network telemetry (docs/wan-outage-observability.md): the
		// poller writes the cache-only link/Wi-Fi gauges the hub's /metrics
		// serves and vmagent spools. Keyed on the hub because the hub is the
		// only thing that serves them; the relayer gauges are fed event-driven
		// by the relayer itself, poller or not. The LinkChecker guard is for
		// the test app (initializeTestApp leaves the seam nil and injects a
		// bare MockExec the poller's nmcli ticks would trip); production
		// wiring always sets it.
		if app.LinkChecker != nil {
			netPoller := netmetrics.NewPoller(app.LinkChecker, app.Exec, app.Clock, app.Logger)
			netPoller.Start(ctx)
			defer netPoller.Stop()
		}

		claim := state.ClaimSnapshot()
		deviceInfo := resolveMDNSDeviceInfo(app.OS, app.JSON, claim, app.Logger)
		deviceInfo.Claimed = claim.Claimed
		advertiser := mdns.New(app.Logger)
		defer advertiser.Stop()

		// Key mDNS on link state, not the internet-reachability `connected` flag.
		app.Mediator.InitializeMDNS(advertiser, deviceInfo, app.LinkChecker)
	}

	// Start the provisioning (setup-AP) domain. controld owns device setup, so the
	// machine runs its own supervised event loop; its startup cannot be aborted by
	// a relayer or CDP failure below (both come after this point). app.Provisioning
	// is nil in the test app; guard for it.
	if app.Provisioning != nil {
		app.Logger.Info("Starting provisioning domain")
		app.Provisioning.Start(ctx)
		defer app.Provisioning.Stop()
	}

	// Get connectivity status and connect to relayer if ready. This gate is a
	// one-shot LATENCY fast path, not the thing that guarantees a relayer
	// connection: its connectivity snapshot can be wrong-and-final (taken
	// while D-Bus is still down on an already-online network, where no
	// connectivity TRANSITION will ever fire to correct it). Durability comes
	// from the mediator, which reconciles the relayer connection against
	// connectivity on every periodic SYSMETRICS heartbeat — see
	// mediator.reconcileRelayer.
	connected, err := getConnectivityStatus(ctx, app.DBus, app.Logger)
	if err != nil {
		app.Logger.Error("Failed to get connectivity status", zap.Error(err))
	} else {
		app.Logger.Info("Connectivity status", zap.Bool("connected", connected))
	}
	// Single snapshot: relayer_ready and topic_id must come from the SAME
	// write, not two separate GetState() field reads racing a concurrent
	// claim or topic assignment.
	startupClaim := state.ClaimSnapshot()
	app.Logger.Info("Initial relayer connection gate evaluated",
		zap.Bool("internet_connected", connected),
		zap.Bool("relayer_ready", startupClaim.TopicReady),
		zap.String("topic_id", startupClaim.TopicID),
	)
	// Gate on reachability ONLY, never on IsReady(): connecting with an empty
	// topic is the designed topic-ASSIGNMENT path (relayer.Connect omits the
	// topicID query param and the server answers with a MESSAGE_ID_SYSTEM
	// carrying the assigned topic, which the mediator persists). Gating on
	// IsReady() stranded factory-fresh devices that boot already online: no
	// connectivity CHANGE event ever fires on them, so the mediator's
	// restore handler never runs either, no topic is ever assigned, and the
	// auto-claim flow times out waiting for one.
	if connected {
		app.Logger.Info("Connecting relayer during startup")
		err = app.Relayer.Connect(ctx)
		if err != nil {
			// Never fatal: returning here would exit before SdNotifyReady, so a relayer
			// outage would crash-loop this daemon and take the in-process SoftAP
			// provisioning and LAN recovery down with it — the exact coupling the
			// unconditional-start model exists to remove (.start-services.sh starts us
			// --no-block precisely because pre-READY failure can happen). Retry in the
			// background instead; the mediator's connectivity-change handler and
			// heartbeat reconcile also re-attempt the connection, and
			// RetryableConnect tolerates racing them (ErrAlreadyConnected is
			// success).
			app.Logger.Error("Failed initial relayer connection, retrying in background", zap.Error(err))
			go func() {
				if retryErr := app.Relayer.RetryableConnect(ctx); retryErr != nil {
					app.Logger.Error("Background relayer connection retry gave up", zap.Error(retryErr))
				}
			}()
		} else {
			app.Logger.Info("Initial relayer connection established")
		}
	} else {
		app.Logger.Info("Skipping initial relayer connection",
			zap.Bool("internet_connected", connected),
			zap.Bool("relayer_ready", state.ClaimSnapshot().TopicReady),
		)
	}
	// Close unconditionally: a connection can exist at shutdown regardless of
	// this gate's snapshot — the background retry, the mediator's
	// connectivity-change handler, or its heartbeat reconcile may have
	// established one — and Close is a no-op on a nil conn.
	defer app.Relayer.Close()

	// Register scheduler Stop before refresher Stop so LIFO shutdown stops the
	// refresher first — otherwise a late Prepare could re-arm a displayAt
	// timer after the scheduler already canceled timers on Stop.
	if app.PlaylistScheduler != nil {
		defer app.PlaylistScheduler.Stop()
	}

	// Start Playlist Refresher
	app.PlaylistRefresher.Start()
	defer app.PlaylistRefresher.Stop()

	if app.MintPairing != nil {
		app.MintPairing.Start(ctx)
		defer app.MintPairing.Stop()
	}

	// Start offline cache (job-queue worker + loopback static server for
	// large cached assets) when config.OfflineCacheConfig.Enabled. Both
	// are best-effort: a start failure here must not crash controld, since
	// the daemon's core playback/command path never depended on this
	// feature before it existed.
	if app.OfflineCacheService != nil {
		// OfflineCacheNotifier's shutdown is deferred BEFORE
		// OfflineCacheService.Stop below (registered first), so Go's
		// LIFO defer order runs Stop FIRST on shutdown: Stop's own doc
		// guarantees no further ProgressObserver callbacks (so no more
		// notifications will ever be enqueued) only once it returns.
		// Stopping the notifier's background delivery worker before that
		// would risk dropping an in-flight final notification for no
		// benefit — that worker is just an idle goroutine until it is
		// stopped anyway.
		//
		// Bounded (CloseWithin, not Close): the notifier's in-flight
		// delivery can take far longer than this whole shutdown allows on
		// either transport — see CloseWithin's doc for the two shapes —
		// so waiting it out would blow past SHUTDOWN_TIMEOUT's forced exit
		// and strand every cleanup step registered BEFORE this one (LIFO
		// again).
		//
		// This is NOT an end-to-end shutdown bound. Both transports take,
		// for their own teardown, the very mutex the abandoned delivery
		// still holds — Relayer.Close and hub.Stop's ws.Close, both later
		// in LIFO order — so the wedge just blocks at whichever comes
		// next. Only mint-pairing and the playlist refresher are
		// guaranteed to run either way. Abandoning the wait leaves at most
		// one delivery still running in a process that is exiting anyway.
		if app.OfflineCacheNotifier != nil {
			defer func() {
				if !app.OfflineCacheNotifier.CloseWithin(offlinecache.ShutdownCloseBudget) {
					app.Logger.Warn("Offline cache notifier did not stop within its shutdown budget; continuing shutdown",
						zap.Duration("budget", offlinecache.ShutdownCloseBudget))
				}
			}()
		}
		if err := app.OfflineCacheService.Start(ctx); err != nil {
			app.Logger.Error("Failed to start offline cache service", zap.Error(err))
		} else {
			defer app.OfflineCacheService.Stop()
		}
	}
	if app.OfflineCacheStaticServer != nil {
		// Listen (bind) synchronously, BEFORE Serve ever runs in the
		// background: net/http's ListenAndServe combines bind+serve
		// into one blocking call, which would only ever surface a bind
		// failure asynchronously via the goroutine's own log line,
		// after Replayer may have already started 302-redirecting large
		// cached assets to a port that either never bound (dead
		// redirect) or, worse, was claimed by some OTHER unrelated
		// loopback process (redirects silently served by the wrong
		// service). Replayer's own IsListening() check is the second
		// half of this fix — this call is what lets it observe the
		// truth. A bind failure here is still best-effort/non-fatal
		// (large-asset offline replay just becomes unavailable; every
		// other offline-cache path is unaffected), consistent with this
		// whole feature's startup posture.
		if err := app.OfflineCacheStaticServer.Listen(); err != nil {
			app.Logger.Error("Failed to bind offline cache static server; large-asset offline replay will be unavailable", zap.Error(err))
		} else {
			go func() {
				if err := app.OfflineCacheStaticServer.Serve(); err != nil {
					app.Logger.Error("Offline cache static server stopped unexpectedly", zap.Error(err))
				}
			}()
			defer func() {
				shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), SHUTDOWN_TIMEOUT)
				defer shutdownCancel()
				if err := app.OfflineCacheStaticServer.Shutdown(shutdownCtx); err != nil {
					app.Logger.Warn("Failed to shut down offline cache static server", zap.Error(err))
				}
			}()
		}
	}

	// Start StatusPoller - it will handle relayer connection status internally
	go app.StatusPoller.Start(ctx)
	defer app.StatusPoller.Stop()

	// Begin establishing CDP in the background. On a headless device Chromium's DevTools
	// endpoint (127.0.0.1:9222) may be absent at boot and appear later (monitor plugged in)
	// or drop and return (kiosk restart), so CDP must never gate daemon readiness. The
	// supervisor retries until connected and reconnects on drops. READY (below) may now
	// precede CDP-connected — that is intended.
	//
	// On every (re)connect the player web app has just (re)loaded with default (awake)
	// state, so any player-side state controld drove before the restart no longer holds.
	// Session.OnConnect bumps the page generation (design doc §2.1 source 1); every
	// off-lane producer that used to run its own ad-hoc resync here (sleep invalidation,
	// playlist recompute, status force-refresh, setup-narration resync, connectivity
	// re-seed) is registered as a reconciler on the session instead (see initializeApp),
	// so it runs once the new document's command handler is actually installed rather than
	// racing a page that has not hydrated yet.
	app.CDP.Start(ctx, func() {
		if app.Session != nil {
			app.Session.OnConnect()
		}
		// The boot player recovery state machine (design doc §5,
		// devicectl/boot_recovery.go) has two connect-edge entry points:
		// MaybeRecoverPlayerOnBootOnline arms it on the first WAN-confirmed
		// online transition (wired into the provisioning notifier, may run
		// before DevTools ever attaches) and attempts inline; this call,
		// CompletePendingBootPlayerRecovery, completes an Armed/Deferred
		// machine still parked waiting for THIS first DevTools connection —
		// a no-op on every connect with nothing parked. Own goroutine, AFTER
		// the generation bump above, for the same reason the provisioning
		// notifier hooks get one: its CDP sends must not stall the connect
		// loop's other resync work.
		if pr, ok := app.Executor.(bootPlayerRecoveryFlow); ok {
			go pr.CompletePendingBootPlayerRecovery()
		}

		// Re-attach offline-cache replay interception on every (re)connect —
		// this covers plain kiosk restarts AND OOM-recovery restarts alike,
		// since both funnel through this same reconnect loop (see
		// offlinecache.KioskReplay.AttachOnReconnect's doc). Nil whenever
		// the offline cache is switched off (offlineCache.enabled unset or
		// false), which is the default — see the Bootstrap call below.
		//
		// This ONE producer stays inline rather than becoming a reconciler
		// like the resyncs named above, because it is the only one that needs
		// no page JS: it arms Fetch interception on offlinecache's own CDP
		// socket, and it must be armed as early as possible so the reloaded
		// document's asset requests are intercepted rather than escaping to
		// the live network. Its COMPANION half — re-applying the previously
		// enabled item scope, which does need a hydrated page to resolve what
		// is playing — is the "replay-scope-resync" reconciler instead; see
		// replayScopeResyncReconciler for why splitting them is load-bearing.
		//
		// Runs AFTER the boot-recovery spawn above, not before: AttachOnReconnect
		// dials a page session synchronously (bounded, but seconds under a sick
		// kiosk) on this connect-loop goroutine, and ordering it first would
		// gate that deliberately-async recovery behind this dial.
		if app.KioskReplay != nil {
			if err := app.KioskReplay.AttachOnReconnect(ctx); err != nil {
				app.Logger.Warn("Failed to attach offline cache replay to kiosk CDP session", zap.Error(err))
			}
		}

		// Re-arm the User-Agent rewrite on the same reconnect boundary and
		// for the same reason: every kiosk restart mints a new target, so a
		// session armed once at startup would silently stop intercepting
		// after the first restart. Failure is logged, not fatal — the
		// daemon's other duties must survive an unavailable kiosk.
		if app.UARewrite != nil {
			if err := app.UARewrite.AttachOnReconnect(ctx); err != nil {
				app.Logger.Warn("Failed to arm kiosk User-Agent rewrite", zap.Error(err))
			}
		}
	})
	defer app.CDP.Close()

	// The rewrite interceptor owns a SEPARATE websocket to the kiosk plus the
	// read-pump goroutine draining it, neither of which app.CDP.Close above
	// touches. Without this the connection and that goroutine outlive run()
	// on every shutdown path, which is both a leak and a divergence from how
	// every other long-lived component here is torn down.
	//
	// Deferred after the CDP close so it runs BEFORE it (defers unwind in
	// reverse): the interceptor's session is the more derived of the two, and
	// closing it while the daemon's own CDP client is still up keeps teardown
	// ordered from the outside in.
	defer func() {
		if app.UARewrite == nil {
			return
		}
		if err := app.UARewrite.Close(); err != nil {
			app.Logger.Warn("Failed to close kiosk User-Agent rewrite session", zap.Error(err))
		}
	}()

	devicectl.StartSleepScheduleLoop(ctx, app.Executor, app.Logger)

	// send ready notification to systemd
	sent, err := app.Daemon.SdNotify(false, go_daemon.SdNotifyReady)
	if err != nil {
		app.Logger.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		app.Logger.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	app.Logger.Info("controld started successfully")

	// Check for unhandled chromium OOM kills and recover if needed.
	// The recoverer handles file I/O, polling, and command dispatch internally.
	app.OOMRecoverer.Start(ctx)

	<-ctx.Done()

	app.Logger.Info("controld shutdown completed")
	return nil
}

func resolveMDNSDeviceInfo(os wrapper.OS, json wrapper.JSON, claim state.ClaimInfo, logger *zap.Logger) mdns.DeviceInfo {
	deviceID := ""
	deviceName := ""
	hostnameBytes, err := os.ReadFile(constants.HOSTNAME_FILE)
	if err != nil {
		logger.Warn("Failed to read hostname for mDNS", zap.Error(err))
	} else {
		hostname := strings.TrimSpace(string(hostnameBytes))
		if hostname != "" {
			deviceID = hostname
			deviceName = hostname
		}
	}

	// The owner's name outranks the hostname as the DISPLAY label only; the
	// hostname remains deviceID above, so the serial stays the identity every
	// resolver keys on. An unnamed unit, an unreadable record, or a corrupt
	// one all leave deviceName as the hostname — the name is cosmetic and must
	// never be able to make a device undiscoverable.
	if record, nameErr := devicename.Load(os, json); nameErr != nil {
		logger.Warn("Failed to read device name for mDNS", zap.Error(nameErr))
	} else if record.Name != "" {
		deviceName = record.Name
	}

	if (deviceID == "" || deviceName == "") && claim.DeviceID != "" {
		logger.Warn("mDNS using connected device state for identity")
		if deviceID == "" {
			deviceID = strings.TrimSpace(claim.DeviceID)
		}
		if deviceName == "" {
			deviceName = strings.TrimSpace(claim.DeviceName)
		}
	}

	if deviceName == "" {
		deviceName = deviceID
	}

	return mdns.DeviceInfo{
		ID:   deviceID,
		Name: deviceName,
		Port: 1111,
	}
}

func getConnectivityStatus(ctx context.Context, dc dbus.DBus, logger *zap.Logger) (bool, error) {
	logger.Info("Getting connectivity status")

	deadlineCtx, cancel := context.WithTimeout(ctx, 7*time.Second)
	defer cancel()

	resp, err := dc.Call(
		deadlineCtx,
		dbus.MONITORD_NAME,
		dbus.MONITORD_PATH,
		dbus.MONITORD_INTERFACE,
		dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS,
		true,
	)
	logger.Info("Connectivity status response", zap.Any("resp", resp), zap.Error(err))
	if err != nil {
		return false, err
	}

	if len(resp) != 1 {
		return false, fmt.Errorf("expected 1 response, got %d", len(resp))
	}

	connected, ok := resp[0].(bool)
	if !ok {
		return false, fmt.Errorf("expected bool, got %T", resp[0])
	}

	return connected, nil
}

// sleepInvalidateReconciler / playlistRecomputeReconciler /
// statusForceRefreshReconciler / setupUIResyncReconciler build the
// session.RegisterReconciler closures initializeApp wires (design doc §4).
// Defined at FILE scope, not as inline closures inside initializeApp, for the
// same reason externalLinkProbe lives in provisioning_wiring.go:
// initializeApp's own local `context` variable (the daemon-lifetime ctx)
// shadows the context PACKAGE for the rest of that function, so a func
// literal written inline there cannot spell out the context.Context
// parameter type it needs.
func sleepInvalidateReconciler(executor devicectl.Executor, logger *zap.Logger) func(context.Context) {
	return func(context.Context) {
		devicectl.InvalidatePlayerSleepState(executor, logger)
	}
}

func playlistRecomputeReconciler(scheduler playlistschedule.Scheduler) func(context.Context) {
	return func(ctx context.Context) {
		scheduler.RecomputeNow(ctx)
	}
}

func statusForceRefreshReconciler(poller status.Poller) func(context.Context) {
	return func(context.Context) {
		poller.ForceRefresh()
	}
}

func setupUIResyncReconciler(ui *setupui.Service) func(context.Context) {
	return func(context.Context) {
		ui.Resync()
	}
}

// replayScopeResyncReconciler re-applies offline-cache replay's
// Fetch-interception SCOPE after a page generation change.
//
// This is the half of the reconnect resync that cannot run inline on the CDP
// connect callback next to AttachOnReconnect. AttachOnReconnect resets scope
// (a new top-level socket starts with Fetch disabled — see replayer.attachRoot)
// and deliberately does not re-apply it; something must call SyncPlaylist.
// ForceRefresh is that something, but the pass it triggers resolves what is
// playing via statusPoller.FetchPlayerStatus, which evaluates
// window.handleCDPRequest in the page. Fired inline at connect time that
// evaluate hits a document that has not hydrated yet, so the pass returns the
// status error BEFORE reaching syncReplayScopeLocked and the scope is never
// restored — leaving replay off until the next periodic pass up to
// PLAYLIST_REFRESH_INTERVAL later, which is exactly the OOM-restart window
// the resync exists to close. As a reconciler it runs once the new document
// has installed its command handler, so the status fetch can actually answer.
//
// Nothing is lost by deferring it: scope is disabled between attach and
// handler-ready either way, so assets fetched in that window miss
// interception regardless of when this runs.
//
// Known residual, deliberately accepted: the generation-ready worker gates on
// StageHandler, which proves window.handleCDPRequest is INSTALLED, not that
// the player has restored playback. If hydration installs the handler before
// the player restores its last playlist, FetchPlayerStatus answers with
// Command != displayPlaylist and the pass returns nil without syncing scope
// (refresher.go's "Player command is not display any playlist" branch),
// leaving the resync to the periodic ticker. There is no "playback restored"
// stage to wait on, so this is as tight as the current primitives allow — and
// still strictly better than firing inline at connect, where the status fetch
// could not answer at all.
func replayScopeResyncReconciler(refresher playlist_refresher.Refresher) func(context.Context) {
	return func(context.Context) {
		refresher.ForceRefresh()
	}
}

func bootRecoveryRetryReconciler(executor devicectl.Executor, logger *zap.Logger) func(context.Context) {
	return func(context.Context) {
		devicectl.RetryBootRecovery(executor, logger)
	}
}

// onAwakeHook composes the displayAt recompute with the boot recovery
// early-re-entry accelerator — see its call site's doc for why this must be
// a named file-scope function.
func onAwakeHook(scheduler playlistschedule.Scheduler, executor devicectl.Executor, logger *zap.Logger) func(context.Context) {
	return func(ctx context.Context) {
		scheduler.RecomputeNow(ctx)
		devicectl.RetryBootRecovery(executor, logger)
	}
}

// initializeApp initializes the app with real dependencies
func initializeApp(
	logger *zap.Logger,
	cdpEndpoint string,
	relayerEndpoint string,
	relayerAPIKey string,
	mintPairingConfig *config.MintPairingConfig,
	offlineCacheConfig *config.OfflineCacheConfig,
	gatewayUserAgentConfig *config.GatewayUserAgentConfig,
	netlogConfig *config.NetlogConfig,
	provTuning provisioning.Tuning,
	dbusName string,
	dbusOpts []dbus_v5.MatchOption,
) *app {
	// Basic components. This is the daemon-lifetime context: it flows into every
	// long-lived component built below, and main cancels it (app.Cancel) on
	// SIGTERM so those components observe shutdown.
	context, cancelApp := context.WithCancel(context.Background())

	// stateLoadKnown starts UNKNOWN (false); run() stores the state.Load
	// verdict before Provisioning.Start reads it via InitialClaimed.
	stateLoadKnown := &atomic.Bool{}

	// Wrappers
	clock := wrapper.NewClock()
	os := wrapper.NewOS()
	signal := wrapper.NewSignal()
	daemon := wrapper.NewDaemon()
	httpClient := wrapper.NewHTTPClient()
	io := wrapper.NewIO()
	json := wrapper.NewJSON()
	randomizer := wrapper.NewRandomizer()
	exec := wrapper.NewExec()
	math := wrapper.NewMath()
	d := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}
	webSocketDialer := wrapper.NewWebSocketDialer(d)

	// Components
	// CDP
	cdp := cdp.New(cdpEndpoint, webSocketDialer, io, json, httpClient, logger)

	// Relayer
	relayer := relayer.New(relayerEndpoint, relayerAPIKey, webSocketDialer, randomizer, clock, os, json, logger)

	// DBus. Wrapped in a Restartable adapter so a failed Start (session bus not
	// up yet, name still held by a dying predecessor) can be retried by run()
	// without racing live consumers: each attempt builds a fresh underlying
	// client and only publishes it once fully started — see dbus.Restartable
	// for why a raw godbus client must not be re-Started in place.
	dbusClient := dbus.NewRestartable(logger, func() dbus.DBus {
		return godbus.NewDBusClient(context, logger, dbusName, dbusOpts...)
	})

	// DeviceStatus
	deviceStatus := status.NewDeviceStatus(json, os, exec, httpClient, io, cdp)

	// DDC panel
	ddcPanel := ddc.New(exec, clock, logger)

	// Websocket handler
	wsUpgrader := wrapper.NewWebsocketUpgrader(&websocket.Upgrader{
		ReadBufferSize:  ws.BUFFER_SIZE,
		WriteBufferSize: ws.BUFFER_SIZE,
		CheckOrigin: func(r *http.Request) bool {
			// Allow all origins
			return true
		},
	})
	wsHandler := ws.NewWSHandler(context, wsUpgrader, clock, logger)

	// StatusPoller
	poller := status.NewPoller(cdp, relayer, wsHandler, deviceStatus, ddcPanel, json, logger)

	// Watchdog
	watchdog := watchdog.New(logger)

	// Executor
	executor := devicectl.New(
		cdp,
		deviceStatus,
		poller,
		ddcPanel,
		json,
		os,
		exec,
		math,
		clock,
		logger,
	)

	// FFIndexer
	ffIndexer := ffindexer.New(httpClient, json, io, logger)

	// DP1
	dp1 := dp1.New(ffIndexer, httpClient, json, io, logger, debug)

	// displayAt scheduler: filters playlists with displayAt items before CDP
	// and advances them on timer / wake / CDP reconnect. Durable state stores
	// only the refreshable source identity; after a controld-only restart the
	// source must be fetched again before scheduled cutovers resume.
	playlistScheduler := playlistschedule.NewWithStore(context, cdp, clock, nil,
		playlistschedule.NewFileStore(os, json), logger)
	devicectl.SetWithPlayerPush(executor, playlistScheduler.WithPlayerPush, logger)
	// onAwake composes the displayAt recompute with the boot recovery
	// early-re-entry accelerator (design doc §5.1: the sleep tracker's
	// onAwake is one of the two accelerators, alongside a generation bump,
	// that let a Deferred round re-enter before its backoff timer — which
	// stays the PRIMARY trigger — fires). A named file-scope function, not an
	// inline closure: initializeApp's own local `context` variable shadows
	// the context PACKAGE for the rest of this function (see
	// sleepInvalidateReconciler's doc), so a func literal written here
	// cannot spell out the context.Context parameter type it needs.
	devicectl.SetOnAwake(executor, onAwakeHook(playlistScheduler, executor, logger), logger)

	// Session is the cross-cutting page-generation/readiness/navigation
	// authority (design doc §2): page generation (three sources — CDP
	// connect, a session-executed navigation, a status-poller stamp
	// mismatch), the per-generation readiness barrier, screen-overlay
	// coordination, and the navigate-to-entry recovery primitive. Off-lane
	// producers (setupui, mediator connectivity, mintpairing, commandrouter)
	// are wired to it below, once their own components exist. The asleep
	// gate is devicectl's four-value sleep tracker (design doc §7): built
	// AFTER executor so PlayerSleepGate can read its live tracker state.
	session := playersession.New(context, cdp, clock, devicectl.PlayerSleepGate(executor), logger)
	// Boot recovery's escalation primitive (design doc §5: "Escalations
	// call session.NavigateHome").
	devicectl.SetBootRecoverySession(executor, session, logger)
	// Roots the backoff timer's sleep on the daemon-lifetime ctx
	// so shutdown cancels a pending sleep instead of leaking the goroutine.
	devicectl.SetBootRecoveryDaemonContext(executor, context, logger)

	// Mint Pairing
	mintPairingOpts := mintpairing.OptionsFromConfig(mintPairingConfig, relayerEndpoint)
	mintPairing := mintpairing.New(mintPairingOpts, relayer, cdp, httpClient, relayerAPIKey, json, logger)

	// Offline cache. Disabled by default (see config.OfflineCacheConfig's
	// doc on why it defaults off) — offlineCache/kioskReplay/staticServer
	// stay nil in that case, and handler.go's/refresher's nil-guards
	// mirror mintPairing's so the rest of the daemon behaves exactly as
	// before this feature existed.
	var offlineCache offlinecache.Service
	var kioskReplay offlinecache.KioskReplay
	var offlineCacheScopeLost offlinecache.ScopeLostRegistrar
	var offlineCacheStaticServer offlinecache.StaticServer
	var offlineCacheNotifier *offlinecache.Notifier
	var offlineCacheSysMetricsSink func(raw []byte)
	if offlineCacheConfig != nil && offlineCacheConfig.Enabled {
		ocOpts := offlinecache.OptionsFromConfig(offlineCacheConfig, cdpEndpoint)
		ocRuntime := offlinecache.Bootstrap(
			ocOpts, relayer, wsHandler, httpClient, webSocketDialer,
			os, io, json, exec, clock, logger,
		)
		offlineCache = ocRuntime.Service
		kioskReplay = ocRuntime.KioskReplay
		offlineCacheScopeLost = ocRuntime.ScopeLost
		offlineCacheStaticServer = ocRuntime.StaticServer
		offlineCacheNotifier = ocRuntime.Notifier
		offlineCacheSysMetricsSink = ocRuntime.SysMetricsSink
	}

	// Kiosk User-Agent rewrite (#296). Built independently of the offline
	// cache above and defaulting ON: an artwork whose origin challenges
	// browser User-Agents never renders at all, and a device whose config
	// predates this key must still get the fix.
	//
	// A bad host entry is NOT fatal, and it must not take the WORKING
	// entries down with it. config.Load failing is fatal under
	// Restart=always and this daemon owns every recovery surface on the
	// device, so nothing here may crash-loop the box out of reach — but
	// "degrade" has to mean degrade, not switch off. Appending one typo'd
	// gateway to a working list used to disable the rewrite entirely,
	// re-opening #296 for ipfs.io and dweb.link over an edit about a
	// different host; NewFromOperatorHosts drops only what it cannot use.
	// The rejected entries are logged at Error individually because on a
	// headless device this log is the only place the operator's mistake
	// is visible at all.
	var uaRewrite *uarewrite.Interceptor
	if gatewayUserAgentConfig.IsEnabled() {
		var uaHosts []string
		var uaAgent string
		if gatewayUserAgentConfig != nil {
			uaHosts = gatewayUserAgentConfig.Hosts
			uaAgent = gatewayUserAgentConfig.UserAgent
		}
		policy, rejected, err := uarewrite.NewFromOperatorHosts(uaHosts, uaAgent)
		if len(rejected) > 0 {
			logger.Error("Ignoring unusable gatewayUserAgent hosts; the rest of the list still applies",
				zap.Strings("rejected", rejected))
		}
		if err != nil {
			// Only reachable if the BUILT-IN host list is unusable, i.e. a
			// programming error — still not worth crash-looping over.
			logger.Error("Invalid gatewayUserAgent config; kiosk User-Agent rewrite disabled",
				zap.Error(err))
		} else {
			uaRewrite = uarewrite.NewInterceptor(
				policy, cdpEndpoint, httpClient, webSocketDialer, json, io, clock, logger,
			)
		}
	}

	// Command handler. The raw handler serves internal daemon lifecycle flows
	// (e.g. OOM recovery) directly; external ingress (relayer + LAN hub) is
	// wrapped with command-storm protection so both paths share one set of
	// rate/concurrency guards (see feral-file/ffos-user#208). Internal recovery
	// must never be shed by external client traffic, so it bypasses the gate.
	rawCmdHandler := commandrouter.New(executor, cdp, dp1, poller, mintPairing, offlineCache, kioskReplay, playlistScheduler, json, logger)
	// Cast-time source preflight (#304): a displayPlaylist whose every item
	// source definitively answers an HTTP error is rejected at accept time
	// instead of being forwarded and self-reported as playing. Wired against
	// the raw handler before NewGate wraps it (SetSourceProber's contract),
	// and independently of whether the offline cache is enabled. The
	// sourceProbe.disabled config flag is the runtime kill switch: the
	// preflight can REFUSE the device's primary function, so an origin class
	// that answers this probe dead while still rendering in the kiosk must be
	// recoverable by a config edit, not a package rollback (see
	// config.SourceProbeConfig). net.DefaultResolver for the same reason the
	// offline cache's classifier uses it: the guard's view of a name must
	// match what would actually be dialed (see offlinecache.ErrUnsafeSource).
	sourceProbeEnabled := true
	if sp := config.Get().SourceProbe; sp != nil && sp.Disabled {
		sourceProbeEnabled = false
		logger.Warn("displayPlaylist source preflight disabled by config; casts are forwarded unprobed")
	} else {
		commandrouter.SetSourceProber(rawCmdHandler, offlinecache.NewSourceProber(net.DefaultResolver), logger)
	}
	gateCfg := commandrouter.DefaultGateConfig()
	if cs := config.Get().CommandStorm; cs != nil {
		if cs.Disabled {
			gateCfg.Enabled = false
		}
		if cs.MaxConcurrent > 0 {
			gateCfg.MaxConcurrent = cs.MaxConcurrent
		}
	}
	if sourceProbeEnabled {
		gateCfg = boundGateForSourceProbe(gateCfg, logger)
	}
	cmdHandler := commandrouter.NewGate(rawCmdHandler, gateCfg, logger)

	// Playlist refresher
	playlistRefresher := playlist_refresher.New(context, dp1, poller, cdp, kioskReplay, offlineCache, json, playlistScheduler, clock, logger)

	// Replay saturation invalidates Fetch-interception scope exactly the way
	// a kiosk restart does: retireOnSaturation closes the root CDP session so
	// the page stops hanging, which also means a fail_closed scope is
	// enforced by nothing until something re-arms it. Without this the
	// "something" is only this refresher's periodic pass, up to
	// PLAYLIST_REFRESH_INTERVAL away, with the artwork silently served from
	// the live network for that whole window. ForceRefresh is the same
	// recovery the onConnect hook below already uses for the identical
	// problem — best-effort acceleration rather than a bound (see
	// docs/offline-artwork-capture.md for the paths that still fall back to
	// the periodic tick). Wired here rather than in offlinecache.Bootstrap
	// because the refresher depends on KioskReplay and so cannot exist until
	// after that call.
	if offlineCacheScopeLost != nil {
		offlineCacheScopeLost.SetOnScopeLost(playlistRefresher.ForceRefresh)
	}

	// OOM Recoverer — internal lifecycle flow, uses the raw (ungated) handler.
	oomRecoverer := oomrecovery.New(poller, rawCmdHandler, logger)

	// Mediator
	mediator := mediator.New(context, relayer, dbusClient, cdp, cmdHandler, executor, playlistRefresher, json, logger)

	// Feed monitord's sysmetrics into the offline cache's admission gate
	// (see offlinecache.Runtime.SysMetricsSink). Wired here for the same
	// no-cross-import reason as the claim observer below: the mediator
	// forwards raw bytes without knowing who consumes them.
	if offlineCacheSysMetricsSink != nil {
		mediator.SetSysMetricsObserver(offlineCacheSysMetricsSink)
	}

	// LinkChecker is the shared link-state seam keying mDNS/hub discoverability
	// on the presence of any LAN link rather than internet reachability.
	linkChecker := status.NewLinkChecker(exec, logger)

	// WAN-outage flight recorder (docs/wan-outage-observability.md stages
	// 1-2). nil when disabled or unavailable; every wiring below nil-guards.
	// Producers feed it through type-asserted observe-only seams (the
	// mediator's applied internet verdicts, the relayer's connection
	// lifecycle, the provisioning machine's transitions via the Config field
	// at construction below) — the recorder never calls back into any of them.
	netlogRecorder := buildNetlogRecorder(context, netlogConfig, linkChecker, exec, clock, dbusClient, relayerEndpoint, executor, deviceStatus, logger)
	if netlogRecorder != nil {
		if sink, ok := mediator.(interface{ SetInternetObserver(func(bool)) }); ok {
			sink.SetInternetObserver(netlogRecorder.ObserveInternet)
		}
		if sink, ok := relayer.(interface {
			SetConnectionObserver(func(connected bool, closeCode int))
		}); ok {
			sink.SetConnectionObserver(netlogRecorder.ObserveRelayer)
		}
	}

	// Claim-state transitions (a successful connect) re-register mDNS with the
	// updated `claimed` TXT, and feed the provisioning machine's loop-visible
	// claim snapshot (escape policy, constraint 8). One observer fans out to
	// both consumers; provMachine is declared below and assigned before the
	// executor can observe any transition (command handling starts after
	// initializeApp returns).
	var provMachineForClaim *provisioning.Machine
	// A rename re-registers mDNS with the new label so a second controller —
	// another phone, ff-cli's discovery — sees it without the owner repeating
	// themselves. The advertised identity (TXT `id`) is untouched.
	executor.SetDeviceNameObserver(func(name string) {
		mediator.SetDeviceName(name)
	})

	executor.SetClaimObserver(func(claimed bool) {
		mediator.SetClaimed(claimed)
		if provMachineForClaim != nil {
			provMachineForClaim.SetClaimed(claimed)
		}
	})
	// Provisioning domain (SoftAP setup). controld owns setup, so run() starts it
	// unconditionally. The connectivity adapter reads sys-monitord over the shared
	// D-Bus client; the ActiveLink guard reuses the link checker so a device with
	// any live local link (ethernet or an associated Wi-Fi station) never pops the
	// setup AP — the AP raises on link loss, not internet loss (#233). The probe
	// excludes the device's own hotspot by NM profile name, so a raised (or
	// half-torn-down) setup AP never counts as an uplink. Narration flows through
	// a setupui.Service.
	setupNarrator := setupui.New(cdp, setupui.DefaultContractPath, logger)
	// One narration surface for the whole process: the executor's controld-owned
	// claim / factory-reset / OTA-failure narration shares this exact instance with
	// the provisioning domain below, so the single on-connect Resync() wired into
	// CDP.Start re-pushes every narration state (not just provisioning's) when
	// Chromium reconnects mid-setup.
	executor.SetSetupUI(setupNarrator)

	// Wire every off-lane producer to the session (design doc §4), now that
	// they all exist. Registration ORDER is the reconciler execution order on
	// every generation-ready: sleep invalidate+poke, playlist recompute,
	// status force-refresh, setup-narration resync, offline-cache replay-scope
	// resync, boot-recovery retry, connectivity — replacing the five ad-hoc
	// CDP-reconnect spawns run() used to do inline.
	session.RegisterReconciler("sleep-invalidate", sleepInvalidateReconciler(executor, logger))
	if playlistScheduler != nil {
		session.RegisterReconciler("playlist-recompute", playlistRecomputeReconciler(playlistScheduler))
	}
	session.RegisterReconciler("status-force-refresh", statusForceRefreshReconciler(poller))
	session.RegisterReconciler("setupui-resync", setupUIResyncReconciler(setupNarrator))
	// Guarded on kioskReplay, not on the refresher. This is not an
	// optimization: ForceRefresh signals a full processPlayingPlaylist pass,
	// which re-resolves the playlist over the network and re-sends
	// displayPlaylist with refresh:true. Registering it unguarded would add a
	// soft artwork refresh on EVERY generation bump (CDP connect, every
	// recovery navigation, every stamp mismatch) to devices running the
	// default config with the offline cache off — breaking the
	// "feature off behaves exactly as before" contract the Bootstrap call
	// above depends on. With the cache off, the periodic ticker stays the
	// only resync, which is byte-identical to develop's behavior.
	if kioskReplay != nil {
		session.RegisterReconciler("replay-scope-resync", replayScopeResyncReconciler(playlistRefresher))
	}
	// Boot recovery's generation-bump accelerator (design doc §5.1): a new
	// generation (CDP reconnect, a recovery navigation, a stamp-mismatch
	// bump) is a signal the page or player state likely changed, so a
	// Deferred round re-enters early instead of waiting out its backoff
	// timer (which stays the PRIMARY trigger either way).
	session.RegisterReconciler("boot-recovery-retry", bootRecoveryRetryReconciler(executor, logger))
	// setupui integration (§4 disposition table): the narration worker
	// parks sends while a recovery navigation is pending, and setupui
	// registers its own Narrating() as an overlay owner so NavigateHome never
	// erases live narration.
	setupNarrator.SetSession(session)
	session.RegisterOverlayOwner("setupui", setupNarrator.Narrating)
	// mintpairing/qrdisplay: a live mint-pairing display (pairing code or
	// request-received) is an overlay owner too, for the same reason. It also
	// parks its own display sends while a recovery navigation is pending,
	// mirroring setupui's park discipline above.
	session.RegisterOverlayOwner("mintpairing", mintPairing.DisplayActive)
	mintPairing.SetSession(session)
	// mediator connectivity ownership (§4): SetSession registers the
	// "connectivity" reconciler internally, so it runs last among the five
	// above, and routes the edge-triggered pushes from connectivity_change
	// through the session's handler-readiness discipline too.
	mediator.SetSession(session)
	// Generation re-check seams (§4): a moved generation across an
	// in-flight setSleepMode send or CDP command reply means the ack may not
	// describe the current document.
	devicectl.SetSessionGeneration(executor, session.Generation, logger)
	commandrouter.SetSessionGeneration(rawCmdHandler, session.Generation, logger)
	// commandrouter's refreshArtwork dead-page recovery escalates to
	// NavigateHomeInline instead of Page.reload (design doc §3).
	commandrouter.SetRecoverySession(rawCmdHandler, session, logger)
	// Status-poller stamp carrier (§2.1 source 3): every checkStatus
	// round-trip reports its observed document stamp so the session can
	// detect a document replaced without going through NavigateHome
	// (feral-watchdog's recovery navigate, a player-initiated reload).
	poller.SetStampObserver(session.ObserveStatusStamp)

	softAP := softap.NewNetworkManager(exec, logger, "", nil)
	wifiCtl := wifictl.New(exec, clock, logger, "")
	// The claim QR auto-paints when an unclaimed device comes online — the
	// launcher-ui replacement (see MaybeShowClaimQROnOnline).
	provisioningNotifier := &setupNotifier{ui: setupNarrator, logger: logger, claimCtx: context,
		// The SERIAL (mDNS TXT `id`), deliberately not the owner's display
		// name: §4.6 trouble-state copy exists so a user reporting a stuck
		// frame can say WHICH one, and support resolves that on the serial —
		// unique across a household and printed on the unit, where "Living
		// Room" is neither. (resolveMDNSDeviceInfo.Name became the owner's
		// label when device naming landed; this caller's need did not change.)
		deviceName: resolveMDNSDeviceInfo(os, json, state.ClaimSnapshot(), logger).ID}
	// The claim flow's topic-wait expiry narration needs a cached internet
	// verdict to tell "no WAN — the topic can never arrive" from "relayer
	// slow" (§4.6, the unclaimed wired no-WAN black screen). Same cached
	// monitord read the hub status provider serves; never a live probe.
	wireInternetProbe(executor, dbusClient, logger)
	if ac, ok := executor.(autoClaimFlow); ok {
		provisioningNotifier.claim = ac.MaybeShowClaimQROnOnline
		// Topic assignment re-triggers the claim flow: a factory-fresh device
		// connects with no topic and may receive its system topic AFTER the
		// flow's bounded topic wait expired — with no further online
		// transition, nothing else would ever re-run it. The flow itself
		// no-ops when settled or already in flight, so a spurious fire is
		// harmless.
		mediator.SetTopicObserver(func() { go ac.MaybeShowClaimQROnOnline(context) })
	}
	// Boot-scoped online hooks. The startup OTA gate is the claimed-device
	// counterpart of the claim flow: a boot online transition re-runs the
	// setupd-era mandatory (Required-mode) update check so a force release is
	// applied on reboot instead of waiting for the daily updater timer. The
	// player recovery repairs the kiosk's deliberately network-ungated first
	// page load, which on Wi-Fi boots routinely predates association and dies
	// without retry. BOTH are wired only inside the kernel boot window — see
	// wireBootLifecycleHooks for why the Restart=always service makes the
	// ungated variants a mid-exhibition hazard.
	wireBootLifecycleHooks(provisioningNotifier, executor,
		func() bool { return uptimeWithin(bootLifecycleWindow, go_os.ReadFile, logger) },
		func() bool { return uptimeWithin(startupOTAGateEntryWindow, go_os.ReadFile, logger) })
	provMachine := provisioning.New(provisioning.Config{
		AP:           softAP,
		Wifi:         wifiCtl,
		Connectivity: &dbusConnectivity{dbus: dbusClient, logger: logger},
		Clock:        clock,
		Logger:       logger,
		Notifier:     provisioningNotifier,
		// The flight recorder sees every state/reason change, silent legs
		// included (nil when the recorder is disabled).
		TransitionObserver: netlogTransitionObserver(netlogRecorder),
		ActiveLink:         externalLinkProbe(linkChecker),
		ActiveLinkDetail:   externalLinkDetailProbe(linkChecker),
		// Ethernet-only verdict for the escape policy's wired guard and the
		// wired exit from a raised AP (constraint 6 — NOT ExternalLink, which
		// counts stations).
		WiredLink: wiredLinkProbe(linkChecker),
		// Boot-time claim snapshot; the executor's claim observer (above)
		// keeps it fresh. known comes from stateLoadKnown, which run() stores
		// AFTER state.Load and BEFORE Provisioning.Start: a quarantined
		// (corrupt) state file reads as empty — i.e. unclaimed — everywhere
		// else, but the machine must treat it as UNKNOWN = claimed
		// (constraint 8's fail-safe: never auto-raise the setup AP over a
		// possibly claimed exhibition frame off a disk error). Defaults false
		// (unknown) so any ordering mistake fails safe.
		InitialClaimed: func() (bool, bool) {
			claim := state.ClaimSnapshot()
			return claim.Claimed, stateLoadKnown.Load()
		},
		Tuning: provTuning,
		// Same boot-vs-restart discriminator as wireBootLifecycleHooks above:
		// the machine narrates its boot offline assessment (and may run the
		// relocation check) only when this process start IS a device boot —
		// a Restart=always daemon restart mid-outage must stay silent.
		// provisioning.New evaluates this exactly once, here at wiring time,
		// so the classification cannot drift past the window's edge while the
		// machine's AP sweep and initial connectivity query run.
		BootAssessment: func() bool { return uptimeWithin(bootLifecycleWindow, go_os.ReadFile, logger) },
	})

	// Hub status provider. The base provider reads identity/version/claim/topic
	// from on-device state and reports a placeholder setup_state; the wrapper lets
	// the live provisioning machine supply the real setup_state.
	baseStatusProvider := hub.NewStateStatusProvider(os, json, linkChecker, logger)
	statusProvider := &provisioningStatusProvider{
		base:     baseStatusProvider,
		machine:  provMachine,
		internet: internetProbeFrom(dbusClient, logger),
		snapshot: provMachine.Snapshot,
	}
	screenshotCapturer := screenshot.New(cdpEndpoint, httpClient, webSocketDialer)
	hub := hub.New(context, wsHandler, cmdHandler, statusProvider, screenshotCapturer, nil, json, logger)
	// Control-plane hub contact defers the escape policy's episode raise
	// (§4.1): a phone with the app open must not have its link yanked. The
	// hub filters (counted routes, non-loopback) and the machine timestamps.
	if sink, ok := hub.(interface{ SetContactObserver(func()) }); ok {
		sink.SetContactObserver(provMachine.ObserveHubContact)
	}
	// The claim observer registered above fans out here (declared before the
	// machine existed).
	provMachineForClaim = provMachine
	// App-triggered Wi-Fi setup (startWifiSetup): the executor's command
	// handler runs the machine's admission and queues the user-requested
	// raise; the §4.2 session machinery bounds the session.
	wireWifiSetupStarter(executor, provMachine)
	// getDeviceStatus carries the same §4.7 health object the hub status
	// routes serve (one diagnosis, every transport).
	wireNetworkHealth(executor, provMachine, internetProbeFrom(dbusClient, logger))

	return &app{
		Ctx:                      context,
		Cancel:                   cancelApp,
		Logger:                   logger,
		Clock:                    clock,
		OS:                       os,
		Signal:                   signal,
		Daemon:                   daemon,
		HTTPClient:               httpClient,
		IO:                       io,
		JSON:                     json,
		Random:                   randomizer,
		Exec:                     exec,
		Math:                     math,
		CDP:                      cdp,
		Relayer:                  relayer,
		DBus:                     dbusClient,
		Mediator:                 mediator,
		OOMRecoverer:             oomRecoverer,
		Executor:                 executor,
		DeviceStatus:             deviceStatus,
		StatusPoller:             poller,
		Watchdog:                 watchdog,
		PlaylistRefresher:        playlistRefresher,
		PlaylistScheduler:        playlistScheduler,
		MintPairing:              mintPairing,
		KioskReplay:              kioskReplay,
		UARewrite:                uaRewrite,
		OfflineCacheService:      offlineCache,
		OfflineCacheStaticServer: offlineCacheStaticServer,
		OfflineCacheNotifier:     offlineCacheNotifier,
		Hub:                      hub,
		LinkChecker:              linkChecker,
		Netlog:                   netlogRecorder,
		Provisioning:             provMachine,
		StateLoadKnown:           stateLoadKnown,
		SetupUI:                  setupNarrator,
		Session:                  session,
	}
}

// initializeTestApp initializes the app with mock dependencies
func initializeTestApp(
	ctx context.Context,
	logger *zap.Logger,
	clock wrapper.Clock,
	os wrapper.OS,
	signal wrapper.Signal,
	daemon wrapper.Daemon,
	http wrapper.HTTPClient,
	io wrapper.IO,
	json wrapper.JSON,
	random wrapper.Randomizer,
	exec wrapper.Exec,
	math wrapper.Math,
	cdp cdp.CDP,
	relayer relayer.Relayer,
	dbus dbus.DBus,
	deviceStatus status.DeviceStatus,
	statusPoller status.Poller,
	watchdog watchdog.Watchdog,
	mediator mediator.Mediator,
	oomRecoverer oomrecovery.Recoverer,
	executor devicectl.Executor,
	dynamicPlaylistRefresher playlist_refresher.Refresher,
	mintPairing mintpairing.Service,
	hub hub.Hub,
) *app {
	return &app{
		Ctx:               ctx,
		Logger:            logger,
		Clock:             clock,
		OS:                os,
		Signal:            signal,
		Daemon:            daemon,
		HTTPClient:        http,
		IO:                io,
		JSON:              json,
		Random:            random,
		Exec:              exec,
		Math:              math,
		CDP:               cdp,
		Relayer:           relayer,
		DBus:              dbus,
		Mediator:          mediator,
		OOMRecoverer:      oomRecoverer,
		Executor:          executor,
		DeviceStatus:      deviceStatus,
		StatusPoller:      statusPoller,
		Watchdog:          watchdog,
		PlaylistRefresher: dynamicPlaylistRefresher,
		MintPairing:       mintPairing,
		Hub:               hub,
	}
}
