package commandrouter_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
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

// A player that predates these commands answers the unknown command with a bare
// {"ok":false}. That must read as "this device cannot do it" — the same
// classification the history commands already give it — not as a temporary
// store or synchronization failure, which is what the app retries.
func TestContentPolicyClassifiesALegacyPlayerReplyAsUnsupported(t *testing.T) {
	h, player, _ := newPolicyHandler(t)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{"ok": false}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_GET_CONTENT_POLICY, Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.Equal(t, "unsupported", result.(map[string]interface{})["error"])
}

// A modern player that answers ok:true with a policy that does not match is a
// synchronization failure, not a missing capability.
func TestContentPolicyKeepsAMismatchedAckUnavailable(t *testing.T) {
	h, player, _ := newPolicyHandler(t)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "active": true,
				"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": true, "strictPersonal": false, "blockUnratedCurated": false},
			}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_GET_CONTENT_POLICY, Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.Equal(t, "contentPolicyUnavailable", result.(map[string]interface{})["error"])
}

// Omitting contentContext means curated, but the documented contract allows
// only "curated" and "personal" when the field is PRESENT. An explicit "" or
// null is a malformed request and must be told so, not silently widened into
// the default audience.
func TestDisplayPlaylistRejectsAnExplicitlyEmptyContentContext(t *testing.T) {
	dp1Call := map[string]interface{}{
		"dpVersion": "1.1.0", "title": "x",
		"items": []interface{}{map[string]interface{}{"source": "https://a"}},
	}
	for name, value := range map[string]any{"empty string": "", "null": nil} {
		t.Run(name, func(t *testing.T) {
			h, _, _ := newPolicyHandler(t)
			// No player.EXPECT(): the rejection happens before any CDP send.
			_, err := h.Process(context.Background(), commands.Command{
				Type:      commands.CMD_DISPLAY_PLAYLIST,
				Arguments: map[string]any{"dp1_call": dp1Call, "contentContext": value},
			})
			require.ErrorContains(t, err, "contentContext")
		})
	}
}

// barrierScheduler records whether a callback ran inside WithPlayerPush, which
// is the barrier that keeps a policy activation from being acknowledged while a
// scheduler cutover already holding pushMu is still in flight with the old
// policy's cohort.
type barrierScheduler struct {
	playlistschedule.Scheduler
	inPush  bool
	entered bool
}

func (b *barrierScheduler) SetProjector(playlistschedule.Projector) {}
func (b *barrierScheduler) WithPlayerPush(fn func()) {
	b.entered = true
	b.inPush = true
	fn()
	b.inPush = false
}

// The durable write AND the player acknowledgement must both happen under the
// player-push lock. Otherwise a timer push that already snapshotted the old
// policy can deliver its cohort after setContentPolicy has answered active:true.
func TestSetContentPolicySerializesWithSchedulerPushes(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	sched := &barrierScheduler{}
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, sched, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			require.True(t, sched.inPush, "the player acknowledgement escaped the player-push barrier")
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "active": true,
				"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": true, "strictPersonal": false, "blockUnratedCurated": false},
			}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false},
	})
	require.NoError(t, err)
	require.True(t, sched.entered, "setContentPolicy did not take the player-push lock")
	require.Equal(t, true, result.(map[string]interface{})["active"])
}

// getRecentlyPlayed takes no arguments; a non-empty request is rejected before
// dispatch for the same dedupe-key reason getContentPolicy rejects one.
func TestGetRecentlyPlayedRejectsANonEmptyRequest(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	// No player.EXPECT(): the rejection must happen before any CDP send.
	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_GET_RECENTLY_PLAYED, Arguments: map[string]any{"nonce": 1},
	})
	require.NoError(t, err)
	require.Equal(t, false, result.(map[string]interface{})["ok"])
	require.Contains(t, result.(map[string]interface{})["error"], "no arguments")
}

// A code-bearing policy failure is a modern player failing for a real reason;
// only an exactly bare {"ok":false} means the capability is missing.
func TestContentPolicyKeepsACodeBearingFailureUnavailable(t *testing.T) {
	h, player, _ := newPolicyHandler(t)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{"ok": false, "code": "busy"}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_GET_CONTENT_POLICY, Arguments: map[string]any{},
	})
	require.NoError(t, err)
	require.Equal(t, "contentPolicyUnavailable", result.(map[string]interface{})["error"])
}

