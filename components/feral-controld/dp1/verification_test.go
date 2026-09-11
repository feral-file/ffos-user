package dp1_test

// Signature verdict attachment at fetch time (feral-file/ffos-user#307): the
// raw body is verified before the typed decode's field loss and before
// dynamic hydration can rewrite items, and the verdict rides the returned
// playlist through hydration.

import (
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

func signedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "sigverify", "testdata", "feed-dd875fbe.json"))
	require.NoError(t, err)
	return raw
}

// expectFetch wires the HTTP GET + ReadAll + Unmarshal trio fetchPlaylist
// drives, handing back body verbatim and letting decode populate the typed
// playlist through decodeInto.
func expectFetch(ts *testSetup, url string, body []byte, decodeInto func(*dp1.Playlist)) {
	ts.mockHTTP.EXPECT().Get(url).Return(createMockResponse(http.StatusOK, string(body)), nil)
	ts.mockIO.EXPECT().ReadAll(gomock.Any()).Return(body, nil)
	ts.mockJSON.EXPECT().
		Unmarshal(body, gomock.Any()).
		DoAndReturn(func(_ []byte, v any) error {
			decodeInto(v.(*dp1.Playlist))
			return nil
		})
}

func TestDP1_ProcessPlaylistURL_AttachesVerdict(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "https://feed.example/p.json"
	expectFetch(ts, url, signedFixture(t), func(p *dp1.Playlist) { p.ID = "pl-1" })

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	require.NotNil(t, result.Verification)
	assert.Equal(t, sigverify.StatusValid, result.Verification.Status)
	assert.Equal(t, "feed", result.Verification.Signers[0].Role)
}

func TestDP1_ProcessPlaylistURLForCast_AttachesVerdict(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "https://feed.example/p.json"
	expectFetch(ts, url, []byte(`{"dpVersion":"1.1.0","title":"t","items":[]}`), func(p *dp1.Playlist) { p.ID = "pl-1" })

	result, err := ts.client.ProcessPlaylistURLForCast(ts.ctx, url)

	require.NoError(t, err)
	require.NotNil(t, result.Verification)
	assert.Equal(t, sigverify.StatusUnsigned, result.Verification.Status)
}

// TestDP1_ProcessPlaylistURL_VerdictSurvivesDynamicHydration pins the
// by-value struct copy hydration relies on: the verdict computed on the
// fetched bytes is still on the playlist after its items were replaced by
// resolver output.
func TestDP1_ProcessPlaylistURL_VerdictSurvivesDynamicHydration(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	url := "https://feed.example/dynamic.json"
	body := []byte(createDynamicPlaylistJSON())
	expectFetch(ts, url, body, func(p *dp1.Playlist) {
		p.DynamicQueries = []dp1.LegacyDynamicQuery{{
			Endpoint: "https://indexer-v2.feralfile.com/graphql",
			Params:   map[string]string{"limit": "50", "offset": "0"},
		}}
	})
	ts.mockFFIndexer.EXPECT().
		QueryTokens(ts.ctx, "https://indexer-v2.feralfile.com/graphql",
			map[string]string{"limit": strconv.Itoa(dp1.MAX_PLAYLIST_ITEMS_LIMIT), "offset": "0"}).
		Return(createMockTokens(), nil)

	result, err := ts.client.ProcessPlaylistURL(ts.ctx, url, false)

	require.NoError(t, err)
	assert.Len(t, result.Items, 2, "hydration replaced the items")
	require.NotNil(t, result.Verification, "verdict must survive hydration")
	assert.Equal(t, sigverify.StatusUnsigned, result.Verification.Status)
}

// TestDP1_ProcessPlaylistURL_VerificationDisabled_NoVerdict: the config kill
// switch leaves the playlist verdict-less, which every reporter treats as
// "not verified" and omits.
func TestDP1_ProcessPlaylistURL_VerificationDisabled_NoVerdict(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	client := dp1.New(mocks.NewMockFFIndexer(ctrl), mockHTTP, mockJSON, mockIO, logger, false, false)

	url := "https://feed.example/p.json"
	body := signedFixture(t)
	mockHTTP.EXPECT().Get(url).Return(createMockResponse(http.StatusOK, string(body)), nil)
	mockIO.EXPECT().ReadAll(gomock.Any()).Return(body, nil)
	mockJSON.EXPECT().Unmarshal(body, gomock.Any()).Return(nil)

	result, err := client.ProcessPlaylistURL(t.Context(), url, false)

	require.NoError(t, err)
	assert.Nil(t, result.Verification)
}
