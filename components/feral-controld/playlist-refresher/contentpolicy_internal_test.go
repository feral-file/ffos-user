package refresher

import (
	"testing"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

func TestCurrentBlockedByRefresh(t *testing.T) {
	general, mature := contentrating.RatingGeneral, contentrating.RatingMature
	index := 0
	old := []dp1playlist.PlaylistItem{{ID: "current", Source: "https://old"}}
	player := &status.PlayerStatus{Index: &index, Items: &old}
	policy := contentpolicy.Default()

	freshMature := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "current", Source: "https://new", ContentRating: &mature},
		{ID: "other", Source: "https://other", ContentRating: &general},
	}}}
	if !currentBlockedByRefresh(player, freshMature, policy, contentpolicy.ContextCurated) {
		t.Fatal("newly mature current item must retire even when an allowed replacement remains")
	}

	freshOtherMature := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "current", Source: "https://new", ContentRating: &general},
		{ID: "other", Source: "https://other", ContentRating: &mature},
	}}}
	if currentBlockedByRefresh(player, freshOtherMature, policy, contentpolicy.ContextCurated) {
		t.Fatal("blocking a non-current item must not remount the allowed current item")
	}
}
