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
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
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

// A display-at source persisted before contentContext existed restores with an
// empty value. It is non-zero, so player-status recovery is skipped and nothing
// else would notice — and defaulting it to curated strips the mature items an
// owner scheduled as personal, at the first refresh after an upgrade.
func TestRefreshLeavesALegacySchedulerSourceUnprojected(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, _, sent := newPolicyRefresher(t, "curated", store)
	// A scheduler source from before the field existed takes precedence over
	// player status, which is exactly why its emptiness has to be noticed here.
	r.scheduler = &legacySourceScheduler{url: "https://example.test/feed.json"}
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*sent, "https://example.test/grown") {
		t.Fatalf("a pre-feature scheduled source must not be reclassified as curated; sent=%s", *sent)
	}
}

// legacySourceScheduler owns a source with no contentContext, the shape a
// schedule persisted before this feature restores as.
type legacySourceScheduler struct {
	playlistschedule.Scheduler
	url string
}

func (s *legacySourceScheduler) Source() playlistschedule.Source {
	return playlistschedule.Source{PlaylistURL: s.url}
}
func (s *legacySourceScheduler) RestoredPending() bool                      { return false }
func (s *legacySourceScheduler) SourceMatches(playlistschedule.Source) bool { return true }
func (s *legacySourceScheduler) AuthorityToken() uint64                     { return 1 }
func (s *legacySourceScheduler) WithPlayerPush(fn func())                   { fn() }
func (s *legacySourceScheduler) SetProjector(playlistschedule.Projector)    {}
func (s *legacySourceScheduler) Snapshot() playlistschedule.Snapshot {
	return playlistschedule.Snapshot{}
}
func (s *legacySourceScheduler) Restore(playlistschedule.Snapshot)     {}
func (s *legacySourceScheduler) Commit()                               {}
func (s *legacySourceScheduler) HasCache() bool                        { return false }
func (s *legacySourceScheduler) Prepare(p *dp1.Playlist) *dp1.Playlist { return p }
func (s *legacySourceScheduler) PrepareWithSource(p *dp1.Playlist, _ playlistschedule.Source) *dp1.Playlist {
	return p
}

// newInlineRefresher builds a refresher whose player is displaying a STATIC
// inline playlist: the shape an app cast usually takes, and the one with no
// refreshable source to re-resolve.
func newInlineRefresher(t *testing.T, statusContext string, store *contentpolicy.Store) (*refresher, *string) {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockPoller := mocks.NewMockStatusPoller(ctrl)

	mature := contentrating.RatingMature
	inline := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "safe", Source: "https://example.test/safe"},
		{ID: "grown", Source: "https://example.test/grown", ContentRating: &mature},
	}}}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockPoller.EXPECT().FetchPlayerStatus(gomock.Any()).DoAndReturn(
		func(context.Context) (*status.PlayerStatus, error) {
			index := 1 // the mature item is the one on screen
			return &status.PlayerStatus{
				Command:        string(commands.CMD_DISPLAY_PLAYLIST),
				Playlist:       inline,
				ContentContext: statusContext,
				Index:          &index,
			}, nil
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
		dp1:           mocks.NewMockDP1(ctrl),
		json:          wrapper.NewJSON(),
		contentPolicy: store,
		logger:        zaptest.NewLogger(t),
	}
	return r, &sent
}

