package refresher

import (
	"testing"

	"github.com/display-protocol/dp1-go/extension/playlists"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

func TestVisiblePlaylistChanged_Nil(t *testing.T) {
	p := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{{ID: "a", Source: "s"}}}}
	assert.True(t, visiblePlaylistChanged(nil, p))
	assert.True(t, visiblePlaylistChanged(p, nil))
}

func TestVisiblePlaylistChanged_PlaylistNote(t *testing.T) {
	a := &dp1.Playlist{Playlist: dp1playlist.Playlist{Note: &playlists.Note{Text: "x"}}}
	b := &dp1.Playlist{Playlist: dp1playlist.Playlist{Note: &playlists.Note{Text: "y"}}}
	assert.True(t, visiblePlaylistChanged(a, b))
}

func TestVisiblePlaylistChanged_ItemAddRemove(t *testing.T) {
	a := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "1", Source: "s"},
	}}}
	b := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "1", Source: "s"},
		{ID: "2", Source: "t"},
	}}}
	assert.True(t, visiblePlaylistChanged(a, b))
}

func TestVisiblePlaylistChanged_ItemNote(t *testing.T) {
	itemsA := []dp1playlist.PlaylistItem{{ID: "1", Source: "s", Note: &playlists.Note{Text: "a"}}}
	itemsB := []dp1playlist.PlaylistItem{{ID: "1", Source: "s", Note: &playlists.Note{Text: "b"}}}
	a := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: itemsA}}
	b := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: itemsB}}
	assert.True(t, visiblePlaylistChanged(a, b))
}

func TestVisiblePlaylistChanged_ItemReorder(t *testing.T) {
	a := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "1", Source: "s"},
		{ID: "2", Source: "t"},
	}}}
	b := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "2", Source: "t"},
		{ID: "1", Source: "s"},
	}}}
	assert.True(t, visiblePlaylistChanged(a, b))
}

func TestVisiblePlaylistChanged_SourceChange(t *testing.T) {
	a := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "1", Source: "http://old", Title: "t"},
	}}}
	b := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "1", Source: "http://new", Title: "t"},
	}}}
	assert.True(t, visiblePlaylistChanged(a, b))
}

func TestVisiblePlaylistChanged_DurationOnlyTweak(t *testing.T) {
	d1, d2 := 30.0, 60.0
	a := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "1", Source: "http://x", Duration: &d1},
	}}}
	b := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "1", Source: "http://x", Duration: &d2},
	}}}
	assert.False(t, visiblePlaylistChanged(a, b))
}

func TestVisiblePlaylistChanged_NoteDurationOnlyTweak(t *testing.T) {
	nd1, nd2 := 3.0, 9.0
	a := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Note:  &playlists.Note{Text: "hello", Duration: &nd1},
		Items: []dp1playlist.PlaylistItem{{ID: "1", Source: "s"}},
	}}
	b := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		Note:  &playlists.Note{Text: "hello", Duration: &nd2},
		Items: []dp1playlist.PlaylistItem{{ID: "1", Source: "s"}},
	}}
	assert.False(t, visiblePlaylistChanged(a, b))

	itemsA := []dp1playlist.PlaylistItem{{
		ID: "1", Source: "s",
		Note: &playlists.Note{Text: "hi", Duration: &nd1},
	}}
	itemsB := []dp1playlist.PlaylistItem{{
		ID: "1", Source: "s",
		Note: &playlists.Note{Text: "hi", Duration: &nd2},
	}}
	assert.False(t, visiblePlaylistChanged(
		&dp1.Playlist{Playlist: dp1playlist.Playlist{Items: itemsA}},
		&dp1.Playlist{Playlist: dp1playlist.Playlist{Items: itemsB}},
	))
}
