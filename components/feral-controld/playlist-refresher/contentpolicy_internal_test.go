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

// A refreshed feed can re-mint item IDs while the artwork behind a source is
// unchanged. Matching on ID alone then finds nothing, and a work that just
// gained a "mature" label is left on the wall instead of being retired.
func TestCurrentBlockedByRefreshFallsBackToTheSourceWhenIDsChange(t *testing.T) {
	general, mature := contentrating.RatingGeneral, contentrating.RatingMature
	index := 0
	old := []dp1playlist.PlaylistItem{{ID: "old-id", Source: "https://stable"}}
	player := &status.PlayerStatus{Index: &index, Items: &old}
	policy := contentpolicy.Default()

	reminted := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "new-id", Source: "https://stable", ContentRating: &mature},
		{ID: "other", Source: "https://other", ContentRating: &general},
	}}}
	if !currentBlockedByRefresh(player, reminted, policy, contentpolicy.ContextCurated) {
		t.Fatal("a re-minted ID must not hide a newly mature current work")
	}

	stillAllowed := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "new-id", Source: "https://stable", ContentRating: &general},
	}}}
	if currentBlockedByRefresh(player, stillAllowed, policy, contentpolicy.ContextCurated) {
		t.Fatal("source fallback must not retire a work the policy still allows")
	}

	// Genuinely gone from the fresh set: the refresh replaces the frame itself,
	// so there is nothing for this predicate to retire.
	absent := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "other", Source: "https://other", ContentRating: &general},
	}}}
	if currentBlockedByRefresh(player, absent, policy, contentpolicy.ContextCurated) {
		t.Fatal("an absent current item is not a policy retirement")
	}
}
