// main.go
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/feral-file/ffos-user/components/feral-app-monitord/logger"

	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/feral-file/godbus"
	dbus_v5 "github.com/godbus/dbus/v5"
	"go.uber.org/zap"
)

const (
	WATCHDOG_INTERVAL = 15 * time.Second
	SHUTDOWN_TIMEOUT  = 2 * time.Second
)

var (
	debug      = false
	log        *zap.Logger
	ctx        context.Context
	dbusClient *godbus.DBusClient
)

func main() {
	// Create context for graceful shutdown
	c, cancel := context.WithCancel(context.Background())
	ctx = c
	defer cancel()

	// Initialize logger with debug enabled for development
	basicLogger, err := logger.New(debug)
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	defer func() {
		_ = basicLogger.Sync()
	}()
	log = basicLogger

	if err := LoadConfig(); err != nil {
		log.Error("Failed to load config.", zap.Error(err))
		return
	}
	log.Info("Configuration loaded successfully.")

	if config.AppMonitordConfig.SentryConfig.IsEnabled() {
		err = logger.InitSentry(config.AppMonitordConfig.SentryConfig)
		if err != nil {
			basicLogger.Error("Failed to initialize Sentry", zap.Error(err))
		}
		finalLogger, err := logger.NewWithSentry(debug, config.AppMonitordConfig.SentryConfig)
		if err != nil {
			log.Error("Failed to create Sentry-integrated logger, falling back to basic logger", zap.Error(err))
		} else {
			log = finalLogger
			log.Info("Sentry initialized successfully",
				zap.String("environment", config.AppMonitordConfig.SentryConfig.Environment),
				zap.String("release", config.AppMonitordConfig.SentryConfig.Release))
			defer logger.FlushSentry(2 * time.Second)
		}
	} else {
		log.Info("Sentry not configured, using basic logger")
	}
	defer func() {
		_ = log.Sync()
	}()

	// Test Sentry integration - intentionally trigger errors for testing
	log.Info("Testing Sentry integration...")

	// Test warning level
	log.Warn("This is a test warning for Sentry",
		zap.String("component", "feral-app-monitord"),
		zap.String("test", "sentry-integration"))

	// Test error level
	log.Error("This is a test error for Sentry",
		zap.Error(errors.New("intentional test error")),
		zap.String("component", "feral-app-monitord"),
		zap.String("test", "sentry-integration"))

	// Test panic recovery
	defer func() {
		if r := recover(); r != nil {
			log.Error("Recovered from panic",
				zap.Any("panic", r),
				zap.String("component", "feral-app-monitord"),
				zap.String("test", "sentry-integration"))
		}
	}()

	// Trigger a panic for testing
	if config.AppMonitordConfig.SentryConfig.IsEnabled() {
		log.Info("Triggering test panic for Sentry testing...")
		panic("intentional test panic for Sentry integration testing")
	}

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

	// Initialize DBus client
	mo := dbus_v5.WithMatchPathNamespace(dbus_v5.ObjectPath("/com/feralfile"))
	dbusClient = godbus.NewDBusClient(c, log, DBUS_NAME, mo)
	err = dbusClient.Start()
	if err != nil {
		log.Fatal("DBus init failed", zap.Error(err))
	}
	defer func() {
		_ = dbusClient.Stop()
	}()

	// Initialize AppMonitordDBus
	appMonitordDBus := NewAppMonitordDBus(log)
	err = dbusClient.Export(appMonitordDBus, DBUS_PATH, DBUS_INTERFACE)
	if err != nil {
		log.Fatal("DBus export failed", zap.Error(err))
	}

	// send ready notification to systemd
	sent, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if err != nil {
		log.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		log.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	// Create a ticker that fires every minute.
	ticker := time.NewTicker(30 * time.Minute)
	defer ticker.Stop()

	// Run the first heartbeat immediately without waiting for the ticker.
	log.Info("Performing initial heartbeat...")
	if !CheckConnectivity() {
		log.Warn("Network not connected. Skipping heartbeat.")
	} else {
		SendHeartbeat()
	}

	// Enter a loop to run the heartbeat on each tick.
	for range ticker.C {
		log.Info("Ticker fired, performing heartbeat...")
		if !CheckConnectivity() {
			log.Warn("Network not connected. Skipping heartbeat.")
		} else {
			SendHeartbeat()
		}
	}
}