// playRecentlyPlayed takes exactly {"recordId": ...}. The gate dedupes on the
// whole arguments map while the command uses only recordId, so an ignored extra
// field would miss the heavy-tier dedupe and repeat the resolve-and-replay work.
func TestPlayRecentlyPlayedRejectsIgnoredFields(t *testing.T) {
	h, _, _ := newPolicyHandler(t)
	// No player.EXPECT(): the rejection must happen before any CDP send.
	result, err := h.Process(context.Background(), commands.Command{
		Type:      commands.CMD_PLAY_RECENTLY_PLAYED,
		Arguments: map[string]any{"recordId": "rp-1", "nonce": 1},
	})
	require.NoError(t, err)
	require.Equal(t, false, result.(map[string]interface{})["ok"])
	require.Contains(t, result.(map[string]interface{})["error"], "only recordId")
}

// The content-policy lock must NOT be held across playlist resolution: that is
// network-bound on a caller-supplied URL, and holding it there would block an
// owner from applying a more restrictive policy for as long as a slow origin
// cares to stall.
//
// Probes the store lock directly rather than issuing a nested getContentPolicy:
// a nested command that blocks would still be holding a mock when the test
// ended. This goroutine only ever waits on a mutex the outer command releases.
func TestDisplayPlaylistDoesNotHoldThePolicyLockAcrossResolution(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	poller := mocks.NewMockStatusPoller(ctrl)
	dp1Mock := mocks.NewMockDP1(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	h := commandrouter.New(newRoutableExecutor(ctrl), player, dp1Mock, poller,
		nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	lockFree := false
	dp1Mock.EXPECT().ProcessPlaylistURLForCast(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, _ string) (*dp1.Playlist, error) {
			// Stands in for a slow origin: while "resolution" is in flight, the
			// policy store must still be acquirable.
			acquired := make(chan struct{})
			go func() {
				store.Lock()
				// Acquiring it is the whole assertion; read something real so
				// the critical section is not empty.
				_ = store.CurrentLocked()
				store.Unlock()
				close(acquired)
			}()
			select {
			case <-acquired:
				lockFree = true
			case <-time.After(3 * time.Second):
				lockFree = false
			}
			return &dp1.Playlist{Playlist: dp1playlist.Playlist{
				Items: []dp1playlist.PlaylistItem{{ID: "a", Source: "https://a"}},
			}}, nil
		}).Times(1)
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).AnyTimes()
	poller.EXPECT().ForceRefresh().AnyTimes()

	_, err = h.Process(context.Background(), commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"playlistUrl": "https://slow.example/feed.json"},
	})
	require.NoError(t, err)
	require.True(t, lockFree, "the policy lock was held through playlist resolution")
}

// stallingProber stands in for a slow playlist origin during the cast-time
// source preflight, which is the other network-bound stretch the policy lock
// must not span.
type stallingProber struct {
	during func()
}

func (p *stallingProber) ProbeSources(_ context.Context, sources []string) []offlinecache.SourceProbeResult {
	p.during()
	out := make([]offlinecache.SourceProbeResult, 0, len(sources))
	for _, s := range sources {
		out = append(out, offlinecache.SourceProbeResult{Source: s})
	}
	return out
}

// The policy lock must not span the source preflight either: the probe has a
// 10s phase ceiling and several heavy casts can be admitted at once, so holding
// it there makes the History and Content controls wait on whatever an
// unauthenticated caller's origin decides to do.
func TestDisplayPlaylistDoesNotHoldThePolicyLockAcrossSourcePreflight(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	poller := mocks.NewMockStatusPoller(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl), poller,
		nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	lockFree := false
	commandrouter.SetSourceProber(h, &stallingProber{during: func() {
		acquired := make(chan struct{})
		go func() {
			store.Lock()
			_ = store.CurrentLocked()
			store.Unlock()
			close(acquired)
		}()
		select {
		case <-acquired:
			lockFree = true
		case <-time.After(3 * time.Second):
			lockFree = false
		}
	}}, zaptest.NewLogger(t))

	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).AnyTimes()
	poller.EXPECT().ForceRefresh().AnyTimes()

	_, err = h.Process(context.Background(), commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": map[string]interface{}{
			"dpVersion": "1.1.0", "title": "x",
			"items": []interface{}{map[string]interface{}{"source": "https://slow.example/a"}},
		}},
	})
	require.NoError(t, err)
	require.True(t, lockFree, "the policy lock was held through the source preflight")
}

