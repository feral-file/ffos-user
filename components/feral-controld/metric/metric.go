package metric

import (
	"context"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// EventProperties contains the properties to track with each event
type EventProperties struct {
	EnvApp           string `json:"env_app"`            // "ff1"
	EnvAppVersion    string `json:"env_app_version"`    // "0.8.1"
	EnvPlatform      string `json:"env_platform"`       // "ff1"
	EnvOS            string `json:"env_os"`             // "ffos"
	EnvOSVersion     string `json:"env_os_version"`     // "1.0.0"
	EnvBuildType     string `json:"env_build_type"`     // "prod"
	PlaylistScope    string `json:"playlist_scope"`     // "feed"
	PlaylistKey      string `json:"playlist_key"`       // "ff-pl-1234"
	PlaylistName     string `json:"playlist_name"`      // "FF1 Playlist"
	PlaylistURL      string `json:"playlist_url"`       // "https://feed.feralfile.com/api/v1/playlists/ff-pl-1234"
	PlaylistFeedHost string `json:"playlist_feed_host"` // "feed.feralfile.com"
}

//go:generate mockgen -source=metric.go -destination=../mocks/metric.go -package=mocks -mock_names=Tracker=MockMetricTracker
type Tracker interface {
	// Initialize initializes the metric tracker
	Initialize() error

	// TrackPlaylistView tracks when a playlist is viewed/displayed
	// playlistURL is the optional source URL of the playlist
	TrackPlaylistView(ctx context.Context, playlist *dp1.Playlist, playlistURL string) error
}
