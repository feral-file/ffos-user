package commandrouter_test

// Cast-time source preflight (feral-file/ffos-user#304): the displayPlaylist
// path rejects a cast only when EVERY resolved item source earned a
// definitive dead verdict, and fails open on everything else. The probe
// mechanics themselves (the single ranged GET, guard, timeouts) are covered in
// offlinecache's probe tests; these cover the handler's decision rule and
// its wiring.

import (
	"context"
	"errors"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
)

// fakeSourceProber is a directly-controllable offlinecache.SourceProber
// double: results is returned verbatim, probed records what was asked.
type fakeSourceProber struct {
	results []offlinecache.SourceProbeResult
	probed  [][]string
}

func (f *fakeSourceProber) ProbeSources(_ context.Context, sources []string) []offlinecache.SourceProbeResult {
	f.probed = append(f.probed, sources)
	return f.results
}

func probeTestPlaylist(sources ...string) *dp1.Playlist {
	items := make([]dp1playlist.PlaylistItem, 0, len(sources))
	for i, s := range sources {
		items = append(items, dp1playlist.PlaylistItem{ID: string(rune('a' + i)), Source: s})
	}
	return &dp1.Playlist{Playlist: dp1playlist.Playlist{Items: items}}
}

func displayPlaylistURLCommand(url string) commands.Command {
	return commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"playlistUrl": url},
	}
}

func TestCommandHandler_Process_DisplayPlaylist_AllSourcesDead_RejectsCast(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/dead"), nil).
		Times(1)
	// No CDP Send and no ForceRefresh expectations: the rejection must
	// happen before anything reaches the player.

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 400},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSourceUnreachable(err))
	assert.Contains(t, err.Error(), "sourceUnreachable")
	assert.Contains(t, err.Error(), "item 0: HTTP 400")
	// Sanitization contract: the error is returned verbatim to casters,
	// and resolved source URLs are playlist content they may never have
	// supplied (signed CDN queries carry credentials) — items are named
	// by index and status only.
	assert.NotContains(t, err.Error(), "origin.example")
	assert.Nil(t, result)
	require.Len(t, prober.probed, 1)
	assert.Equal(t, []string{"https://origin.example/dead"}, prober.probed[0])
}

// TestCommandHandler_Process_DisplayPlaylist_AllDeadButCached_CastsForReplay
// pins the cache rescue (#305 review F4): a definitively dead origin is not
// a dead cast when the offline cache holds a prior capture — replay serves
// cached items regardless of origin state — so an all-dead preflight
// consults the cache before rejecting, and a replayable item lets the cast
// proceed.
func TestCommandHandler_Process_DisplayPlaylist_AllDeadButCached_CastsForReplay(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockService := mocks.NewMockOfflineCacheService(ts.ctrl)
	mockService.EXPECT().
		HasReplayableItem("https://origin.example/dead").
		Return(true).
		Times(1)
	// The rescue requires replay to actually arm: successful scope sync is
	// what lets the cast proceed (see the sync-failure test below for the
	// other half of that contract).
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().Times(1)
	mockKioskReplay.EXPECT().UnlockPlayback().Times(1)
	mockKioskReplay.EXPECT().SyncPlaylist(ts.ctx, []string{"https://origin.example/dead"}).Return(1, nil).Times(1)
	mockKioskReplay.EXPECT().MarkPlaybackChanged().Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockService, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	playlistURL := "https://example.com/playlist.json"
	expectDisplayPlaylistSuccess(ts, playlistURL,
		probeTestPlaylist("https://origin.example/dead"))

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 404},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_CachedButScopeSyncFails_Rejects
// pins the second half of the rescue contract (#308 review): a cache
// record replay cannot arm rescues nothing — a rescued cast's ONLY path to
// the screen is replay, so a scope-sync failure must reject the cast with
// the preflight's own error instead of forwarding it to render origins
// already proven dead.
func TestCommandHandler_Process_DisplayPlaylist_CachedButScopeSyncFails_Rejects(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockService := mocks.NewMockOfflineCacheService(ts.ctrl)
	mockService.EXPECT().
		HasReplayableItem("https://origin.example/dead").
		Return(true).
		Times(1)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().
		SyncPlaylist(ts.ctx, []string{"https://origin.example/dead"}).
		Return(0, errors.New("replay session down")).
		Times(1)
	mockKioskReplay.EXPECT().MarkPlaybackChanged().Times(1)
	// The failed sync TOUCHED scope (generation bumped), so the failure
	// defer's corrective resync must run — unlike the pre-sync rejection
	// path (see RejectionSkipsReplayResync). Its player-status fetch
	// failing is fine: the resync is best-effort by contract.
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(gomock.Any()).
		Return(nil, errors.New("player unavailable")).
		Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockService, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/dead"), nil).
		Times(1)
	// No CDP Send expectation: the sync failure must stop the cast.

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 404},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSourceUnreachable(err))
	assert.Nil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_CachedButScopeArmsNothing_Rejects
