package provisioning

import (
	"context"
	"runtime/debug"
	"sync/atomic"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// supervisor is the restart-based safety net for the provisioning domain's own
// goroutines (the state-machine event loop, and the portal listener it owns
// indirectly). It runs a function, recovers a panic, and re-runs it with capped
// exponential backoff until its context is canceled.
//
// Scope, stated honestly: this only contains panics that originate INSIDE the
// supervised function. An unrecovered panic on any OTHER goroutine in the
// process still tears the whole daemon down — that is Go's semantics, not
// something a recover() here can change. Process-level crash recovery is
// systemd's job (Restart=on-failure); this supervisor exists so a transient
// fault in setup provisioning does not require a full-daemon restart, and so the
// setup AP/portal can come back on their own. It deliberately spawns its own
// goroutines and never runs work on feral-controld's watchdog sd_notify
// goroutine, so a stall here cannot suppress the systemd keepalive.
type supervisor struct {
	clock       wrapper.Clock
	logger      *zap.Logger
	baseBackoff time.Duration
	maxBackoff  time.Duration

	// restarts counts how many times the supervised function was restarted after
	// a panic. Exported via restartCount for observability and tests.
	restarts atomic.Int64
}

func newSupervisor(clock wrapper.Clock, logger *zap.Logger) *supervisor {
	return &supervisor{
		clock:       clock,
		logger:      logger,
		baseBackoff: 500 * time.Millisecond,
		maxBackoff:  30 * time.Second,
	}
}

// run executes fn, restarting it on panic with capped backoff until ctx is
// canceled. It returns when fn completes without panicking (e.g. because ctx was
// canceled inside it) or when the backoff sleep observes ctx cancellation.
func (s *supervisor) run(ctx context.Context, name string, fn func(context.Context)) {
	backoff := s.baseBackoff
	for {
		if ctx.Err() != nil {
			return
		}
		if !s.runOnce(ctx, name, fn) {
			return // clean return: fn is not expected to be restarted
		}
		s.restarts.Add(1)
		if err := s.clock.SleepContext(ctx, backoff); err != nil {
			return // ctx canceled while backing off
		}
		if backoff *= 2; backoff > s.maxBackoff {
			backoff = s.maxBackoff
		}
	}
}

// runOnce runs fn once and reports whether it panicked (and was recovered).
func (s *supervisor) runOnce(ctx context.Context, name string, fn func(context.Context)) (panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			s.logger.Error("provisioning: supervised goroutine panicked; restarting",
				zap.String("goroutine", name),
				zap.Any("panic", r),
				zap.ByteString("stack", debug.Stack()),
			)
			panicked = true
		}
	}()
	fn(ctx)
	return false
}

func (s *supervisor) restartCount() int64 { return s.restarts.Load() }
