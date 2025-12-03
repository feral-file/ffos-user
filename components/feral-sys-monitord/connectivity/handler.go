package connectivity

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

const (
	// Ping interval in seconds
	SLOW_PING_INTERVAL = 30 * time.Second
	FAST_PING_INTERVAL = 3 * time.Second

	// Connection timeout
	BACKGROUND_PING_TIMEOUT = 5 * time.Second
	RPC_PING_TIMEOUT        = 2 * time.Second
)

var PING_TARGET_ADDRESS = []string{
	"1.1.1.1:443", // Cloudflare
	"8.8.8.8:443", // Google
}

type HandlerFunc func(ctx context.Context, connected bool)

//go:generate mockgen -source=handler.go -destination=../mocks/connectivity.go -package=mocks -mock_names=Handler=MockConnectivityHandler
type Handler interface {
	// GetLastConnected gets the last connected state
	GetLastConnected() bool

	// Start starts the handler
	Start()

	// Stop stops the handler
	Stop()

	// AddHandler adds a callback for connectivity changes
	AddHandler(f HandlerFunc)

	// RemoveHandler removes a callback for connectivity changes
	RemoveHandler(f HandlerFunc)

	// CheckConnectivity checks the current connectivity status
	CheckConnectivity(timeout time.Duration) (bool, error)
}

type handler struct {
	sync.Mutex

	ctx           context.Context
	logger        *zap.Logger
	handlers      []HandlerFunc
	doneChan      chan struct{}
	lastConnected *bool
}

func NewHandler(ctx context.Context, logger *zap.Logger) Handler {
	return &handler{
		ctx:      ctx,
		logger:   logger,
		handlers: []HandlerFunc{},
		doneChan: make(chan struct{}),
	}
}

func (h *handler) GetLastConnected() bool {
	h.Lock()
	defer h.Unlock()
	if h.lastConnected == nil {
		return false
	}
	return *h.lastConnected
}

func (h *handler) Start() {
	h.logger.Info("Starting Connectivity Watcher")
	h.background()
}

func (h *handler) restart() {
	h.Stop()
	h.doneChan = make(chan struct{})
	h.Start()
}

func (h *handler) Stop() {
	h.Lock()
	defer h.Unlock()

	select {
	case <-h.doneChan:
		h.logger.Info("Connectivity Watcher already stopped")
	default:
		close(h.doneChan)
	}
	h.logger.Info("Connectivity Watcher stopped")
}

func (h *handler) AddHandler(handler HandlerFunc) {
	h.Lock()
	defer h.Unlock()
	h.handlers = append(h.handlers, handler)
}

func (h *handler) RemoveHandler(f HandlerFunc) {
	h.Lock()
	defer h.Unlock()

	for i, handler := range h.handlers {
		if fmt.Sprintf("%p", handler) == fmt.Sprintf("%p", f) {
			h.handlers = append(h.handlers[:i], h.handlers[i+1:]...)
			break
		}
	}
}

// notifyHandlers notifies all registered handlers about connectivity status
func (h *handler) notifyHandlers(ctx context.Context, connected bool) {
	h.Lock()
	handlers := make([]HandlerFunc, len(h.handlers))
	copy(handlers, h.handlers)
	h.Unlock()

	for _, handler := range handlers {
		go func(f HandlerFunc) {
			select {
			case <-ctx.Done():
				return
			case <-h.doneChan:
				return
			default:
				f(ctx, connected)
			}
		}(handler)
	}
}

func (h *handler) background() {
	go func() {
		h.logger.Info("Connectivity background goroutine started")

		// Get the last connected state
		h.Lock()
		lastConnected := h.lastConnected
		h.Unlock()

		// Always check connectivity for the first time
		if lastConnected == nil {
			connected, err := h.CheckConnectivity(BACKGROUND_PING_TIMEOUT)
			if err != nil {
				// We accept not being able to check connectivity and only log the warning
				h.logger.Warn("Connectivity check failed", zap.Error(err))
			}
			h.Lock()
			h.lastConnected = &connected
			lastConnected = h.lastConnected
			h.Unlock()

			h.notifyHandlers(h.ctx, connected)
		}

		// determine the interval based on the initial connectivity
		interval := SLOW_PING_INTERVAL
		if lastConnected == nil || !*lastConnected {
			interval = FAST_PING_INTERVAL
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		h.logger.Debug("Ticker started", zap.Duration("interval secs", interval))

		for {
			select {
			case <-h.ctx.Done():
				h.logger.Info("Connectivity background goroutine stopped")
				return
			case <-h.doneChan:
				h.logger.Info("Connectivity Watcher stopped")
				return
			case <-ticker.C:
				h.logger.Info("Checking connectivity")
				connected, err := h.CheckConnectivity(BACKGROUND_PING_TIMEOUT)
				h.logger.Info("Connectivity check result", zap.Bool("connected", connected))
				if err != nil {
					// We accept not being able to check connectivity and only log the warning
					h.logger.Warn("Connectivity check failed", zap.Error(err))
					continue
				}

				h.Lock()
				lastConnected := h.lastConnected
				h.lastConnected = &connected
				h.Unlock()

				if lastConnected != nil && connected != *lastConnected {
					h.notifyHandlers(h.ctx, connected)

					// restart the background goroutine when connectivity changes
					time.Sleep(200 * time.Millisecond) // Add a small delay
					h.restart()

					return
				}
			}
		}
	}()
}

// CheckConnectivity attempts to connect to the PING_TARGET address to check connectivity
func (h *handler) CheckConnectivity(timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(h.ctx, timeout+1*time.Second)
	defer cancel()

	eg, egCtx := errgroup.WithContext(ctx)
	resultChan := make(chan bool, len(PING_TARGET_ADDRESS))

	for _, target := range PING_TARGET_ADDRESS {
		t := target
		eg.Go(func() error {
			before := time.Now()
			dialer := net.Dialer{Timeout: timeout}
			conn, err := dialer.DialContext(egCtx, "tcp", t)
			after := time.Now()
			h.logger.Debug("Connectivity check result", zap.String("target", t), zap.Duration("duration", after.Sub(before)), zap.Error(err))
			if conn != nil {
				if err := conn.Close(); err != nil {
					h.logger.Warn("Failed to close connection", zap.Error(err))
				}
			}

			resultChan <- err == nil

			return err
		})
	}

	err := eg.Wait()
	if err != nil {
		// We accept not being able to check connectivity and only log the warning
		h.logger.Warn("Connectivity check failed", zap.Error(err))
	}

	connected := false
	for range PING_TARGET_ADDRESS {
		result := <-resultChan
		if result {
			connected = true
			break
		}
	}

	return connected, nil
}
