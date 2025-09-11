package main

import (
	"context"
	"flag"
	"fmt"
	go_os "os"
	"syscall"
	"time"

	go_daemon "github.com/coreos/go-systemd/v22/daemon"
	"github.com/feral-file/godbus"
	"github.com/getsentry/sentry-go"
	dbus_v5 "github.com/godbus/dbus/v5"
	"github.com/gorilla/websocket"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/command"
	"github.com/feral-file/ffos-user/components/feral-controld/config"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mediator"
	"github.com/feral-file/ffos-user/components/feral-controld/refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/watchdog"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
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
	Clock  wrapper.Clock
	OS     wrapper.OS
	Signal wrapper.Signal
	Daemon wrapper.Daemon
	HTTP   wrapper.HTTP
	IO     wrapper.IO
	JSON   wrapper.JSON
	Random wrapper.Randomizer
	Exec   wrapper.Exec
	Math   wrapper.Math

	// Components
	CDP          cdp.CDP
	Relayer      relayer.Relayer
	DBus         dbus.DBus
	Mediator     mediator.Mediator
	Command      command.CommandHandler
	DeviceStatus status.DeviceStatus
	StatusPoller status.Poller
	Watchdog     watchdog.Watchdog
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

	// Initialize Sentry if needed
	err = logger.InitSentry(config.SentryConfig)
	if err != nil {
		basicLogger.Error("Failed to initialize Sentry", zap.Error(err))
		// Don't fail the application if Sentry initialization fails
	}

	// Create the final logger (with Sentry if configured)
	var finalLogger *zap.Logger
	if config.SentryConfig.IsEnabled() {
		finalLogger, err = logger.NewWithSentry(debug, config.SentryConfig)
		if err != nil {
			basicLogger.Error("Failed to create Sentry-integrated logger, falling back to basic logger", zap.Error(err))
			finalLogger = basicLogger
		} else {
			finalLogger.Info("Sentry initialized successfully",
				zap.String("environment", config.SentryConfig.Environment),
				zap.String("release", config.SentryConfig.Release))
			defer logger.FlushSentry(2 * time.Second)
		}
	} else {
		finalLogger = basicLogger
		finalLogger.Info("Sentry not configured, using basic logger")
	}
	defer func() {
		_ = finalLogger.Sync()
	}()

	// Initialize app
	app := initializeApp(
		finalLogger,
		config.CDPConfig.Endpoint,
		config.RelayerConfig.Endpoint,
		config.RelayerConfig.APIKey,
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

	// Initialize CDP client
	err = app.CDP.Init(ctx)
	if err != nil {
		return err
	}
	defer app.CDP.Close()

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
		app.Logger.Warn("Failed to get connectivity status", zap.Error(err))
	} else {
		app.Logger.Info("Connectivity status", zap.Bool("connected", connected))
	}
	if connected && s.Relayer.IsReady() {
		err = app.Relayer.Connect(ctx)
		if err != nil {
			return err
		}
		defer app.Relayer.Close()
	}

	// Set the StatusPoller reference in mediator for force refresh
	app.Mediator.SetStatusPoller(app.StatusPoller)

	// Set the StatusPoller reference in command handler for force refresh
	app.Command.SetStatusPoller(app.StatusPoller)

	// Start StatusPoller - it will handle relayer connection status internally
	go app.StatusPoller.Start(ctx)
	defer app.StatusPoller.Stop()

	// send ready notification to systemd
	sent, err := app.Daemon.SdNotify(false, go_daemon.SdNotifyReady)
	if err != nil {
		app.Logger.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		app.Logger.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	app.Logger.Info("controld started successfully")

	<-ctx.Done()

	app.Logger.Info("controld shutdown completed")
	return nil
}

func getConnectivityStatus(ctx context.Context, dc dbus.DBus, logger *zap.Logger) (bool, error) {
	logger.Info("Getting connectivity status")

	deadlineCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	resp, err := dc.Call(
		deadlineCtx,
		dbus.MONITORD_NAME,
		dbus.MONITORD_PATH,
		dbus.MONITORD_INTERFACE,
		dbus.MONITORD_METHOD_GET_CONNECTIVITY_STATUS,
		true,
	)
	logger.Debug("Connectivity status", zap.Any("resp", resp), zap.Error(err))
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
	http := wrapper.NewHTTP()
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
	cdp := cdp.New(cdpEndpoint, webSocketDialer, io, json, http, logger)

	// Relayer
	relayer := relayer.New(relayerEndpoint, relayerAPIKey, webSocketDialer, randomizer, clock, os, logger)

	// DBus
	dbusClient := godbus.NewDBusClient(context, logger, dbusName, dbusOpts...)

	// DeviceStatus
	deviceStatus := status.NewDeviceStatus(json, os, exec, http, io)

	// StatusPoller
	poller := status.NewPoller(cdp, relayer, deviceStatus, logger)

	// Watchdog
	watchdog := watchdog.New(logger)

	// CommandHandler
	commandHandler := command.New(cdp, dbusClient, deviceStatus, json, os, exec, math, logger)

	// refresher
	refresher := refresher.New(json, clock, logger)

	// Mediator
	mediator := mediator.New(relayer, dbusClient, cdp, commandHandler, clock, json, refresher, logger)

	return &app{
		Ctx:          context,
		Logger:       logger,
		Clock:        clock,
		OS:           os,
		Signal:       signal,
		Daemon:       daemon,
		HTTP:         http,
		IO:           io,
		JSON:         json,
		Random:       randomizer,
		Exec:         exec,
		Math:         math,
		CDP:          cdp,
		Relayer:      relayer,
		DBus:         dbusClient,
		Mediator:     mediator,
		Command:      commandHandler,
		DeviceStatus: deviceStatus,
		StatusPoller: poller,
		Watchdog:     watchdog,
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
	http wrapper.HTTP,
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
	command command.CommandHandler) *app {
	return &app{
		Ctx:          ctx,
		Logger:       logger,
		Clock:        clock,
		OS:           os,
		Signal:       signal,
		Daemon:       daemon,
		HTTP:         http,
		IO:           io,
		JSON:         json,
		Random:       random,
		Exec:         exec,
		Math:         math,
		CDP:          cdp,
		Relayer:      relayer,
		DBus:         dbus,
		Mediator:     mediator,
		Command:      command,
		DeviceStatus: deviceStatus,
		StatusPoller: statusPoller,
		Watchdog:     watchdog,
	}
}
