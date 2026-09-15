package commandrouter_test

import (
	"context"
	"path/filepath"
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
