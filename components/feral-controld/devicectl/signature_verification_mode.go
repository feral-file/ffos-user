package devicectl

import (
	"context"
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/config"
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
	// With the verifier switched off by config nothing enforces a mode, so
	// storing and acknowledging one would advertise a policy — strict, in
	// particular — that no cast path applies. Refuse instead; device status
	// omits the field for the same reason, so a controller hides the setting.
	if !e.signatureVerificationEnabled() {
		return nil, errors.New("signature verification is disabled by device configuration; the mode cannot be set")
	}

	if err := e.storeVerificationMode(mode); err != nil {
		return nil, err
	}
	e.logger.Info("Signature verification mode set", zap.String("mode", string(mode)))

	// Notified OUTSIDE the record lock, unlike the device-name observer: what
	// the lock orders is the disk, and the observer's consumer (the displayAt
	// scheduler's re-drive) reads the mode from disk itself at push time and
	// may spend a CDP round-trip — not something to hold a factory reset's
	// clear behind. Suppressed while a reset is staged (one that latched
	// between the store and here): a scheduler push must not go out under
	// the reset panel, and the latch release re-drives from the effective
	// record if the reset rolls back (releaseStuckResetLatch).
	if e.resetStaged.Load() {
		e.logger.Info("Signature verification mode stored during a staged factory reset; re-drive deferred to the latch release")
	} else {
		e.notifyVerificationMode()
	}
	return map[string]interface{}{"ok": true, "signatureVerificationMode": string(mode)}, nil
}

// notifyVerificationMode hands the observer the mode actually on disk (never
// the mode a caller asked for): the setter's store and the reset's clear
// both flow through here, and the consumer must act on what the cast path
// will read. No-op when nothing is wired, and then the record is not read.
func (e *executor) notifyVerificationMode() {
	if e.modeObserver == nil {
		return
	}
	mode, err := sigverify.LoadMode(e.os, e.json)
	if err != nil {
		e.logger.Warn("Signature verification mode record unreadable; notifying the default", zap.Error(err))
	}
	e.modeObserver(mode)
}

// signatureVerificationEnabled reports whether the verifier runs; the
// injected predicate wins so tests need no global config.
func (e *executor) signatureVerificationEnabled() bool {
	if e.verificationEnabled != nil {
		return e.verificationEnabled()
	}
	return config.Get().SignatureVerificationEnabled()
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
