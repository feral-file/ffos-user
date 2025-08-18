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
	logger, err := New(debug)
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer func() {
		_ = logger.Sync()
	}()

	logger.Info("Starting feral-watchdog daemon")
	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logger.Info("Received signal, initiating shutdown...",
			zap.String("signal", sig.String()))
		cancel()
	}()

	// Load configuration
	config, err := LoadConfig(logger)
	if err != nil {
		logger.Fatal("Failed to load configuration", zap.Error(err))
	}

	// Initialize DBus client
	mo := dbus.WithMatchPathNamespace(dbus.ObjectPath("/com/feralfile/sysmonitord"))
	dbusClient := godbus.NewDBusClient(ctx, logger, DBUS_NAME, mo)
	err = dbusClient.Start()
	if err != nil {
		logger.Fatal("DBus init failed", zap.Error(err))
	}
	defer func() {
		_ = dbusClient.Stop()
	}()

	// Initialize system command executor
	commandHandler := NewCommandHandler(logger)

	// Initialize CDP client
	cdpClient := cdp.NewDefault(&cdp.Config{Endpoint: config.CDPEndpoint}, logger)
	err = cdpClient.Init(ctx)
	if err != nil {
		logger.Fatal("CDP init failed", zap.Error(err))
	}
	defer cdpClient.Close()

	// Initialize resource monitors
	ramHandler := NewMemoryHandler(logger, commandHandler)
	diskHandler := NewDiskHandler(logger, commandHandler)
	gpuHandler := NewGPUHandler(logger, commandHandler)
	cpuHandler := NewCPUHandler(logger, cdpClient)
	defer gpuHandler.GracefulShutdown(ctx)

	// Initialize mediator
	mediator := NewMediator(dbusClient, diskHandler, ramHandler, gpuHandler, cpuHandler, logger)
	mediator.Start()
	defer mediator.Stop()

	// Create a WaitGroup to track all the monitoring goroutines
	var wg sync.WaitGroup

	// Start systemd watchdog
	systemdWatchdog := NewSystemdWatchdog(logger)
	wg.Add(1)
	go func() {
		defer wg.Done()
		systemdWatchdog.Start(ctx)
	}()

	// Start Chromium monitor
	chromiumMonitor := NewChromiumMonitor(config.CDPEndpoint, logger, commandHandler)
	defer chromiumMonitor.Stop()
	wg.Add(1)
	go func() {
		defer wg.Done()
		chromiumMonitor.Start(ctx)
	}()

	// Notify systemd that we're ready
	if err := systemdWatchdog.NotifyReady(); err != nil {
		logger.Warn("Failed to notify systemd, but continuing", zap.Error(err))
	}

	// Block until context is done (cancel is called)
	<-ctx.Done()
	logger.Info("Shutdown signal received, cleaning up...")

	// Wait for all goroutines to finish (with timeout)
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		logger.Info("All goroutines have terminated cleanly")
	case <-time.After(GOROUTINE_TIMEOUT):
		logger.Warn("Some goroutines did not terminate in time")
	}

	logger.Info("feral-watchdog daemon shutdown complete")
}
