package main

import (
	"context"
	"flag"
	"syscall"
	"time"

	"github.com/getsentry/sentry-go"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/config"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/connectivity"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/dbus"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/event"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/logger"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/mediator"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/prom_server"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/watchdog"

	go_os "os"

	go_daemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/feral-file/godbus"
	dbus_v5 "github.com/godbus/dbus/v5"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/wrapper"
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
	Clock    wrapper.Clock
	OS       wrapper.OS
	Signal   wrapper.Signal
	JSON     wrapper.JSON
	Exec     wrapper.Exec
	Strconv  wrapper.Strconv
	Filepath wrapper.Filepath
	Daemon   wrapper.Daemon

	// Components
	Watchdog            watchdog.Watchdog
	ConnectivityHandler connectivity.Handler
	SystemMonitor       metric.Monitor
	EventWatcher        event.Watcher
	Mediator            mediator.Mediator
	PromServer          prom_server.PromServer
	DBus                dbus.DBus
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
		dbus.NAME,
		[]dbus_v5.MatchOption{
			dbus_v5.WithMatchPathNamespace(dbus_v5.ObjectPath("/com/feralfile")),
		},
		finalLogger,
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
	err = app.run(ctx, config)
	if err != nil {
		app.Logger.Fatal("Failed to run app", zap.Error(err))
	}
}

// initializeApp initializes the app with real dependencies
func initializeApp(
	dbusName string,
	dbusOpts []dbus_v5.MatchOption,
	logger *zap.Logger,
) *app {
	// Basic components
	ctx := context.Background()

	// Wrappers
	clock := wrapper.NewClock()
	os := wrapper.NewOS()
	signal := wrapper.NewSignal()
	json := wrapper.NewJSON()
	exec := wrapper.NewExec()
	strconv := wrapper.NewStrconv()
	filepath := wrapper.NewFilepath()
	daemon := wrapper.NewDaemon()

	// Components
	// Watchdog
	watchdog := watchdog.New(logger)

	// Connectivity
	connectivityHandler := connectivity.NewHandler(ctx, logger)

	// System Monitor
	systemMonitor := metric.NewMonitor(ctx, logger, clock, os, json, strconv, filepath, exec)

	// EventWatcher
	eventWatcher := event.NewWatcher(ctx, logger, clock, exec)

	// DBus
	dbusClient := godbus.NewDBusClient(ctx, logger, dbusName, dbusOpts...)

	// Mediator
	mediator := mediator.New(dbusClient, systemMonitor, connectivityHandler, eventWatcher, logger, json)

	// Prometheus server
	promServer := prom_server.New(logger)

	return &app{
		Ctx:                 ctx,
		Logger:              logger,
		Clock:               clock,
		OS:                  os,
		Signal:              signal,
		JSON:                json,
		Strconv:             strconv,
		Filepath:            filepath,
		Exec:                exec,
		Daemon:              daemon,
		Watchdog:            watchdog,
		DBus:                dbusClient,
		SystemMonitor:       systemMonitor,
		EventWatcher:        eventWatcher,
		Mediator:            mediator,
		PromServer:          promServer,
		ConnectivityHandler: connectivityHandler,
	}
}

func (app *app) run(ctx context.Context, conf *config.Config) error {
	// Start watchdog
	go app.Watchdog.Start(ctx)
	defer app.Watchdog.Stop()

	// Start DBus
	if err := app.DBus.Start(); err != nil {
		return err
	}
	defer func() {
		if err := app.DBus.Stop(); err != nil {
			app.Logger.Warn("Failed to stop DBus", zap.Error(err))
		}
	}()
	dbusHandler := dbus.NewHandler(app.ConnectivityHandler, app.SystemMonitor, app.Logger)
	if err := app.DBus.Export(dbusHandler, dbus.PATH, dbus.INTERFACE); err != nil {
		return err
	}

	// Start connectivity
	app.ConnectivityHandler.Start()
	defer app.ConnectivityHandler.Stop()

	// Start system monitor
	app.SystemMonitor.Start()
	defer app.SystemMonitor.Stop()

	// Start event watcher
	app.EventWatcher.Start()
	defer app.EventWatcher.Stop()

	// Start mediator
	app.Mediator.Start()
	defer app.Mediator.Stop()

	// Start Prometheus server
	if err := app.PromServer.Start(); err != nil {
		return err
	}
	defer func() {
		if err := app.PromServer.Stop(); err != nil {
			app.Logger.Warn("Failed to stop promServer", zap.Error(err))
		}
	}()

	// send ready notification to systemd
	sent, err := app.Daemon.SdNotify(false, go_daemon.SdNotifyReady)
	if err != nil {
		app.Logger.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		app.Logger.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	app.Logger.Info("sys-monitord started successfully")

	<-ctx.Done()

	app.Logger.Info("sys-monitord shutdown completed")
	return nil
}
