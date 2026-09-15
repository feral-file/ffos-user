package refresher

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
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

// newPolicyRefresher builds the smallest refresher that can run one
// processPlayingPlaylist pass against a player status.
func newPolicyRefresher(t *testing.T, statusContext string, store *contentpolicy.Store) (*refresher, *mocks.MockCDP, *string) {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)

	playlistURL := "https://example.test/feed.json"
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockPoller.EXPECT().FetchPlayerStatus(gomock.Any()).DoAndReturn(
		func(context.Context) (*status.PlayerStatus, error) {
			index := 0
			return &status.PlayerStatus{
				Command:        string(commands.CMD_DISPLAY_PLAYLIST),
				PlaylistURL:    &playlistURL,
				ContentContext: statusContext,
				Index:          &index,
			}, nil
		}).AnyTimes()

	mature := contentrating.RatingMature
	mockDP1.EXPECT().ProcessPlaylistURL(gomock.Any(), playlistURL, false).DoAndReturn(
		func(context.Context, string, bool) (*dp1.Playlist, error) {
			return &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
				{ID: "safe", Source: "https://example.test/safe"},
				{ID: "grown", Source: "https://example.test/grown", ContentRating: &mature},
			}}}, nil
		}).AnyTimes()

	var sent string
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			sent, _ = params["expression"].(string)
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).AnyTimes()

	r := &refresher{
		context:       context.Background(),
		cdp:           mockCDP,
		statusPoller:  mockPoller,
		dp1:           mockDP1,
		json:          wrapper.NewJSON(),
		contentPolicy: store,
		logger:        zaptest.NewLogger(t),
	}
	return r, mockCDP, &sent
}

// A player that predates this feature omits contentContext from its status, so
// the source the refresher rebuilds carries an UNKNOWN origin — not a curated
// one. Defaulting it to curated would silently reclassify a cast the owner made
// as personal and strip the mature items they deliberately put on the wall on
// the very next refresh, which is the opposite of the documented guarantee that
// context survives refresh.
func TestRefreshLeavesAnUnknownContextUnprojected(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, _, sent := newPolicyRefresher(t, "", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*sent, "https://example.test/grown") {
		t.Fatalf("a status without contentContext must not be reclassified as curated; sent=%s", *sent)
	}
}

// A current player always echoes the context the router sent, so a curated
// status still filters normally.
func TestRefreshProjectsAKnownCuratedContext(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, _, sent := newPolicyRefresher(t, "curated", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*sent, "https://example.test/grown") {
		t.Fatalf("a curated refresh must still withhold mature items; sent=%s", *sent)
	}
	if !strings.Contains(*sent, "https://example.test/safe") {
		t.Fatalf("the allowed item must still be cast; sent=%s", *sent)
	}
}
