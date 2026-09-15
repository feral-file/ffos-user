package main

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"

	"go.uber.org/zap"
)

const (
	SYSTEMD_SERVICE_HANG_THRESHOLD_SECOND int64 = 90

	SYSTEMD_SERVICE_STATUS_ACTIVE   SystemdServiceStatus = "active"
	SYSTEMD_SERVICE_STATUS_FAILED   SystemdServiceStatus = "failed"
	SYSTEMD_SERVICE_STATUS_INACTIVE SystemdServiceStatus = "inactive"

	// KIOSK_FALLBACK_UNIT is the ffos system unit that shows the stable
	// "Something went wrong..." screen (plymouth on the free DRM device) once
	// the Chromium restart budget is exhausted. chromium-kiosk.service stops
	// it in ExecStartPre, so any kiosk start clears the screen again.
	KIOSK_FALLBACK_UNIT = "feral-kiosk-fallback.service"
)

// kioskFallbackResult is the outcome of showKioskFallback. The three cases
// need different policy: shown arms the hold, unavailable means the kiosk was
// stopped but no screen came up (reboot now), busy means nothing happened at
// all because another kiosk operation held the lock (retry next tick).
type kioskFallbackResult int

const (
	kioskFallbackShown kioskFallbackResult = iota
	kioskFallbackUnavailable
	kioskFallbackBusy
)

type SystemdServiceStatus string

func (s SystemdServiceStatus) AsPointer() *SystemdServiceStatus {
	return &s
}

// CommandHandler implements system health checking and remediation actions
type CommandHandler struct {
	logger         *zap.Logger
	vmagentClient  *VmagentClient
	mu             sync.Mutex
	isCleaningDisk bool
	// kioskOpInFlight serializes every operation on chromium-kiosk.service
	// (restartKiosk, showKioskFallback): the RAM and GPU handlers restart the
	// kiosk from their own goroutines, and a restart interleaved with the
	// fallback sequence would leave plymouth holding DRM master under a
	// crash-looping kiosk.
	kioskOpInFlight bool
	// fallbackShown is true while feral-kiosk-fallback.service is up on
	// purpose (Chromium restart budget exhausted). restartKiosk refuses while
	// it is set — the kiosk was stopped deliberately and its ExecStartPre
	// would erase the customer's error screen — until the Chromium monitor
	// clears it (recovery, or the hold abandoned for headless).
	fallbackShown bool
}

func NewCommandHandler(logger *zap.Logger, vmagentClient *VmagentClient) *CommandHandler {
	return &CommandHandler{
		logger:        logger,
		vmagentClient: vmagentClient,
	}
}

// restartKiosk attempts to restart the chromium-kiosk service. It is a no-op
// while another kiosk operation is in flight and while the fallback screen is
// deliberately showing (see fallbackShown).
func (c *CommandHandler) restartKiosk(ctx context.Context) {
	c.mu.Lock()
	if c.kioskOpInFlight {
		c.mu.Unlock()
		return
	}
	if c.fallbackShown {
		c.mu.Unlock()
		c.logger.Info("Kiosk restart refused: fallback screen is showing after Chromium restart budget exhaustion")
		return
	}

	c.kioskOpInFlight = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.kioskOpInFlight = false
		c.mu.Unlock()
	}()

	cmd := exec.CommandContext(ctx, "systemctl", "--user", "restart", "chromium-kiosk.service")
	if output, err := cmd.CombinedOutput(); err != nil {
		c.logger.Error("Failed to restart chromium-kiosk service",
			zap.Error(err),
			zap.ByteString("output", output))
	} else {
		c.logger.Info("Successfully restarted chromium-kiosk service")
	}
}

