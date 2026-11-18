package commandrouter_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

type controlPlayer struct {
	state       playerlink.Snapshot
	err         error
	snapshotErr error
	call        func(string, any, *playerlink.Preconditions)
	calls       int
}

func (p *controlPlayer) Snapshot(context.Context) (playerlink.Snapshot, error) {
	return p.state, p.snapshotErr
}
func (p *controlPlayer) Call(_ context.Context, method string, params, result any, expected *playerlink.Preconditions) error {
	p.calls++
	if p.call != nil {
		p.call(method, params, expected)
	}
	if p.err != nil {
		return p.err
	}
	occurrence := "showing-one"
	floor := uint64(7)
	*result.(*playerlink.Receipt) = playerlink.Receipt{Outcome: "accepted", Generation: 2, Revision: 8, Occurrence: &occurrence, AcceptedCompositionRevision: &floor}
	return nil
}
func controlSnapshot() playerlink.Snapshot {
	item := json.RawMessage(`{"source":"https://example.org/same"}`)
	selected := &playerlink.Occurrence{ID: "showing-one", Generation: 2, Index: 0, Stage: "presented", NavigationCommitted: true, Item: item}
	presented := *selected
	return playerlink.Snapshot{Instance: "host-one", Generation: 2, Revision: 7, Host: "responsive", Selected: selected, Presented: &presented, PresentedPlaylist: json.RawMessage(`{"items":[{"source":"https://example.org/same"}]}`)}
}
func TestNativeControlTranslation(t *testing.T) {
	cases := []struct {
		name, method string
		args, want   map[string]any
	}{
		{"moveToArtwork", "playback.select", map[string]any{"index": float64(1)}, map[string]any{"index": float64(1)}},
		{"setShuffle", "playback.mode", map[string]any{"enabled": true}, map[string]any{"shuffle": true}},
		{"setLoop", "playback.mode", map[string]any{"mode": "one"}, map[string]any{"loop": "one"}},
		{"updateDefaultDuration", "settings.set", nil, map[string]any{"durationSeconds": nil}},
		{"updateDefaultDuration", "settings.set", map[string]any{"durationSeconds": float64(30)}, map[string]any{"durationSeconds": float64(30)}},
		{"updateArtFraming", "settings.set", map[string]any{"frameConfig": float64(1)}, map[string]any{"scaling": "fill"}},
		{"updateDisplaySettings", "settings.set", map[string]any{"isSaved": true, "framing": "artwork", "tombstone": "on"}, map[string]any{"framing": "artwork", "tombstone": "on"}},
		{"updateDisplaySettings", "presentation.set", map[string]any{"isSaved": false, "margin": "10%"}, map[string]any{"occurrence": "showing-one", "display": map[string]any{"margin": "10%"}}},
		{"updateDisplaySettings", "presentation.set", map[string]any{"isSaved": false, "showingKey": "showing-one", "scaling": "fit", "background": "#112233"}, map[string]any{"occurrence": "showing-one", "display": map[string]any{"scaling": "fit", "background": "#112233"}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := setup(t)
			p := &controlPlayer{state: controlSnapshot()}
			p.call = func(method string, params any, expected *playerlink.Preconditions) {
				require.Equal(t, tc.method, method)
				require.Equal(t, tc.want, params)
				require.Equal(t, &playerlink.Preconditions{Instance: "host-one", Generation: 2}, expected)
			}
			commandrouter.SetNativeControls(ts.handler, p)
			ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)
			got, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.Type(tc.name), Arguments: tc.args})
			require.NoError(t, err)
			reply := got.(map[string]any)["message"].(map[string]any)
			require.Equal(t, true, reply["ok"])
			require.Equal(t, "accepted", reply["outcome"])
			require.Equal(t, uint64(7), reply["acceptedCompositionRevision"])
			require.Equal(t, 1, p.calls)
		})
	}
}
func TestNativeControlsRejectWrongShowingAndFields(t *testing.T) {
	cases := []struct {
		name     string
		args     map[string]any
		incoming bool
		code     string
	}{
		{"same URL other occurrence", map[string]any{"isSaved": false, "showingKey": "previous-occurrence", "scaling": "fit"}, false, "stale_occurrence"},
		{"keyless while loading", map[string]any{"isSaved": false, "margin": 20}, true, "stale_occurrence"},
		{"framing preview", map[string]any{"isSaved": false, "framing": "fill"}, false, "invalid_params"},
		{"label preview", map[string]any{"isSaved": false, "tombstone": "off"}, false, "invalid_params"},
		{"no values", map[string]any{"isSaved": true}, false, "invalid_params"},
		{"missing saved flag", map[string]any{"scaling": "fit"}, false, "invalid_params"},
		{"unknown mixed with valid", map[string]any{"isSaved": true, "scaling": "fill", "unrecognized": true}, false, "invalid_params"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ts := setup(t)
			p := &controlPlayer{state: controlSnapshot()}
			if tc.incoming {
				p.state.Selected.ID = "incoming"
			}
			commandrouter.SetNativeControls(ts.handler, p)
			got, err := ts.handler.Process(ts.ctx, commands.Command{Type: "updateDisplaySettings", Arguments: tc.args})
			require.NoError(t, err)
			reply := got.(map[string]any)["message"].(map[string]any)
			require.Equal(t, false, reply["ok"])
			require.Equal(t, tc.code, reply["code"])
			require.Zero(t, p.calls)
		})
	}
}
func TestNativeControlFailuresDoNotFallBackOrRetry(t *testing.T) {
	for _, failure := range []error{&playerlink.RemoteError{Code: "stale_generation", Message: "cast changed"}, &playerlink.RemoteError{Code: "storage_unavailable", Message: "disk full"}, errors.New("transport closed")} {
		t.Run(failure.Error(), func(t *testing.T) {
			ts := setup(t)
			p := &controlPlayer{state: controlSnapshot(), err: failure}
			commandrouter.SetNativeControls(ts.handler, p)
			got, err := ts.handler.Process(ts.ctx, commands.Command{Type: "setLoop", Arguments: map[string]any{"mode": "one"}})
			var remote *playerlink.RemoteError
			if errors.As(failure, &remote) {
				require.NoError(t, err)
				reply := got.(map[string]any)["message"].(map[string]any)
				require.Equal(t, false, reply["ok"])
				require.Equal(t, remote.Code, reply["code"])
			} else {
				require.NoError(t, err)
				require.Equal(t, "outcome_unknown", got.(map[string]any)["message"].(map[string]any)["code"])
			}
			require.Equal(t, 1, p.calls)
		})
	}
	ts := setup(t)
	got, err := ts.handler.Process(ts.ctx, commands.Command{Type: "setLoop", Arguments: map[string]any{"mode": "one"}})
	require.NoError(t, err)
	require.Equal(t, "unavailable", got.(map[string]any)["message"].(map[string]any)["code"])
}

