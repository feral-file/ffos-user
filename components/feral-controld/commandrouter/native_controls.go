package commandrouter

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

// NativeControlPlayer is the host boundary, never an artwork document. Snapshot
// and mutation share a generation fence; a concurrent replacement rejects the
// command instead of applying it to a work the controller did not observe.
type NativeControlPlayer interface {
	Snapshot(context.Context) (playerlink.Snapshot, error)
	Call(context.Context, string, any, any, *playerlink.Preconditions) error
}

// SetNativeControls wires typed playback/presentation controls. Missing wiring
// fails explicitly; these commands have no CDP fallback during migration.
func SetNativeControls(h Handler, player NativeControlPlayer) {
	h.(*handler).nativeControls = player
}

func nativeControl(command commands.Type) bool {
	switch command {
	case commands.CMD_REFRESH_ARTWORK, "moveToArtwork", "setShuffle", "setLoop", "updateArtFraming", "updateDisplaySettings", "updateDefaultDuration":
		return true
	}
	return false
}

func controlFailure(code, message string) (interface{}, error) {
	return map[string]any{"message": map[string]any{"ok": false, "code": code, "error": message}}, nil
}

// Fields without implemented native semantics fail visibly instead of claiming
// success. Further porting of legacy per-item flags remains explicit.
// Playback policy stays in the host; this adapter only translates public names.
func nativeControlOperation(command commands.Command, state playerlink.Snapshot) (string, map[string]any, error) {
	args := command.Arguments
	params := map[string]any{}
	method := ""
	var keys []string
	switch command.Type {
	case commands.CMD_REFRESH_ARTWORK:
		method = "playback.refresh"
		if state.Selected == nil {
			return "", nil, &playerlink.RemoteError{Code: "no_artwork", Message: "No active artwork to refresh"}
		}
	case "moveToArtwork":
		keys = []string{"index"}
		method = "playback.select"
		params["index"] = args["index"]
	case "setShuffle":
		keys = []string{"enabled"}
		method = "playback.mode"
		params["shuffle"] = args["enabled"]
	case "setLoop":
		keys = []string{"mode"}
		method = "playback.mode"
		params["loop"] = args["mode"]
	case "updateDefaultDuration":
		keys = []string{"durationSeconds"}
		method = "settings.set"
		params["durationSeconds"] = args["durationSeconds"]
	case "updateArtFraming":
		keys = []string{"frameConfig"}
		method = "settings.set"
		// The legacy enum is numeric: 0 fit, 1 fill. Do not coerce arbitrary
		// values to fit as the browser used to do.
		value, ok := args["frameConfig"].(float64)
		if !ok || math.IsNaN(value) || (value != 0 && value != 1) {
			return "", nil, fmt.Errorf("frameConfig must be 0 (fit) or 1 (fill)")
		}
		params["scaling"] = "fit"
		if value == 1 {
			params["scaling"] = "fill"
		}
	case "updateDisplaySettings":
		keys = []string{"isSaved", "showingKey", "scaling", "margin", "background", "framing", "tombstone"}
		saved, ok := args["isSaved"].(bool)
		if !ok {
			return "", nil, fmt.Errorf("isSaved must be a boolean")
		}
		for _, key := range []string{"scaling", "margin", "background", "framing", "tombstone"} {
			if value, present := args[key]; present {
				params[key] = value
			}
		}
		if len(params) == 0 {
			return "", nil, fmt.Errorf("no display settings supplied")
		}
		if saved {
			method = "settings.set"
			break
		}
		if _, present := args["framing"]; present {
			return "", nil, fmt.Errorf("framing must be a saved preference")
		}
		if _, present := args["tombstone"]; present {
			return "", nil, fmt.Errorf("tombstone must be a saved preference")
		}
		// A keyless older phone can target the current showing, but only when
		// selection and presentation agree. The host rechecks this at dispatch.
		if state.Selected == nil || state.Presented == nil || state.Selected.ID != state.Presented.ID {
			return "", nil, &playerlink.RemoteError{Code: "stale_occurrence", Message: "The showing changed or is still loading."}
		}
		if key, present := args["showingKey"]; present && key != state.Presented.ID {
			return "", nil, &playerlink.RemoteError{Code: "stale_occurrence", Message: "The showing changed or is still loading."}
		}
		method = "presentation.set"
		params = map[string]any{"occurrence": state.Presented.ID, "display": params}
	default:
		return "", nil, fmt.Errorf("unknown native control")
	}
	for key := range args {
		if !slices.Contains(keys, key) {
			return "", nil, fmt.Errorf("unsupported request field %q", key)
		}
	}
	return method, params, nil
}

func (h *handler) processNativeControl(ctx context.Context, command commands.Command) (interface{}, error) {
	if h.nativeControls == nil {
		return controlFailure("unavailable", "Native player control is unavailable")
	}
	state, err := h.nativeControls.Snapshot(ctx)
	if err != nil {
		h.logger.Warn("Native control snapshot failed", zap.Error(err))
		return controlFailure("unavailable", "Native player state is unavailable; no command was sent")
	}
	if err = state.Validate(); err != nil {
		h.logger.Warn("Native control snapshot invalid", zap.Error(err))
		return controlFailure("invalid_response", "Native player state is invalid; no command was sent")
	}
	method, params, err := nativeControlOperation(command, state)
	if err != nil {
		var remote *playerlink.RemoteError
		if errors.As(err, &remote) {
			return controlFailure(remote.Code, remote.Message)
		}
		return controlFailure("invalid_params", err.Error())
	}
	var receipt playerlink.Receipt
	err = h.nativeControls.Call(ctx, method, params, &receipt, state.Preconditions())
	if err != nil {
		var remote *playerlink.RemoteError
		if errors.As(err, &remote) {
			return controlFailure(remote.Code, remote.Message)
		}
		// The host may have committed before the connection failed. Preserve
		// uncertainty in the external reply; legacy relayers drop raw Go errors.
		h.logger.Warn("Native control outcome unknown", zap.Error(err))
		return controlFailure("outcome_unknown", "Native command result is unknown; reconcile status before retrying")
	}
	if receipt.Outcome != "accepted" && receipt.Outcome != "applied" {
		return controlFailure("outcome_unknown", "Native command returned an invalid receipt; reconcile status before retrying")
	}
	reply := map[string]any{"ok": true, "outcome": receipt.Outcome, "runtime": map[string]any{"instance": state.Instance, "generation": receipt.Generation, "revision": receipt.Revision, "occurrence": receipt.Occurrence}}
	if receipt.AcceptedCompositionRevision != nil {
		reply["acceptedCompositionRevision"] = *receipt.AcceptedCompositionRevision
	}
	if h.statusPoller != nil {
		h.statusPoller.ForceRefresh()
	}
	return map[string]any{"message": reply}, nil
}
