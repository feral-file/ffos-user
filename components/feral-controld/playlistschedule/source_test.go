package playlistschedule_test

import (
	"testing"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
)

func TestSourceMatchesPlaylistURL(t *testing.T) {
	source := playlistschedule.Source{PlaylistURL: "https://example.com/feed.json"}

	assert.True(t, source.Matches(playlistschedule.Source{PlaylistURL: "https://example.com/feed.json"}))
	assert.False(t, source.Matches(playlistschedule.Source{PlaylistURL: "https://example.com/other.json"}))
	assert.False(t, source.Matches(playlistschedule.Source{}))
}

func TestSourceMatchesDynamicPlaylistIdentityIgnoringResolvedItems(t *testing.T) {
	query := &playlists.DynamicQuery{
		Profile:  "graphql-v1",
		Endpoint: "https://api.example/graphql",
		Query:    "query { items(limit: {{limit}}, offset: {{offset}}) { id title source } }",
	}
	source := playlistschedule.Source{DynamicPlaylist: &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID:           "daily",
			DynamicQuery: query,
			Items:        []dp1playlist.PlaylistItem{{ID: "full"}},
		},
	}}

	assert.True(t, source.Matches(playlistschedule.Source{DynamicPlaylist: &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID:           "daily",
			DynamicQuery: query,
			Items:        []dp1playlist.PlaylistItem{{ID: "active-only"}},
		},
	}}))
	assert.False(t, source.Matches(playlistschedule.Source{DynamicPlaylist: &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID:           "other",
			DynamicQuery: query,
		},
	}}))
}
