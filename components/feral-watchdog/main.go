package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/feral-file/godbus"
	"github.com/getsentry/sentry-go"
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

	// Create context for graceful shutdown
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Initialize logger with debug enabled for development
	log, err := logger.New(debug)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Failed to initialize logger: %s\n", err)
		os.Exit(1)
	}
	defer func() {
		_ = log.Sync()
	}()

	if err := LoadConfig(log); err != nil {
		log.Error("Failed to load config.", zap.Error(err))
		return
	}
	log.Info("Configuration loaded successfully.")

	// Initialize Sentry if configured
	if config.SentryConfig.IsEnabled() {
		sc, err := sentry.NewClient(sentry.ClientOptions{
			Dsn:              config.SentryConfig.DSN,
			Debug:            config.SentryConfig.GetDebug(),
			SampleRate:       config.SentryConfig.GetSampleRate(),
			Environment:      config.SentryConfig.Environment,
			Release:          config.SentryConfig.Release,
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
				zap.String("environment", config.SentryConfig.Environment),
				zap.String("release", config.SentryConfig.Release))
			defer logger.FlushSentry(2 * time.Second)
		}
	} else {
		log.Info("Sentry not configured, using basic logger")
	}

	// Handle signals for graceful shutdown
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		log.Info("Received signal, initiating shutdown...",
			zap.String("signal", sig.String()))
		cancel()
	}()

	// Initialize DBus client
	mo := dbus.WithMatchPathNamespace(dbus.ObjectPath("/com/feralfile/sysmonitord"))
	dbusClient := godbus.NewDBusClient(ctx, log, DBUS_NAME, mo)
	err = dbusClient.Start()
	if err != nil {
		log.Fatal("DBus init failed", zap.Error(err))
	}
	defer func() {
		_ = dbusClient.Stop()
	}()

	// Initialize vmagent client
	var vmagentURL string
	if config.VmagentConfig != nil && config.VmagentConfig.URL != "" {
		vmagentURL = config.VmagentConfig.URL
	}
	vmagentClient := NewVmagentClient(vmagentURL, log)

	// Initialize system command executor
	commandHandler := NewCommandHandler(log, vmagentClient)

	// Initialize CDP client
	cdpClient := cdp.NewDefault(&cdp.Config{Endpoint: config.CDPConfig.Endpoint}, log)
	err = cdpClient.Init(ctx)
	if err != nil {
		log.Fatal("CDP init failed", zap.Error(err))
	}
	defer cdpClient.Close()

	// Initialize resource monitors
	ramHandler := NewMemoryHandler(log, commandHandler)
	diskHandler := NewDiskHandler(log, commandHandler)
	gpuHandler := NewGPUHandler(log, commandHandler)
	cpuHandler := NewCPUHandler(log, cdpClient)
	defer gpuHandler.GracefulShutdown(ctx)

	// Initialize mediator
	mediator := NewMediator(dbusClient, diskHandler, ramHandler, gpuHandler, cpuHandler, log)
	mediator.Start()
	defer mediator.Stop()

	// Create a WaitGroup to track all the monitoring goroutines
	var wg sync.WaitGroup

	// Start systemd watchdog
	systemdWatchdog := NewSystemdWatchdog(log)
	wg.Add(1)
	go func() {
		defer wg.Done()
		systemdWatchdog.Start(ctx)
	}()

	// Start Chromium monitor
	chromiumMonitor := NewChromiumMonitor(config.CDPConfig.Endpoint, log, commandHandler)
	defer chromiumMonitor.Stop()
	wg.Add(1)
	go func() {
		defer wg.Done()
		chromiumMonitor.Start(ctx)
	}()

	// Start Systemd monitor
	systemdMonitor := NewSystemdMonitor(cdpClient, log, commandHandler, vmagentClient)
	wg.Add(1)
	go func() {
		defer wg.Done()
		systemdMonitor.Start(ctx)
	}()

	// Notify systemd that we're ready
	if err := systemdWatchdog.NotifyReady(); err != nil {
		log.Warn("Failed to notify systemd, but continuing", zap.Error(err))
	}

	// Block until context is done (cancel is called)
	<-ctx.Done()
	log.Info("Shutdown signal received, cleaning up...")

	// Wait for all goroutines to finish (with timeout)
	waitCh := make(chan struct{})
	go func() {
		wg.Wait()
		close(waitCh)
	}()

	select {
	case <-waitCh:
		log.Info("All goroutines have terminated cleanly")
	case <-time.After(GOROUTINE_TIMEOUT):
		log.Warn("Some goroutines did not terminate in time")
	}

	log.Info("feral-watchdog daemon shutdown complete")
}
