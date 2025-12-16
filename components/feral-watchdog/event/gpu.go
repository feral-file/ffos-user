package event

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/command"
	"github.com/feral-file/ffos-user/components/feral-watchdog/vmagent"
	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	REBOOT_DELAY = 15 * time.Second

	// GPU events
	GPU_HANGING_SIGNAL = "gpu_hanging"
	GPU_RECOVER_SIGNAL = "gpu_recover"
)

// GPUHandler handles GPU events and do appropriate actions based on the events.
//
//go:generate mockgen -source=gpu.go -destination=../mocks/gpu.go -package=mocks -mock_names=GPUHandler=MockGPUHandler
type GPUHandler interface {
	// HandleEvent handles the GPU event
	HandleEvent(ctx context.Context, event string)

	// CancelScheduledReboot cancels the scheduled reboot
	CancelScheduledReboot()
}

type gpuHandler struct {
	mu sync.Mutex

	// Dependencies
	commandExec command.Executor
	clock       wrapper.Clock
	logger      *zap.Logger

	// State
	rebootTimer     *time.Timer
	rebootScheduled bool
}

func NewGPUHandler(logger *zap.Logger, commandExec command.Executor, clock wrapper.Clock) GPUHandler {
	return &gpuHandler{
		logger:          logger,
		commandExec:     commandExec,
		clock:           clock,
		rebootScheduled: false,
	}
}

func (h *gpuHandler) HandleEvent(ctx context.Context, event string) {
	switch event {
	case GPU_HANGING_SIGNAL:
		h.handleHangingGPU(ctx)
	case GPU_RECOVER_SIGNAL:
		h.handleGPURecovery(ctx)
	}
}

// handleHangingGPU handles the hanging GPU event
func (h *gpuHandler) handleHangingGPU(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()

	// If a reboot is already scheduled, ignore this request
	if h.rebootScheduled {
		h.logger.Info("GPU: reboot already scheduled, ignoring request")
		return
	}

	h.logger.Info("GPU: scheduling reboot")
	h.rebootScheduled = true

	// Create a timer to reboot after a delay
	h.rebootTimer = h.clock.AfterFunc(REBOOT_DELAY, func() {
		select {
		case <-ctx.Done():
			h.logger.Info("GPU: context canceled, skipping reboot")
		default:
			h.mu.Lock()
			h.rebootScheduled = false
			h.rebootTimer = nil
			h.mu.Unlock()

			h.logger.Info("GPU: executing reboot")
			if err := h.commandExec.RebootSystem(ctx, vmagent.CrashReasonGPUHang); err != nil {
				h.logger.Warn("GPU: failed to reboot system", zap.Error(err))
			}
		}
	})
}

// handleGPURecovery handles the GPU recovery event
func (h *gpuHandler) handleGPURecovery(ctx context.Context) {
	h.mu.Lock()
	isRebootScheduled := h.rebootScheduled
	h.mu.Unlock()

	if isRebootScheduled {
		h.cancelScheduledReboot()
		h.restartPlayer(ctx)
	}
}

func (h *gpuHandler) cancelScheduledReboot() {
	h.mu.Lock()
	defer h.mu.Unlock()

	if !h.rebootScheduled {
		h.logger.Info("GPU: no reboot scheduled, nothing to cancel")
		return
	}

	h.logger.Info("GPU: canceling scheduled reboot")
	if h.rebootTimer == nil {
		h.logger.Warn("GPU: timer is nil, cannot cancel")
		return
	}

	stopped := h.rebootTimer.Stop()
	if !stopped {
		h.logger.Warn("GPU: timer already fired, cannot cancel")
		return
	}

	h.rebootScheduled = false
	h.rebootTimer = nil
}

func (h *gpuHandler) restartPlayer(ctx context.Context) {
	h.logger.Info("GPU: restarting player")
	if err := h.commandExec.RestartKiosk(ctx); err != nil {
		h.logger.Warn("GPU: failed to restart kiosk", zap.Error(err))
	}
}

func (h *gpuHandler) CancelScheduledReboot() {
	h.cancelScheduledReboot()
}
