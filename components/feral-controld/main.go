package main

import (
	"context"
	"errors"
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
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/watchdog"
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
	Ctx    context.Context
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
	MintPairing       mintpairing.Service
	// KioskReplay, OfflineCacheService, and OfflineCacheStaticServer may
	// all be nil (feature disabled via config.OfflineCacheConfig).
	KioskReplay              offlinecache.KioskReplay
	OfflineCacheService      offlinecache.Service
	OfflineCacheStaticServer offlinecache.StaticServer
	Hub                      hub.Hub
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

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(app.Ctx)
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
	// Load state
	s, err := state.Load(app.Logger)
	if err != nil {
		return err
	}

	// Set global topic ID in Sentry if available
	if conf.SentryConfig.IsEnabled() && s.Relayer.TopicID != "" {
		logger.SetGlobalTopicID(s.Relayer.TopicID)
	}

	// Start watchdog
	app.Watchdog.Start(ctx)
	defer app.Watchdog.Stop()

	// Initialize DBus client
	err = app.DBus.Start()
	if err != nil {
		return err
	}
	defer func() {
		_ = app.DBus.Stop()
	}()

	dbusHandler := dbus.NewHandler(ctx, app.Relayer, app.Logger)
	err = app.DBus.Export(dbusHandler, dbus.PATH, dbus.INTERFACE)
	if err != nil {
		return err
	}

	// Initialize Mediator
	app.Mediator.Start()
	defer app.Mediator.Stop()

	// Get connectivity status and connect to relayer if ready
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
	if connected && s.Relayer.IsReady() {
		app.Logger.Info("Connecting relayer during startup")
		err = app.Relayer.Connect(ctx)
		if err != nil {
			// Never fatal: returning here would exit before SdNotifyReady and before our
			// D-Bus interface has been up long enough for setupd's wait_for_controld, so a
			// relayer outage would crash-loop this daemon AND take BLE provisioning down
			// with it — the exact coupling the unconditional-start model exists to remove
			// (.start-services.sh starts us --no-block precisely because pre-READY failure
			// can happen). Retry in the background instead; the mediator's
			// connectivity-restored handler and the GetRelayerInfo D-Bus path also
			// re-attempt the connection, and RetryableConnect tolerates racing them
			// (ErrAlreadyConnected is success).
			app.Logger.Error("Failed initial relayer connection, retrying in background", zap.Error(err))
			go func() {
				if retryErr := app.Relayer.RetryableConnect(ctx); retryErr != nil {
					app.Logger.Error("Background relayer connection retry gave up", zap.Error(retryErr))
				}
			}()
		} else {
			app.Logger.Info("Initial relayer connection established")
		}
		// Close regardless of whether the *initial* connect succeeded: the background
		// retry (or a later mediator/D-Bus reconnect) may have established the
		// connection by shutdown time, and Close is a no-op on a nil conn.
		defer app.Relayer.Close()
	} else {
		app.Logger.Info("Skipping initial relayer connection",
			zap.Bool("internet_connected", connected),
			zap.Bool("relayer_ready", s.Relayer.IsReady()),
		)
	}

	// Start Hub if enabled
	if conf.EnableHub {
		app.Hub.Start()
		defer func() {
			if err := app.Hub.Stop(); err != nil {
				app.Logger.Warn("Failed to stop hub", zap.Error(err))
			}
		}()

		deviceInfo := resolveMDNSDeviceInfo(app.OS, s, app.Logger)
		advertiser := mdns.New(app.Logger)
		defer advertiser.Stop()

		app.Mediator.InitializeMDNS(advertiser, deviceInfo, connected)
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
		if err := app.OfflineCacheService.Start(ctx); err != nil {
			app.Logger.Error("Failed to start offline cache service", zap.Error(err))
		} else {
			defer app.OfflineCacheService.Stop()
		}
	}
	if app.OfflineCacheStaticServer != nil {
		go func() {
			if err := app.OfflineCacheStaticServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
	app.CDP.Start(ctx, func() {
		devicectl.InvalidatePlayerSleepState(app.Executor, app.Logger)
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
	// Basic components
	context := context.Background()

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

	// DBus
	dbusClient := godbus.NewDBusClient(context, logger, dbusName, dbusOpts...)

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
		dbusClient,
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
	var offlineCacheStaticServer offlinecache.StaticServer
	if offlineCacheConfig != nil && offlineCacheConfig.Enabled {
		ocOpts := offlinecache.OptionsFromConfig(offlineCacheConfig, cdpEndpoint)
		ocRuntime := offlinecache.Bootstrap(
			ocOpts, relayer, wsHandler, httpClient, webSocketDialer,
			os, io, json, exec, clock, logger,
		)
		offlineCache = ocRuntime.Service
		kioskReplay = ocRuntime.KioskReplay
		offlineCacheStaticServer = ocRuntime.StaticServer
	}

	// Command handler. The raw handler serves internal daemon lifecycle flows
	// (e.g. OOM recovery) directly; external ingress (relayer + LAN hub) is
	// wrapped with command-storm protection so both paths share one set of
	// rate/concurrency guards (see feral-file/ffos-user#208). Internal recovery
	// must never be shed by external client traffic, so it bypasses the gate.
	rawCmdHandler := commandrouter.New(executor, cdp, dp1, poller, mintPairing, offlineCache, kioskReplay, json, logger)
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
	playlistRefresher := playlist_refresher.New(context, dp1, poller, cdp, kioskReplay, offlineCache, json, clock, logger)

	// OOM Recoverer — internal lifecycle flow, uses the raw (ungated) handler.
	oomRecoverer := oomrecovery.New(poller, rawCmdHandler, logger)

	// Mediator
	mediator := mediator.New(relayer, dbusClient, cdp, cmdHandler, executor, playlistRefresher, json, logger)

	// Hub
	hub := hub.New(context, wsHandler, cmdHandler, nil, json, logger)

	return &app{
		Ctx:                      context,
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
		MintPairing:              mintPairing,
		KioskReplay:              kioskReplay,
		OfflineCacheService:      offlineCache,
		OfflineCacheStaticServer: offlineCacheStaticServer,
		Hub:                      hub,
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
