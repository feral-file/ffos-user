package command

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/vmagent"
	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	SYSTEMD_SERVICE_HANG_THRESHOLD_SECOND int64 = 90

	// Systemd service status
	SYSTEMD_SERVICE_STATUS_ACTIVE   SystemdServiceStatus = "active"
	SYSTEMD_SERVICE_STATUS_FAILED   SystemdServiceStatus = "failed"
	SYSTEMD_SERVICE_STATUS_INACTIVE SystemdServiceStatus = "inactive"
)

type SystemdServiceStatus string

func (s SystemdServiceStatus) AsPointer() *SystemdServiceStatus {
	return &s
}

//go:generate mockgen -source=executor.go -destination=../mocks/executor.go -package=mocks -mock_names=Executor=MockCommandExecutor
type Executor interface {
	// RestartKiosk attempts to restart the chromium-kiosk service
	RestartKiosk(ctx context.Context) error

	// RebootSystem initiates a system reboot
	RebootSystem(ctx context.Context, reason vmagent.CrashReason) error

	// CleanupPacmanCache cleans up the pacman cache
	CleanupPacmanCache(ctx context.Context) error

	// CheckSystemdUserServiceStatus checks the status of a systemd user service
	CheckSystemdUserServiceStatus(ctx context.Context, serviceName string) (*SystemdServiceStatus, error)
}

type executor struct {
	mu sync.Mutex

	// Dependencies
	exec          wrapper.Exec
	vmagentClient vmagent.Client
	logger        *zap.Logger

	// State
	isRestartingKiosk bool
	isCleaningDisk    bool
}

func NewCommandExecutor(logger *zap.Logger, vmagentClient vmagent.Client, exec wrapper.Exec) Executor {
	return &executor{
		logger:        logger,
		vmagentClient: vmagentClient,
		exec:          exec,
	}
}

func (c *executor) RestartKiosk(ctx context.Context) error {
	c.mu.Lock()
	if c.isRestartingKiosk {
		c.mu.Unlock()
		return nil
	}

	c.isRestartingKiosk = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isRestartingKiosk = false
		c.mu.Unlock()
	}()

	cmd := c.exec.CommandContext(ctx, "systemctl", "--user", "restart", "chromium-kiosk.service")
	if output, err := cmd.CombinedOutput(); err != nil {
		c.logger.Warn("Failed to restart chromium-kiosk service",
			zap.Error(err),
			zap.ByteString("output", output))
		return err
	}

	return nil
}

func (c *executor) RebootSystem(ctx context.Context, reason vmagent.CrashReason) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Send crash_reboot metric to vmagent before rebooting
	if c.vmagentClient != nil {
		if err := c.vmagentClient.SendCrashRebootMetric(ctx, reason); err != nil {
			c.logger.Warn("Failed to send crash_reboot metric", zap.Error(err))
		}
	} else {
		c.logger.Warn("Vmagent client is nil, skipping crash_reboot metric")
	}

	cmd := c.exec.CommandContext(ctx, "sudo", "systemctl", "reboot")
	if output, err := cmd.CombinedOutput(); err != nil {
		c.logger.Warn("Failed to reboot system",
			zap.Error(err),
			zap.ByteString("output", output))
		return err
	}

	return nil
}

func (c *executor) CleanupPacmanCache(ctx context.Context) error {
	c.mu.Lock()
	if c.isCleaningDisk {
		c.mu.Unlock()
		return nil
	}

	c.isCleaningDisk = true
	c.mu.Unlock()

	defer func() {
		c.mu.Lock()
		c.isCleaningDisk = false
		c.mu.Unlock()
	}()

	// Clean pacman cache
	cmd := c.exec.CommandContext(ctx, "sudo", "pacman", "-Scc", "--noconfirm")
	if output, err := cmd.CombinedOutput(); err != nil {
		c.logger.Warn("Failed to clean pacman cache",
			zap.Error(err),
			zap.ByteString("output", output))
		return err
	}

	return nil
}

func (c *executor) CheckSystemdUserServiceStatus(ctx context.Context, svc string) (*SystemdServiceStatus, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	cmd := c.exec.CommandContext(ctx, "systemctl",
		"--user", "show", svc,
		"--property=ActiveState,ExecMainExitTimestampMonotonic",
		"--no-page")

	output, err := cmd.CombinedOutput()
	if err != nil {
		c.logger.Error("Failed to check service status",
			zap.String("service", svc),
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

		nowCmd := c.exec.CommandContext(ctx, "cat", "/proc/uptime")
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
				zap.String("service", svc),
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