// showKioskFallback stops the kiosk and starts the fallback screen unit,
// reporting the outcome (see kioskFallbackResult). The kiosk must be stopped
// first: cage holds DRM master and plymouth cannot draw beside it, and a
// kiosk left in Restart=always would tear the screen down again on its next
// attempt. kioskFallbackUnavailable means the caller must fall back to the
// immediate reboot (the unit is absent on images that predate it — the
// watchdog ships on the package rail independently of the image — or sudo
// refused); the kiosk has already been stopped by then, so holding for 15
// minutes on a black screen would be strictly worse than the old behavior.
// kioskFallbackBusy means nothing was done: a RAM/GPU restart held the lock.
func (c *CommandHandler) showKioskFallback(ctx context.Context) kioskFallbackResult {
	c.mu.Lock()
	if c.kioskOpInFlight {
		c.mu.Unlock()
		c.logger.Warn("Kiosk fallback not shown: another kiosk operation is in flight")
		return kioskFallbackBusy
	}
	c.kioskOpInFlight = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.kioskOpInFlight = false
		c.mu.Unlock()
	}()

	stop := exec.CommandContext(ctx, "systemctl", "--user", "stop", "chromium-kiosk.service")
	if output, err := stop.CombinedOutput(); err != nil {
		c.logger.Error("Failed to stop chromium-kiosk service before fallback",
			zap.Error(err),
			zap.ByteString("output", output))
	}

	start := exec.CommandContext(ctx, "sudo", "-n", "systemctl", "start", KIOSK_FALLBACK_UNIT)
	if output, err := start.CombinedOutput(); err != nil {
		c.logger.Error("Failed to start kiosk fallback screen",
			zap.String("unit", KIOSK_FALLBACK_UNIT),
			zap.Error(err),
			zap.ByteString("output", output))
		return kioskFallbackUnavailable
	}
	c.mu.Lock()
	c.fallbackShown = true
	c.mu.Unlock()
	c.logger.Warn("Kiosk fallback screen shown", zap.String("unit", KIOSK_FALLBACK_UNIT))
	return kioskFallbackShown
}

// clearKioskFallback forgets that the fallback screen is up, re-enabling
// restartKiosk. Called by the Chromium monitor when Chromium recovers during
// the hold, when the hold ends in a reboot, or when the hold is abandoned
// because the display went away. It does not stop the unit: the kiosk's own
// ExecStartPre does that on the next start.
func (c *CommandHandler) clearKioskFallback() {
	c.mu.Lock()
	c.fallbackShown = false
	c.mu.Unlock()
}

// isFallbackShown reports whether the fallback screen is deliberately up.
func (c *CommandHandler) isFallbackShown() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fallbackShown
}

// isKioskActivating reports whether chromium-kiosk.service is currently in
// systemd's "activating" sub-state. It is consulted by the Chromium hang
// detector to suppress redundant restarts while systemd is mid-restart —
// whether the restart was issued by us, by chromium-kiosk.service's own
// Restart=always policy, or by an external actor (OTA, operator).
//
// Failure modes are deliberately treated as "not activating" rather than
// surfaced as an error: this is a defensive check on the escalation path,
// and a systemctl outage should not block the watchdog from acting if
// Chromium is genuinely hung.
//
// `systemctl is-active` returns non-zero for any non-active state including
// "activating", so we ignore the exit code and parse stdout. `is-active`
// prints the SubState-equivalent ("active", "activating", "failed", ...)
// on a single line.
func (c *CommandHandler) isKioskActivating(ctx context.Context) bool {
	cmd := exec.CommandContext(ctx, "systemctl", "--user", "is-active", "chromium-kiosk.service")
	output, _ := cmd.Output()
	return strings.TrimSpace(string(output)) == "activating"
}

// rebootSystem initiates a system reboot
func (c *CommandHandler) rebootSystem(ctx context.Context, reason CrashReason) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Send crash_reboot metric to vmagent before rebooting
	if c.vmagentClient != nil {
		c.vmagentClient.SendCrashRebootMetric(ctx, reason)
	} else {
		c.logger.Warn("Vmagent client is nil, skipping crash_reboot metric")
	}

	cmd := exec.CommandContext(ctx, "sudo", "systemctl", "reboot")
	if output, err := cmd.CombinedOutput(); err != nil {
		c.logger.Error("Failed to reboot system",
			zap.Error(err),
			zap.ByteString("output", output))
	}
}

