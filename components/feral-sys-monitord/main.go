package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/feral-file/godbus"
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
	logger, err := New(debug)
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer func() {
		_ = logger.Sync()
	}()

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal, initiating shutdown...",
			zap.String("signal", sig.String()))
		cancel()

		time.Sleep(SHUTDOWN_TIMEOUT)
		logger.Error("Shutdown timed out, forcing exit...",
			zap.Duration("timeout", SHUTDOWN_TIMEOUT))
		os.Exit(1)
	}()

	// Start watchdog in a goroutine
	watchdog := NewWatchdog(WATCHDOG_INTERVAL, logger)
	go watchdog.Start(ctx)
	defer watchdog.Stop()

	// Initialize Connectivity
	connectivity := NewConnectivity(ctx, logger)
	connectivity.Start()
	defer connectivity.Stop()

	// Initialize DBus client
	dbusClient := godbus.NewDBusClient(ctx, logger, DBUS_NAME)
	err = dbusClient.Start()
	if err != nil {
		logger.Fatal("DBus init failed", zap.Error(err))
	}
	defer func() {
		_ = dbusClient.Stop()
	}()

	// Initialize Monitor
	monitor := metric.NewSysResMonitor(ctx, logger)
	monitor.Start()
	defer monitor.Stop()

	// Initialize SysMonitordDBus
	sysMonitordDBus := NewSysMonitordDBus(connectivity, monitor, logger)
	err = dbusClient.Export(sysMonitordDBus, DBUS_PATH, DBUS_INTERFACE)
	if err != nil {
		logger.Fatal("DBus export failed", zap.Error(err))
	}

	// Initialize SysEventWatcher
	eventWatcher := NewSysEventWatcher(ctx, logger)
	eventWatcher.Start()
	defer eventWatcher.Stop()

	// Initialize Mediator
	mediator := NewMediator(
		dbusClient,
		monitor,
		connectivity,
		eventWatcher,
		logger,
	)
	mediator.Start()
	defer mediator.Stop()

	// Initialize Prometheus server
	promServer := NewPromServer(logger)
	err = promServer.Start()
	if err != nil {
		logger.Fatal("Prometheus server start failed", zap.Error(err))
	}
	defer promServer.Stop()

	// send ready notification to systemd
	sent, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if err != nil {
		logger.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		logger.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	<-ctx.Done()
}
