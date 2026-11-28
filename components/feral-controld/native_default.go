package main

import (
	"context"
	"encoding/json"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/playercontrol"
)

// This is the shipped ff-player fallback source. It goes through the same
// resolution, admission and verification pipeline as an external cast.
const nativeDefaultPlaylistURL = "https://dp1-feed-operator-api-prod.autonomy-system.workers.dev/api/v1/playlists/503e271c-7d96-4d80-ae10-ae2ba658d535"

func wireNativeDefault(player *playercontrol.Native, router commandrouter.Handler) {
	player.Default = func(ctx context.Context, only bool) (interface{}, error) {
		if only {
			snapshot, err := player.Player.Snapshot(ctx)
			if err != nil {
				return nil, err
			}
			var playlist struct {
				Items []json.RawMessage `json:"items"`
			}
			if json.Unmarshal(snapshot.Playlist, &playlist) == nil && len(playlist.Items) > 0 {
				return map[string]any{"message": map[string]any{"ok": true, "outcome": "applied"}}, nil
			}
		}
		return router.Process(ctx, commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: map[string]any{"playlistUrl": nativeDefaultPlaylistURL, "contentContext": "curated", "onlyIfNoPlaylist": only}})
	}
}

func nativePairingReconciler(pairing mintpairing.Service) func(context.Context) {
	return func(context.Context) { mintpairing.ResyncNative(pairing) }
}