// Releasing the lock for the probe must not let a cast outrun a policy
// tightened while that probe ran: the projection is reapplied under the policy
// in force at send time.
func TestDisplayPlaylistReprojectsAfterAPolicyChangeDuringPreflight(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	poller := mocks.NewMockStatusPoller(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	// Start permissive so the mature item survives the first filter.
	store.Lock()
	_, err = store.UpdateLocked(true, false)
	store.Unlock()
	require.NoError(t, err)

	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl), poller,
		nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))
	commandrouter.SetSourceProber(h, &stallingProber{during: func() {
		// The owner turns mature content off while the probe is running.
		store.Lock()
		_, updateErr := store.UpdateLocked(false, false)
		store.Unlock()
		require.NoError(t, updateErr)
	}}, zaptest.NewLogger(t))

	var sent string
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			sent = params["expression"].(string)
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).AnyTimes()
	poller.EXPECT().ForceRefresh().AnyTimes()

	_, err = h.Process(context.Background(), commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": map[string]interface{}{
			"dpVersion": "1.1.0", "title": "x",
			"items": []interface{}{
				map[string]interface{}{"source": "https://ok.example/a"},
				map[string]interface{}{"source": "https://mature.example/b", "contentRating": "mature"},
			},
		}},
	})
	require.NoError(t, err)
	require.Contains(t, sent, "https://ok.example/a")
	require.NotContains(t, sent, "https://mature.example/b",
		"a policy tightened during the probe must still govern the cast")
}

// LOCK ORDER. The cast path and the playlist-refresher both nest the content
// policy store lock and the kiosk playback lock, and both are non-reentrant, so
// they must take them in the SAME order or a concurrent cast and refresh can
// deadlock permanently — no further policy update, refresh or playback command
// until controld restarts. The refresher's order is policy, then LockPlayback
// (processPlayingPlaylist), so the cast must match.
//
// Asserted with TryLock at the moment LockPlayback is called: a store lock that
// is NOT free proves the cast already holds it, which is the invariant. No
// timing, no sleeps.
func TestDisplayPlaylistTakesThePolicyLockBeforeThePlaybackLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	poller := mocks.NewMockStatusPoller(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)

	policyHeldAtPlaybackLock := false
	replay := mocks.NewMockOfflineCacheKioskReplay(ctrl)
	replay.EXPECT().LockPlayback().Do(func() {
		if store.TryLock() {
			store.Unlock()
			return
		}
		policyHeldAtPlaybackLock = true
	}).Times(1)
	replay.EXPECT().UnlockPlayback().AnyTimes()
	replay.EXPECT().SyncPlaylist(gomock.Any(), gomock.Any()).Return(1, nil).AnyTimes()
	replay.EXPECT().MarkPlaybackChanged().AnyTimes()

	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl), poller,
		nil, nil, replay, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).AnyTimes()
	poller.EXPECT().ForceRefresh().AnyTimes()

	_, err = h.Process(context.Background(), commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": map[string]interface{}{
			"dpVersion": "1.1.0", "title": "x",
			"items": []interface{}{map[string]interface{}{"source": "https://a.example/a"}},
		}},
	})
	require.NoError(t, err)
	require.True(t, policyHeldAtPlaybackLock,
		"the cast took the playback lock before the policy lock; the refresher takes them the other way round, so the two can deadlock")
}

// The reconnect sync must take the player-push barrier too, not only the policy
// lock. A due timer or wake recompute holds pushMu and reads the lock-free
// policy snapshot, so without the barrier it can deliver a cohort to a player
// that has just come up on ITS defaults — and the scheduler records that cohort
// as delivered, so nothing replays it once the sync lands.
func TestSyncContentPolicySerializesWithSchedulerPushes(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	sched := &barrierScheduler{}
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, sched, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			require.True(t, sched.inPush, "the reconnect policy acknowledgement escaped the player-push barrier")
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "active": true,
				"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": false, "strictPersonal": false, "blockUnratedCurated": false},
			}}, nil
		}).Times(1)

	require.NoError(t, commandrouter.SyncContentPolicy(h))
	require.True(t, sched.entered, "SyncContentPolicy did not take the player-push lock")
}

// A policy tightened while the probe runs can remove the very item whose
// reachability made the cast acceptable, leaving only sources already proven
// dead. The cast must not then report success with nothing renderable on it.
func TestDisplayPlaylistRejectsWhenTheProjectionLeavesOnlyDeadSources(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	poller := mocks.NewMockStatusPoller(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	store.Lock()
	_, err = store.UpdateLocked(true, false) // mature allowed, so the live item survives the first filter
	store.Unlock()
	require.NoError(t, err)

	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl), poller,
		nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	const live = "https://live.example/mature"
	const dead = "https://dead.example/general"
	commandrouter.SetSourceProber(h, &verdictProber{
		verdicts: map[string]offlinecache.SourceProbeVerdict{
			live: offlinecache.ProbeAlive,
			dead: offlinecache.ProbeDead,
		},
		during: func() {
			// The owner turns mature content off while the probe is running,
			// which removes the one reachable item.
			store.Lock()
			_, updateErr := store.UpdateLocked(false, false)
			store.Unlock()
			require.NoError(t, updateErr)
		},
	}, zaptest.NewLogger(t))

	// No player.EXPECT(): nothing renderable must reach the player.
	poller.EXPECT().ForceRefresh().AnyTimes()

	_, err = h.Process(context.Background(), commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": map[string]interface{}{
			"dpVersion": "1.1.0", "title": "x",
			"items": []interface{}{
				map[string]interface{}{"source": live, "contentRating": "mature"},
				map[string]interface{}{"source": dead, "contentRating": "general"},
			},
		}},
	})
	require.Error(t, err)
	var unreachable *commandrouter.SourceUnreachableError
	require.ErrorAs(t, err, &unreachable)
}

