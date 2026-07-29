package main

import (
	"context"
	"flag"
	"fmt"
	"net/http"
	go_os "os"
	"strings"
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
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/devicectl"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	ffindexer "github.com/feral-file/ffos-user/components/feral-controld/ff-indexer"
	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mdns"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	oomrecovery "github.com/feral-file/ffos-user/components/feral-controld/oom-recovery"
	playlist_refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/setupui"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
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
	Hub                  hub.Hub
	LinkChecker          *status.LinkChecker

	// Provisioning is the setup-AP trigger state machine. run() starts it
	// unconditionally; left nil in the test app. Typed as an interface so tests
	// can inject an ordering spy.
	Provisioning provisioningRunner
	// SetupUI is the on-screen setup-narration surface driven by the provisioning
	// domain. run() re-pushes its last state (Resync) when CDP (re)connects so a
	// late-loading player catches up. Nil in the test app.
	SetupUI *setupui.Service
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
	s, err := state.Load(app.Logger)
	if err != nil {
		app.Logger.Error("Failed to load persisted state; quarantining it and continuing with empty state", zap.Error(err))
		if rerr := app.OS.Rename(constants.STATE_FILE, constants.STATE_FILE+".corrupt"); rerr != nil {
			app.Logger.Warn("Failed to quarantine state file", zap.Error(rerr))
		}
		s = state.GetState() // installs and returns a fresh empty state
	}

	// Set global topic ID in Sentry if available
	if conf.SentryConfig.IsEnabled() && s.Relayer.TopicID != "" {
		logger.SetGlobalTopicID(s.Relayer.TopicID)
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

		deviceInfo := resolveMDNSDeviceInfo(app.OS, s, app.Logger)
		deviceInfo.Claimed = s.ConnectedDevice != nil && strings.TrimSpace(s.ConnectedDevice.ID) != ""
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
	app.Logger.Info("Initial relayer connection gate evaluated",
		zap.Bool("internet_connected", connected),
		zap.Bool("relayer_ready", s.Relayer.IsReady()),
		zap.String("topic_id", s.Relayer.TopicID),
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
			zap.Bool("relayer_ready", s.Relayer.IsReady()),
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
	// state, so any player-side state controld drove before the restart no longer holds:
	// invalidate the sleep tracker's player leg so the schedule loop re-drives it (the old
	// PartOf=chromium-ready.target design got this for free by restarting controld), then
	// force a status re-poll so upstream re-syncs to what a fresh boot would produce.
	// Recompute displayAt from the in-memory cache as well: after a kiosk restart the
	// player no longer holds the last filtered list, and a threshold may have crossed
	// while CDP was down.
	app.CDP.Start(ctx, func() {
		devicectl.InvalidatePlayerSleepState(app.Executor, app.Logger)
		if app.PlaylistScheduler != nil {
			app.PlaylistScheduler.RecomputeNow(ctx)
		}
		app.StatusPoller.ForceRefresh()

		// Re-attach offline-cache replay interception on every (re)connect —
		// this covers plain kiosk restarts AND OOM-recovery restarts alike,
		// since both funnel through this same reconnect loop (see
		// offlinecache.KioskReplay.AttachOnReconnect's doc). Nil until the
		// config/wiring todo constructs a real KioskReplay.
		if app.KioskReplay != nil {
			if err := app.KioskReplay.AttachOnReconnect(ctx); err != nil {
				app.Logger.Warn("Failed to attach offline cache replay to kiosk CDP session", zap.Error(err))
			}
			// AttachOnReconnect deliberately does not re-apply the
			// previously-enabled item scope (Fetch.enable is not even
			// reissued until something calls SyncPlaylist). Without this,
			// a playlist that was already scoped for offline replay
			// before the restart would silently fall back to live
			// network for up to PLAYLIST_REFRESH_INTERVAL — exactly the
			// window OOM-recovery restarts happen in — until
			// PlaylistRefresher's next periodic pass. ForceRefresh runs
			// that same resync immediately instead of waiting.
			app.PlaylistRefresher.ForceRefresh()
		}

		// Re-push the last setup-narration state so a freshly-(re)loaded player
		// catches up to where provisioning currently is. No-op if nothing has been
		// narrated yet or (test app) no narrator is wired.
		if app.SetupUI != nil {
			app.SetupUI.Resync()
		}
	})
	defer app.CDP.Close()

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

func resolveMDNSDeviceInfo(os wrapper.OS, s *state.State, logger *zap.Logger) mdns.DeviceInfo {
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

	if (deviceID == "" || deviceName == "") && s != nil && s.ConnectedDevice != nil {
		logger.Warn("mDNS using connected device state for identity")
		if deviceID == "" {
			deviceID = strings.TrimSpace(s.ConnectedDevice.ID)
		}
		if deviceName == "" {
			deviceName = strings.TrimSpace(s.ConnectedDevice.Name)
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

// initializeApp initializes the app with real dependencies
func initializeApp(
	logger *zap.Logger,
	cdpEndpoint string,
	relayerEndpoint string,
	relayerAPIKey string,
	mintPairingConfig *config.MintPairingConfig,
	offlineCacheConfig *config.OfflineCacheConfig,
	dbusName string,
	dbusOpts []dbus_v5.MatchOption,
) *app {
	// Basic components. This is the daemon-lifetime context: it flows into every
	// long-lived component built below, and main cancels it (app.Cancel) on
	// SIGTERM so those components observe shutdown.
	context, cancelApp := context.WithCancel(context.Background())

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
	devicectl.SetOnAwake(executor, playlistScheduler.RecomputeNow, logger)

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
	}

	// Command handler. The raw handler serves internal daemon lifecycle flows
	// (e.g. OOM recovery) directly; external ingress (relayer + LAN hub) is
	// wrapped with command-storm protection so both paths share one set of
	// rate/concurrency guards (see feral-file/ffos-user#208). Internal recovery
	// must never be shed by external client traffic, so it bypasses the gate.
	rawCmdHandler := commandrouter.New(executor, cdp, dp1, poller, mintPairing, offlineCache, kioskReplay, playlistScheduler, json, logger)
	gateCfg := commandrouter.DefaultGateConfig()
	if cs := config.Get().CommandStorm; cs != nil {
		if cs.Disabled {
			gateCfg.Enabled = false
		}
		if cs.MaxConcurrent > 0 {
			gateCfg.MaxConcurrent = cs.MaxConcurrent
		}
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
	mediator := mediator.New(relayer, dbusClient, cdp, cmdHandler, executor, playlistRefresher, json, logger)

	// LinkChecker is the shared link-state seam keying mDNS/hub discoverability
	// on the presence of any LAN link rather than internet reachability.
	linkChecker := status.NewLinkChecker(exec, logger)

	// Claim-state transitions (a successful connect) re-register mDNS with the
	// updated `claimed` TXT. Wire the executor's observer to the mediator here so
	// neither package depends on the other's concrete type.
	executor.SetClaimObserver(mediator.SetClaimed)
	// Factory reset revokes the live relayer session, not just the persisted
	// topic (the staged reboot can be delayed or fail). Wired here for the same
	// no-cross-import reason as the claim observer.
	executor.SetRelayerCloser(relayer.Close)

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
	softAP := softap.NewNetworkManager(exec, logger, "", nil)
	wifiCtl := wifictl.New(exec, clock, logger, "")
	// The claim QR auto-paints when an unclaimed device comes online — the
	// launcher-ui replacement (see MaybeShowClaimQROnOnline).
	provisioningNotifier := &setupNotifier{ui: setupNarrator, logger: logger, claimCtx: context}
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
	provMachine := provisioning.New(provisioning.Config{
		AP:           softAP,
		Wifi:         wifiCtl,
		Connectivity: &dbusConnectivity{dbus: dbusClient, logger: logger},
		Clock:        clock,
		Logger:       logger,
		Notifier:     provisioningNotifier,
		ActiveLink:   externalLinkProbe(linkChecker),
	})

	// Hub status provider. The base provider reads identity/version/claim/topic
	// from on-device state and reports a placeholder setup_state; the wrapper lets
	// the live provisioning machine supply the real setup_state.
	baseStatusProvider := hub.NewStateStatusProvider(os, json, linkChecker, logger)
	statusProvider := &provisioningStatusProvider{
		base:     baseStatusProvider,
		machine:  provMachine,
		internet: internetProbeFrom(dbusClient, logger),
	}
	hub := hub.New(context, wsHandler, cmdHandler, statusProvider, nil, json, logger)

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
		OfflineCacheService:      offlineCache,
		OfflineCacheStaticServer: offlineCacheStaticServer,
		OfflineCacheNotifier:     offlineCacheNotifier,
		Hub:                      hub,
		LinkChecker:              linkChecker,
		Provisioning:             provMachine,
		SetupUI:                  setupNarrator,
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
