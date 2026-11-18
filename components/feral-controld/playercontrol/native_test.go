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