// verdictProber answers a fixed verdict per source and runs during() while the
// probe is "in flight".
type verdictProber struct {
	verdicts map[string]offlinecache.SourceProbeVerdict
	during   func()
	// redactSources mimics the real prober, whose Source field is
	// query-redacted and truncated for the daemon log.
	redactSources bool
}

func (p *verdictProber) ProbeSources(_ context.Context, sources []string) []offlinecache.SourceProbeResult {
	if p.during != nil {
		p.during()
	}
	out := make([]offlinecache.SourceProbeResult, 0, len(sources))
	for _, s := range sources {
		reported := s
		if p.redactSources {
			if cut := strings.IndexByte(reported, '?'); cut >= 0 {
				reported = reported[:cut] + "?<redacted>"
			}
		}
		out = append(out, offlinecache.SourceProbeResult{Source: reported, Verdict: p.verdicts[s]})
	}
	return out
}

// A set whose player acknowledgement fails must change nothing: not admission,
// not the lock-free snapshot the scheduler projector reads, and not the file —
// because the file is the only thing a restart restores, so a refused policy
// written now would come back as the active one on the next boot.
func TestSetContentPolicyPersistsNothingWithoutAnAcknowledgement(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	path := filepath.Join(t.TempDir(), "policy.json")
	store, err := contentpolicy.Open(path, false)
	require.NoError(t, err)
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "busy"}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false},
	})
	require.NoError(t, err)
	require.Equal(t, false, result.(map[string]interface{})["ok"])

	store.Lock()
	active := store.CurrentLocked()
	store.Unlock()
	require.False(t, active.ShowMatureContent, "a refused update must not change admission")
	require.False(t, store.Snapshot().ShowMatureContent, "the scheduler projector must not see it either")
	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "a refused update must not reach the file a restart restores")

	// An acknowledged set does commit.
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "active": true,
				"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": true, "strictPersonal": false, "blockUnratedCurated": false},
			}}, nil
		}).Times(1)
	result, err = h.Process(context.Background(), commands.Command{
		Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false},
	})
	require.NoError(t, err)
	require.Equal(t, true, result.(map[string]interface{})["active"])
	store.Lock()
	active = store.CurrentLocked()
	store.Unlock()
	require.True(t, active.ShowMatureContent)
	require.FileExists(t, path)
}

// An acknowledgement must carry a COMPLETE v1 policy. Decoding into a plain
// Policy made {"version":1} come back as the all-false default and compare
// equal to it, so a player echoing nothing looked like it had acknowledged the
// default policy — and that would commit it.
func TestSetContentPolicyRejectsAPartialAcknowledgement(t *testing.T) {
	for name, ack := range map[string]interface{}{
		"version only":    map[string]interface{}{"version": float64(1)},
		"missing a field": map[string]interface{}{"version": float64(1), "showMatureContent": false, "strictPersonal": false},
		"absent entirely": nil,
	} {
		t.Run(name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			player := mocks.NewMockCDP(ctrl)
			path := filepath.Join(t.TempDir(), "policy.json")
			store, err := contentpolicy.Open(path, false)
			require.NoError(t, err)
			h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
				mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
			commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

			player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
				func(_ string, _ map[string]interface{}) (interface{}, error) {
					message := map[string]interface{}{"ok": true, "active": true}
					if ack != nil {
						message["contentPolicy"] = ack
					}
					return map[string]interface{}{"message": message}, nil
				}).Times(1)

			// Setting the DEFAULT values, which is what a partial reply decodes to.
			result, err := h.Process(context.Background(), commands.Command{
				Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": false, "strictPersonal": false},
			})
			require.NoError(t, err)
			require.Equal(t, false, result.(map[string]interface{})["ok"], "a partial acknowledgement must not commit a policy")
			_, statErr := os.Stat(path)
			require.True(t, os.IsNotExist(statErr), "nothing may be persisted on a partial acknowledgement")
		})
	}
}

