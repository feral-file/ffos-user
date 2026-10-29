package main

import (
	"context"
	"fmt"
	"net"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-sys-monitord/metric"
)

const (
	// Ping interval in seconds
	SLOW_PING_INTERVAL = 30 * time.Second
	FAST_PING_INTERVAL = 3 * time.Second

	// Connection timeout
	PING_TIMEOUT = 5 * time.Second
)

// Reachability targets, dialed in two stages (happy-eyeballs style).
//
// PRIMARY_PING_TARGETS are dialed first. FALLBACK_PING_TARGETS are dialed only
// when no primary target has connected within PING_FALLBACK_DELAY, or as soon
// as every primary target has failed. The first successful TCP connect from
// either stage wins, and primary dials stay in flight after the fallback
// starts, so a slow-but-working primary path still counts. Adding the fallback
// can therefore only turn a false "offline" into "online", never the reverse.
//
// Why the fallback exists: the Google pair is unreachable from every
// mainland-China network (the firewall blocks the prefixes outright), which
// left a device on working office Wi-Fi narrating "no internet access"
// forever (feral-file#3539). The AliDNS and DNSPod resolvers answer TCP 443
// (their DoH endpoints) from inside and outside the mainland.
//
// Why it is a fallback rather than a peer: dialing the mainland resolvers on
// every probe would send every device worldwide a TCP connect to Chinese
// infrastructure every 30 s, which enterprise and venue IDS/GeoIP policies
// flag or silently drop. Staging keeps that traffic to networks where Google
// is already failing. Do not collapse the two lists back into one.
var PRIMARY_PING_TARGETS = []string{
	"8.8.8.8:443",
	"8.8.4.4:443",
}

var FALLBACK_PING_TARGETS = []string{
	"223.5.5.5:443",
	"1.12.12.12:443",
}

// PING_FALLBACK_DELAY is how long the primary stage runs alone before the
// fallback stage starts. A TCP connect to anycast 8.8.8.8 finishes well under
// 1 s on any working link, while a blackholed prefix (the mainland case)
// never answers. The whole check, fallback included, still ends at the
// caller's timeout: controld's GetConnectivityStatus caller gives the D-Bus
// call 7 s against PING_TIMEOUT's 5 s, so staging must not extend the total.
const PING_FALLBACK_DELAY = 1 * time.Second

type ConnectivityHandler func(ctx context.Context, connected bool)

type Connectivity struct {
	sync.Mutex

	ctx           context.Context
	logger        *zap.Logger
	handlers      []ConnectivityHandler
	doneChan      chan struct{}
	lastConnected *bool

	// probe performs one reachability check. Defaults to CheckConnectivity;
	// a seam so the generation-guard regression tests can block a probe
	// deterministically without dialing real ping targets. Set once at
	// construction, never mutated after.
	probe func(timeout time.Duration) (bool, error)

	// dial performs one TCP connect to a probe target. Defaults to a
	// net.Dialer bounded by the per-target timeout; a seam so the staged-probe
	// tests can script refusals, blackholes, and slow successes without real
	// sockets. Set once at
	// construction, never mutated after.
	dial func(ctx context.Context, target string, timeout time.Duration) (net.Conn, error)

	// fallbackDelay is PING_FALLBACK_DELAY in production; a field so tests
	// can make the stage boundary deterministic. Set once at construction,
	// never mutated after.
	fallbackDelay time.Duration
}

func NewConnectivity(ctx context.Context, logger *zap.Logger) *Connectivity {
	c := &Connectivity{
		ctx:      ctx,
		logger:   logger,
		handlers: []ConnectivityHandler{},
		doneChan: make(chan struct{}),
	}
	c.probe = c.CheckConnectivity
	c.fallbackDelay = PING_FALLBACK_DELAY
	c.dial = func(ctx context.Context, target string, timeout time.Duration) (net.Conn, error) {
		dialer := net.Dialer{Timeout: timeout}
		return dialer.DialContext(ctx, "tcp", target)
	}
	return c
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
		zap.Strings("primary_targets", PRIMARY_PING_TARGETS),
		zap.Strings("fallback_targets", FALLBACK_PING_TARGETS),
		zap.Duration("slow_interval", SLOW_PING_INTERVAL),
		zap.Duration("fast_interval", FAST_PING_INTERVAL),
	)
	c.background()
}

func (c *Connectivity) restart() {
	c.Stop()
	c.resetDone()
	c.Start()
}

