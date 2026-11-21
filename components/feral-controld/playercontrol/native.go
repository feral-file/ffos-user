package playercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/display-protocol/dp1-go/displayat"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

// Sender is the internal external-command adapter used after controld admission.
// Its wire-facing implementation emits only typed native protocol operations.
type Sender interface {
	Send(context.Context, commands.Command) (interface{}, error)
	Ready(context.Context) bool
}

type Player interface {
	Snapshot(context.Context) (playerlink.Snapshot, error)
	Call(context.Context, string, any, any, *playerlink.Preconditions) error
}

type Native struct {
	GatewayOverride func(string, string) (string, bool)
	Player          Player
	Logger          *zap.Logger
	Default         func(context.Context, bool) (interface{}, error)
}

type fenceKey struct{}

func WithFence(ctx context.Context, fence *playerlink.Preconditions) context.Context {
	return context.WithValue(ctx, fenceKey{}, fence)
}
func (h *Native) Fence(ctx context.Context) (*playerlink.Preconditions, error) {
	s, err := h.Player.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err = s.Validate(); err != nil {
		return nil, err
	}
	return s.Preconditions(), nil
}
func (h *Native) OwnsSchedule() bool { return true }

func (h *Native) Ready(ctx context.Context) bool {
	if h.Player == nil {
		return false
	}
	state, err := h.Player.Snapshot(ctx)
	return err == nil && state.Validate() == nil
}
func controlFailure(code, message string) (interface{}, error) {
	return map[string]any{"message": map[string]any{"ok": false, "code": code, "error": message}}, nil
}

// Send is the external-command translation boundary. The host sees
// only typed playback operations; neither controller names nor JavaScript cross
// the socket. Resolution and admission have already run in Process.
func (h *Native) Send(ctx context.Context, command commands.Command) (interface{}, error) {
	if h.Player == nil {
		return controlFailure("unavailable", "Native player is unavailable")
	}
	if command.Type == commands.CMD_DISPLAY_DEFAULT_PLAYLIST {
		if h.Default == nil {
			return controlFailure("unavailable", "Default playback source is unavailable")
		}
		only, _ := command.Arguments["onlyIfNoPlaylist"].(bool)
		return h.Default(ctx, only)
	}
	if command.Type == commands.CMD_GET_CONTENT_POLICY {
		var policy json.RawMessage
		if err := h.Player.Call(ctx, "policy.get", map[string]any{}, &policy, nil); err != nil {
			return controlFailure("unavailable", "Native policy is unavailable")
		}
		var values map[string]any
		if err := json.Unmarshal(policy, &values); err != nil {
			return controlFailure("invalid_response", "Invalid policy response")
		}
		return map[string]any{"message": map[string]any{"ok": true, "active": true, "contentPolicy": values}}, nil
	}
	state, err := h.Player.Snapshot(ctx)
	if err != nil || state.Validate() != nil {
		return controlFailure("unavailable", "Native player state is unavailable; no command was sent")
	}
	method, params, err := castOperationWithRoutes(command, h.GatewayOverride)
	if err != nil {
		return controlFailure("invalid_params", err.Error())
	}
	fence := state.Preconditions()
	if prior, ok := ctx.Value(fenceKey{}).(*playerlink.Preconditions); ok && prior != nil {
		fence = prior
	}
	var receipt playerlink.Receipt
	if err = h.Player.Call(ctx, method, params, &receipt, fence); err != nil {
		var remote *playerlink.RemoteError
		if errors.As(err, &remote) {
			return controlFailure(remote.Code, remote.Message)
		}
		if h.Logger != nil {
			h.Logger.Warn("Native cast outcome unknown", zap.Error(err))
		}
		return controlFailure("outcome_unknown", "Native command result is unknown; reconcile status before retrying")
	}
	if receipt.Outcome != "accepted" && receipt.Outcome != "applied" {
		return controlFailure("outcome_unknown", "Native player returned an invalid receipt")
	}
	if command.Type == commands.CMD_SET_CONTENT_POLICY {
		return map[string]any{"message": map[string]any{"ok": true, "active": true, "contentPolicy": command.Arguments["contentPolicy"]}}, nil
	}
	return map[string]any{"message": map[string]any{
		"ok": true, "outcome": receipt.Outcome,
		"runtime": map[string]any{"instance": state.Instance, "generation": receipt.Generation, "revision": receipt.Revision, "occurrence": receipt.Occurrence},
	}}, nil
}

func castOperation(command commands.Command) (string, map[string]any, error) {
	return castOperationWithRoutes(command, nil)
}