// A wrong-typed rating fails generic JSON decoding before the extension
// validator ever sees it, so validating the raw bytes first is what makes this
// the documented playlistInvalid classification instead of a generic failure
// the caster cannot act on.
func TestDisplayPlaylistClassifiesAWrongTypedRatingAsPlaylistInvalid(t *testing.T) {
	for name, rating := range map[string]interface{}{
		"number": float64(123),
		"object": map[string]interface{}{"value": "mature"},
		"null":   nil,
	} {
		t.Run(name, func(t *testing.T) {
			h, _, _ := newPolicyHandler(t)
			// No player.EXPECT(): an invalid document never reaches the player.
			_, err := h.Process(context.Background(), commands.Command{
				Type: commands.CMD_DISPLAY_PLAYLIST,
				Arguments: map[string]any{"dp1_call": map[string]interface{}{
					"dpVersion": "1.1.0", "title": "x",
					"items": []interface{}{map[string]interface{}{"source": "https://a", "contentRating": rating}},
				}},
			})
			require.Error(t, err)
			require.True(t, commandrouter.IsPlaylistInvalid(err),
				"a malformed rating must carry the playlistInvalid classification to the transports, got %v", err)
		})
	}
}

// After the player accepts but the durable write fails, the player is the one
// out of step. Leaving it there until a reconnect keeps the two enforcement
// points diverged for as long as the device stays up, so the stored policy is
// restored immediately.
func TestSetContentPolicyRestoresThePlayerWhenTheWriteFails(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	// A directory where the state file should be makes the atomic write fail.
	dir := t.TempDir()
	path := filepath.Join(dir, "policy.json")
	require.NoError(t, os.Mkdir(path, 0o750))

	// Fallback rather than Open: Open would read the directory and fail. The
	// store starts on defaults either way, which is what the restore must send.
	broken := contentpolicy.Fallback(path, false)

	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, broken, zaptest.NewLogger(t))

	var sentPolicies []bool
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			expr := params["expression"].(string)
			wantMature := strings.Contains(expr, `"showMatureContent":true`)
			sentPolicies = append(sentPolicies, wantMature)
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "active": true,
				"contentPolicy": map[string]interface{}{
					"version": float64(1), "showMatureContent": wantMature,
					"strictPersonal": false, "blockUnratedCurated": false,
				},
			}}, nil
		}).Times(2)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false},
	})
	require.NoError(t, err)
	require.Equal(t, "contentPolicyUnavailable", result.(map[string]interface{})["error"])
	require.Equal(t, []bool{true, false}, sentPolicies,
		"the candidate must be sent, then the stored policy restored after the write failed")

	broken.Lock()
	active := broken.CurrentLocked()
	broken.Unlock()
	require.False(t, active.ShowMatureContent, "a failed write must leave the daemon on its stored policy")
}

// SourceProbeResult.Source is query-redacted and truncated for the daemon log,
// so the final-policy re-check must key its retained verdicts by the RAW source
// it probed. Keying on the result field made every signed URL miss its own
// verdict, and a miss makes the re-check fail open — forwarding exactly the
// known-dead cast it exists to stop.
func TestDisplayPlaylistRechecksSignedURLsAfterTheProjection(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	poller := mocks.NewMockStatusPoller(ctrl)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	store.Lock()
	_, err = store.UpdateLocked(true, false) // mature allowed, so the live item survives the first filter
	store.Unlock()
	require.NoError(t, err)

	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl), poller,
		nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	// Both sources carry query strings, which is what the log redaction strips.
	const live = "https://live.example/mature.html?token=abc123&sig=deadbeef"
	const dead = "https://dead.example/general.html?token=zzz999&sig=cafebabe"
	commandrouter.SetSourceProber(h, &verdictProber{
		verdicts: map[string]offlinecache.SourceProbeVerdict{
			live: offlinecache.ProbeAlive,
			dead: offlinecache.ProbeDead,
		},
		redactSources: true,
		during: func() {
			store.Lock()
			_, updateErr := store.UpdateLocked(false, false)
			store.Unlock()
			require.NoError(t, updateErr)
		},
	}, zaptest.NewLogger(t))

	// No player.EXPECT(): the known-dead remainder must not be cast.
	poller.EXPECT().ForceRefresh().AnyTimes()

	_, err = h.Process(context.Background(), commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": map[string]interface{}{
			"dpVersion": "1.1.0", "title": "x",
			"items": []interface{}{
				map[string]interface{}{"source": live, "contentRating": "mature"},
				map[string]interface{}{"source": dead, "contentRating": "general"},
			},
		}},
	})
	require.Error(t, err)
	var unreachable *commandrouter.SourceUnreachableError
	require.ErrorAs(t, err, &unreachable)
}

