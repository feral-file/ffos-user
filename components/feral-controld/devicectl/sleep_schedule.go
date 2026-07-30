package devicectl

import (
	"context"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/ddc"
	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
)

// ffpSleepPowerControlTimeout bounds each DDC ApplyControl attempt in the sleep
// power-alignment worker. ApplyControl can take many seconds on failure; relayer
// paths must not block on it (see applyFfpPowerStateAsync).
//
// The worker uses a detached timeout context so relayer request cancellation does
// not abort an in-flight DDC call. Process exit still abandons queued or running
// work without draining the worker (best-effort only).
const ffpSleepPowerControlTimeout = 60 * time.Second

// sleepPowerAlignJob carries one best-effort FFP panel power alignment request.
// A single worker processes these in order and coalesces bursts to the latest state.
type sleepPowerAlignJob struct {
	state  sleepschedule.State
	reason string
}

type sleepScheduleCommand struct {
	Enabled   bool   `json:"enabled"`
	SleepTime string `json:"sleepTime"`
	WakeTime  string `json:"wakeTime"`
	// Days is optional on the wire: nil (absent, or JSON null) preserves the
	// record's current days, mirroring the empty-string behavior of the time
	// fields, so pre-days apps toggling enabled cannot clobber a days
	// selection made by a newer app. An explicit empty array is rejected.
	Days []string `json:"days"`
}

func StartSleepScheduleLoop(ctx context.Context, exec Executor, logger *zap.Logger) {
	runner, ok := exec.(interface{ startSleepScheduleLoop(context.Context) })
	if !ok {
		logger.Warn("Executor does not support sleep schedule loop")
		return
	}
	runner.startSleepScheduleLoop(ctx)
}

func (e *executor) startSleepScheduleLoop(ctx context.Context) {
	e.sleepScheduleMu.Lock()
	defer e.sleepScheduleMu.Unlock()

	if e.sleepScheduleRun {
		return
	}

	e.sleepScheduleRun = true
	e.sleepScheduleWakeCh = make(chan struct{}, 1)

	go e.runSleepScheduleLoop(ctx)
}

// sleepScheduleMaxTick caps how long the loop sleeps before re-evaluating, even
// when the next scheduled transition is hours away. Capping the wait means:
//
//   - A transition whose first apply failed (e.g. the player page was not yet
//     ready at boot, or Chromium was mid-restart) is retried within one tick
//     instead of being dropped until the next schedule boundary hours later.
//   - Wall-clock jumps (RTC-less boot, NTP correction, DST) are absorbed: the
//     loop re-reads the clock and timezone every tick, so an armed monotonic
//     timer can no longer fire the transition at the wrong civil time.
//
// applySleepTransitionIfChanged de-dups a healthy, unchanged state so the extra
// ticks stay cheap (a file read + timezone read, no player round-trip).
const sleepScheduleMaxTick = 60 * time.Second

// panelRetryMax bounds how many consecutive best-effort panel (DDC) alignment
// attempts the loop makes for a single target state before giving up until the
// next real transition. It keeps transient ddcutil failures self-healing (a few
// capped-tick retries) without hammering ddcutil forever on devices whose panel
// cannot do DDC power control.
const panelRetryMax = 3

func (e *executor) runSleepScheduleLoop(ctx context.Context) {
	for {
		e.sleepScheduleFileMu.Lock()
		record, err := sleepschedule.Load(e.os, e.json)
		if err != nil {
			e.sleepScheduleFileMu.Unlock()
			e.logger.Error("Failed to load sleep schedule", zap.Error(err))
			if !e.waitForSleepScheduleSignal(ctx, sleepScheduleMaxTick) {
				return
			}
			continue
		}

		loc, tzResolved := sleepschedule.LocalTimezoneResolved()
		if !tzResolved {
			// The time.Local fallback is usually stale UTC (device booted before
			// the timezone landed): evaluating the schedule on it would sleep or
			// wake the display hours off the user's wall clock. Defer until the
			// timezone resolves; the capped tick re-checks within a minute.
			e.sleepScheduleFileMu.Unlock()
			e.logger.Warn("Sleep schedule: device timezone unresolved; deferring schedule evaluation")
			if !e.waitForSleepScheduleSignal(ctx, sleepScheduleMaxTick) {
				return
			}
			continue
		}
		now := e.clock.Now().In(loc)
		normalized, changed := sleepschedule.Normalize(record, now)
		if changed {
			if err := sleepschedule.Save(e.os, e.json, normalized); err != nil {
				e.logger.Error("Failed to persist normalized sleep schedule", zap.Error(err))
			}
		}
		e.sleepScheduleFileMu.Unlock()

		status, _ := sleepschedule.EffectiveStatus(now, normalized)
		if err := e.applySleepTransitionIfChanged(ctx, status.CurrentState, "schedule-loop"); err != nil {
			e.logger.Error("Failed to apply sleep schedule transition",
				zap.Error(err),
				zap.String("state", string(status.CurrentState)))
			// The failed apply leaves sleepApplyOK false, so the next capped tick
			// retries rather than waiting until the next schedule boundary.
		}

		if !e.waitForSleepScheduleSignal(ctx, sleepScheduleTickWait(status.NextTransitionAt)) {
			return
		}
	}
}

