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
	if err := sigverify.SaveMode(e.os, e.json, mode); err != nil {
		return nil, fmt.Errorf("failed to persist signature verification mode: %w", err)
	}
	e.logger.Info("Signature verification mode set", zap.String("mode", string(mode)))
	return map[string]interface{}{"ok": true, "signatureVerificationMode": string(mode)}, nil
}

// clearSignatureVerificationMode is factory reset's hand-on step for the
// mode record: a previous owner's `strict` must not silently block the next
// owner's casts. Best-effort like the device-name clear beside it.
func (e *executor) clearSignatureVerificationMode() error {
	return sigverify.ClearMode(e.os)
}
