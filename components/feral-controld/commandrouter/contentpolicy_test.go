package commandrouter_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/display-protocol/dp1-go/extension/contentrating"
	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/contentpolicy"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func newPolicyHandler(t *testing.T) (commandrouter.Handler, *mocks.MockCDP, *contentpolicy.Store) {
	t.Helper()
	ctrl := gomock.NewController(t)
	ex := newRoutableExecutor(ctrl)
	player := mocks.NewMockCDP(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	h := commandrouter.New(ex, player, mocks.NewMockDP1(ctrl), mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))
	return h, player, store
}

// newPolicyHandlerWithPoller is newPolicyHandler for the cases that reach the
// ordinary displayPlaylist branch, which force-refreshes the status poller.
func newPolicyHandlerWithPoller(t *testing.T) (commandrouter.Handler, *mocks.MockCDP, *mocks.MockStatusPoller) {
	t.Helper()
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	poller := mocks.NewMockStatusPoller(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl), poller, nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))
	return h, player, poller
}

func TestSetContentPolicyAwaitsMatchingPlayerAck(t *testing.T) {
	h, player, _ := newPolicyHandler(t)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(_ string, params map[string]interface{}) (interface{}, error) {
		require.Equal(t, true, params["awaitPromise"])
		require.Equal(t, true, params["returnByValue"])
		return map[string]interface{}{"message": map[string]interface{}{
			"ok": true, "active": true,
			"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": true, "strictPersonal": false, "blockUnratedCurated": false},
		}}, nil
	})
	result, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false}})
	require.NoError(t, err)
	require.Equal(t, true, result.(map[string]interface{})["active"])
}

func TestDisplayPlaylistAllBlockedDoesNotReachPlayer(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	_, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: map[string]any{
		"dp1_call": map[string]interface{}{"dpVersion": "1.1.0", "title": "x", "items": []interface{}{map[string]interface{}{"source": "https://a", "contentRating": "mature"}}},
	}})
	require.Error(t, err)
	require.True(t, commandrouter.IsContentBlocked(err))
}

func TestSetContentPolicyCannotWriteAuditGate(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	result, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{
		"showMatureContent": false, "strictPersonal": false, "blockUnratedCurated": true,
	}})
	require.NoError(t, err)
	require.Equal(t, "invalidRequest", result.(map[string]interface{})["error"])
}

func TestDisplayPlaylistRejectsMalformedPresentRating(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	_, err := h.Process(context.Background(), commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: map[string]any{
		"dp1_call": map[string]interface{}{"items": []interface{}{map[string]interface{}{"source": "https://a", "contentRating": nil}}},
	}})
	require.ErrorContains(t, err, "playlistInvalid")
}

// A replay must be re-admitted under the context the work was recorded under.
// Without this, a work that played as "personal" (mature allowed while
// strictPersonal is off) comes back as "curated" and the policy gate refuses
// it — History would list a work it can never put back.
func TestPlayRecentlyPlayedPreservesTheRecordedContentContext(t *testing.T) {
	h, player, poller := newPolicyHandlerWithPoller(t)
	poller.EXPECT().ForceRefresh().Times(1)

	var displayExpression string
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			require.Contains(t, params["expression"].(string), "resolveRecentlyPlayed")
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "status": "ok",
				"contentContext": "personal",
				"item": map[string]interface{}{
					"id": "retained", "source": "https://example.test/retained", "contentRating": "mature",
				},
			}}, nil
		}).Times(1)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			displayExpression = params["expression"].(string)
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_PLAY_RECENTLY_PLAYED, Arguments: map[string]any{"recordId": "rp-7"},
	})
	require.NoError(t, err)
	require.Equal(t, "rp-7", result.(map[string]interface{})["recordId"])
	// The mature item survived the policy filter, which only happens when the
	// recorded "personal" context reached the displayPlaylist branch.
	require.Contains(t, displayExpression, "https://example.test/retained")
	require.Contains(t, displayExpression, `"contentContext":"personal"`)
}