// sleepScheduleTickWait returns how long the loop should wait before the next
// re-evaluation: the time until the next transition, floored so a due/past
// transition re-checks promptly (0 is reserved by waitForSleepScheduleSignal as
// the "wait for a signal only" sentinel) and capped at sleepScheduleMaxTick.
func sleepScheduleTickWait(next *time.Time) time.Duration {
	waitFor := sleepScheduleMaxTick
	if next != nil {
		if d := time.Until(*next); d < waitFor {
			waitFor = d
		}
	}
	if waitFor < time.Second {
		waitFor = time.Second
	}
	return waitFor
}

func (e *executor) waitForSleepScheduleSignal(ctx context.Context, waitFor time.Duration) bool {
	if e.sleepScheduleWakeCh == nil {
		return false
	}

	// waitFor == 0 means "block until a wake signal (or cancellation)". The loop
	// no longer passes 0 — sleepScheduleTickWait floors at 1s so a past-due
	// transition re-checks promptly instead of blocking forever. This branch is
	// kept only as a defensive sentinel for any future caller that opts into
	// signal-only waiting; do not funnel a clamped negative duration through it.
	if waitFor == 0 {
		select {
		case <-ctx.Done():
			return false
		case <-e.sleepScheduleWakeCh:
			return true
		}
	}

	timer := time.NewTimer(waitFor)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-e.sleepScheduleWakeCh:
		return true
	case <-timer.C:
		return true
	}
}

func (e *executor) wakeSleepScheduleLoop() {
	e.sleepScheduleMu.Lock()
	defer e.sleepScheduleMu.Unlock()

	if e.sleepScheduleWakeCh == nil {
		return
	}

	select {
	case e.sleepScheduleWakeCh <- struct{}{}:
	default:
	}
}

// InvalidatePlayerSleepState marks the player (CDP) sleep leg as not applied and
// wakes the schedule loop so it re-drives the player. Called on every CDP
// (re)connect: controld now outlives Chromium, and a restarted kiosk reloads the
// player web app awake — a tracker still recording "sleeping" would make
// applySleepTransitionIfChanged skip the re-drive until the next schedule
// boundary, leaving the display playing all night. The panel (DDC) leg is
// deliberately left alone: panel hardware power state survives a browser
// restart, so re-driving it would be wrong, not just redundant.
func InvalidatePlayerSleepState(exec Executor, logger *zap.Logger) {
	inv, ok := exec.(interface{ invalidatePlayerSleepState() })
	if !ok {
		logger.Warn("Executor does not support player sleep-state invalidation")
		return
	}
	inv.invalidatePlayerSleepState()
}

func (e *executor) invalidatePlayerSleepState() {
	e.sleepApplyMu.Lock()
	e.sleepAppliedState, e.sleepApplyOK = nil, false
	e.sleepApplyMu.Unlock()
	// Safe before the loop starts (nil wake channel is a no-op) — the loop's first
	// iteration evaluates the schedule from scratch anyway.
	e.wakeSleepScheduleLoop()
}

