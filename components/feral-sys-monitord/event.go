package main

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"
)

type Event string

const (
	EVENT_GPU_HANGING Event = "gpu_hanging"
	EVENT_GPU_RECOVER Event = "gpu_recover"
)

type EventHandler func(event Event)

type SysEventWatcher struct {
	sync.Mutex

	ctx      context.Context
	logger   *zap.Logger
	doneChan chan struct{}
	handlers []EventHandler
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
		gpuHangChan := make(chan bool)
		defer close(gpuHangChan)
		errChan := make(chan error)
		defer close(errChan)
		go p.monitorHangingGPU(p.ctx, gpuHangChan, errChan)

		for {
			select {
			case <-p.doneChan:
				p.logger.Info("SysEventWatcher stopped")
				return
			case <-p.ctx.Done():
				p.logger.Info("SysEventWatcher background goroutine stopped")
				return
			case err := <-errChan:
				p.logger.Error("GPU hanging event error", zap.Error(err))
				continue
			case isHanging := <-gpuHangChan:
				if isHanging {
					p.notifyHandlers(p.ctx, EVENT_GPU_HANGING)
				} else {
					p.notifyHandlers(p.ctx, EVENT_GPU_RECOVER)
				}
			}
		}
	}()
}

// classifyAMDGPUJournalLine maps an amdgpu kernel journal line to a GPU event.
// amdgpu reports hangs as ring timeouts ("*ERROR* ring ... timeout") followed
// by "GPU reset begin!", and reports successful recovery as
// "GPU reset(N) succeeded!". Lines without those markers return ok=false.
func classifyAMDGPUJournalLine(line string) (event Event, ok bool) {
	if strings.Contains(line, "GPU reset begin") ||
		(strings.Contains(line, "*ERROR* ring") && strings.Contains(line, "timeout")) {
		return EVENT_GPU_HANGING, true
	}
	if strings.Contains(line, "GPU reset") && strings.Contains(line, "succeeded") {
		return EVENT_GPU_RECOVER, true
	}
	return "", false
}

func (p *SysEventWatcher) monitorHangingGPU(ctx context.Context, resultChan chan<- bool, errChan chan<- error) {
	// Follow the kernel journal for the amdgpu hang/recover markers (see
	// classifyAMDGPUJournalLine) to drive the events consumed by the kiosk
	// watchdog.
	cmd := exec.CommandContext(ctx, "sudo", "journalctl", "--lines=0", "-f", "-k", "-g", "amdgpu")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		errChan <- err
		return
	}

	err = cmd.Start()
	if err != nil {
		errChan <- err
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
			event, ok := classifyAMDGPUJournalLine(sc.Text())
			if !ok {
				continue
			}
			resultChan <- event == EVENT_GPU_HANGING
		}
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
