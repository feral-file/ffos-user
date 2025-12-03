package event

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/wrapper"
)

type Event string

const (
	EVENT_GPU_HANGING Event = "gpu_hanging"
	EVENT_GPU_RECOVER Event = "gpu_recover"
)

type HandlerFunc func(event Event)

//go:generate mockgen -source=watcher.go -destination=../mocks/watcher.go -package=mocks -mock_names=Watcher=MockWatcher
type Watcher interface {
	// AddHandler adds a callback for system events
	AddHandler(f HandlerFunc)

	// RemoveHandler removes a callback for system events
	RemoveHandler(f HandlerFunc)

	// Start starts the system event watcher
	Start()

	// Stop stops the system event watcher
	Stop()
}

type watcher struct {
	sync.Mutex

	ctx      context.Context
	logger   *zap.Logger
	doneChan chan struct{}
	handlers []HandlerFunc

	// Dependencies
	clock wrapper.Clock
	exec  wrapper.Exec
}

func NewWatcher(ctx context.Context, logger *zap.Logger, clock wrapper.Clock, exec wrapper.Exec) *watcher {
	return &watcher{
		ctx:      ctx,
		logger:   logger,
		doneChan: make(chan struct{}),
		clock:    clock,
		exec:     exec,
	}
}

func (w *watcher) AddHandler(f HandlerFunc) {
	w.Lock()
	defer w.Unlock()
	w.handlers = append(w.handlers, f)
}

func (w *watcher) RemoveHandler(f HandlerFunc) {
	w.Lock()
	defer w.Unlock()
	for i, h := range w.handlers {
		if fmt.Sprintf("%p", h) == fmt.Sprintf("%p", f) {
			w.handlers = append(w.handlers[:i], w.handlers[i+1:]...)
			return
		}
	}
}

func (w *watcher) notifyHandlers(ctx context.Context, event Event) {
	w.Lock()
	handlers := make([]HandlerFunc, len(w.handlers))
	copy(handlers, w.handlers)
	w.Unlock()

	for _, handler := range handlers {
		go func(h HandlerFunc) {
			select {
			case <-ctx.Done():
				return
			case <-w.doneChan:
				return
			default:
				h(event)
			}
		}(handler)
	}
}

func (w *watcher) Start() {
	w.logger.Info("Event watcher starting in the background")
	go func() {
		gpuHangChan := make(chan bool)
		defer close(gpuHangChan)
		errChan := make(chan error)
		defer close(errChan)
		go w.monitorHangingGPU(w.ctx, gpuHangChan, errChan)

		for {
			select {
			case <-w.doneChan:
				w.logger.Info("Event watcher stopped")
				return
			case <-w.ctx.Done():
				w.logger.Info("Event watcher background goroutine stopped")
				return
			case err := <-errChan:
				w.logger.Error("GPU hanging event error", zap.Error(err))
				continue
			case isHanging := <-gpuHangChan:
				if isHanging {
					w.notifyHandlers(w.ctx, EVENT_GPU_HANGING)
				} else {
					w.notifyHandlers(w.ctx, EVENT_GPU_RECOVER)
				}
			}
		}
	}()
}

func (w *watcher) monitorHangingGPU(ctx context.Context, resultChan chan<- bool, errChan chan<- error) {
	// TODO check the GPU type and use the appropriate command
	// This only works for Intel GPUs
	cmd := w.exec.CommandContext(ctx, "sudo", "journalctl", "--lines=0", "-f", "-k", "-g", "i915")
	var stderr bytes.Buffer
	cmd.SetStderr(&stderr)
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

	ticker := w.clock.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	sc := bufio.NewScanner(stdout)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return
		case <-w.doneChan:
			return
		case <-ticker.C():
			line := sc.Text()
			if strings.Contains(line, "GPU HANG") {
				resultChan <- true
			} else if strings.Contains(line, "GUC: submission enabled") {
				resultChan <- false
			}
		}
	}
}

func (w *watcher) Stop() {
	select {
	case <-w.doneChan:
		return
	default:
		close(w.doneChan)
	}

	w.logger.Info("Event watcher stopped")
}
