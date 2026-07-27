package playlistschedule

import (
	"reflect"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

type Source struct {
	PlaylistURL     string        `json:"playlistUrl,omitempty"`
	DynamicPlaylist *dp1.Playlist `json:"dynamicPlaylist,omitempty"`
}

func (s Source) IsZero() bool {
	return s.PlaylistURL == "" && s.DynamicPlaylist == nil
}

func (s Source) Matches(other Source) bool {
	if s.PlaylistURL != "" || other.PlaylistURL != "" {
		return s.PlaylistURL != "" && s.PlaylistURL == other.PlaylistURL
	}
	if s.DynamicPlaylist != nil || other.DynamicPlaylist != nil {
		return dynamicPlaylistSourceMatches(s.DynamicPlaylist, other.DynamicPlaylist)
	}
	return false
}

func snapshotSource(source Source) Source {
	return Source{
		PlaylistURL:     source.PlaylistURL,
		DynamicPlaylist: cloneSourcePlaylist(source.DynamicPlaylist),
	}
}

func dynamicPlaylistSourceMatches(a, b *dp1.Playlist) bool {
	if a == nil || b == nil {
		return false
	}
	if a.ID != b.ID {
		return false
	}
	return reflect.DeepEqual(a.DynamicQuery, b.DynamicQuery) &&
		reflect.DeepEqual(a.DynamicQueries, b.DynamicQueries)
}

func cloneSourcePlaylist(p *dp1.Playlist) *dp1.Playlist {
	if p == nil {
		return nil
	}
	clone := clonePlaylist(p)
	clone.Items = nil
	return clone
}