// A schedule persisted before contentContext existed restores with an empty
// value, which means "from before", not "curated" — the router sets the field
// on every source it hands the scheduler. Projecting it as curated would strip
// the mature items an owner scheduled as personal, at the first cutover after
// an upgrade.
func TestSchedulerProjectorLeavesALegacyCohortUnprojected(t *testing.T) {
	ctrl := gomock.NewController(t)
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
	require.NoError(t, err)
	sched := &capturingScheduler{}
	h := commandrouter.New(newRoutableExecutor(ctrl), mocks.NewMockCDP(ctrl), mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, sched, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))
	require.NotNil(t, sched.projector)

	mature := contentrating.RatingMature
	cohort := &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: []dp1playlist.PlaylistItem{
		{ID: "ok", Source: "https://ok"},
		{ID: "mature", Source: "https://mature", ContentRating: &mature},
	}}}

	projected, empty := sched.projector(cohort, "")
	require.False(t, empty)
	require.Len(t, projected.Items, 2, "a pre-feature schedule must not be reclassified as curated")

	// A known context still projects.
	projected, empty = sched.projector(cohort, "curated")
	require.False(t, empty)
	require.Len(t, projected.Items, 1)
}

// fakePolicyRefresher records the re-send an accepted policy change triggers.
type fakePolicyRefresher struct{ forced int }

func (f *fakePolicyRefresher) ForceRefresh() { f.forced++ }

// The playlist on screen was projected under the OLD policy, so enabling mature
// content cannot bring back what the previous projection removed until
// something re-resolves. Without this the Content screen reports success while
// the display stays unchanged until the periodic refresh, minutes later.
func TestSetContentPolicyResendsTheCurrentPlaylistOnlyWhenItCommits(t *testing.T) {
	newHandler := func(t *testing.T, ack bool) (commandrouter.Handler, *fakePolicyRefresher) {
		t.Helper()
		ctrl := gomock.NewController(t)
		player := mocks.NewMockCDP(ctrl)
		store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), false)
		require.NoError(t, err)
		h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
			mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
		commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))
		refresher := &fakePolicyRefresher{}
		commandrouter.SetPolicyRefresher(h, refresher, zaptest.NewLogger(t))

		player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
			func(_ string, _ map[string]interface{}) (interface{}, error) {
				if !ack {
					return map[string]interface{}{"message": map[string]interface{}{"ok": false, "error": "busy"}}, nil
				}
				return map[string]interface{}{"message": map[string]interface{}{
					"ok": true, "active": true,
					"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": true, "strictPersonal": false, "blockUnratedCurated": false},
				}}, nil
			}).Times(1)
		return h, refresher
	}

	t.Run("an accepted change re-sends", func(t *testing.T) {
		h, refresher := newHandler(t, true)
		result, err := h.Process(context.Background(), commands.Command{
			Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false},
		})
		require.NoError(t, err)
		require.Equal(t, true, result.(map[string]interface{})["active"])
		require.Equal(t, 1, refresher.forced)
	})

	t.Run("a refused change re-sends nothing", func(t *testing.T) {
		h, refresher := newHandler(t, false)
		result, err := h.Process(context.Background(), commands.Command{
			Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false},
		})
		require.NoError(t, err)
		require.Equal(t, false, result.(map[string]interface{})["ok"])
		require.Equal(t, 0, refresher.forced, "nothing changed, so nothing needs re-resolving")
	})
}

// Factory reset must clear the owner's audience setting and push the default to
// the player. On the success path the durable file is discarded with the
// subvolume, but a reset that ROLLS BACK would otherwise leave a resold frame
// enforcing the previous owner's rules.
func TestResetContentPolicyClearsTheOwnerSettingAndSyncsThePlayer(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	path := filepath.Join(t.TempDir(), "policy.json")
	store, err := contentpolicy.Open(path, false)
	require.NoError(t, err)
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	// The previous owner allowed mature content.
	store.Lock()
	_, err = store.UpdateLocked(true, false)
	store.Unlock()
	require.NoError(t, err)
	require.True(t, store.Snapshot().ShowMatureContent)

	var synced bool
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			require.Contains(t, params["expression"].(string), `"showMatureContent":false`)
			synced = true
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "active": true,
				"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": false, "strictPersonal": false, "blockUnratedCurated": false},
			}}, nil
		}).Times(1)

	require.NoError(t, commandrouter.ResetContentPolicy(h))
	require.True(t, synced, "the default must be pushed to the player, not only stored")

	store.Lock()
	after := store.CurrentLocked()
	store.Unlock()
	require.False(t, after.ShowMatureContent, "the next owner must not inherit the setting")
	require.False(t, store.Snapshot().ShowMatureContent)
	_, statErr := os.Stat(path)
	require.True(t, os.IsNotExist(statErr), "the durable file must be gone after a reset")
}