// An unrecognized stored context must fall back to the strict curated default
// rather than failing the replay or being forwarded as-is.
func TestPlayRecentlyPlayedIgnoresAnUnrecognizedRecordedContext(t *testing.T) {
	h, player, poller := newPolicyHandlerWithPoller(t)
	poller.EXPECT().ForceRefresh().Times(1)

	var displayExpression string
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "status": "ok",
				"contentContext": "not-a-context",
				"item":           map[string]interface{}{"id": "retained", "source": "https://example.test/retained"},
			}}, nil
		}).Times(1)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			displayExpression = params["expression"].(string)
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).Times(1)

	_, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_PLAY_RECENTLY_PLAYED, Arguments: map[string]any{"recordId": "rp-8"},
	})
	require.NoError(t, err)
	require.NotContains(t, displayExpression, "not-a-context")
}

// getContentPolicy takes no arguments, and a non-empty request is REJECTED
// rather than ignored: the storm gate dedupes on type+arguments, so ignoring
// junk arguments would hand a LAN caller unlimited distinct dedupe keys for a
// command that holds the policy lock across a CDP round trip.
func TestGetContentPolicyRejectsANonEmptyRequest(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	// No player.EXPECT(): the rejection must happen before any CDP send.
	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_GET_CONTENT_POLICY, Arguments: map[string]any{"nonce": 1},
	})
	require.NoError(t, err)
	require.Equal(t, "invalidRequest", result.(map[string]interface{})["error"])
}

// A store whose durable file could not be read keeps admitting content on safe
// defaults, but must never present those defaults as the owner's saved setting.
func TestGetContentPolicyIsUnavailableWhileTheStoreIsNotDurable(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl), mocks.NewMockStatusPoller(ctrl),
		nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, contentpolicy.Fallback(filepath.Join(t.TempDir(), "policy.json"), false), zaptest.NewLogger(t))

	// No player.EXPECT(): an unavailable store must not reach the player.
	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_GET_CONTENT_POLICY, Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.Equal(t, "contentPolicyUnavailable", result.(map[string]interface{})["error"])
}

// capturingScheduler records the projector the router installs. Embedding the
// interface satisfies the rest of it; every other method would panic if called,
// which is the point — this test exercises only the projector wiring.
type capturingScheduler struct {
	playlistschedule.Scheduler
	projector playlistschedule.Projector
}

func (c *capturingScheduler) SetProjector(p playlistschedule.Projector) { c.projector = p }

// The router must install the scheduler projector when the content policy is
// wired, and that projector must apply the CURRENT policy — a displayAt cutover
// replays a cohort of a document the router filtered once, at cast time, so a
// policy tightened afterwards reaches those cohorts only here.
func TestSetContentPolicyInstallsTheSchedulerProjector(t *testing.T) {
	ctrl := gomock.NewController(t)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	sched := &capturingScheduler{}
	h := commandrouter.New(newRoutableExecutor(ctrl), mocks.NewMockCDP(ctrl), mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, sched, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))
	require.NotNil(t, sched.projector, "scheduler pushes would escape the content policy")

	mature := contentrating.RatingMature
	cohort := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "ok", Source: "https://ok"},
		{ID: "mature", Source: "https://mature", ContentRating: &mature},
	}}}

	projected, empty := sched.projector(cohort, "curated")
	require.False(t, empty)
	require.Len(t, projected.Items, 1)
	require.Equal(t, "ok", projected.Items[0].ID)

	// Relaxed personal context admits the same cohort whole.
	projected, empty = sched.projector(cohort, "personal")
	require.False(t, empty)
	require.Len(t, projected.Items, 2)

	// A policy change is picked up without re-installing the projector.
	store.Lock()
	_, err = store.UpdateLocked(false, true)
	store.Unlock()
	require.NoError(t, err)
	projected, empty = sched.projector(cohort, "personal")
	require.False(t, empty)
	require.Len(t, projected.Items, 1, "a tightened policy must reach later cutovers")

	// An all-blocked cohort reports empty so the push is dropped rather than
	// sent as an empty displayPlaylist the player would reject.
	allMature := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "mature", Source: "https://mature", ContentRating: &mature},
	}}}
	_, empty = sched.projector(allMature, "curated")
	require.True(t, empty)
}