func castOperationWithRoutes(command commands.Command, lookup func(string, string) (string, bool)) (string, map[string]any, error) {
	args := command.Arguments
	switch command.Type {
	case commands.CMD_SET_CONTENT_POLICY:
		data, err := json.Marshal(args["contentPolicy"])
		if err != nil {
			return "", nil, err
		}
		var values map[string]any
		if err = json.Unmarshal(data, &values); err != nil || values == nil {
			return "", nil, fmt.Errorf("invalid content policy")
		}
		return "policy.set", values, nil
	case "mintPairingDisplay":
		fields := map[string]any{}
		for key, value := range args {
			if key != "state" {
				fields[key] = value
			}
		}
		return "overlay.set", map[string]any{"owner": "pairing", "state": args["state"], "fields": fields}, nil

	case "disconnect":
		return "playback.stop", map[string]any{}, nil
	case commands.CMD_SET_SLEEP_MODE:
		return "playback.sleep", map[string]any{"sleeping": args["sleepMode"]}, nil
	case commands.CMD_DISPLAY_PLAYLIST:
		data, err := json.Marshal(args["dp1_call"])
		if err != nil {
			return "", nil, err
		}
		var playlist map[string]any
		if err = json.Unmarshal(data, &playlist); err != nil || playlist == nil {
			return "", nil, fmt.Errorf("playlistInvalid")
		}
		contentContext, _ := args["contentContext"].(string)
		if contentContext == "" {
			contentContext = "curated"
		}
		params := map[string]any{"playlist": playlist, "context": contentContext}
		times := []any{}
		timed := false
		items, _ := playlist["items"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			// Only the local media-health store may supply an observed route.
			delete(item, "ffObservedGateway")
			if source, ok := item["source"].(string); ok && lookup != nil {
				if target, ok := lookup(source, ""); ok {
					item["ffObservedGateway"] = target
				}
			}
			if value, exists := item["displayAt"]; exists && value != nil {
				text, ok := value.(string)
				if !ok {
					return "", nil, fmt.Errorf("invalid displayAt")
				}
				parsed := displayat.Parse(text, sleepschedule.LocalTimezone())
				if !parsed.IsValid() {
					return "", nil, fmt.Errorf("invalid displayAt")
				}
				times = append(times, parsed.Resolved.UnixMilli())
				timed = true
			} else {
				times = append(times, nil)
			}
		}
		if timed {
			params["activationTimes"] = times
		}
		if start, ok := args["startItem"].(map[string]any); ok {
			found := false
			for _, key := range []string{"id", "source"} {
				wanted, _ := start[key].(string)
				if wanted == "" {
					continue
				}
				for i, raw := range items {
					item, _ := raw.(map[string]any)
					if item[key] == wanted {
						params["index"] = i
						found = true
						break
					}
				}
				if found {
					break
				}
			}
		}

		if refresh, _ := args["refresh"].(bool); refresh {
			params = map[string]any{"items": playlist["items"], "context": contentContext}
			if timed {
				params["activationTimes"] = times
			}
			if retire, exists := args["retireBlockedCurrent"]; exists {
				params["retireBlockedCurrent"] = retire
			}
			return "playback.update", params, nil
		}
		intent, _ := args["intent"].(map[string]any)
		action, _ := intent["action"].(string)
		switch action {
		case "", "now_display":
			if only, ok := args["onlyIfNoPlaylist"].(bool); ok {
				params["onlyIfNoPlaylist"] = only
			}
			if url, ok := args["playlistUrl"].(string); ok {
				params["playlistUrl"] = url
			}
			return "playback.replace", params, nil
		case "display_at_boot":
			params["boot"] = true
			if url, ok := args["playlistUrl"].(string); ok {
				params["playlistUrl"] = url
			}
			return "playback.replace", params, nil
		case "schedule_play":
			at, ok := intent["schedule_time"].(string)
			if !ok {
				return "", nil, fmt.Errorf("schedule_time must be an RFC3339 timestamp")
			}
			parsed, err := time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return "", nil, fmt.Errorf("invalid schedule_time")
			}
			params["atMs"] = parsed.UnixMilli()
			if url, ok := args["playlistUrl"].(string); ok {
				params["playlistUrl"] = url
			}
			return "schedule.set", params, nil
		default:
			return "", nil, fmt.Errorf("unsupported playback intent %q", action)
		}
	default:
		return "", nil, fmt.Errorf("unsupported player command %q", command.Type)
	}
}
