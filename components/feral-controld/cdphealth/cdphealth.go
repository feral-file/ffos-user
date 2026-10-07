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
	// current unhealthy episode. Cleared on reconnect.
	reported bool
	// lastReportedAt is when "stuck" was last successfully sent. While the
	// episode continues, tick() re-affirms "stuck" every threshold instead
	// of sending it only once (ffos-user#356 review round 2, F1):
	// feral-watchdog's cdpStuck is in-memory only, and ships Restart=always
	// on a systemd unit that is routinely restarted independently of
	// controld (package updates, crashes). A watchdog restart mid-episode
	// zeroes its in-memory latch with no way to learn the episode is still
	// live, since this signal is otherwise edge-triggered and controld has
	// no other reason to resend "stuck" while Initialized() never changes.
	// Periodic re-affirmation bounds how long that gap can last to one more
	// threshold, without turning this into a resend-every-tick storm —
	// chromium.go already treats a repeated "stuck=true" as a safe no-op
	// whenever its own gates are already suppressing escalation.
	lastReportedAt time.Time
	// everConfirmed is set the first time Initialized() is observed true
	// since this Monitor (i.e. this controld process) started, regardless
	// of whether an episode was ever reported. Until then, tick() sends an
	// explicit stuck=false EVEN WITH NOTHING to clear (ffos-user#356
	// feralfile-bot review, second pass): feral-watchdog's own
	// canForgetRebootBudget gate must never infer "CDP is healthy" from
	// the mere ABSENCE of a stuck report plus elapsed local time —
	// cdphealth's unhealthySince timer runs on controld's own clock,
	// anchored to when IT first observes a disconnect, with no shared
	// epoch or ordering guarantee against watchdog's own clock (two
	// separate systemd processes); a watchdog-side timeout can expire
	// before controld's own StuckThreshold has even elapsed, silently
	// forgiving a reboot cap for a failure that simply hasn't been
	// reported YET. This first-ever confirmation is the one positive
	// signal watchdog can safely wait for instead.
	everConfirmed bool
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
		// Send false when clearing a reported episode, OR (ffos-user#356,
		// feralfile-bot's second pass) on the very first healthy
		// observation this process has ever made, even if nothing was ever
		// reported stuck — that first confirmation is the only positive
		// signal feral-watchdog can safely treat as proof of health; see
		// everConfirmed's doc for why silence-plus-timeout is not enough.
		needsConfirm := m.reported || !m.everConfirmed
		// Only retire the episode (and latch everConfirmed) once the send
		// actually got out. A failed Send here (see emit's doc) must retry
		// on the next tick rather than silently forgetting feral-watchdog
		// still holds cdpStuck=true, or never establishing everConfirmed —
		// leaving the fields below untouched is exactly what makes that
		// retry happen, since this whole branch runs again unchanged.
		if needsConfirm {
			if !m.emit(false) {
				return
			}
		}
		m.unhealthySince = time.Time{}
		m.reported = false
		m.lastReportedAt = time.Time{}
		m.everConfirmed = true
		return
	}

	now := m.clock.Now()
	if m.unhealthySince.IsZero() {
		m.unhealthySince = now
	}
	if now.Sub(m.unhealthySince) < m.threshold {
		return
	}
	// Past threshold: send once, then keep re-affirming every threshold
	// while the episode continues (lastReportedAt's doc explains why —
	// ffos-user#356 review round 2, F1). Same success-gating as the clear
	// above: only advance lastReportedAt once THIS send actually succeeded,
	// so a transient failure at the re-affirm moment retries next tick
	// instead of silently extending the gap it exists to bound.
	if !m.reported || now.Sub(m.lastReportedAt) >= m.threshold {
		if m.emit(true) {
			m.reported = true
			m.lastReportedAt = now
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
