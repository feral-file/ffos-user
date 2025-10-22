package main

import (
	"context"
	"flag"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/feral-file/godbus"
	"github.com/godbus/dbus/v5"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/logger"
	"github.com/feral-file/ffos-user/components/feral-watchdog/packages/cdp"
)

const (
	// Timeouts
	GOROUTINE_TIMEOUT = 1500 * time.Millisecond // 1.5 seconds

	DBUS_NAME = "com.feralfile.watchdog"
)

var debug = false

func main() {
	// Read from options
	flag.BoolVar(&debug, "debug", false, "Enable debug mode")
	flag.Parse()

	// Initialize logger
	basicLogger, err := logger.New(debug)
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer func() {
		_ = basicLogger.Sync()
	}()

	// Initialize Sentry if needed
	err = logger.InitSentry(config.SentryConfig)
	if err != nil {
		basicLogger.Error("Failed to initialize Sentry", zap.Error(err))
		// Don't fail the application if Sentry initialization fails
	}

	basicLogger.Info("Starting feral-watchdog daemon")
	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Load configuration
	config, err := LoadConfig(basicLogger)
	if err != nil {
		basicLogger.Fatal("Failed to load configuration", zap.Error(err))
	}

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

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		finalLogger.Info("Received signal, initiating shutdown...",
			zap.String("signal", sig.String()))
		cancel()
	}()

	// Initialize DBus client
	mo := dbus.WithMatchPathNamespace(dbus.ObjectPath("/com/feralfile/sysmonitord"))
	dbusClient := godbus.NewDBusClient(ctx, finalLogger, DBUS_NAME, mo)
	err = dbusClient.Start()
	if err != nil {
		finalLogger.Fatal("DBus init failed", zap.Error(err))
	}
	defer func() {
		_ = dbusClient.Stop()
	}()

	// Initialize system command executor
	commandHandler := NewCommandHandler(finalLogger)

	// Initialize CDP client
	cdpClient := cdp.NewDefault(&cdp.Config{Endpoint: config.CDPConfig.Endpoint}, finalLogger)
	err = cdpClient.Init(ctx)
	if err != nil {
		finalLogger.Fatal("CDP init failed", zap.Error(err))
	}
	defer cdpClient.Close()

	// Initialize resource monitors
	ramHandler := NewMemoryHandler(finalLogger, commandHandler)
	diskHandler := NewDiskHandler(finalLogger, commandHandler)
	gpuHandler := NewGPUHandler(finalLogger, commandHandler)
	cpuHandler := NewCPUHandler(finalLogger, cdpClient)
	defer gpuHandler.GracefulShutdown(ctx)

	// Initialize mediator
	mediator := NewMediator(dbusClient, diskHandler, ramHandler, gpuHandler, cpuHandler, finalLogger)
	mediator.Start()
	defer mediator.Stop()

	// Create a WaitGroup to track all the monitoring goroutines
	var wg sync.WaitGroup

	// Start systemd watchdog
	systemdWatchdog := NewSystemdWatchdog(finalLogger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		systemdWatchdog.Start(ctx)
	}()

	// Start Chromium monitor
	chromiumMonitor := NewChromiumMonitor(config.CDPConfig.Endpoint, finalLogger, commandHandler)
	defer chromiumMonitor.Stop()
	wg.Add(1)
	go func() {
		defer wg.Done()
		chromiumMonitor.Start(ctx)
	}()

	// Start Systemd monitor
	systemdMonitor := NewSystemdMonitor(cdpClient, finalLogger, commandHandler)
	wg.Add(1)
	go func() {
		defer wg.Done()
		systemdMonitor.Start(ctx)
	}()

	// Notify systemd that we're ready
	if err := systemdWatchdog.NotifyReady(); err != nil {
		finalLogger.Warn("Failed to notify systemd, but continuing", zap.Error(err))
	}

	// Block until context is done (cancel is called)
	<-ctx.Done()
	finalLogger.Info("Shutdown signal received, cleaning up...")

	// Wait for all goroutines to finish (with timeout)
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		finalLogger.Info("All goroutines have terminated cleanly")
	case <-time.After(GOROUTINE_TIMEOUT):
		finalLogger.Warn("Some goroutines did not terminate in time")
	}

	finalLogger.Info("feral-watchdog daemon shutdown complete")
}