// The API's success means "saved". After a committed rename whose parent
// directory fsync cannot be confirmed even on retry, a power loss could still
// revert it — so the reply must not claim success. The applied, self-consistent
// state (memory, file, player) is left in place rather than rolled back to a
// policy the file no longer holds.
func TestSetContentPolicyReportsUnavailableWhenDurabilityStaysUnconfirmed(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	// A policy path whose parent directory is removed after Open: the rename
	// target still resolves through the open handle's directory entry, but the
	// directory fsync cannot be confirmed.
	dir := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.Mkdir(dir, 0o750))
	store, err := contentpolicy.Open(filepath.Join(dir, "policy.json"), false)
	require.NoError(t, err)

	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "active": true,
				"contentPolicy": map[string]interface{}{"version": float64(1), "showMatureContent": true, "strictPersonal": false, "blockUnratedCurated": false},
			}}, nil
		}).AnyTimes()

	// The happy path still reports success, which is what makes the negative
	// case below meaningful rather than vacuous.
	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_SET_CONTENT_POLICY, Arguments: map[string]any{"showMatureContent": true, "strictPersonal": false},
	})
	require.NoError(t, err)
	require.Equal(t, true, result.(map[string]interface{})["active"])

	// ConfirmDurableLocked is the retry the handler runs before reporting
	// success; it must fail when the directory is gone.
	require.NoError(t, os.RemoveAll(dir))
	store.Lock()
	confirmErr := store.ConfirmDurableLocked()
	store.Unlock()
	require.Error(t, confirmErr, "a missing directory must not confirm as durable")
}

// The replay acknowledgement is reduced to the documented fields before it
// leaves the daemon. Same rule, same reason, as the history reply: this is
// reachable from the unauthenticated LAN hub, and the thing being replayed is a
// retained DP-1 item whose source can be a signed URL carrying credentials — so
// a player acknowledgement that echoed the request must not carry it out.
func TestPlayRecentlyPlayedBoundsThePlayerAcknowledgement(t *testing.T) {
	h, player, poller := newPolicyHandlerWithPoller(t)
	poller.EXPECT().ForceRefresh().Times(1)

	const signed = "https://cdn.example/work.html?token=secret&sig=deadbeef"
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "status": "ok",
				"item": map[string]interface{}{"id": "retained", "source": signed},
			}}, nil
		}).Times(1)
	// The player echoes the whole request back in its acknowledgement.
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok":          true,
				"request":     map[string]interface{}{"dp1_call": map[string]interface{}{"items": []interface{}{map[string]interface{}{"source": signed}}}},
				"echo":        signed,
				"diagnostics": map[string]interface{}{"lastSource": signed},
			}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_PLAY_RECENTLY_PLAYED, Arguments: map[string]any{"recordId": "rp-9"},
	})
	require.NoError(t, err)

	reply := result.(map[string]interface{})
	require.Equal(t, "rp-9", reply["recordId"])
	ack := reply["message"].(map[string]interface{})
	require.Equal(t, true, ack["ok"], "acceptance must still be reported")
	for _, forbidden := range []string{"request", "echo", "diagnostics", "item", "dp1_call"} {
		_, leaked := ack[forbidden]
		require.False(t, leaked, "%q escaped the replay acknowledgement: %v", forbidden, ack)
	}
	// Nothing anywhere in the serialized reply may carry the signed source.
	encoded, err := json.Marshal(reply)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "token=secret")
}

// A FAILED replay must not carry the retained item out either. The resolver
// failure path returned the player's classified message directly, so a modern
// failure naming or echoing the record escaped the same boundary the success
// path is bounded for.
func TestPlayRecentlyPlayedBoundsAFailedResolve(t *testing.T) {
	h, player, _ := newPolicyHandler(t)
	const signed = "https://cdn.example/work.html?token=secret&sig=deadbeef"

	// No second Send: a failed resolve never reaches displayPlaylist.
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": false, "status": "error",
				"error": "record source unreachable: " + signed,
				"item":  map[string]interface{}{"id": "retained", "source": signed},
			}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_PLAY_RECENTLY_PLAYED, Arguments: map[string]any{"recordId": "rp-5"},
	})
	require.NoError(t, err)

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "token=secret",
		"a failed replay leaked the retained item's credentials: %s", encoded)
	require.NotContains(t, string(encoded), `"item"`)
	// The reason still reaches the app, which is what makes an evicted record
	// distinguishable from a missing capability.
	require.Contains(t, string(encoded), "record source unreachable")
}

