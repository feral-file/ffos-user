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

		now := e.clock.Now()
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
		record = &sleepschedule.Record{}
	}

	record.Enabled = cmd.Enabled
	if cmd.SleepTime != "" {
		record.SleepTime = cmd.SleepTime
	}
	if cmd.WakeTime != "" {
		record.WakeTime = cmd.WakeTime
	}

	if !record.Enabled {
		record.OverrideState = nil
		record.OverrideUntil = nil
	}

	if err := sleepschedule.Save(e.os, e.json, record); err != nil {
		return nil, err
	}

	now := e.clock.Now()
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

	now := e.clock.Now()
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
	// Keep FF1 sleep mode and FFP panel power changes in one transition helper so
	// schedule ticks and manual overrides cannot drift into separate control paths.
	if err := e.applyPlayerSleepMode(ctx, state == sleepschedule.StateSleeping); err != nil {
		return err
	}

	// if e.panelDDC != nil {
	// 	if err := e.applyFfpPowerState(ctx, state); err != nil {
	// 		e.logger.Warn("Failed to align FFP power with sleep state",
	// 			zap.Error(err),
	// 			zap.String("state", string(state)),
	// 			zap.String("reason", reason))
	// 	}
	// }

	if e.statusPoller != nil {
		e.statusPoller.ForceRefresh()
	}

	return nil
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
