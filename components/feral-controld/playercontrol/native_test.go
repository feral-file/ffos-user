package playercontrol

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

type fakePlayer struct {
	method string
	params any
	fence  *playerlink.Preconditions
	err    error
	calls  int
}

func (f *fakePlayer) Snapshot(context.Context) (playerlink.Snapshot, error) {
	return playerlink.Snapshot{Instance: "new-runtime", Generation: 7, Revision: 9, Host: "responsive"}, nil
}
func (f *fakePlayer) Call(_ context.Context, method string, params, result any, fence *playerlink.Preconditions) error {
	f.calls++
	f.method, f.params, f.fence = method, params, fence
	if f.err != nil {
		return f.err
	}
	data := []byte(`{"outcome":"accepted","generation":8,"revision":10,"occurrence":"new-work"}`)
	return json.Unmarshal(data, result)
}
func cast() commands.Command {
	return commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: map[string]any{"dp1_call": map[string]any{"items": []any{map[string]any{"source": "https://example.org/art"}}}, "playlistUrl": "https://example.org/list"}}
}
func TestCastUsesTypedOperationAndDoesNotClaimPresentation(t *testing.T) {
	f := &fakePlayer{}
	n := &Native{Player: f}
	reply, err := n.Send(context.Background(), cast())
	require.NoError(t, err)
	require.Equal(t, "playback.replace", f.method)
	require.Equal(t, uint64(7), f.fence.Generation)
	body, _ := json.Marshal(reply)
	require.Contains(t, string(body), `"outcome":"accepted"`)
	require.NotContains(t, string(body), "presented")
	require.NotContains(t, f.params, "dp1_call")
}
func TestRefreshKeepsPreResolutionFenceAndNeverRetries(t *testing.T) {
	f := &fakePlayer{err: &playerlink.RemoteError{Code: "stale_generation", Message: "new cast"}}
	n := &Native{Player: f}
	command := cast()
	command.Arguments["refresh"] = true
	reply, err := n.Send(WithFence(context.Background(), &playerlink.Preconditions{Instance: "old-runtime", Generation: 2}), command)
	require.NoError(t, err)
	require.Equal(t, "playback.update", f.method)
	require.Equal(t, uint64(2), f.fence.Generation)
	require.Equal(t, 1, f.calls)
	body, _ := json.Marshal(reply)
	require.Contains(t, string(body), "stale_generation")
	f.err = errors.New("transport lost")
	reply, err = n.Send(context.Background(), cast())
	require.NoError(t, err)
	body, _ = json.Marshal(reply)
	require.Contains(t, string(body), "outcome_unknown")
	require.Equal(t, 2, f.calls)
}
func TestDisplayAtCompilesWithoutFilteringAndBootIsAtomic(t *testing.T) {
	c := cast()
	c.Arguments["intent"] = map[string]any{"action": "display_at_boot"}
	c.Arguments["dp1_call"] = map[string]any{"items": []any{map[string]any{"source": "https://example.org/a", "displayAt": "2030-01-01T00:00:00Z"}, map[string]any{"source": "https://example.org/b"}}}
	method, p, err := castOperation(c)
	require.NoError(t, err)
	require.Equal(t, "playback.replace", method)
	require.Equal(t, true, p["boot"])
	require.Equal(t, []any{int64(1893456000000), nil}, p["activationTimes"])
}

