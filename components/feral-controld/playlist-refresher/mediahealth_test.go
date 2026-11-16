package refresher_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	refresher "github.com/feral-file/ffos-user/components/feral-controld/playlist-refresher"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Every refresher send carries the media-health hint argument for the items
// it pushes (feral-file/ffos-user#392): the soft refresh here, and the force
// casts through the same attach helper.
func TestRefresher_SoftRefreshCarriesMediaHealthHints(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockCDP := mocks.NewMockCDP(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	stubPassthroughCastPlan(mockDP1)
	mockClock := mocks.NewMockClock(ctrl)
	fakeSched := &fakePlaylistScheduler{hasCache: false}

	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	setupBackgroundMocks(&testSetup{ctrl: ctrl, mockClock: mockClock})

	playlistURL := "https://example.com/daily.json"
	pl := createMockPlaylist()
	mockStatusPoller.EXPECT().
		FetchPlayerStatus(ctx).
		Return(createMockPlayerStatus(string(commands.CMD_DISPLAY_PLAYLIST), &playlistURL, nil), nil).
		AnyTimes()
	mockDP1.EXPECT().
		FetchPlaylist(ctx, playlistURL).
		Return(pl, json.RawMessage(nil), nil).
		AnyTimes()
	// createMockPlaylist carries legacy DynamicQueries, so
	// narrowedPlaylistFromURL's HasDynamicContent check routes through
	// ProcessDynamicPlaylist for real now (previously opaque to the mock,
	// since the whole ProcessPlaylistURL call — dynamic branch included —
	// was mocked wholesale). Echo pl back unchanged: this test only cares
	// about the items pushed, not dynamic resolution itself.
	mockDP1.EXPECT().
		ProcessDynamicPlaylist(ctx, *pl, false).
		Return(pl, nil).
		AnyTimes()

	var mu sync.Mutex
	var expressions []string
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
			expr, _ := params["expression"].(string)
			mu.Lock()
			expressions = append(expressions, expr)
			mu.Unlock()
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).
		AnyTimes()

	r := refresher.New(ctx, mockDP1, mockStatusPoller, mockCDP, nil, nil, wrapper.NewJSON(), fakeSched, mockClock,
		zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel)))
	var asked [][]dp1playlist.PlaylistItem
	refresher.SetMediaHealthHints(r, func(items []dp1playlist.PlaylistItem) interface{} {
		mu.Lock()
		asked = append(asked, items)
		mu.Unlock()
		return map[string]map[string]string{items[0].Source: {"state": "broken", "category": "source_missing"}}
	}, zap.NewNop())
	r.Start()
	time.Sleep(300 * time.Millisecond)
	r.Stop()

	mu.Lock()
	defer mu.Unlock()
	if assert.NotEmpty(t, expressions, "a refresh must have been sent") {
		for _, expr := range expressions {
			assert.True(t, strings.Contains(expr, `"mediaHealth":{"http://example.com/video1.mp4":{"category":"source_missing","state":"broken"}}`), expr)
			assert.Contains(t, expr, `"refresh":true`)
		}
	}
	if assert.NotEmpty(t, asked) {
		assert.Equal(t, "http://example.com/video1.mp4", asked[0][0].Source, "asked for the items being pushed")
	}
}