// pins the #310 F2 rule: an errorless sync that armed ZERO cached sources
// converts the rescue back into the rejection. HasReplayableItem's answer
// can be stale (the record cleared between lookup and sync) or fail-open
// past its bounded scan (the over-512 unique-source flood) — the count
// from the scope actually installed is what decides, and both scenarios
// reach the handler as exactly this shape: rescue granted, sync returns
// (0, nil).
func TestCommandHandler_Process_DisplayPlaylist_CachedButScopeArmsNothing_Rejects(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockService := mocks.NewMockOfflineCacheService(ts.ctrl)
	mockService.EXPECT().
		HasReplayableItem("https://origin.example/dead").
		Return(true).
		Times(1)
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	mockKioskReplay.EXPECT().LockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().UnlockPlayback().AnyTimes()
	mockKioskReplay.EXPECT().PlaybackGeneration().Return(uint64(0)).AnyTimes()
	mockKioskReplay.EXPECT().
		SyncPlaylist(ts.ctx, []string{"https://origin.example/dead"}).
		Return(0, nil).
		Times(1)
	mockKioskReplay.EXPECT().MarkPlaybackChanged().Times(1)
	// Scope was touched (sync ran), so the failure defer's corrective
	// resync runs; best-effort as ever.
	ts.mockStatusPoller.EXPECT().
		FetchPlayerStatus(gomock.Any()).
		Return(nil, errors.New("player unavailable")).
		Times(1)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockService, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/dead"), nil).
		Times(1)
	// No CDP Send expectation: an empty armed scope must stop the cast.

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 404},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSourceUnreachable(err))
	assert.Nil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_CachedButNoReplayWired_Rejects
// pins the degenerate case of the same contract: with no kiosk replay
// wired at all, a cached capture is unreachable, so the cache must not
// even be consulted and the all-dead rejection stands.
func TestCommandHandler_Process_DisplayPlaylist_CachedButNoReplayWired_Rejects(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockService := mocks.NewMockOfflineCacheService(ts.ctrl)
	// No HasReplayableItem expectation: without replay the lookup is moot.
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockService, nil, nil, ts.mockJSON, ts.logger)

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/dead"), nil).
		Times(1)

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 404},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSourceUnreachable(err))
	assert.Nil(t, result)
}

// ...and the negative half: with the cache consulted and empty-handed, the
// all-dead rejection stands.
func TestCommandHandler_Process_DisplayPlaylist_AllDeadNotCached_StillRejects(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockService := mocks.NewMockOfflineCacheService(ts.ctrl)
	mockService.EXPECT().
		HasReplayableItem("https://origin.example/dead").
		Return(false).
		Times(1)
	// Replay wired (so the cache IS consulted) but empty-handed: the
	// rejection stands, and it fires before the playback lock — no other
	// kioskReplay expectations on purpose.
	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockService, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/dead"), nil).
		Times(1)

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 404},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSourceUnreachable(err))
	assert.Nil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_Scheduled_SkipsPreflightEntirely
// pins the premiere carve-out in its #308-round-3 form: a scheduled
// playlist's probe verdict could never affect the cast (dead-now is not
// dead-at-display-time), so the preflight must not run AT ALL — its only
// possible contribution is up to probePhaseCeiling of latency sitting in
// front of the scheduler arming a due cutover.
func TestCommandHandler_Process_DisplayPlaylist_Scheduled_SkipsPreflightEntirely(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	displayAt := "2999-01-01T00:00:00Z"
	playlist := probeTestPlaylist("https://origin.example/premiere")
	playlist.Items[0].DisplayAt = &displayAt

	playlistURL := "https://example.com/playlist.json"
	// No scheduler wired in this setup, so the cast proceeds down the
	// default send path — the assertions are that it PROCEEDS and that
	// the prober was never consulted.
	expectDisplayPlaylistSuccess(ts, playlistURL, playlist)

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/premiere", Verdict: offlinecache.ProbeDead, Status: 404},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Empty(t, prober.probed,
		"a scheduled playlist must not be probed at all — the verdict could never affect it, only delay it")
}

