package main

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

type ConnectivityHandler func(ctx context.Context, connected bool)

type Connectivity struct {
	sync.Mutex

	ctx           context.Context
	logger        *zap.Logger
	handlers      []ConnectivityHandler
	doneChan      chan struct{}
	lastConnected *bool
}

func NewConnectivity(ctx context.Context, logger *zap.Logger) *Connectivity {
	return &Connectivity{
		ctx:      ctx,
		logger:   logger,
		handlers: []ConnectivityHandler{},
		doneChan: make(chan struct{}),
	}
}

func (c *Connectivity) GetLastConnected() bool {
	c.Lock()
	defer c.Unlock()
	if c.lastConnected == nil {
		return false
	}
	return *c.lastConnected
}

func (c *Connectivity) Start() {
	c.logger.Info("Starting Connectivity Watcher",
		zap.Int("targets", len(PING_TARGET_ADDRESS)),
		zap.Duration("slow_interval", SLOW_PING_INTERVAL),
		zap.Duration("fast_interval", FAST_PING_INTERVAL),
	)
	c.background()
}

func (c *Connectivity) restart() {
	c.Stop()
	c.doneChan = make(chan struct{})
	c.Start()
}

func (c *Connectivity) Stop() {
	c.Lock()
	defer c.Unlock()

	select {
	case <-c.doneChan:
		c.logger.Info("Connectivity Watcher already stopped")
	default:
		close(c.doneChan)
	}
	c.logger.Info("Connectivity Watcher stopped")
}

func (c *Connectivity) OnConnectivityChange(handler ConnectivityHandler) {
	c.Lock()
	defer c.Unlock()
	c.handlers = append(c.handlers, handler)
}

func (c *Connectivity) RemoveConnectivityChange(h ConnectivityHandler) {
	c.Lock()
	defer c.Unlock()

	for i, handler := range c.handlers {
		if fmt.Sprintf("%p", handler) == fmt.Sprintf("%p", h) {
			c.handlers = append(c.handlers[:i], c.handlers[i+1:]...)
			break
		}
	}
}

// notifyHandlers notifies all registered handlers about connectivity status
func (c *Connectivity) notifyHandlers(ctx context.Context, connected bool) {
	c.Lock()
	handlers := make([]ConnectivityHandler, len(c.handlers))
	copy(handlers, c.handlers)
	c.Unlock()

	for _, handler := range handlers {
		go func(h ConnectivityHandler) {
			select {
			case <-ctx.Done():
				return
			case <-c.doneChan:
				return
			default:
				h(ctx, connected)
			}
		}(handler)
	}
}

func (c *Connectivity) background() {
	go func() {
		c.logger.Info("Connectivity background goroutine started")

		// Get the last connected state
		c.Lock()
		lastConnected := c.lastConnected
		c.Unlock()

		// Always check connectivity for the first time
		if lastConnected == nil {
			connected, err := c.CheckConnectivity(BACKGROUND_PING_TIMEOUT)
			if err != nil {
				// We accept not being able to check connectivity and only log the warning
				c.logger.Warn("Connectivity check failed", zap.Error(err))
			}
			c.Lock()
			c.lastConnected = &connected
			lastConnected = c.lastConnected
			c.Unlock()

			c.logger.Info("Initial connectivity state determined", zap.Bool("connected", connected))

			c.notifyHandlers(c.ctx, connected)
		}

		// determine the interval based on the initial connectivity
		interval := SLOW_PING_INTERVAL
		if lastConnected == nil || !*lastConnected {
			interval = FAST_PING_INTERVAL
		}

		ticker := time.NewTicker(interval)
		defer ticker.Stop()

		c.logger.Debug("Ticker started", zap.Duration("interval secs", interval))

		for {
			select {
			case <-c.ctx.Done():
				c.logger.Info("Connectivity background goroutine stopped")
				return
			case <-c.doneChan:
				c.logger.Info("Connectivity Watcher stopped")
				return
			case <-ticker.C:
				c.logger.Info("Checking connectivity")
				connected, err := c.CheckConnectivity(BACKGROUND_PING_TIMEOUT)
				c.logger.Info("Connectivity check result", zap.Bool("connected", connected))
				if err != nil {
					// We accept not being able to check connectivity and only log the warning
					c.logger.Warn("Connectivity check failed", zap.Error(err))
					continue
				}

				c.Lock()
				lastConnected := c.lastConnected
				c.lastConnected = &connected
				c.Unlock()

				if lastConnected != nil && connected != *lastConnected {
					c.logger.Info("Connectivity state changed",
						zap.Bool("previous_connected", *lastConnected),
						zap.Bool("current_connected", connected),
					)
					c.notifyHandlers(c.ctx, connected)

					// restart the background goroutine when connectivity changes
					time.Sleep(200 * time.Millisecond) // Add a small delay
					c.restart()

					return
				}
			}
		}
	}()
}

// CheckConnectivity attempts to connect to the PING_TARGET address to check connectivity
func (c *Connectivity) CheckConnectivity(timeout time.Duration) (bool, error) {
	ctx, cancel := context.WithTimeout(c.ctx, timeout+1*time.Second)
	defer cancel()

	eg, egCtx := errgroup.WithContext(ctx)
	type targetResult struct {
		target string
		ok     bool
	}
	resultChan := make(chan targetResult, len(PING_TARGET_ADDRESS))

	for _, target := range PING_TARGET_ADDRESS {
		t := target
		eg.Go(func() error {
			before := time.Now()
			dialer := net.Dialer{Timeout: timeout}
			conn, err := dialer.DialContext(egCtx, "tcp", t)
			after := time.Now()
			c.logger.Debug("Connectivity check result", zap.String("target", t), zap.Duration("duration", after.Sub(before)), zap.Error(err))
			if conn != nil {
				if err := conn.Close(); err != nil {
					c.logger.Warn("Failed to close connection", zap.Error(err))
				}
			}

			resultChan <- targetResult{target: t, ok: err == nil}

			return err
		})
	}

	err := eg.Wait()
	if err != nil {
		// We accept not being able to check connectivity and only log the warning
		c.logger.Warn("Connectivity check failed", zap.Error(err))
	}

	connected := false
	successfulTarget := ""
	for range PING_TARGET_ADDRESS {
		result := <-resultChan
		if result.ok {
			connected = true
			if successfulTarget == "" {
				successfulTarget = result.target
			}
			break
		}
	}

	c.logger.Info("Connectivity check summary",
		zap.Bool("connected", connected),
		zap.String("successful_target", successfulTarget),
		zap.Duration("timeout", timeout),
	)

	return connected, nil
}