// Opt-in hardware test: the caller must prepare an isolated two-item fixture.
// This never targets an ordinary user playlist or the production socket.
func TestNativeControlsDevice(t *testing.T) {
	path := os.Getenv("FF_NATIVE_CONTROLS_TEST_SOCKET")
	if path == "" {
		t.Skip("isolated FF1 controls fixture required")
	}
	client := playerlink.New(path)
	defer func() { _ = client.Close() }()
	ctx := context.Background()
	before, err := client.Snapshot(ctx)
	require.NoError(t, err)
	var playlist struct {
		ID string `json:"id"`
	}
	require.NoError(t, json.Unmarshal(before.Playlist, &playlist))
	require.Equal(t, "controld-controls-test", playlist.ID)
	require.NotNil(t, before.Presented)
	ts := setup(t)
	commandrouter.SetNativeControls(ts.handler, client)
	ts.mockStatusPoller.EXPECT().ForceRefresh().AnyTimes()
	send := func(name string, args map[string]any) map[string]any {
		t.Helper()
		got, err := ts.handler.Process(ctx, commands.Command{Type: commands.Type(name), Arguments: args})
		require.NoError(t, err)
		return got.(map[string]any)["message"].(map[string]any)
	}
	require.Equal(t, true, send("updateDisplaySettings", map[string]any{"isSaved": true, "framing": "artwork", "tombstone": "on"})["ok"])
	require.Equal(t, true, send("updateDefaultDuration", map[string]any{"durationSeconds": float64(120)})["ok"])
	require.Equal(t, true, send("setLoop", map[string]any{"mode": "one"})["ok"])
	require.Equal(t, true, send("setShuffle", map[string]any{"enabled": true})["ok"])
	state, err := client.Snapshot(ctx)
	require.NoError(t, err)
	require.Equal(t, "on", state.Settings.Tombstone)
	require.Equal(t, 120., *state.Settings.DurationSeconds)
	require.Equal(t, "one", state.Loop)
	require.True(t, state.Shuffle)
	old := state.Presented.ID
	require.Equal(t, true, send("moveToArtwork", map[string]any{"index": float64(1)})["ok"])
	require.Eventually(t, func() bool {
		state, err = client.Snapshot(ctx)
		return err == nil && state.Presented != nil && state.Presented.Index == 1 && state.Presented.ID != old
	}, 15*time.Second, 100*time.Millisecond)
	rejected := send("updateDisplaySettings", map[string]any{"isSaved": false, "showingKey": old, "margin": 40})
	require.Equal(t, false, rejected["ok"])
	require.Equal(t, "stale_occurrence", rejected["code"])
	accepted := send("updateDisplaySettings", map[string]any{"isSaved": false, "showingKey": state.Presented.ID, "margin": "10%", "background": "#112233"})
	require.Equal(t, true, accepted["ok"])
	floor := accepted["acceptedCompositionRevision"].(uint64)
	require.Eventually(t, func() bool {
		state, err = client.Snapshot(ctx)
		return err == nil && state.Composition != nil && state.Composition.Revision > floor && string(state.Composition.Display.Margin) == `"10%"` && state.Composition.Display.Background == "#112233"
	}, 10*time.Second, 100*time.Millisecond)
	rejected = send("setShuffle", map[string]any{"enabled": "yes"})
	require.Equal(t, false, rejected["ok"])
	require.Equal(t, "invalid_params", rejected["code"])
	require.Equal(t, true, send("updateDefaultDuration", nil)["ok"])
	state, err = client.Snapshot(ctx)
	require.NoError(t, err)
	require.Nil(t, state.Settings.DurationSeconds)
	t.Log("saved settings, loop/shuffle, same-URL occurrence selection, stale preview rejection, composed preview, invalid host parameter and duration reset passed")
}

func TestNativeControlSnapshotFailureRepliesWithoutMutation(t *testing.T) {
	for _, invalid := range []bool{false, true} {
		t.Run(fmt.Sprint(invalid), func(t *testing.T) {
			ts := setup(t)
			p := &controlPlayer{state: controlSnapshot()}
			code := "unavailable"
			if invalid {
				p.state.Instance = ""
				code = "invalid_response"
			} else {
				p.snapshotErr = errors.New("socket unavailable")
			}
			commandrouter.SetNativeControls(ts.handler, p)
			got, err := ts.handler.Process(ts.ctx, commands.Command{Type: "setLoop", Arguments: map[string]any{"mode": "one"}})
			require.NoError(t, err)
			reply := got.(map[string]any)["message"].(map[string]any)
			require.Equal(t, false, reply["ok"])
			require.Equal(t, code, reply["code"])
			require.Zero(t, p.calls)
		})
	}
}
