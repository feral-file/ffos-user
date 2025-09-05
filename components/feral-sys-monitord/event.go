package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type Event string

const (
	EVENT_GPU_HANGING       Event = "gpu_hanging"
	EVENT_GPU_RECOVER       Event = "gpu_recover"
	EVENT_DISPLAY_CONNECTED Event = "display_connected"
)

type EventHandler func(event Event)

type SysEventWatcher struct {
	sync.Mutex

	ctx      context.Context
	logger   *zap.Logger
	doneChan chan struct{}
	handlers []EventHandler
}

type SystemEventResult struct {
	EventType Event
}

func NewSysEventWatcher(ctx context.Context, logger *zap.Logger) *SysEventWatcher {
	return &SysEventWatcher{
		ctx:      ctx,
		logger:   logger,
		doneChan: make(chan struct{}),
	}
}

func (p *SysEventWatcher) OnEvent(handler EventHandler) {
	p.Lock()
	defer p.Unlock()
	p.handlers = append(p.handlers, handler)
}

func (p *SysEventWatcher) RemoveEventHandler(handler EventHandler) {
	p.Lock()
	defer p.Unlock()
	for i, h := range p.handlers {
		if fmt.Sprintf("%p", h) == fmt.Sprintf("%p", handler) {
			p.handlers = append(p.handlers[:i], p.handlers[i+1:]...)
			return
		}
	}
}

func (p *SysEventWatcher) notifyHandlers(ctx context.Context, event Event) {
	p.Lock()
	handlers := make([]EventHandler, len(p.handlers))
	copy(handlers, p.handlers)
	p.Unlock()

	for _, handler := range handlers {
		go func(h EventHandler) {
			select {
			case <-ctx.Done():
				return
			case <-p.doneChan:
				return
			default:
				h(event)
			}
		}(handler)
	}
}

func (p *SysEventWatcher) Start() {
	p.logger.Info("SysEventWatcher starting in the background")
	go func() {
		systemEventChan := make(chan SystemEventResult)
		defer close(systemEventChan)
		errChan := make(chan error)
		defer close(errChan)
		go p.monitorDisplayEvents(p.ctx, systemEventChan, errChan)
		go p.monitorGPUEvents(p.ctx, systemEventChan, errChan)

		for {
			select {
			case <-p.doneChan:
				p.logger.Info("SysEventWatcher stopped")
				return
			case <-p.ctx.Done():
				p.logger.Info("SysEventWatcher background goroutine stopped")
				return
			case err := <-errChan:
				p.logger.Error("System event monitoring error", zap.Error(err))
				continue
			case eventResult := <-systemEventChan:
				p.logger.Info("System event detected", zap.String("event", string(eventResult.EventType)))
				p.notifyHandlers(p.ctx, eventResult.EventType)
			}
		}
	}()
}

func (p *SysEventWatcher) monitorGPUEvents(ctx context.Context, resultChan chan<- SystemEventResult, errChan chan<- error) {
	cmd := exec.CommandContext(ctx, "sudo", "journalctl", "--lines=0", "-f", "-k", "-g", "i915")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		errChan <- fmt.Errorf("failed to create stdout pipe for GPU monitoring: %w", err)
		return
	}

	err = cmd.Start()
	if err != nil {
		errChan <- fmt.Errorf("failed to start GPU monitoring: %w", err)
		return
	}
	defer func() {
		_ = cmd.Wait()
	}()

	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return
		case <-p.doneChan:
			return
		case <-ticker.C:
			line := sc.Text()
			if strings.Contains(line, "GPU HANG") {
				resultChan <- SystemEventResult{EventType: EVENT_GPU_HANGING}
			} else if strings.Contains(line, "GUC: submission enabled") {
				resultChan <- SystemEventResult{EventType: EVENT_GPU_RECOVER}
			}
		}
	}

	if err := sc.Err(); err != nil {
		errChan <- fmt.Errorf("error reading GPU events: %w", err)
	}
}

func (p *SysEventWatcher) monitorDisplayEvents(ctx context.Context, resultChan chan<- SystemEventResult, errChan chan<- error) {
	p.logger.Info("Starting display event listener with udevadm monitor")

	// Check if udevadm is available
	if _, err := exec.LookPath("udevadm"); err != nil {
		p.logger.Warn("udevadm not found, display monitoring disabled", zap.Error(err))
		errChan <- fmt.Errorf("udevadm not found: %w", err)
		return
	}

	// Use udevadm monitor to watch for DRM subsystem changes
	cmd := exec.CommandContext(ctx, "udevadm", "monitor", "--kernel", "--subsystem-match=drm")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		p.logger.Error("Failed to create stdout pipe for display monitoring", zap.Error(err))
		errChan <- fmt.Errorf("failed to create stdout pipe for display monitoring: %w", err)
		return
	}

	p.logger.Info("Starting udevadm monitor command")
	err = cmd.Start()
	if err != nil {
		p.logger.Error("Failed to start udevadm command", zap.Error(err), zap.String("stderr", stderr.String()))
		errChan <- fmt.Errorf("failed to start display monitoring: %w", err)
		return
	}
	defer func() {
		p.logger.Info("Cleaning up udevadm command")
		if waitErr := cmd.Wait(); waitErr != nil {
			p.logger.Error("udevadm command failed", zap.Error(waitErr), zap.String("stderr", stderr.String()))
		}
	}()

	p.logger.Info("udevadm monitor started successfully, waiting for display events...")

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return
		case <-p.doneChan:
			return
		default:
			line := sc.Text()
			p.logger.Debug("Received udevadm output", zap.String("line", line))

			if strings.Contains(line, "change") {
				p.logger.Debug("Change event detected", zap.String("line", line))

				// Check HDMI connection status
				displayStatePath := "card1-HDMI-A-1"
				statusPath := fmt.Sprintf("/sys/class/drm/%s/status", displayStatePath)

				if statusData, err := os.ReadFile(statusPath); err == nil {
					status := strings.TrimSpace(string(statusData))
					p.logger.Debug("HDMI status check",
						zap.String("path", displayStatePath),
						zap.String("status", status),
						zap.String("event", line))

					if status == "connected" {
						p.logger.Info("Display connected, triggering state restore")
						resultChan <- SystemEventResult{EventType: EVENT_DISPLAY_CONNECTED}
					}
				} else {
					p.logger.Debug("Could not read HDMI status",
						zap.String("path", statusPath),
						zap.Error(err))
				}
			}
		}
	}

	if err := sc.Err(); err != nil {
		errChan <- fmt.Errorf("error reading display events: %w", err)
	}
}

func (p *SysEventWatcher) Stop() {
	select {
	case <-p.doneChan:
		return
	default:
		close(p.doneChan)
	}

	p.logger.Info("SysEventWatcher stopped")
}
