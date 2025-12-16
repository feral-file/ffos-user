package main

import (
	"context"
	"flag"
	"syscall"
	"time"

	go_os "os"

	"github.com/getsentry/sentry-go"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	go_daemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/feral-file/godbus"
	dbus_v5 "github.com/godbus/dbus/v5"

	"github.com/feral-file/ffos-user/components/feral-watchdog/cdp"
	"github.com/feral-file/ffos-user/components/feral-watchdog/command"
	"github.com/feral-file/ffos-user/components/feral-watchdog/config"
	"github.com/feral-file/ffos-user/components/feral-watchdog/event"
	"github.com/feral-file/ffos-user/components/feral-watchdog/logger"
	"github.com/feral-file/ffos-user/components/feral-watchdog/monitor"
	"github.com/feral-file/ffos-user/components/feral-watchdog/vmagent"
	"github.com/feral-file/ffos-user/components/feral-watchdog/watchdog"
	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	SHUTDOWN_TIMEOUT = 2 * time.Second
	DBUS_NAME        = "com.feralfile.watchdog"
)

var debug = false

type app struct {
	// Basic components
	Ctx    context.Context
	Logger *zap.Logger

	// Wrappers
	Clock  wrapper.Clock
	OS     wrapper.OS
	Signal wrapper.Signal
	JSON   wrapper.JSON
	Exec   wrapper.Exec
	Daemon wrapper.Daemon

	// Components
	Watchdog        watchdog.Watchdog
	EventWatcher    event.Watcher
	CDP             cdp.CDP
	Executor        command.Executor
	SystemdMonitor  monitor.SystemdMonitor
	ChromiumMonitor monitor.ChromiumMonitor
	VmAgentClient   vmagent.Client
}

func main() {
	// Read from options
	flag.BoolVar(&debug, "debug", false, "Enable debug mode")
	flag.Parse()

	// Initialize basic logger first for config loading
	basicLogger, err := logger.New(debug)
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
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
		config.VmagentConfig.URL,
		DBUS_NAME,
		[]dbus_v5.MatchOption{
			dbus_v5.WithMatchPathNamespace(dbus_v5.ObjectPath("/com/feralfile")),
		},
	)

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
	err = app.run(ctx)
	if err != nil {
		app.Logger.Fatal("Failed to run app", zap.Error(err))
	}
}

func (app *app) run(ctx context.Context) error {
	// Start watchdog
	go app.Watchdog.Start(ctx)
	defer app.Watchdog.Stop()

	// Initialize CDP
	err := app.CDP.Init(ctx)
	if err != nil {
		return err
	}
	defer app.CDP.Close()

	// Initialize SystemdMonitor
	app.SystemdMonitor.Start(ctx)
	defer app.SystemdMonitor.Stop()

	// Initialize ChromiumMonitor
	app.ChromiumMonitor.Start(ctx)
	defer app.ChromiumMonitor.Stop()

	// Initialize EventWatcher
	app.EventWatcher.Start()
	defer app.EventWatcher.Stop()

	// send ready notification to systemd
	sent, err := app.Daemon.SdNotify(false, go_daemon.SdNotifyReady)
	if err != nil {
		app.Logger.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		app.Logger.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	app.Logger.Info("watchdog started successfully")

	<-ctx.Done()

	app.Logger.Info("watchdog shutdown completed")
	return nil
}

// initializeApp initializes the app with real dependencies
func initializeApp(
	logger *zap.Logger,
	cdpEndpoint string,
	vmagentURL string,
	dbusName string,
	dbusOpts []dbus_v5.MatchOption,
) *app {
	// Basic components
	ctx := context.Background()

	// Wrappers
	clock := wrapper.NewClock()
	os := wrapper.NewOS()
	signal := wrapper.NewSignal()
	httpClient := wrapper.NewHTTPClient(15 * time.Second)
	io := wrapper.NewIO()
	json := wrapper.NewJSON()
	exec := wrapper.NewExec()
	daemon := wrapper.NewDaemon()
	d := &websocket.Dialer{
		HandshakeTimeout: 5 * time.Second,
	}
	webSocketDialer := wrapper.NewWebSocketDialer(d)

	// Components
	// CDP
	cdp := cdp.New(&cdp.Config{Endpoint: cdpEndpoint}, logger, webSocketDialer, io, json, httpClient)

	// VmagentClient
	vmagentClient := vmagent.NewClient(vmagentURL, logger, httpClient, io)

	// Watchdog
	watchdog := watchdog.New(logger)

	// DBus
	dbusClient := godbus.NewDBusClient(ctx, logger, dbusName, dbusOpts...)

	// Executor
	commandExecutor := command.NewCommandExecutor(logger, vmagentClient, exec)

	// SystemdMonitor
	systemdMonitor := monitor.NewSystemdMonitor(cdp, commandExecutor, logger)

	// ChromiumMonitor
	chromiumMonitor := monitor.NewChromiumMonitor(cdpEndpoint, logger, commandExecutor, httpClient, clock, io)

	// EventWatcher
	diskHandler := event.NewDiskHandler(logger, commandExecutor, clock)
	memoryHandler := event.NewMemoryHandler(logger, commandExecutor, clock)
	gpuHandler := event.NewGPUHandler(logger, commandExecutor, clock)
	cpuHandler := event.NewCPUHandler(logger, cdp, clock)
	eventWatcher := event.New(dbusClient, diskHandler, memoryHandler, gpuHandler, cpuHandler, json, logger)

	// DBus
	return &app{
		Ctx:             ctx,
		Logger:          logger,
		Clock:           clock,
		OS:              os,
		Signal:          signal,
		JSON:            json,
		Exec:            exec,
		Daemon:          daemon,
		CDP:             cdp,
		Executor:        commandExecutor,
		SystemdMonitor:  systemdMonitor,
		ChromiumMonitor: chromiumMonitor,
		VmAgentClient:   vmagentClient,
		EventWatcher:    eventWatcher,
		Watchdog:        watchdog,
	}
}
