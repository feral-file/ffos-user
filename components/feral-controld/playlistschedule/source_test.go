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

func TestSourceMatchesIncludesContentContext(t *testing.T) {
	source := playlistschedule.Source{PlaylistURL: "https://example.com/feed.json", ContentContext: "personal"}
	assert.True(t, source.Matches(playlistschedule.Source{PlaylistURL: source.PlaylistURL, ContentContext: "personal"}))
	assert.False(t, source.Matches(playlistschedule.Source{PlaylistURL: source.PlaylistURL, ContentContext: "curated"}))
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

func TestSourceMatchesDynamicPlaylistIncludesPagination(t *testing.T) {
	query := &playlists.DynamicQuery{
		Profile:  "graphql-v1",
		Endpoint: "https://api.raster.art/graphql",
		Query:    `query WalletWalk($after: String) { address(address: "0xabc") { tokensOwned(first: 250, after: $after) { nodes { token { id } } } } }`,
		ResponseMapping: playlists.ResponseMapping{
			ItemsPath:  "data.address.tokensOwned.nodes",
			ItemSchema: "raster/tokensOwned/1.0",
		},
	}
	pagination := &dp1.DynamicQueryPagination{
		Variable:    "after",
		CursorPath:  "data.address.tokensOwned.pageInfo.endCursor",
		HasNextPath: "data.address.tokensOwned.pageInfo.hasNextPage",
	}
	walk := func(p *dp1.DynamicQueryPagination) playlistschedule.Source {
		return playlistschedule.Source{DynamicPlaylist: &dp1.Playlist{
			Playlist:               dp1playlist.Playlist{ID: "wallet", DynamicQuery: query},
			DynamicQueryPagination: p,
		}}
	}

	assert.True(t, walk(pagination).Matches(walk(&dp1.DynamicQueryPagination{
		Variable: "after", CursorPath: pagination.CursorPath, HasNextPath: pagination.HasNextPath,
	})))
	assert.False(t, walk(pagination).Matches(walk(nil)), "a walk with and without pagination are different sources")
	assert.False(t, walk(pagination).Matches(walk(&dp1.DynamicQueryPagination{Variable: "cursor"})))
}
