package commandrouter

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/semaphore"
	"golang.org/x/sync/singleflight"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
)

// Policy describes how the storm gate protects a single command type.
type Policy struct {
	// Group is the limiter key. Command types that share a non-empty Group
	// share one token bucket (e.g. all input gestures share an "input" budget).
	// When empty, the command type string is used as its own key.
	Group string
	// Rate is the sustained token-bucket refill rate in tokens per second.
	// A value <= 0 disables rate limiting for this policy.
	Rate float64
	// Burst is the token-bucket capacity (max commands accepted in a spike).
	Burst int
	// Weight is the cost charged against the global concurrency budget while
	// the command is in flight. Heavier commands reserve more of the budget.
	Weight int64
	// Dedupe collapses identical in-flight commands (same type + arguments)
	// into a single execution whose result is shared. Suitable for idempotent
	// commands such as casting a playlist or polling status.
	Dedupe bool
}

// GateConfig configures the command storm gate.
type GateConfig struct {
	// Enabled toggles the whole gate. When false, NewGate returns the inner
	// handler unchanged.
	Enabled bool
	// MaxConcurrent is the global concurrency budget shared by all commands
	// (sum of in-flight Policy.Weight). A value <= 0 disables the concurrency
	// guard.
	MaxConcurrent int64
	// Policies maps a command type to its policy. Types absent from the map use
	// Default.
	Policies map[commands.Type]Policy
	// Default applies to command types with no explicit policy.
	Default Policy

	// now is an injectable clock for deterministic tests; nil means time.Now.
	now func() time.Time
}

// inputGroup is the shared limiter key for high-frequency pointer/keyboard
// gestures: a legitimate drag emits many events, so they share one generous
// budget rather than each type being throttled in isolation.
const inputGroup = "input"

// DefaultGateConfig returns a tuned, ready-to-use configuration. It is safe with
// zero external configuration and errs toward never blocking legitimate use
// while capping floods of high-cost or disruptive commands.
func DefaultGateConfig() GateConfig {
	disruptive := Policy{Rate: 0.2, Burst: 2, Weight: 1, Dedupe: true} // ~1 per 5s
	heavy := Policy{Rate: 1, Burst: 3, Weight: 4, Dedupe: true}        // casting / uploads
	query := Policy{Rate: 5, Burst: 10, Weight: 1, Dedupe: true}       // status polls
	input := Policy{Group: inputGroup, Rate: 50, Burst: 100, Weight: 1}
	// slowWrite covers state-changing writes that shell out and are physically
	// disruptive (DDC brightness/contrast/power), so they get a heavier weight
	// than a cheap query but are not throttled as hard as a reboot.
	slowWrite := Policy{Rate: 2, Burst: 5, Weight: 2}
	// userAction covers latency-sensitive, user-initiated power toggles. They
	// must not reject normal repeated taps, and the executor already coalesces
	// sleep/wake bursts to the latest state, so the gate only needs a loose
	// flood cap here rather than the disruptive 1-per-5s limit.
	userAction := Policy{Rate: 2, Burst: 5, Weight: 1}

	policies := map[commands.Type]Policy{
		// Disruptive / system-level: tightly limited, deduped.
		commands.CMD_REBOOT:             disruptive,
		commands.CMD_SHUTDOWN:           disruptive,
		commands.CMD_FACTORY_RESET:      disruptive,
		commands.CMD_UPDATE_TO_LATEST:   disruptive,
		commands.CMD_SET_SLEEP_SCHEDULE: disruptive,
		commands.CMD_SET_SLEEP_MODE:     disruptive,
		commands.CMD_SCREEN_ROTATION:    disruptive,

		// User-initiated power toggles: loosely capped (executor coalesces).
		commands.CMD_SLEEP_NOW: userAction,
		commands.CMD_WAKE_NOW:  userAction,

		// Heavy: the externally reachable cast path and other costly work.
		commands.CMD_DISPLAY_PLAYLIST:         heavy,
		commands.CMD_DISPLAY_DEFAULT_PLAYLIST: heavy,
		commands.CMD_REFRESH_ARTWORK:          heavy,
		commands.CMD_UPLOAD_LOGS:              heavy,
		commands.CMD_SSH_ACCESS:               heavy,

		// Slow, disruptive panel writes (DDC, incl. power): moderate + heavier.
		commands.CMD_DDC_PANEL_CONTROL: slowWrite,

		// Cheap queries: deduped so a poll storm collapses to one execution.
		commands.CMD_DEVICE_STATUS:    query,
		commands.CMD_PROFILE:          query,
		commands.CMD_DDC_PANEL_STATUS: query,

		// High-frequency input events: shared generous budget.
		commands.CMD_KEYBOARD_EVENT:             input,
		commands.CMD_MOUSE_DRAG_EVENT:           input,
		commands.CMD_MOUSE_TAP_EVENT:            input,
		commands.CMD_MOUSE_DOUBLE_TAP_EVENT:     input,
		commands.CMD_MOUSE_LONG_PRESS_EVENT:     input,
		commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT: input,
		commands.CMD_ZOOM_GESTURE:               input,
	}

	return GateConfig{
		Enabled:       true,
		MaxConcurrent: 16,
		Policies:      policies,
		// Anything unlisted (toggles, volume, panel control, connect, pairing):
		// generous but bounded.
		Default: Policy{Rate: 10, Burst: 20, Weight: 1},
	}
}