func TestNativeCapsuleCastBindsLocalRouteAndSelectedEdition(t *testing.T) {
	f := &fakePlayer{}
	n := &Native{Player: f, GatewayOverride: func(source, _ string) (string, bool) {
		return "https://gateway.test/second", source == "https://example.org/b"
	}}
	cmd := cast()
	cmd.OfflineCapsuleID = "verified-capture"
	cmd.Arguments["startItem"] = map[string]any{"id": "second"}
	cmd.Arguments["dp1_call"] = map[string]any{"items": []any{
		map[string]any{"id": "first", "source": "https://example.org/a", "ffObservedGateway": "untrusted"},
		map[string]any{"id": "second", "source": "https://example.org/b", "ffCapsuleId": "untrusted"},
	}}
	prior := &playerlink.Preconditions{Instance: "cast-instance", Generation: 4}
	_, err := n.Send(WithFence(t.Context(), prior), cmd)
	require.NoError(t, err)
	require.Equal(t, "offlineCapsule.v1", f.fence.RequiredCapability)
	require.Empty(t, prior.RequiredCapability)
	require.Equal(t, prior.Generation, f.fence.Generation)
	params := f.params.(map[string]any)
	require.Equal(t, 1, params["index"])
	items := params["playlist"].(map[string]any)["items"].([]any)
	require.NotContains(t, items[0], "ffObservedGateway")
	require.Equal(t, "https://gateway.test/second", items[1].(map[string]any)["ffObservedGateway"])
	for _, item := range items {
		require.Equal(t, cmd.OfflineCapsuleID, item.(map[string]any)["ffCapsuleId"])
	}
}

func TestNativeScheduledCastAndRefresh(t *testing.T) {
	const at = "2030-01-01T00:00:00Z"
	const millis = int64(1893456000000)
	cmd := cast()
	cmd.Arguments["dp1_call"] = map[string]any{"items": []any{map[string]any{"source": "https://example.org/art", "displayAt": at}}}
	cmd.Arguments["intent"] = map[string]any{"action": "schedule_play", "schedule_time": at}
	method, params, err := castOperation(cmd)
	require.NoError(t, err)
	require.Equal(t, "schedule.set", method)
	require.Equal(t, millis, params["atMs"])
	require.Equal(t, cmd.Arguments["playlistUrl"], params["playlistUrl"])
	cmd.Arguments["refresh"] = true
	cmd.Arguments["retireBlockedCurrent"] = true
	method, params, err = castOperation(cmd)
	require.NoError(t, err)
	require.Equal(t, "playback.update", method)
	require.Equal(t, []any{millis}, params["activationTimes"])
	require.Equal(t, true, params["retireBlockedCurrent"])
	require.NotContains(t, params, "atMs")
}

func TestNativeRejectsInvalidCastBeforeSending(t *testing.T) {
	for _, args := range []map[string]any{
		{"dp1_call": nil},
		{"dp1_call": map[string]any{"items": []any{map[string]any{"displayAt": 123}}}},
		{"dp1_call": map[string]any{"items": []any{map[string]any{"displayAt": "no-such-day"}}}},
		{"dp1_call": map[string]any{}, "intent": map[string]any{"action": "schedule_play"}},
		{"dp1_call": map[string]any{}, "intent": map[string]any{"action": "schedule_play", "schedule_time": "tomorrow"}},
		{"dp1_call": map[string]any{}, "intent": map[string]any{"action": "other"}},
	} {
		f := &fakePlayer{}
		reply, err := (&Native{Player: f}).Send(t.Context(), commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: args})
		require.NoError(t, err)
		require.Equal(t, "invalid_params", reply.(map[string]any)["message"].(map[string]any)["code"])
		require.Zero(t, f.calls)
	}
}

func TestNativeDeviceCommands(t *testing.T) {
	for _, tc := range []struct {
		command commands.Type
		args    map[string]any
		method  string
		params  map[string]any
	}{
		{"disconnect", nil, "playback.stop", map[string]any{}},
		{commands.CMD_SET_SLEEP_MODE, map[string]any{"sleepMode": true}, "playback.sleep", map[string]any{"sleeping": true}},
		{"mintPairingDisplay", map[string]any{"state": "code", "code": "123"}, "overlay.set", map[string]any{"owner": "pairing", "state": "code", "fields": map[string]any{"code": "123"}}},
		{commands.CMD_SET_CONTENT_POLICY, map[string]any{"contentPolicy": map[string]any{"showMatureContent": false}}, "policy.set", map[string]any{"showMatureContent": false}},
	} {
		t.Run(string(tc.command), func(t *testing.T) {
			f := &fakePlayer{}
			reply, err := (&Native{Player: f}).Send(t.Context(), commands.Command{Type: tc.command, Arguments: tc.args})
			require.NoError(t, err)
			require.Equal(t, true, reply.(map[string]any)["message"].(map[string]any)["ok"])
			require.Equal(t, tc.method, f.method)
			require.Equal(t, tc.params, f.params)
		})
	}
}

