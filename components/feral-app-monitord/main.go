// main.go
package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"
	"time"

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
	logger     *zap.Logger
	ctx        context.Context
	dbusClient *godbus.DBusClient
)

func main() {
	// Create context for graceful shutdown
	c, cancel := context.WithCancel(context.Background())
	ctx = c
	defer cancel()

	// Initialize logger with debug enabled for development
	l, err := NewLogger(debug)
	if err != nil {
		panic("Failed to initialize logger: " + err.Error())
	}
	logger = l
	defer func() {
		_ = logger.Sync()
	}()

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

	if err := EnsureKeyPair(); err != nil {
		logger.Error("Failed to ensure key pair exists.", zap.Error(err))
		return
	}
	logger.Info("Key pair check passed.")

	if err := LoadConfig(); err != nil {
		logger.Error("Failed to load config.", zap.Error(err))
		return
	}
	logger.Info("Configuration loaded successfully.")

	// Initialize DBus client
	mo := dbus_v5.WithMatchPathNamespace(dbus_v5.ObjectPath("/com/feralfile"))
	dbusClient = godbus.NewDBusClient(c, l, DBUS_NAME, mo)
	err = dbusClient.Start()
	if err != nil {
		l.Fatal("DBus init failed", zap.Error(err))
	}
	defer func() {
		_ = dbusClient.Stop()
	}()

	// Initialize AppMonitordDBus
	appMonitordDBus := NewAppMonitordDBus(logger)
	err = dbusClient.Export(appMonitordDBus, DBUS_PATH, DBUS_INTERFACE)
	if err != nil {
		logger.Fatal("DBus export failed", zap.Error(err))
	}

	// send ready notification to systemd
	sent, err := daemon.SdNotify(false, daemon.SdNotifyReady)
	if err != nil {
		logger.Error("Failed to notify systemd", zap.Error(err))
	}
	if !sent {
		logger.Warn("Failed to notify systemd, notification not supported. It could because NOTIFY_SOCKET is unset")
	}

	// Create a ticker that fires every minute.
	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	// Run the first heartbeat immediately without waiting for the ticker.
	logger.Info("Performing initial heartbeat...")
	if !CheckConnectivity() {
		logger.Warn("Network not connected. Skipping heartbeat.")
	} else {
		SendHeartbeat()
	}

	// Enter a loop to run the heartbeat on each tick.
	for range ticker.C {
		logger.Info("Ticker fired, performing heartbeat...")
		if !CheckConnectivity() {
			logger.Warn("Network not connected. Skipping heartbeat.")
		} else {
			SendHeartbeat()
		}
	}
}