// gate is a Handler decorator that enforces command-storm protection on the
// shared command path, covering both the LAN hub and relayer ingress.
type gate struct {
	inner    Handler
	cfg      GateConfig
	limiters *limiterSet
	sem      *semaphore.Weighted
	flight   singleflight.Group
	logger   *zap.Logger
}

// NewGate wraps inner with command-storm protection. When cfg.Enabled is false
// it returns inner unchanged.
func NewGate(inner Handler, cfg GateConfig, logger *zap.Logger) Handler {
	if !cfg.Enabled {
		return inner
	}
	var sem *semaphore.Weighted
	if cfg.MaxConcurrent > 0 {
		sem = semaphore.NewWeighted(cfg.MaxConcurrent)
	}
	return &gate{
		inner:    inner,
		cfg:      cfg,
		limiters: newLimiterSet(cfg.now),
		sem:      sem,
		logger:   logger,
	}
}

// policyFor returns the policy and limiter key for a command type.
func (g *gate) policyFor(t commands.Type) (Policy, string) {
	p, ok := g.cfg.Policies[t]
	if !ok {
		p = g.cfg.Default
	}
	key := p.Group
	if key == "" {
		key = string(t)
	}
	return p, key
}

func (g *gate) Process(ctx context.Context, command commands.Command) (interface{}, error) {
	// Empty-type commands are a no-op in the inner handler; pass straight
	// through so the gate adds no surprising behavior.
	if command.Type == "" {
		return g.inner.Process(ctx, command)
	}

	p, key := g.policyFor(command.Type)

	// Deduped commands collapse identical in-flight requests into one
	// execution. Only the leader consumes rate/concurrency budget; followers
	// share its result. Doing this before the rate check means a burst of
	// identical casts costs a single token rather than being rejected.
	//
	// Followers share the leader's outcome, including its error: if the leader
	// is rate-limited every follower sees RateLimitedError, and if the leader's
	// underlying execution fails transiently (e.g. a cast backend error) the
	// coalesced followers see that same failure rather than re-running. This is
	// intended — only truly concurrent, byte-identical commands coalesce, the
	// failure is legible to every caller, and any caller may simply retry.
	if p.Dedupe {
		flightKey := key + "\x00" + argsHash(command.Arguments)
		res, err, _ := g.flight.Do(flightKey, func() (interface{}, error) {
			return g.admit(ctx, command, p, key)
		})
		return res, err
	}

	return g.admit(ctx, command, p, key)
}

// admit applies the rate-limit and concurrency guards, then runs the command.
func (g *gate) admit(ctx context.Context, command commands.Command, p Policy, key string) (interface{}, error) {
	if !g.limiters.allow(key, p) {
		g.logger.Warn("Command rejected: rate limit exceeded",
			zap.String("command", command.Type.String()),
			zap.String("limiterKey", key),
		)
		return nil, &RateLimitedError{Command: command.Type, Reason: "rate limit exceeded"}
	}

	if g.sem != nil {
		weight := p.Weight
		if weight < 1 {
			weight = 1
		}
		if !g.sem.TryAcquire(weight) {
			g.logger.Warn("Command rejected: concurrency budget exhausted",
				zap.String("command", command.Type.String()),
				zap.Int64("weight", weight),
			)
			return nil, &RateLimitedError{Command: command.Type, Reason: "concurrency budget exhausted"}
		}
		defer g.sem.Release(weight)
	}

	return g.inner.Process(ctx, command)
}

// argsHash returns a stable hash of command arguments for dedupe keying.
// json.Marshal emits map keys in sorted order, so identical argument maps hash
// identically regardless of Go's randomized map iteration order. Arguments that
// cannot be marshaled fall back to an empty hash, which simply means such
// commands dedupe by type alone — a safe conservative behavior.
func argsHash(args map[string]any) string {
	if len(args) == 0 {
		return ""
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}
