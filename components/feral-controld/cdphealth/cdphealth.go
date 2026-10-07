// Package cdphealth watches whether the cdp package's connect supervisor is
// (re)connected to Chromium's CDP page target, and reports a sustained
// disconnect to feral-watchdog over D-Bus (ffos-user#356).
//
// This package deliberately takes NO recovery action itself — it only
// observes and reports. A real-device incident (ffos-user#356) showed
// cdp.go's connect loop can retry forever without Chromium ever exposing a
// usable page target again, while feral-watchdog's ChromiumMonitor — the
// ONLY component allowed to decide on a kiosk restart or reboot (see both
// components' AGENTS.md) — had no visibility into that: it polls
// /json/version only, which can keep answering fine even while the specific,
// stricter page-target dial controld needs stays stuck. This package closes
// that blind spot by giving feral-watchdog the one fact only controld can
// see, not by growing a second, competing recovery policy here.
package cdphealth

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/godbus"

	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// StuckThreshold is how long CDP may stay disconnected before Monitor
// reports it as stuck. Sized to roughly match feral-watchdog's own
// CHROMIUM_STARTUP_GRACE (90s, components/feral-watchdog/chromium.go) so
// this signal does not fire faster than a legitimate cold boot or kiosk
// restart would already explain.
const StuckThreshold = 90 * time.Second

// pollInterval mirrors cdp's own connectRetryInterval cadence (cdp.go):
// checking more often than the connect loop itself retries would not learn
// anything new between attempts.
const pollInterval = 5 * time.Second

// cdpStatus is the one method Monitor needs from cdp.CDP. A narrow,
// consumer-owned interface so this package depends on neither the full CDP
// interface nor a concrete *cdp.cdp.
type cdpStatus interface {
	Initialized() bool
}

// sender is the one method Monitor needs from dbus.DBus.
type sender interface {
	Send(payload godbus.DBusPayload) error
}

// Monitor polls cdpStatus.Initialized() and emits dbus.EVENT_CDP_STUCK when
// it has stayed false for StuckThreshold, and again (cleared) on reconnect.
type Monitor struct {
	cdp    cdpStatus
	bus    sender
	clock  wrapper.Clock
	logger *zap.Logger

	threshold time.Duration

	// unhealthySince is zero while CDP is connected, or while it has been
	// disconnected for less than threshold. Set the moment Initialized()
	// is first observed false after being true (or after startup).
	unhealthySince time.Time
	// reported is true once EVENT_CDP_STUCK(true) has been sent for the
	// current unhealthy episode, so a continued disconnect does not re-send
	// it every tick. Cleared on reconnect.
	reported bool
}

// New creates a Monitor. clock is injected (rather than using time directly)
// so tests can drive the threshold deterministically, matching the pattern
// cdp.go itself uses for its connect-loop ticker.
func New(cdp cdpStatus, bus sender, clock wrapper.Clock, logger *zap.Logger) *Monitor {
	return &Monitor{
		cdp:       cdp,
		bus:       bus,
		clock:     clock,
		logger:    logger,
		threshold: StuckThreshold,
	}
}

// Start runs the poll loop until ctx is done. Callers run it in its own
// goroutine; CDP availability must never gate daemon startup (see cdp.go's
// CDP.Start doc), and this monitor is no exception.
func (m *Monitor) Start(ctx context.Context) {
	ticker := m.clock.NewTicker(pollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C():
			m.tick()
		}
	}
}

func (m *Monitor) tick() {
	if m.cdp.Initialized() {
		// Only retire the episode once the clear actually got out. A failed
		// Send here (see emit's doc) must retry on the next tick rather than
		// silently forgetting feral-watchdog still holds cdpStuck=true —
		// leaving reported/unhealthySince untouched is exactly what makes
		// that retry happen, since this whole branch runs again unchanged.
		if m.reported {
			if !m.emit(false) {
				return
			}
		}
		m.unhealthySince = time.Time{}
		m.reported = false
		return
	}

	now := m.clock.Now()
	if m.unhealthySince.IsZero() {
		m.unhealthySince = now
	}
	if !m.reported && now.Sub(m.unhealthySince) >= m.threshold {
		// Same reasoning as above: only latch reported once the send for
		// THIS episode actually succeeded, so a transient failure at the
		// threshold-crossing moment retries next tick instead of
		// permanently dropping the one signal this package exists to
		// deliver — a real risk given the real incident this targets ran
		// for 7+ hours (ffos-user#356's own timeline).
		if m.emit(true) {
			m.reported = true
		}
	}
}

// emit reports success so tick() can decide whether to retry. Logged
// failures alone are not enough: tick()'s state machine must know whether
// the send actually left the process, or it cannot tell "delivered" apart
// from "dropped, retry next tick" — see both call sites' comments.
func (m *Monitor) emit(stuck bool) bool {
	err := m.bus.Send(godbus.DBusPayload{
		Interface: dbus.INTERFACE,
		Path:      dbus.PATH,
		Member:    dbus.EVENT_CDP_STUCK,
		Body:      []interface{}{stuck},
	})
	if err != nil {
		m.logger.Error("cdphealth: failed to send DBus signal; will retry next tick", zap.Bool("stuck", stuck), zap.Error(err))
		return false
	}
	if stuck {
		m.logger.Warn("cdphealth: CDP has not (re)connected for a sustained period; reporting stuck to feral-watchdog (ffos-user#356)",
			zap.Duration("threshold", m.threshold))
	} else {
		m.logger.Info("cdphealth: CDP reconnected; clearing stuck signal")
	}
	return true
}
