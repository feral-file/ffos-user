package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playercontrol"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

type defaultPlaylistSnapshot struct {
	playercontrol.Player
	playlist string
	err      error
}

func (s defaultPlaylistSnapshot) Snapshot(context.Context) (playerlink.Snapshot, error) {
	return playerlink.Snapshot{Playlist: json.RawMessage(s.playlist)}, s.err
}

type defaultRoute struct{ commands []commands.Command }

func (r *defaultRoute) Process(_ context.Context, c commands.Command) (any, error) {
	r.commands = append(r.commands, c)
	return "routed", nil
}

func TestConditionalDefaultPreservesExistingArtwork(t *testing.T) {
	for _, tc := range []struct {
		name, playlist string
		only           bool
		readErr        error
		route          bool
	}{
		{"playlist already installed", `{"items":[{"source":"https://example.org/edition"}]}`, true, nil, false},
		{"empty playlist", `{"items":[]}`, true, nil, true},
		{"missing playlist", "null", true, nil, true},
		{"unavailable device", "", true, errors.New("offline"), false},
		{"explicit default", `{"items":[{}]}`, false, nil, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			player := &playercontrol.Native{Player: defaultPlaylistSnapshot{playlist: tc.playlist, err: tc.readErr}}
			router := &defaultRoute{}
			wireNativeDefault(player, router)
			reply, err := player.Send(t.Context(), commands.Command{Type: commands.CMD_DISPLAY_DEFAULT_PLAYLIST, Arguments: map[string]any{"onlyIfNoPlaylist": tc.only}})
			if tc.readErr != nil {
				require.ErrorIs(t, err, tc.readErr)
				require.Empty(t, router.commands)
				return
			}
			require.NoError(t, err)
			if !tc.route {
				require.Empty(t, router.commands)
				require.Equal(t, "applied", reply.(map[string]any)["message"].(map[string]any)["outcome"])
				return
			}
			require.Equal(t, "routed", reply)
			require.Len(t, router.commands, 1)
			require.Equal(t, commands.CMD_DISPLAY_PLAYLIST, router.commands[0].Type)
			require.Equal(t, map[string]any{"playlistUrl": nativeDefaultPlaylistURL, "contentContext": "curated", "onlyIfNoPlaylist": tc.only}, router.commands[0].Arguments)
		})
	}
}
