package event

import (
	"context"
	"errors"
	"reflect"
	"sync"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	DBUS_SYS_MONITORD_EVENT_SYSMETRICS godbus.Member = "sysmetrics"
	DBUS_SYSTEM_EVENT                  godbus.Member = "sysevent"
)

// SystemMetrics represents the system metrics fired by the sys-monitord
type SysMetrics struct {
	CPU struct {
		CurrentTemperature float64 `json:"current_temperature"`
	} `json:"cpu"`
	Memory struct {
		MaxCapacity  float64 `json:"max_capacity"`
		UsedCapacity float64 `json:"used_capacity"`
	} `json:"memory"`
	Disk struct {
		TotalCapacity float64 `json:"total_capacity"`
		UsedCapacity  float64 `json:"used_capacity"`
	} `json:"disk"`
}

// MemoryUsagePercent calculates the memory usage percentage
func (sm SysMetrics) MemoryUsagePercent() (float64, error) {
	if sm.Memory.MaxCapacity == 0 {
		return 0, errors.New("max capacity is 0")
	}
	return sm.Memory.UsedCapacity / sm.Memory.MaxCapacity * 100, nil
}

// DiskUsagePercent calculates the disk usage percentage
func (sm SysMetrics) DiskUsagePercent() (float64, error) {
	if sm.Disk.TotalCapacity == 0 {
		return 0, errors.New("total capacity is 0")
	}
	return sm.Disk.UsedCapacity / sm.Disk.TotalCapacity * 100, nil
}

// Watcher watches the system metrics, events and triggers the appropriate actions
//
//go:generate mockgen -source=watcher.go -destination=../mocks/watcher.go -package=mocks -mock_names=Watcher=MockEventWatcher
type Watcher interface {
	// Start starts the watcher
	Start()

	// Stop stops the watcher
	Stop()
}

type watcher struct {
	mu sync.Mutex

	// Dependencies
	dbusClient    *godbus.DBusClient
	logger        *zap.Logger
	diskHandler   DiskHandler
	memoryHandler MemoryHandler
	gpuHandler    GPUHandler
	cpuHandler    CPUHandler
	json          wrapper.JSON

	// State
	isProcessingMetrics bool
}

func New(
	dbusClient *godbus.DBusClient,
	diskHandler DiskHandler,
	memoryHandler MemoryHandler,
	gpuHandler GPUHandler,
	cpuHandler CPUHandler,
	json wrapper.JSON,
	logger *zap.Logger) Watcher {
	return &watcher{
		dbusClient:    dbusClient,
		logger:        logger,
		diskHandler:   diskHandler,
		memoryHandler: memoryHandler,
		gpuHandler:    gpuHandler,
		cpuHandler:    cpuHandler,
		json:          json,
	}
}

func (w *watcher) Start() {
	w.dbusClient.OnBusSignal(w.handleDBusSignal)
}

func (w *watcher) Stop() {
	w.gpuHandler.CancelScheduledReboot()
	w.dbusClient.RemoveBusSignal(w.handleDBusSignal)
}

func (w *watcher) handleDBusSignal(
	ctx context.Context,
	payload godbus.DBusPayload) ([]interface{}, error) {
	if payload.Member.IsACK() {
		return nil, nil
	}

	switch payload.Member {
	case DBUS_SYSTEM_EVENT:
		if len(payload.Body) != 1 {
			w.logger.Error("Invalid number of arguments", zap.Int("expected", 1), zap.Int("actual", len(payload.Body)))
			return nil, nil
		}

		eventType, ok := payload.Body[0].(string)
		if !ok {
			w.logger.Error("Invalid body type", zap.String("expected", "string"), zap.String("actual", reflect.TypeOf(payload.Body[0]).String()))
			return nil, nil
		}

		w.gpuHandler.HandleEvent(ctx, eventType)

		return nil, nil
	case DBUS_SYS_MONITORD_EVENT_SYSMETRICS:
		if len(payload.Body) != 1 {
			w.logger.Error("Invalid number of arguments", zap.Int("expected", 1), zap.Int("actual", len(payload.Body)))
			return nil, nil
		}

		body, ok := payload.Body[0].([]byte)
		if !ok {
			w.logger.Error("Invalid body type", zap.String("expected", "[]byte"), zap.String("actual", reflect.TypeOf(payload.Body[0]).String()))
			return nil, nil
		}

		var metrics SysMetrics
		if err := w.json.Unmarshal(body, &metrics); err != nil {
			w.logger.Error("Failed to unmarshal metrics", zap.Error(err))
			return nil, nil
		}

		// Process system metrics
		w.processSystemMetrics(ctx, &metrics)
	}

	return nil, nil
}

// processSystemMetrics processes the system metrics
func (w *watcher) processSystemMetrics(ctx context.Context, metrics *SysMetrics) {
	w.mu.Lock()
	if w.isProcessingMetrics {
		w.mu.Unlock()
		return
	}

	w.isProcessingMetrics = true
	w.mu.Unlock()

	defer func() {
		w.mu.Lock()
		w.isProcessingMetrics = false
		w.mu.Unlock()
	}()

	// Handle memory usage
	memoryUsagePercent, err := metrics.MemoryUsagePercent()
	if err != nil {
		w.logger.Error("Failed to calculate memory usage percentage", zap.Error(err))
		return
	}
	w.memoryHandler.HandleUsage(ctx, memoryUsagePercent)

	// Handle disk usage
	diskUsagePercent, err := metrics.DiskUsagePercent()
	if err != nil {
		w.logger.Error("Failed to calculate disk usage percentage", zap.Error(err))
		return
	}
	w.diskHandler.HandleUsage(ctx, diskUsagePercent)

	// Handle CPU temperature
	w.cpuHandler.HandleTemperature(ctx, metrics.CPU.CurrentTemperature)
}
