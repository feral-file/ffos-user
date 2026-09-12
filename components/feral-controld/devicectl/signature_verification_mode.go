package devicectl

import (
	"context"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

// setSignatureVerificationMode persists the owner's DP-1 signature
// verification policy (feral-file/ffos-user#307). The record is read at
// every displayPlaylist by commandrouter and the refresher, so a change takes
// effect on the next cast with no restart. The reply echoes the stored mode
// so a controller adopts what the device holds rather than what it sent.
func (e *executor) setSignatureVerificationMode(_ context.Context, args []byte) (interface{}, error) {
	var req struct {
		Mode string `json:"mode"`
	}
	if err := e.json.Unmarshal(args, &req); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	mode, err := sigverify.ParseMode(req.Mode)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if err := e.storeVerificationMode(mode); err != nil {
		return nil, err
	}
	e.logger.Info("Signature verification mode set", zap.String("mode", string(mode)))

	// Notified OUTSIDE the record lock, unlike the device-name observer: what
	// the lock orders is the disk, and the observer's consumer (the displayAt
	// scheduler's re-drive) reads the mode from disk itself at push time and
	// may spend a CDP round-trip — not something to hold a factory reset's
	// clear behind. A clear that lands between the store and this notify is
	// harmless: the scheduler's gate reads the record the clear left.
	if e.modeObserver != nil {
		e.modeObserver(mode)
	}
	return map[string]interface{}{"ok": true, "signatureVerificationMode": string(mode)}, nil
}

// storeVerificationMode is the locked half of the setter.
//
// One mutation lock for the record, shared with factory reset's clear — the
// device-name discipline, for the device-name reasons: two setters stage
// through the SAME .tmp path, so one could rename the other's bytes and then
// echo a mode the disk does not hold; and a setter admitted before a reset
// staged could land after the reset cleared the record, leaving a
// rolled-back unit under the previous owner's policy.
func (e *executor) storeVerificationMode(mode sigverify.Mode) error {
	e.verificationModeMu.Lock()
	defer e.verificationModeMu.Unlock()

	// Re-checked INSIDE the lock: the router's reset check only proves no
	// reset had staged when this request was admitted. factoryReset latches
	// resetStaged before it takes this lock, so a setter that loses the race
	// sees the latch here.
	if e.resetStaged.Load() {
		return fmt.Errorf("factory reset in progress")
	}
	if err := sigverify.SaveMode(e.os, e.json, mode); err != nil {
		return fmt.Errorf("failed to persist signature verification mode: %w", err)
	}
	return nil
}

// clearSignatureVerificationMode is factory reset's hand-on step for the
// mode record: a previous owner's `strict` must not silently block the next
// owner's casts. Best-effort like the device-name clear beside it.
func (e *executor) clearSignatureVerificationMode() error {
	e.verificationModeMu.Lock()
	defer e.verificationModeMu.Unlock()
	return sigverify.ClearMode(e.os)
}