// resetDone replaces the generation channel under the lock: notifyHandlers'
// goroutines and background's capture read c.doneChan, so an unlocked swap
// here is a data race with them. Split from restart so the swap is
// individually exercisable in the concurrency regression test without
// spawning the real ping loop.
func (c *Connectivity) resetDone() {
	c.Lock()
	defer c.Unlock()
	c.doneChan = make(chan struct{})
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
	// Capture the generation channel under the same lock as the handlers copy:
	// restart() swaps c.doneChan (via resetDone), so the spawned goroutines
	// must not read the mutable field directly — that is a data race, and a
	// notification from the OLD generation gating on the NEW channel would
	// also outlive the stop it belongs to.
	c.Lock()
	handlers := make([]ConnectivityHandler, len(c.handlers))
	copy(handlers, c.handlers)
	done := c.doneChan
	c.Unlock()

	for _, handler := range handlers {
		go func(h ConnectivityHandler) {
			select {
			case <-ctx.Done():
				return
			case <-done:
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

		// Capture THIS watcher generation's done channel: restart() swaps
		// c.doneChan, so reading the field from the loop races the swap — and a
		// check already in flight across a stop/restart must not apply its
		// stale result as current state.
		c.Lock()
		done := c.doneChan
		c.Unlock()

		// Get the last connected state
		c.Lock()
		lastConnected := c.lastConnected
		c.Unlock()

		// Always check connectivity for the first time
		if lastConnected == nil {
			probeStart := time.Now()
			connected, err := c.probe(PING_TIMEOUT)
			probeDuration := time.Since(probeStart)
			if err != nil {
				// We accept not being able to check connectivity and only log the warning
				c.logger.Warn("Connectivity check failed", zap.Error(err))
			}
			// Same generation guard as the ticker branch below: the watcher may
			// have been stopped/restarted while this probe was dialing, and a
			// retired generation applying its result would overwrite the
			// replacement watcher's state and emit a stale transition.
			select {
			case <-c.ctx.Done():
				return
			case <-done:
				c.logger.Info("Connectivity Watcher stopped before initial state applied; discarding stale probe result")
				return
			default:
			}
			c.Lock()
			c.lastConnected = &connected
			lastConnected = c.lastConnected
			c.Unlock()

			// Export only APPLIED verdicts (past the generation guard above),
			// so the Prometheus timeline can never disagree with the state the
			// D-Bus consumers were told about.
			metric.SetInternetReachability(connected, probeDuration)

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
			case <-done:
				c.logger.Info("Connectivity Watcher stopped")
				return
			case <-ticker.C:
				c.logger.Info("Checking connectivity")
				probeStart := time.Now()
				connected, err := c.probe(PING_TIMEOUT)
				probeDuration := time.Since(probeStart)
				// The watcher may have been stopped/restarted while the check
				// was dialing (the change-triggered restart): its result belongs
				// to the OLD generation and must not be applied or logged as
				// current — a stale "offline" here would flap the whole stack.
				select {
				case <-done:
					c.logger.Debug("Discarding connectivity result from stopped watcher",
						zap.Bool("connected", connected))
					return
				default:
				}
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

				// Same rule as the initial-state site: only applied verdicts
				// (past the guard and the error check) reach the gauges.
				metric.SetInternetReachability(connected, probeDuration)

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

// CheckConnectivity reports whether any reachability target accepts a TCP
// connect within timeout. It returns on the first success instead of waiting
// for every dial: a blackholed target (SYNs silently dropped) would otherwise
// hold every check for the full timeout even when another target answered in
// milliseconds, which eats into controld's 7 s D-Bus deadline. The error
// return is always nil; an unreachable network is a false verdict, not an
// error.
func (c *Connectivity) CheckConnectivity(timeout time.Duration) (bool, error) {
	// One deadline for both stages, so the fallback never extends the check
	// past the caller's budget.
	ctx, cancel := context.WithTimeout(c.ctx, timeout)
	defer cancel()

	type targetResult struct {
		target string
		ok     bool
	}
	// Buffered for every target so a dial finishing after the verdict never
	// blocks its goroutine.
	results := make(chan targetResult, len(PRIMARY_PING_TARGETS)+len(FALLBACK_PING_TARGETS))

	var wg sync.WaitGroup
	pending := 0
	launch := func(targets []string) {
		for _, target := range targets {
			pending++
			wg.Add(1)
			go func(t string) {
				defer wg.Done()
				before := time.Now()
				conn, err := c.dial(ctx, t, timeout)
				c.logger.Debug("Connectivity check result", zap.String("target", t), zap.Duration("duration", time.Since(before)), zap.Error(err))
				if conn != nil {
					if err := conn.Close(); err != nil {
						c.logger.Warn("Failed to close connection", zap.Error(err))
					}
				}
				// A per-target failure stays local to its target: it must
				// never cancel a peer dial that is about to succeed.
				results <- targetResult{target: t, ok: err == nil}
			}(target)
		}
	}

	fallbackTimer := time.NewTimer(c.fallbackDelay)
	defer fallbackTimer.Stop()
	fallbackStarted := false
	startFallback := func() {
		if fallbackStarted {
			return
		}
		fallbackStarted = true
		launch(FALLBACK_PING_TARGETS)
	}

	launch(PRIMARY_PING_TARGETS)
	if pending == 0 {
		startFallback()
	}

	connected := false
	successfulTarget := ""
	// Every dial is bounded by ctx, so pending always drains to zero; the
	// loop cannot outlive the timeout.
wait:
	for pending > 0 {
		select {
		case r := <-results:
			pending--
			if r.ok {
				connected = true
				successfulTarget = r.target
				break wait
			}
			// Every primary target failed fast (RST, no route): do not sit
			// out the rest of the fallback delay.
			if pending == 0 {
				startFallback()
			}
		case <-fallbackTimer.C:
			startFallback()
		}
	}

	// Abort the losers and join them, so no dial goroutine or socket outlives
	// the check.
	cancel()
	wg.Wait()

	c.logger.Info("Connectivity check summary",
		zap.Bool("connected", connected),
		zap.String("successful_target", successfulTarget),
		zap.Bool("fallback_used", fallbackStarted),
		zap.Duration("timeout", timeout),
	)

	return connected, nil
}
