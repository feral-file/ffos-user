package refresher

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// assertPlayerRetires states what ff-player requires before it acts on a
// retirement: displayPlaylist routes to refreshUnderPolicy, the only reader of
// retireBlockedCurrent, only when request.refresh is true
// (ff-player src/services/canvas/PlaylistCommands.ts, 3.0.0 through main).
func assertPlayerRetires(t *testing.T, expression string) {
	t.Helper()
	payload := strings.TrimSuffix(strings.TrimPrefix(expression, "window.handleCDPRequest("), ")")
	var wire struct {
		Command string `json:"command"`
		Request struct {
			Refresh              bool `json:"refresh"`
			RetireBlockedCurrent bool `json:"retireBlockedCurrent"`
		} `json:"request"`
	}
	if err := json.Unmarshal([]byte(payload), &wire); err != nil {
		t.Fatalf("not a player command: %v; sent=%s", err, expression)
	}
	if wire.Command != string(commands.CMD_DISPLAY_PLAYLIST) || !wire.Request.RetireBlockedCurrent {
		t.Fatalf("not a retirement; sent=%s", expression)
	}
	if !wire.Request.Refresh {
		t.Fatalf("the player ignores retireBlockedCurrent without refresh:true; sent=%s", expression)
	}
}

func TestRetirementWire_UnresolvableSource(t *testing.T) {
	mature := contentrating.RatingMature
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, sent := newUnresolvableSourceRefresher(t, &mature, store)
	if err := r.processPlayingPlaylist(false); err == nil {
		t.Fatal("an unresolvable source must still report the refresh failure")
	}
	assertPlayerRetires(t, *sent)
}

func TestRetirementWire_StaticInline(t *testing.T) {
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	if err != nil {
		t.Fatal(err)
	}
	r, sent := newInlineRefresher(t, "curated", store)
	if err := r.processPlayingPlaylist(false); err != nil {
		t.Fatal(err)
	}
	assertPlayerRetires(t, *sent)
}

// Both ordinary shapes of a source-backed retirement: the periodic soft refresh
// and the forced push the startup loop escalates to.
func TestRetirementWire_SourceRefresh(t *testing.T) {
	for name, forceCast := range map[string]bool{"periodic": false, "forced": true} {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			mockCDP := mocks.NewMockCDP(ctrl)
			mockPoller := mocks.NewMockStatusPoller(ctrl)
			mockDP1 := mocks.NewMockDP1(ctrl)
			stubPassthroughCastPlan(mockDP1)

			playlistURL := "https://example.test/list.json"
			mature := contentrating.RatingMature
			items := []dp1playlist.PlaylistItem{
				{ID: "grown", Source: "https://example.test/grown", ContentRating: &mature},
				{ID: "safe", Source: "https://example.test/safe"},
			}
			index := 0

			mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
			mockPoller.EXPECT().FetchPlayerStatus(gomock.Any()).Return(&status.PlayerStatus{
				Command:        string(commands.CMD_DISPLAY_PLAYLIST),
				PlaylistURL:    &playlistURL,
				ContentContext: "curated",
				Index:          &index,
				Items:          &items,
			}, nil).AnyTimes()
			mockDP1.EXPECT().FetchPlaylist(gomock.Any(), playlistURL).Return(
				&dp1.Playlist{Playlist: dp1playlist.Playlist{Items: items}}, nil, nil).AnyTimes()
			var sent string
			mockCDP.EXPECT().Send(gomock.Any(), gomock.Any()).DoAndReturn(
				func(_ string, params map[string]interface{}) (interface{}, error) {
					sent, _ = params["expression"].(string)
					return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
				}).Times(1)

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
			if err := r.processPlayingPlaylist(forceCast); err != nil && !errors.Is(err, errPlayerRejectedRefresh) {
				t.Fatal(err)
			}
			assertPlayerRetires(t, sent)
		})
	}
}