// An inline cast has no source to re-resolve, so this path used to send
// nothing. That left the one case the policy most needs to cover: an owner
// turning mature content OFF while a mature work is on screen, with no later
// refresh that would ever remove it.
func TestStaticInlineRefreshRetiresNewlyBlockedContent(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, sent := newInlineRefresher(t, "curated", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if *sent == "" {
		t.Fatal("a newly blocked inline item must be re-sent without it, not left on screen")
	}
	if strings.Contains(*sent, "https://example.test/grown") {
		t.Fatalf("the blocked item survived the projection; sent=%s", *sent)
	}
	if !strings.Contains(*sent, "https://example.test/safe") {
		t.Fatalf("the allowed item must remain; sent=%s", *sent)
	}
	// The blocked item is the one on screen, so the player must be told to
	// retire it rather than keep the frame it already committed.
	if !strings.Contains(*sent, "retireBlockedCurrent") {
		t.Fatalf("the current blocked frame must be retired; sent=%s", *sent)
	}
}

// Nothing newly blocked must stay as cheap as it was before this path projected
// at all: scope sync only, no CDP send.
func TestStaticInlineRefreshSendsNothingWhenPolicyAdmitsEverything(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	store.Lock()
	if _, err := store.UpdateLocked(true, false); err != nil { // mature allowed
		store.Unlock()
		t.Fatal(err)
	}
	store.Unlock()

	r, sent := newInlineRefresher(t, "curated", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if *sent != "" {
		t.Fatalf("an unchanged projection must not re-send; sent=%s", *sent)
	}
}

// A player that omits contentContext leaves the origin unknown, and guessing
// curated here would strip an owner's personal mature content off the wall.
func TestStaticInlineRefreshLeavesAnUnknownContextAlone(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, sent := newInlineRefresher(t, "", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if *sent != "" {
		t.Fatalf("an unknown context must not be reclassified as curated; sent=%s", *sent)
	}
}

// authoritySpyScheduler reports a token that has MOVED since the pass started,
// standing in for a newer cast completing while this refresh was projecting.
type authoritySpyScheduler struct {
	playlistschedule.Scheduler
	pushed bool
}

func (s *authoritySpyScheduler) Source() playlistschedule.Source { return playlistschedule.Source{} }
func (s *authoritySpyScheduler) RestoredPending() bool           { return false }
func (s *authoritySpyScheduler) AuthorityToken() uint64 {
	if s.pushed {
		return 2 // moved by the time the send closure runs
	}
	return 1
}
func (s *authoritySpyScheduler) WithPlayerPush(fn func()) {
	s.pushed = true
	fn()
}
func (s *authoritySpyScheduler) SetProjector(playlistschedule.Projector) {}

// The inline policy re-send is built from a status read taken BEFORE the
// projection, so it must run under the scheduler push lock and re-check the
// authority token: a cast that completed in between would otherwise be
// overwritten by this stale playlist, restoring the previous artwork and its
// offline-replay scope.
func TestStaticInlineRefreshSkipsWhenPlaylistAuthorityMoved(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, sent := newInlineRefresher(t, "curated", store)
	r.scheduler = &authoritySpyScheduler{}
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if *sent != "" {
		t.Fatalf("a newer cast must not be overwritten by a stale policy re-send; sent=%s", *sent)
	}
}

// An item that DISAPPEARS from the source feed during the same refresh matches
// nothing in the refreshed set, so the projection is unchanged and the old
// membership-gated check never looked at it — while a soft refresh defers
// precisely when its current item is absent from the new list. The blocked
// frame therefore stayed on screen indefinitely after mature content was
// disabled. Retirement is now judged from the item the player actually has.
func TestRefreshRetiresABlockedCurrentItemThatLeftTheFeed(t *testing.T) {
	general, mature := contentrating.RatingGeneral, contentrating.RatingMature
	ctrl := gomock.NewController(t)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)

	playlistURL := "https://example.test/feed.json"
	index := 0
	// On screen: a mature item. It is NOT in the refreshed feed below.
	onScreen := []dp1playlist.PlaylistItem{{ID: "gone", Source: "https://example.test/gone", ContentRating: &mature}}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockPoller.EXPECT().FetchPlayerStatus(gomock.Any()).DoAndReturn(
		func(context.Context) (*status.PlayerStatus, error) {
			return &status.PlayerStatus{
				Command:        string(commands.CMD_DISPLAY_PLAYLIST),
				PlaylistURL:    &playlistURL,
				ContentContext: "curated",
				Index:          &index,
				Items:          &onScreen,
			}, nil
		}).AnyTimes()
	// The refreshed feed has replaced that item entirely: nothing is projected
	// away, so a membership-gated check sees no change at all.
	mockDP1.EXPECT().ProcessPlaylistURL(gomock.Any(), playlistURL, false).DoAndReturn(
		func(context.Context, string, bool) (*dp1.Playlist, error) {
			return &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
				{ID: "fresh", Source: "https://example.test/fresh", ContentRating: &general},
			}}}, nil
		}).AnyTimes()

	var sent string
	mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			sent, _ = params["expression"].(string)
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).AnyTimes()

	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r := &refresher{
		context:       context.Background(),
		cdp:           mockCDP,
		statusPoller:  mockPoller,
		dp1:           mockDP1,
		json:          wrapper.NewJSON(),
		contentPolicy: store,
		logger:        zaptest.NewLogger(t),
	}
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sent, "retireBlockedCurrent") {
		t.Fatalf("a blocked frame absent from the refreshed feed must still be retired; sent=%s", sent)
	}
}

// The mirror case: an allowed current item must not be retired just because
// this pass ran, or every refresh would restart playback.
func TestRefreshDoesNotRetireAnAllowedCurrentItem(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, _, sent := newPolicyRefresher(t, "personal", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(*sent, "retireBlockedCurrent") {
		t.Fatalf("an allowed current item must not be retired; sent=%s", *sent)
	}
}

// PINS A KNOWN, OPEN GAP — see the legacy-context discussion on
// feral-file/ffos-user#349.
//
// controld leaves a pre-feature source unprojected, but the value it puts on
// the wire is still "curated" (NormalizeContext("") normalizes it), and a
// CURRENT player applies its own mirror to that. So the daemon's restraint does
// not reach the player, and a legacy personal schedule can have mature items
// withheld there.
//
// This asserts the player-facing context deliberately, so the gap is visible in
// code rather than only in a thread: whoever migrates legacy sources (recovering
// and persisting the original origin) will see this fail and must decide what
// the wire value becomes. Every deferral-based alternative was measured and
// rejected — deferring the send stops playlist refreshing on every device still
// running a pre-feature player.
func TestRefreshSendsCuratedForALegacySource_KnownGap(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	// Status omits contentContext, which is how a pre-feature origin reaches
	// the refresher.
	r, _, sent := newPolicyRefresher(t, "", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*sent, `"contentContext":"curated"`) {
		t.Fatalf("the player-facing context for a legacy source changed; if that was intentional, this gap may now be closed — sent=%s", *sent)
	}
	// And the daemon's own restraint still holds: it did not strip the item.
	if !strings.Contains(*sent, "https://example.test/grown") {
		t.Fatalf("controld must still leave an unknown-origin playlist unprojected; sent=%s", *sent)
	}
}
