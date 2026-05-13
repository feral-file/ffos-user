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

func (e *executor) runSleepScheduleLoop(ctx context.Context) {
	for {
		record, err := sleepschedule.Load(e.os, e.json)
		if err != nil {
			e.logger.Error("Failed to load sleep schedule", zap.Error(err))
			if !e.waitForSleepScheduleSignal(ctx, 30*time.Second) {
				return
			}
			continue
		}

		now := e.clock.Now().In(sleepschedule.LocalTimezone())
		normalized, changed := sleepschedule.Normalize(record, now)
		if changed {
			if err := sleepschedule.Save(e.os, e.json, normalized); err != nil {
				e.logger.Error("Failed to persist normalized sleep schedule", zap.Error(err))
			}
		}

		status, _ := sleepschedule.EffectiveStatus(now, normalized)
		if err := e.applySleepTransition(ctx, status.CurrentState, "schedule-loop"); err != nil {
			e.logger.Error("Failed to apply sleep schedule transition",
				zap.Error(err),
				zap.String("state", string(status.CurrentState)))
		}

		if status.NextTransitionAt == nil {
			if !e.waitForSleepScheduleSignal(ctx, 0) {
				return
			}
			continue
		}

		waitFor := time.Until(*status.NextTransitionAt)
		if waitFor < 0 {
			waitFor = 0
		}
		if !e.waitForSleepScheduleSignal(ctx, waitFor) {
			return
		}
	}
}

func (e *executor) waitForSleepScheduleSignal(ctx context.Context, waitFor time.Duration) bool {
	if e.sleepScheduleWakeCh == nil {
		return false
	}

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

func (e *executor) setSleepSchedule(ctx context.Context, args []byte) (interface{}, error) {
	var cmd sleepScheduleCommand
	if err := e.json.Unmarshal(args, &cmd); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	record, err := sleepschedule.Load(e.os, e.json)
	if err != nil {
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

	// Recomputing the schedule should always drop any stale manual override.
	// Otherwise a previous sleepNow/wakeNow call can keep winning after the
	// schedule is re-enabled.
	record.OverrideState = nil
	record.OverrideUntil = nil

	if err := sleepschedule.Save(e.os, e.json, record); err != nil {
		return nil, err
	}

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
	record, err := sleepschedule.Load(e.os, e.json)
	if err != nil {
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
		return nil, err
	}

	if err := sleepschedule.Save(e.os, e.json, updated); err != nil {
		return nil, err
	}

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
	if err := e.applyPlayerSleepMode(ctx, state == sleepschedule.StateSleeping); err != nil {
		return err
	}

	e.applyFfpPowerStateAsync(state, reason)

	if e.statusPoller != nil {
		e.statusPoller.ForceRefresh()
	}

	return nil
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
		// Detached from the relayer/schedule ctx so a cancelled HTTP/WebSocket request
		// does not abort DDC mid-flight; alignment remains best-effort with bounded
		// wall time only (see ffpSleepPowerControlTimeout).
		ddcCtx, cancel := context.WithTimeout(context.Background(), ffpSleepPowerControlTimeout)
		if err := e.applyFfpPowerState(ddcCtx, job.state); err != nil {
			e.logger.Warn("Failed to align FFP power with sleep state (best effort)",
				zap.Error(err),
				zap.String("state", string(job.state)),
				zap.String("reason", job.reason))
		}
		cancel()
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