// TestCommandHandler_Process_DisplayPlaylist_RejectionSkipsReplayResync pins
// the preflight-rejection fast path: the rejection happens before any
// replay-scope change, so the failure defer's corrective resync (a
// network-bound FetchPlayerStatus plus playlist resolution) must NOT run —
// running it would delay an otherwise immediate error reply, in the worst
// case past the hub's write deadline. The strict mocks are the assertion:
// any KioskReplay or FetchPlayerStatus call here fails the test.
func TestCommandHandler_Process_DisplayPlaylist_RejectionSkipsReplayResync(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockKioskReplay := mocks.NewMockOfflineCacheKioskReplay(ts.ctrl)
	// No EXPECT calls on purpose: the rejection path must never touch the
	// playback lock, scope sync, or the corrective resync.
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, nil, mockKioskReplay, nil, ts.mockJSON, ts.logger)

	playlistURL := "https://example.com/playlist.json"
	ts.mockDP1.EXPECT().
		ProcessPlaylistURLForCast(ts.ctx, playlistURL).
		Return(probeTestPlaylist("https://origin.example/dead"), nil).
		Times(1)

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 404},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSourceUnreachable(err))
	assert.Nil(t, result)
}

// TestSourceUnreachableError_CapsAndSanitizesDetail pins the response
// amplification bound: past the detail cap a single omitted-count entry
// stands in, and no source URL appears no matter how many results ride
// the error.
func TestSourceUnreachableError_CapsAndSanitizesDetail(t *testing.T) {
	results := make([]offlinecache.SourceProbeResult, 15)
	for i := range results {
		results[i] = offlinecache.SourceProbeResult{
			Source:  "https://origin.example/signed?token=secret",
			Verdict: offlinecache.ProbeDead,
			Status:  404,
		}
	}
	err := &commandrouter.SourceUnreachableError{Results: results}

	msg := err.Error()
	assert.Contains(t, msg, "item 9: HTTP 404")
	assert.Contains(t, msg, "and 5 more")
	assert.NotContains(t, msg, "item 10:")
	assert.NotContains(t, msg, "origin.example")
	assert.NotContains(t, msg, "token=secret")
}

func TestCommandHandler_Process_DisplayPlaylist_PartiallyDead_StillCasts(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	expectDisplayPlaylistSuccess(ts, playlistURL,
		probeTestPlaylist("https://origin.example/dead", "https://origin.example/alive"))

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 404},
		{Source: "https://origin.example/alive", Verdict: offlinecache.ProbeAlive, Status: 200},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_AllInconclusive_FailsOpen pins
// the offline-device contract: when no probe got a definitive answer (the
// shape an offline device produces for every item), the cast proceeds —
// otherwise the cached-copy fallback would be unreachable exactly when it
// is needed.
func TestCommandHandler_Process_DisplayPlaylist_AllInconclusive_FailsOpen(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	expectDisplayPlaylistSuccess(ts, playlistURL,
		probeTestPlaylist("https://origin.example/one", "https://origin.example/two"))

	probeErr := errors.New("dial tcp: network is unreachable")
	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/one", Verdict: offlinecache.ProbeInconclusive, Err: probeErr},
		{Source: "https://origin.example/two", Verdict: offlinecache.ProbeInconclusive, Err: probeErr},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

// TestCommandHandler_Process_DisplayPlaylist_NoProber_SkipsPreflight pins the
// nil seam: a handler with no prober wired behaves exactly as before the
// preflight existed. (Every other displayPlaylist test in this package also
// exercises this implicitly; this one makes the contract explicit.)
func TestCommandHandler_Process_DisplayPlaylist_NoProber_SkipsPreflight(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	playlistURL := "https://example.com/playlist.json"
	expectDisplayPlaylistSuccess(ts, playlistURL,
		probeTestPlaylist("https://origin.example/dead"))

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	assert.NoError(t, err)
	assert.NotNil(t, result)
}

func TestCommandHandler_Process_DisplayPlaylist_InlineDP1Call_AllDead_RejectsCast(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// The inline dp1_call variant round-trips the payload through the JSON
	// wrapper; stub the round-trip the same way the existing inline test
	// does (Marshal returns opaque bytes, Unmarshal plants the typed
	// playlist).
	playlistMap := map[string]interface{}{
		"items": []interface{}{
			map[string]interface{}{"id": "a", "source": "https://origin.example/dead"},
		},
	}
	playlistBytes := []byte(`{"items":[{"id":"a"}]}`)
	mockPlaylist := probeTestPlaylist("https://origin.example/dead")

	ts.mockJSON.EXPECT().
		Marshal(playlistMap).
		Return(playlistBytes, nil).
		Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(playlistBytes, gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			playlist := v.(**dp1.Playlist)
			*playlist = mockPlaylist
			return nil
		}).
		Times(1)
	// No CDP Send / ForceRefresh expectations: the rejection precedes both.

	prober := &fakeSourceProber{results: []offlinecache.SourceProbeResult{
		{Source: "https://origin.example/dead", Verdict: offlinecache.ProbeDead, Status: 400},
	}}
	commandrouter.SetSourceProber(ts.handler, prober, ts.logger)

	command := commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]interface{}{"dp1_call": playlistMap},
	}

	result, err := ts.handler.Process(ts.ctx, command)

	require.Error(t, err)
	assert.True(t, commandrouter.IsSourceUnreachable(err))
	assert.Nil(t, result)
}