func (e *executor) setSleepSchedule(ctx context.Context, args []byte) (interface{}, error) {
	var cmd sleepScheduleCommand
	if err := e.json.Unmarshal(args, &cmd); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	e.sleepScheduleFileMu.Lock()
	record, err := sleepschedule.Load(e.os, e.json)
	if err != nil {
		e.sleepScheduleFileMu.Unlock()
		return nil, err
	}
	if record == nil {
		record = sleepschedule.DefaultRecord()
	}

	record.Enabled = cmd.Enabled
	if cmd.SleepTime != "" {
		record.SleepTime = cmd.SleepTime
	}
	if cmd.WakeTime != "" {
		record.WakeTime = cmd.WakeTime
	}
	if cmd.Days != nil {
		days, err := sleepschedule.NormalizeDays(cmd.Days)
		if err != nil {
			e.sleepScheduleFileMu.Unlock()
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		record.Days = days
	}

	// Recomputing the schedule should always drop any stale manual override.
	// Otherwise a previous sleepNow/wakeNow call can keep winning after the
	// schedule is re-enabled.
	record.OverrideState = nil
	record.OverrideUntil = nil

	if err := sleepschedule.Save(e.os, e.json, record); err != nil {
		e.sleepScheduleFileMu.Unlock()
		return nil, err
	}
	e.sleepScheduleFileMu.Unlock()

	now := e.clock.Now().In(sleepschedule.LocalTimezone())
	status, _ := sleepschedule.EffectiveStatus(now, record)
	if err := e.applySleepTransition(ctx, status.CurrentState, "schedule-update"); err != nil {
		return nil, err
	}

	e.wakeSleepScheduleLoop()
	return map[string]any{
		"ok":            true,
		"sleepSchedule": status,
	}, nil
}

func (e *executor) sleepNow(ctx context.Context) (interface{}, error) {
	return e.applyManualSleepOverride(ctx, sleepschedule.StateSleeping)
}

func (e *executor) wakeNow(ctx context.Context) (interface{}, error) {
	return e.applyManualSleepOverride(ctx, sleepschedule.StateAwake)
}

func (e *executor) applyManualSleepOverride(ctx context.Context, state sleepschedule.State) (interface{}, error) {
	e.sleepScheduleFileMu.Lock()
	record, err := sleepschedule.Load(e.os, e.json)
	if err != nil {
		e.sleepScheduleFileMu.Unlock()
		return nil, err
	}

	now := e.clock.Now().In(sleepschedule.LocalTimezone())
	var updated *sleepschedule.Record
	switch state {
	case sleepschedule.StateSleeping:
		updated, err = sleepschedule.ManualSleep(record, now)
	case sleepschedule.StateAwake:
		updated, err = sleepschedule.ManualWake(record, now)
	default:
		err = fmt.Errorf("unsupported manual sleep override state %q", state)
	}
	if err != nil {
		e.sleepScheduleFileMu.Unlock()
		return nil, err
	}

	if err := sleepschedule.Save(e.os, e.json, updated); err != nil {
		e.sleepScheduleFileMu.Unlock()
		return nil, err
	}
	e.sleepScheduleFileMu.Unlock()

	status, _ := sleepschedule.EffectiveStatus(now, updated)
	if err := e.applySleepTransition(ctx, state, "manual-override"); err != nil {
		return nil, err
	}

	e.wakeSleepScheduleLoop()
	return map[string]any{
		"ok":            true,
		"sleepSchedule": status,
	}, nil
}

func (e *executor) applySleepTransition(ctx context.Context, state sleepschedule.State, reason string) error {
	// Single entry point so schedule ticks and manual overrides cannot drift across CDP vs. DDC.
	// Cross-subsystem contract (schedule loop + setSleepSchedule + sleepNow/wakeNow):
	//
	//   • Player sleep mode (CDP) is synchronous on ctx: failure returns to the
	//     caller; success means the FF1 player is driven to the requested mode.
	//
	//   • FFP panel power (DDC) is best-effort and asynchronous: it must not block
	//     relayer deadlines. A single worker serializes ApplyControl and coalesces
	//     rapid transitions; failures are logged only.
	//
	//   • Status refresh runs immediately after CDP succeeds. Notifications may
	//     therefore show the new sleepSchedule / player sleep state while DDC-derived
	//     panel fields (e.g. from ddcPanelStatus) are still catching up — eventual
	//     consistency across hardware vs. player.
	//
	//   • Shutdown does not wait for queued or in-flight DDC alignment.
	e.sleepApplyMu.Lock()
	wasSleeping := e.sleepAppliedState != nil && *e.sleepAppliedState == sleepschedule.StateSleeping
	err := e.applySleepTransitionLocked(ctx, state, reason)
	onAwake := e.onAwake
	e.sleepApplyMu.Unlock()
	// Fire outside sleepApplyMu: displayAt recompute does its own CDP round-trip
	// and must not serialize behind (or extend) the sleep-transition critical section.
	// Fire only on the sleep→awake EDGE, never on a successful re-drive of an
	// already-awake player: the hook's job is to catch a displayAt timer that
	// fired while the player slept, and re-drives are routine (every CDP
	// reconnect invalidates the tracker; wakeNow re-drives unconditionally).
	// Firing on those would force-push — and visibly remount — a byte-identical
	// active set on top of the on-connect RecomputeNow that already serves a
	// reloaded page. A FAILED previous sleep apply still counts as the edge:
	// the player state is unknown and a timer may have fired into it, so the
	// recompute errs toward one redundant push over a stale playlist.
	if err == nil && state == sleepschedule.StateAwake && wasSleeping && onAwake != nil {
		onAwake(ctx)
	}
	return err
}

// applySleepTransitionLocked performs the transition assuming e.sleepApplyMu is
// already held. Holding the lock across the player send and the tracker write is
// what makes the tracker a faithful record of the player's last commanded state
// even when a manual override and a schedule tick race.
func (e *executor) applySleepTransitionLocked(ctx context.Context, state sleepschedule.State, reason string) error {
	if err := e.applyPlayerSleepMode(ctx, state == sleepschedule.StateSleeping); err != nil {
		// Record the attempted state as not-applied so the schedule loop retries
		// on its next tick instead of assuming the state took effect.
		s := state
		e.sleepAppliedState, e.sleepApplyOK = &s, false
		return err
	}

	e.applyFfpPowerStateAsync(state, reason)

	if e.statusPoller != nil {
		e.statusPoller.ForceRefresh()
	}

	s := state
	e.sleepAppliedState, e.sleepApplyOK = &s, true
	return nil
}

// SetOnAwake registers a callback invoked after a successful sleep→awake
// transition (the edge, not every awake re-drive). Safe before or after the
// sleep schedule loop starts.
func SetOnAwake(exec Executor, fn func(context.Context), logger *zap.Logger) {
	setter, ok := exec.(interface{ setOnAwake(func(context.Context)) })
	if !ok {
		logger.Warn("Executor does not support on-awake hook")
		return
	}
	setter.setOnAwake(fn)
}

func (e *executor) setOnAwake(fn func(context.Context)) {
	e.sleepApplyMu.Lock()
	defer e.sleepApplyMu.Unlock()
	e.onAwake = fn
}

// SetWithPlayerPush registers the playlist-write serialization hook shared with
// displayAt scheduling. It intentionally serializes claim-time default fallback
// writes without giving devicectl permission to clear scheduler-owned cache.
func SetWithPlayerPush(exec Executor, fn func(func()), logger *zap.Logger) {
	setter, ok := exec.(interface{ setWithPlayerPush(func(func())) })
	if !ok {
		logger.Warn("Executor does not support player-push serialization hook")
		return
	}
	setter.setWithPlayerPush(fn)
}

func (e *executor) setWithPlayerPush(fn func(func())) {
	e.sleepApplyMu.Lock()
	defer e.sleepApplyMu.Unlock()
	e.withPlayerPush = fn
}

// applySleepTransitionIfChanged drives a transition only when it is not already
// aligned. It is the schedule loop's entry point: manual overrides
// (sleepNow/wakeNow/setSleepSchedule) call applySleepTransition directly so an
// explicit user action always re-drives the player.
//
// Two legs are tracked independently:
//   - Player (CDP) not at the desired state -> full re-drive.
//   - Player already aligned but the best-effort panel (DDC) leg fell behind
//     (a transient ddcutil failure) -> re-enqueue only the panel alignment,
//     without a redundant player round-trip. The align worker's coalescing keeps
//     this cheap and a newer transition always supersedes it.
func (e *executor) applySleepTransitionIfChanged(ctx context.Context, state sleepschedule.State, reason string) error {
	e.sleepApplyMu.Lock()

	playerAligned := e.sleepApplyOK && e.sleepAppliedState != nil && *e.sleepAppliedState == state
	// BOTH panel verdicts — the success ("panel already aligned") and the
	// capped give-up (sleepPanelFailStreak >= panelRetryMax) — only hold for
	// the display generation they were earned against: Generation() bumps when
	// the DRM fingerprint changes, so a newly plugged/swapped panel is
	// re-driven to the scheduled power state (a swapped-in monitor boots
	// awake even mid sleep-window) and gets a fresh retry budget instead of
	// inheriting the old panel's failure verdict.
	panelAligned := e.panelDDC == nil ||
		(e.sleepPanelState != nil && *e.sleepPanelState == state &&
			e.sleepPanelGen == e.panelDDC.Generation() &&
			(e.sleepPanelOK || e.sleepPanelFailStreak >= panelRetryMax))

	if playerAligned && panelAligned {
		e.sleepApplyMu.Unlock()
		return nil
	}
	if playerAligned && !panelAligned {
		// Player is already in the desired state; only nudge the panel. The enqueue
		// is non-blocking, but it must happen under sleepApplyMu — the same lock
		// hold that observed the state — like the full-transition path does. If the
		// lock were released first, a stale tick could observe the old state, lose
		// the lock to a manual transition that enqueues the new panel state, and
		// then enqueue its stale state afterwards; the coalescing worker applies
		// the last enqueued job, so the stale panel command would supersede the
		// manual one.
		e.applyFfpPowerStateAsync(state, reason)
		e.sleepApplyMu.Unlock()
		return nil
	}

	// The player leg needs a full re-drive but CDP is down (headless boot with
	// no monitor, or a kiosk/Chromium restart in progress). Attempting the send
	// would fail with "CDP connection is not initialized" on every capped tick —
	// an Error-level log flood that runs forever on headless devices. Defer
	// quietly instead: the CDP (re)connect callback calls
	// InvalidatePlayerSleepState, which wakes this loop, so the transition is
	// re-driven the moment the player becomes reachable. Manual overrides
	// (sleepNow/wakeNow/setSleepSchedule) go through applySleepTransition and
	// still surface the CDP error to their caller.
	if e.cdp != nil && !e.cdp.Initialized() {
		e.logger.Debug("Deferring sleep schedule transition: CDP not connected",
			zap.String("state", string(state)),
			zap.String("reason", reason))
		e.sleepApplyMu.Unlock()
		return nil
	}

	wasSleeping := e.sleepAppliedState != nil && *e.sleepAppliedState == sleepschedule.StateSleeping
	err := e.applySleepTransitionLocked(ctx, state, reason)
	onAwake := e.onAwake
	e.sleepApplyMu.Unlock()
	// Sleep→awake edge only — see applySleepTransition for why re-drives
	// (tracker invalidated on CDP reconnect, or a failed awake retry) must
	// not re-fire the displayAt recompute.
	if err == nil && state == sleepschedule.StateAwake && wasSleeping && onAwake != nil {
		onAwake(ctx)
	}
	return err
}

// recordPanelApply stores the state the async FFP power worker last drove the
// panel to and whether it succeeded. ok=false (or a state mismatch vs. desired)
// makes the next applySleepTransitionIfChanged re-enqueue the panel alignment.
//
// gen is the display generation the attempt ran against, captured by the
// worker BEFORE the apply. A failure observed on a generation different from
// the recorded one belongs to a NEW display and restarts the streak at 1 —
// the fresh panel must get the full panelRetryMax budget (its DDC often needs
// a few attempts right after hotplug), not inherit the old panel's exhausted
// verdict.
func (e *executor) recordPanelApply(state sleepschedule.State, ok bool, gen uint64) {
	e.sleepApplyMu.Lock()
	defer e.sleepApplyMu.Unlock()

	sameDisplay := gen == e.sleepPanelGen
	e.sleepPanelGen = gen

	switch {
	case ok:
		e.sleepPanelFailStreak = 0
	case !sameDisplay:
		// First failure observed on a new display generation.
		e.sleepPanelFailStreak = 1
	case e.sleepPanelState != nil && *e.sleepPanelState == state:
		// Consecutive failure for the same target state on the same display.
		e.sleepPanelFailStreak++
	default:
		// First failure for a new target state.
		e.sleepPanelFailStreak = 1
	}

	s := state
	e.sleepPanelState, e.sleepPanelOK = &s, ok
}

// panelGeneration reads the current display generation (0 without a panel).
func (e *executor) panelGeneration() uint64 {
	if e.panelDDC == nil {
		return 0
	}
	return e.panelDDC.Generation()
}

// applyFfpPowerStateAsync enqueues best-effort panel power alignment; it never blocks
// the caller. See applySleepTransition for the full contract (eventual DDC vs.
// synchronous player, status refresh timing, shutdown).
//
// A dedicated worker serializes ApplyControl and coalesces pending jobs so a slow
// DDC completion cannot apply stale power after a newer transition (e.g. sleep
// then wake while the first round-trip is still in flight).
func (e *executor) applyFfpPowerStateAsync(state sleepschedule.State, reason string) {
	if e.panelDDC == nil {
		return
	}
	e.sleepPowerAlignOnce.Do(func() {
		e.sleepPowerAlignCh = make(chan sleepPowerAlignJob, 1)
		go e.runSleepPowerAlignWorker()
	})
	e.enqueueCoalescedSleepPowerAlign(sleepPowerAlignJob{state: state, reason: reason})
}

func (e *executor) enqueueCoalescedSleepPowerAlign(job sleepPowerAlignJob) {
	e.sleepPowerAlignEnqueueMu.Lock()
	defer e.sleepPowerAlignEnqueueMu.Unlock()
	for {
		select {
		case e.sleepPowerAlignCh <- job:
			return
		default:
			// Drop at most one pending item without blocking: the worker may consume
			// the queue between our full send attempt and this receive, and a blocking
			// receive here would deadlock with enqueueMu held.
			select {
			case <-e.sleepPowerAlignCh:
			default:
			}
		}
	}
}

func (e *executor) runSleepPowerAlignWorker() {
	for {
		job := <-e.sleepPowerAlignCh
		job = e.drainCoalescedSleepPowerAlignJobs(job)
		// Stamp the attempt with the display generation it actually runs
		// against, captured BEFORE the (possibly multi-second) DDC round-trip:
		// a hotplug landing mid-apply must not attribute the OLD display's
		// failure to the NEW generation, which would silently consume the new
		// display's fresh retry budget.
		gen := e.panelGeneration()
		// Detached from the relayer/schedule ctx so a canceled HTTP/WebSocket request
		// does not abort DDC mid-flight; alignment remains best-effort with bounded
		// wall time only (see ffpSleepPowerControlTimeout).
		ddcCtx, cancel := context.WithTimeout(context.Background(), ffpSleepPowerControlTimeout)
		err := e.applyFfpPowerState(ddcCtx, job.state)
		cancel()
		if err != nil {
			e.logger.Warn("Failed to align FFP power with sleep state (best effort)",
				zap.Error(err),
				zap.String("state", string(job.state)),
				zap.String("reason", job.reason))
		}
		// Record the outcome either way: on success the loop stops re-enqueuing this
		// state; on failure the mismatch makes the next tick retry the panel leg.
		e.recordPanelApply(job.state, err == nil, gen)
	}
}

func (e *executor) drainCoalescedSleepPowerAlignJobs(job sleepPowerAlignJob) sleepPowerAlignJob {
	for {
		select {
		case j := <-e.sleepPowerAlignCh:
			job = j
		default:
			return job
		}
	}
}

func (e *executor) applyPlayerSleepMode(ctx context.Context, sleepMode bool) error {
	if e.cdp == nil {
		return fmt.Errorf("cdp client is not configured")
	}

	command := commands.Command{
		Type: commands.CMD_SET_SLEEP_MODE,
		Arguments: map[string]any{
			"sleepMode": sleepMode,
		},
	}
	payload, err := command.JSON()
	if err != nil {
		return fmt.Errorf("marshal setSleepMode payload: %w", err)
	}

	_, err = e.cdp.Send(cdp.METHOD_EVALUATE, map[string]any{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(payload)),
	})
	if err != nil {
		return fmt.Errorf("send setSleepMode command to player: %w", err)
	}
	return nil
}

func (e *executor) applyFfpPowerState(ctx context.Context, state sleepschedule.State) error {
	var powerState string
	switch state {
	case sleepschedule.StateSleeping:
		powerState = string(ddc.DdcPowerStandby)
	case sleepschedule.StateAwake:
		powerState = string(ddc.DdcPowerOn)
	default:
		return fmt.Errorf("unsupported sleep state %q", state)
	}

	return e.panelDDC.ApplyControl(ctx, ddc.DdcPanelActionPower, []byte(fmt.Sprintf("%q", powerState)))
}