type playbackResponse struct {
	state                playerlink.Snapshot
	readErr, errorOnCall error
	body                 string
}

func (p playbackResponse) Snapshot(context.Context) (playerlink.Snapshot, error) {
	return p.state, p.readErr
}
func (p playbackResponse) Call(_ context.Context, _ string, _, out any, _ *playerlink.Preconditions) error {
	if p.errorOnCall != nil {
		return p.errorOnCall
	}
	return json.Unmarshal([]byte(p.body), out)
}

func TestNativeReadinessAndRefusals(t *testing.T) {
	good := playerlink.Snapshot{Instance: "host", Generation: 4, Revision: 3, Host: "responsive"}
	for _, tc := range []struct {
		name    string
		player  Player
		command commands.Type
		code    string
	}{
		{"unwired", nil, commands.CMD_DISPLAY_PLAYLIST, "unavailable"},
		{"default unavailable", playbackResponse{state: good}, commands.CMD_DISPLAY_DEFAULT_PLAYLIST, "unavailable"},
		{"policy unavailable", playbackResponse{state: good, errorOnCall: errors.New("offline")}, commands.CMD_GET_CONTENT_POLICY, "unavailable"},
		{"malformed policy", playbackResponse{state: good, body: `"wrong"`}, commands.CMD_GET_CONTENT_POLICY, "invalid_response"},
		{"missing host", playbackResponse{readErr: errors.New("offline")}, commands.CMD_DISPLAY_PLAYLIST, "unavailable"},
		{"invalid host", playbackResponse{}, commands.CMD_DISPLAY_PLAYLIST, "unavailable"},
		{"unknown receipt", playbackResponse{state: good, body: `{"outcome":"loading"}`}, commands.CMD_DISPLAY_PLAYLIST, "outcome_unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := &Native{Player: tc.player}
			cmd := cast()
			cmd.Type = tc.command
			reply, err := n.Send(t.Context(), cmd)
			require.NoError(t, err)
			require.Equal(t, tc.code, reply.(map[string]any)["message"].(map[string]any)["code"])
			if tc.player == nil {
				require.False(t, n.Ready(t.Context()))
				return
			}
			fence, err := n.Fence(t.Context())
			require.Equal(t, n.Ready(t.Context()), err == nil)
			if err == nil {
				require.Equal(t, good.Preconditions(), fence)
			}
		})
	}
	n := &Native{Player: playbackResponse{state: good, body: `{"showMatureContent":false}`}}
	v, err := n.Send(t.Context(), commands.Command{Type: commands.CMD_GET_CONTENT_POLICY})
	require.NoError(t, err)
	require.Equal(t, map[string]any{"showMatureContent": false}, v.(map[string]any)["message"].(map[string]any)["contentPolicy"])
}

func TestNativeStartItemPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start map[string]any
		index any
	}{
		{"prefer edition ID", map[string]any{"id": "second", "source": "https://example.org/first"}, 1},
		{"fall back to source", map[string]any{"id": "missing", "source": "https://example.org/first"}, 0},
		{"no match", map[string]any{"source": "absent"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := cast()
			cmd.Arguments["startItem"] = tc.start
			cmd.Arguments["onlyIfNoPlaylist"] = true
			cmd.Arguments["dp1_call"] = map[string]any{"items": []any{map[string]any{"id": "first", "source": "https://example.org/first"}, map[string]any{"id": "second", "source": "https://example.org/second"}}}
			method, p, err := castOperation(cmd)
			require.NoError(t, err)
			require.Equal(t, "playback.replace", method)
			require.Equal(t, tc.index, p["index"])
			require.Equal(t, true, p["onlyIfNoPlaylist"])
		})
	}
}