func (c *CommandHandler) cleanupPacmanCache(ctx context.Context) {
	c.mu.Lock()
	if c.isCleaningDisk {
		c.mu.Unlock()
		return
	}

	c.isCleaningDisk = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isCleaningDisk = false
		c.mu.Unlock()
	}()

	// Clean pacman cache
	cmd := exec.CommandContext(ctx, "sudo", "pacman", "-Scc", "--noconfirm")
	if output, err := cmd.CombinedOutput(); err != nil {
		c.logger.Error("Failed to clean pacman cache",
			zap.Error(err),
			zap.ByteString("output", output))
	}
}

func (c *CommandHandler) checkSystemdUserServiceStatus(ctx context.Context, serviceName string) (*SystemdServiceStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !systemdServices[serviceName] {
		c.logger.Error("unauthorized service name",
			zap.String("service", serviceName))
		return nil, fmt.Errorf("unauthorized service: %s", serviceName)
	}

	cmd := exec.CommandContext(ctx, "systemctl",
		"--user", "show", serviceName,
		"--property=ActiveState,ExecMainExitTimestampMonotonic",
		"--no-page")

	output, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Error("Failed to check service status",
			zap.String("service", serviceName),
			zap.Error(err),
			zap.ByteString("output", output))
		return nil, err
	}

	statusMap := make(map[string]string)
	lines := string(output)
	for _, line := range splitLines(lines) {
		if kv := splitKV(line); kv != nil {
			statusMap[kv[0]] = kv[1]
		}
	}
	state, ok := statusMap["ActiveState"]
	if !ok {
		return nil, errors.New("ActiveState not found in service status")
	}

	switch state {
	case "activating", "active":
		return SYSTEMD_SERVICE_STATUS_ACTIVE.AsPointer(), nil
	case "failed":
		tsStr, ok := statusMap["ExecMainExitTimestampMonotonic"]
		if !ok || tsStr == "" {
			return nil, errors.New("ExecMainExitTimestampMonotonic not found in service status")
		}

		nowCmd := exec.CommandContext(ctx, "cat", "/proc/uptime")
		nowOut, nowErr := nowCmd.CombinedOutput()
		if nowErr != nil {
			c.logger.Error("Failed to get system uptime",
				zap.Error(nowErr),
				zap.ByteString("output", nowOut))
			return nil, nowErr
		}
		var uptimeSec float64
		_, scanErr := fmt.Sscanf(string(nowOut), "%f", &uptimeSec)
		if scanErr != nil {
			c.logger.Error("Failed to parse uptime",
				zap.Error(scanErr))
			return nil, scanErr
		}
		var exitTsMicros int64
		_, tsErr := fmt.Sscanf(tsStr, "%d", &exitTsMicros)
		if tsErr != nil {
			c.logger.Error("Failed to parse ExecMainExitTimestampMonotonic",
				zap.Error(tsErr))
			return nil, tsErr
		}
		uptimeMicros := int64(uptimeSec * 1e6)
		if (uptimeMicros-exitTsMicros)/1e6 < SYSTEMD_SERVICE_HANG_THRESHOLD_SECOND {
			c.logger.Warn("Service is in failed state but within hang threshold, might be restarting",
				zap.String("service", serviceName),
				zap.Int64("failed_seconds", (uptimeMicros-exitTsMicros)/1e6))
			return SYSTEMD_SERVICE_STATUS_ACTIVE.AsPointer(), nil
		}
		return SYSTEMD_SERVICE_STATUS_FAILED.AsPointer(), nil
	case "inactive", "deactivating":
		return SYSTEMD_SERVICE_STATUS_INACTIVE.AsPointer(), nil
	default:
		return nil, fmt.Errorf("unknown service state: %s", state)
	}
}

func splitLines(s string) []string {
	var lines []string
	start := 0
	for i := range s {
		if s[i] == '\n' {
			lines = append(lines, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		lines = append(lines, s[start:])
	}
	return lines
}

func splitKV(s string) []string {
	for i := range s {
		if s[i] == '=' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return nil
}
