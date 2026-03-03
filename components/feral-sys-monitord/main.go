package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/logger"
	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/feral-file/godbus"
	"github.com/getsentry/sentry-go"
	"go.uber.org/zap"
)

const (
	WATCHDOG_INTERVAL = 15 * time.Second
	SHUTDOWN_TIMEOUT  = 2 * time.Second
)

var debug = false

func main() {
	// Read from options
	flag.BoolVar(&debug, "debug", false, "Enable debug mode")
	flag.Parse()

	// Initialize logger with debug enabled for development
	log, err := logger.New(debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %s\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = log.Sync()
	}()

	if err := LoadConfig(); err != nil {
		log.Error("Failed to load config.", zap.Error(err))
		return
	}
	log.Info("Configuration loaded successfully.")

	// Initialize Sentry if configured
	if config.SysMonitordConfig.SentryConfig.IsEnabled() {
		sc, err := sentry.NewClient(sentry.ClientOptions{
			Dsn:              config.SysMonitordConfig.SentryConfig.DSN,
			Debug:            config.SysMonitordConfig.SentryConfig.GetDebug(),
			SampleRate:       config.SysMonitordConfig.SentryConfig.GetSampleRate(),
			Environment:      config.SysMonitordConfig.SentryConfig.Environment,
			Release:          config.SysMonitordConfig.SentryConfig.Release,
			SendDefaultPII:   true,
			AttachStacktrace: true,
		})
		if err != nil {
			log.Error("Failed to init sentry.NewClient.", zap.Error(err))
			return
		}
		defer sc.Flush(2 * time.Second)
		finalLogger, err := logger.AddSentry(log, sc)
		if err != nil {
			log.Error("Failed to create Sentry-integrated logger, falling back to basic logger", zap.Error(err))
		} else {
			log = finalLogger
			log.Info("Sentry initialized successfully",
				zap.String("environment", config.SysMonitordConfig.SentryConfig.Environment),
				zap.String("release", config.SysMonitordConfig.SentryConfig.Release))
			defer logger.FlushSentry(2 * time.Second)
		}
	} else {
		log.Info("Sentry not configured, using basic logger")
	}

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("Received signal, initiating shutdown...",
			zap.String("signal", sig.String()))
		cancel()

		time.Sleep(SHUTDOWN_TIMEOUT)
		log.Error("Shutdown timed out, forcing exit...",
			zap.Duration("timeout", SHUTDOWN_TIMEOUT))
		os.Exit(1)
	}()

	// Start watchdog in a goroutine
	watchdog := NewWatchdog(WATCHDOG_INTERVAL, log)
	go watchdog.Start(ctx)
	defer watchdog.Stop()

	// Initialize Connectivity
	connectivity := NewConnectivity(ctx, log)
	connectivity.Start()
	defer connectivity.Stop()

	// Initialize DBus client
	dbusClient := godbus.NewDBusClient(ctx, log, DBUS_NAME)
	err = dbusClient.Start()
	if err != nil {
		log.Fatal("DBus init failed", zap.Error(err))
	}
	defer func() {
		_ = dbusClient.Stop()
	}()

	// Initialize Monitor
	monitor := metric.NewSysResMonitor(ctx, log)
	monitor.Start()
	defer monitor.Stop()

	// Initialize SysMonitordDBus
	sysMonitordDBus := NewSysMonitordDBus(connectivity, monitor, log)
	err = dbusClient.Export(sysMonitordDBus, DBUS_PATH, DBUS_INTERFACE)
	if err != nil {
		log.Fatal("DBus export failed", zap.Error(err))
	}

	// Initialize SysEventWatcher
	eventWatcher := NewSysEventWatcher(ctx, log)
	eventWatcher.Start()
	defer eventWatcher.Stop()

	// Initialize Mediator
	mediator := NewMediator(
		dbusClient,
		monitor,
		connectivity,
		eventWatcher,
		log,
	)
	mediator.Start()
	defer mediator.Stop()

	// Initialize Prometheus server
	promServer := NewPromServer(log)
	err = promServer.Start()
	if err != nil {
		log.Fatal("Prometheus server start failed", zap.Error(err))
	}
	defer func() {
		if err := promServer.Stop(); err != nil {
			log.Warn("Failed to stop promServer", zap.Error(err))
		}
	}()

	// send ready notification to systemd
	sent, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if err != nil {
		log.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		log.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	<-ctx.Done()
}
