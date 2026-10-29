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

	// Initialize DBus client. Match the common /com/feralfile ancestor, not
	// sysmonitord's and controld's object paths as two separate
	// WithMatchPathNamespace options: AddMatchSignalContext joins every
	// option into ONE bus match-rule string, so two different
	// path_namespace keys in that one rule collapse to a single predicate
	// rather than an OR — the first match attempt for (ffos-user#356)
	// controld's EVENT_CDP_STUCK did exactly that and would have silently
	// dropped one of the two namespaces (which one is bus-implementation
	// dependent). components/feral-controld/main.go already solved this
	// identical problem the same way for its own match rule — follow that
	// precedent here instead of inventing a second one.
	mo := dbus.WithMatchPathNamespace(dbus.ObjectPath("/com/feralfile"))
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

	// Initialize CDP client. Chromium owns the CDP socket lifecycle and can be
	// temporarily absent during boot, OTA, kiosk restarts, or crash recovery. The
	// watchdog must keep its own recovery monitors alive in those states instead
	// of letting systemd restart this process and amplify Sentry noise.
	cdpClient := cdp.NewDefault(&cdp.Config{Endpoint: config.CDPConfig.Endpoint}, log)
	initCDPBestEffort(ctx, cdpClient, log)
	defer cdpClient.Close()

	// Initialize resource monitors
	ramHandler := NewMemoryHandler(log, commandHandler)
	diskHandler := NewDiskHandler(log, commandHandler)
	gpuHandler := NewGPUHandler(log, commandHandler)
	cpuHandler := NewCPUHandler(log, cdpClient)
	defer gpuHandler.GracefulShutdown(ctx)

	// Chromium monitor is constructed before the mediator so the mediator
	// can route feral-controld's EVENT_CDP_STUCK signal to it
	// (ffos-user#356) — this daemon stays the sole place that decides to
	// restart chromium-kiosk.service or reboot.
	chromiumMonitor := NewChromiumMonitor(config.CDPConfig.Endpoint, log, commandHandler)
	defer chromiumMonitor.Stop()

	// Initialize mediator
	mediator := NewMediator(dbusClient, diskHandler, ramHandler, gpuHandler, cpuHandler, chromiumMonitor, log)
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

func initCDPBestEffort(ctx context.Context, cdpClient cdp.ClientInterface, log *zap.Logger) bool {
	if err := cdpClient.Init(ctx); err != nil {
		log.Warn("CDP init unavailable; watchdog will retry on demand", zap.Error(err))
		return false
	}
	return true
}