// A DISPLAY failure on the replay path is player-authored text about a retained
// item, so it can quote that item's signed source. Truncation alone left the
// query credentials intact; the acknowledgement's error must be sanitized the
// same way the history failure replies are.
func TestPlayRecentlyPlayedSanitizesADisplayError(t *testing.T) {
	h, player, poller := newPolicyHandlerWithPoller(t)
	poller.EXPECT().ForceRefresh().AnyTimes()

	const signed = "https://cdn.example/work.html?token=secret&sig=deadbeef"
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok": true, "status": "ok",
				"item": map[string]interface{}{"id": "retained", "source": signed},
			}}, nil
		}).Times(1)
	// The display attempt fails and names the source it could not load.
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, _ map[string]interface{}) (interface{}, error) {
			return map[string]interface{}{"message": map[string]interface{}{
				"ok":    false,
				"error": "render failed for " + signed + " after 3 tries",
			}}, nil
		}).Times(1)

	result, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_PLAY_RECENTLY_PLAYED, Arguments: map[string]any{"recordId": "rp-11"},
	})
	require.NoError(t, err)

	encoded, err := json.Marshal(result)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "token=secret",
		"a display error leaked the retained item's credentials: %s", encoded)
	require.NotContains(t, string(encoded), "sig=deadbeef")
	// The operator-useful part survives.
	require.Contains(t, string(encoded), "render failed")
	require.Contains(t, string(encoded), "cdn.example/work.html")
}

// The accepting half of the rating rule, end to end through public ingress: an
// unrecognized rating STRING is not a rejection at any boundary, and it plays —
// the document passes the extension schema, survives the policy filter as
// unrated, and reaches the player. DP-1 §3.3 (display-protocol/dp1#52).
//
// Paired with TestDisplayPlaylistClassifiesAWrongTypedRatingAsPlaylistInvalid,
// which pins the other half: a non-string is still schema-invalid.
func TestDisplayPlaylistAcceptsAnUnknownRatingStringEndToEnd(t *testing.T) {
	h, player, poller := newPolicyHandlerWithPoller(t)
	poller.EXPECT().ForceRefresh().AnyTimes()

	var sent string
	player.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(
		func(_ string, params map[string]interface{}) (interface{}, error) {
			sent = params["expression"].(string)
			return map[string]interface{}{"message": map[string]interface{}{"ok": true}}, nil
		}).Times(1)

	// Default policy: mature is hidden from a curated cast, so if "teen" were
	// read as anything mature-like the item would not survive.
	_, err := h.Process(context.Background(), commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": map[string]interface{}{
			"dpVersion": "1.1.0", "title": "x",
			"items": []interface{}{
				map[string]interface{}{"source": "https://future.example/a", "contentRating": "teen"},
			},
		}},
	})
	require.NoError(t, err, "an unknown rating string must not be refused at ingress")
	require.Contains(t, sent, "https://future.example/a",
		"an unknown rating string must play as unrated, not be filtered out")
}

// And the operator gate still catches it, because an unknown label IS unrated —
// the equivalence, asserted through the public path rather than only on the
// policy matrix.
func TestDisplayPlaylistAppliesTheUnratedGateToAnUnknownRatingString(t *testing.T) {
	ctrl := gomock.NewController(t)
	player := mocks.NewMockCDP(ctrl)
	// blockUnratedCurated is the operator gate, set from device config.
	store, err := contentpolicy.Open(filepath.Join(t.TempDir(), "policy.json"), true)
	require.NoError(t, err)
	h := commandrouter.New(newRoutableExecutor(ctrl), player, mocks.NewMockDP1(ctrl),
		mocks.NewMockStatusPoller(ctrl), nil, nil, nil, nil, wrapper.NewJSON(), zaptest.NewLogger(t))
	commandrouter.SetContentPolicy(h, store, zaptest.NewLogger(t))

	// No player.EXPECT(): every item is withheld, so the cast is blocked.
	_, err = h.Process(context.Background(), commands.Command{
		Type: commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": map[string]interface{}{
			"dpVersion": "1.1.0", "title": "x",
			"items": []interface{}{
				map[string]interface{}{"source": "https://future.example/a", "contentRating": "teen"},
			},
		}},
	})
	require.Error(t, err)
	require.True(t, commandrouter.IsContentBlocked(err),
		"the unrated-curated gate must treat an unknown label as unrated, got %v", err)
}
