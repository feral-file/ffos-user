package refresher_test

// The refresher's share of DP-1 signature verification
// (feral-file/ffos-user#307): it re-publishes the verdict of what it
// re-pushes, and publishes nothing for the cached-copy fallback, which
// carries no verdict.

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func signedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "sigverify", "testdata", "feed-dd875fbe.json"))
	require.NoError(t, err)
	return raw
}

// TestRefresher_RepublishesVerdictAfterRefresh: the resolver attaches the
// verdict at fetch time; after a successful re-push the refresher writes it
// to the shared slot so player_status keeps describing what is on screen.
func TestRefresher_RepublishesVerdictAfterRefresh(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupBackgroundMocks(ts)
	active := &sigverify.Active{}
	refresher.SetSignatureVerification(ts.refresher, active, zaptest.NewLogger(t))

	playlistURL := "http://example.com/playlist.json"
	verdict := sigverify.Verify(signedFixture(t))
	require.Equal(t, sigverify.StatusValid, verdict.Status)
	playlist := createMockPlaylistNoDynamic()
	playlist.ID = "pl-refreshed"
	playlist.Verification = &verdict

	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(playlist, nil).AnyTimes()
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOKResponse(), nil).AnyTimes()

	ts.refresher.Start()
	require.Eventually(t, func() bool {
		_, ok := active.Lookup("pl-refreshed", playlistURL)
		return ok
	}, 2*time.Second, 10*time.Millisecond)
	ts.refresher.Stop()

	st, _ := active.Lookup("", playlistURL)
	assert.Equal(t, sigverify.StatusValid, st)
}

// TestRefresher_CachedFallback_ClearsSlot mirrors commandrouter's rule: the
// cached copy carries no verdict, so a re-push from it CLEARS the slot an
// earlier live fetch of the same URL had set — player_status must not keep
// vouching for bytes nobody verified. The cached body is the signed fixture
// itself, so a loader that judged it would flip this test.
func TestRefresher_CachedFallback_ClearsSlot(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	setupBackgroundMocks(ts)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	mockOfflineCache := mocks.NewMockOfflineCacheService(ts.ctrl)
	r := refresher.New(ts.ctx, ts.mockDP1, ts.mockStatusPoller, ts.mockCDP, nil, mockOfflineCache, wrapper.NewJSON(), nil, ts.mockClock, logger)
	active := &sigverify.Active{}
	refresher.SetSignatureVerification(r, active, logger)

	playlistURL := "http://example.com/playlist.json"
	active.Set("live-1", playlistURL, sigverify.StatusValid) // an earlier live pass of this URL
	cachedRaw := signedFixture(t)
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(ts.ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	ts.mockDP1.EXPECT().ProcessPlaylistURL(ts.ctx, playlistURL, false).Return(nil, errors.New("offline")).AnyTimes()
	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).Return(json.RawMessage(cachedRaw), nil).AnyTimes()
	sent := make(chan struct{}, 1)
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(string, map[string]any) (any, error) {
		select {
		case sent <- struct{}{}:
		default:
		}
		return playerOKResponse(), nil
	}).AnyTimes()

	r.Start()
	select {
	case <-sent:
	case <-time.After(2 * time.Second):
		t.Fatal("refresher never re-pushed the cached copy")
	}
	r.Stop()

	_, found := active.Lookup("live-1", playlistURL)
	assert.False(t, found, "the earlier verdict must not survive an unverified re-push of the same URL")
}

// TestRefresher_SetSignatureVerification_ForeignImplementationIsLeftAlone
// pins the setter's contract: a non-concrete Refresher logs and is untouched
// rather than panicking.
func TestRefresher_SetSignatureVerification_ForeignImplementationIsLeftAlone(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	foreign := mocks.NewMockRefresher(ctrl)

	assert.NotPanics(t, func() {
		refresher.SetSignatureVerification(foreign, &sigverify.Active{}, zap.NewNop())
	})
}
