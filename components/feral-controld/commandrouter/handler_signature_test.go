package commandrouter_test

// DP-1 signature verification on the displayPlaylist path
// (feral-file/ffos-user#307), phase 1: verify-and-report. Every cast still
// plays; the verdict rides the reply and the active-verdict slot. The
// verifier itself is covered in sigverify; these cover the handler's wiring:
// which bytes it verifies, where the verdict lands, and when the slot moves.

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

// signedFixture is the live feed playlist sigverify's tests pin; reused here
// so the handler is exercised against a real signature, not a synthetic one.
func signedFixture(t *testing.T) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "sigverify", "testdata", "feed-dd875fbe.json"))
	require.NoError(t, err)
	return raw
}

func replyMessage(t *testing.T, result any) map[string]any {
	t.Helper()
	m, ok := result.(map[string]any)
	require.True(t, ok, "result is %T", result)
	msg, ok := m["message"].(map[string]any)
	require.True(t, ok, "result has no message map: %v", m)
	return msg
}

func wireVerification(ts *testSetup) *sigverify.Active {
	active := &sigverify.Active{}
	commandrouter.SetSignatureVerification(ts.handler, commandrouter.SignatureVerificationOptions{Active: active}, ts.logger)
	return active
}

// TestCommandHandler_Process_DisplayPlaylist_URL_ReportsVerdictFromResolver:
// on the URL path the verdict is attached by the dp1 package at fetch time;
// the handler only reports it and publishes it for player_status.
func TestCommandHandler_Process_DisplayPlaylist_URL_ReportsVerdictFromResolver(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerification(ts)

	playlistURL := "https://feed.example/p.json"
	verdict := sigverify.Verify(signedFixture(t))
	require.Equal(t, sigverify.StatusValid, verdict.Status)
	playlist := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "playlist-1",
		Items: []dp1playlist.PlaylistItem{{ID: "item1", Source: "https://example.com/a"}},
	}, Verification: &verdict}
	expectDisplayPlaylistSuccess(ts, playlistURL, playlist)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.NoError(t, err)
	msg := replyMessage(t, result)
	assert.Equal(t, true, msg["ok"])
	assert.Equal(t, "valid", msg["signatureStatus"])
	signers, ok := msg["signers"].([]any)
	require.True(t, ok)
	require.Len(t, signers, 1)
	signer := signers[0].(map[string]any)
	assert.Equal(t, "feed", signer["role"])
	assert.Equal(t, "ed25519", signer["alg"])
	assert.Equal(t, true, signer["ok"])
	assert.NotContains(t, msg, "legacySignature")
	assert.NotContains(t, signer, "reason")

	st, found := active.Lookup("playlist-1", "")
	assert.True(t, found)
	assert.Equal(t, sigverify.StatusValid, st)
	st, found = active.Lookup("", playlistURL)
	assert.True(t, found, "URL casts must be matchable by playlistURL too")
	assert.Equal(t, sigverify.StatusValid, st)
}

// inlineCast returns the command plus the mock JSON expectations the
// dp1_call branch drives: Marshal of the map yields rawBytes (what the
// handler verifies), Unmarshal of those bytes yields typed.
func inlineCast(ts *testSetup, rawBytes []byte, typed *dp1.Playlist) commands.Command {
	playlistMap := map[string]any{"marker": "inline"}
	ts.mockJSON.EXPECT().Marshal(playlistMap).Return(rawBytes, nil).Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(rawBytes, gomock.Any()).
		DoAndReturn(func(_ []byte, v any) error {
			*(v.(**dp1.Playlist)) = typed
			return nil
		}).
		Times(1)
	return commands.Command{
		Type:      commands.CMD_DISPLAY_PLAYLIST,
		Arguments: map[string]any{"dp1_call": playlistMap},
	}
}

func inlineTyped(id string) *dp1.Playlist {
	return &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    id,
		Items: []dp1playlist.PlaylistItem{{ID: "item1", Source: "https://example.com/a"}},
	}}
}

// TestCommandHandler_Process_DisplayPlaylist_Inline_UnsignedStillPlays is
// today's ordinary app cast: no signatures at all. It plays, the reply says
// so, and the slot records it.
func TestCommandHandler_Process_DisplayPlaylist_Inline_UnsignedStillPlays(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerification(ts)

	raw := []byte(`{"dpVersion":"1.1.0","id":"app-1","title":"t","items":[{"id":"i","source":"https://example.com/a","duration":10,"license":"open"}]}`)
	command := inlineCast(ts, raw, inlineTyped("app-1"))
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	require.NoError(t, err)
	msg := replyMessage(t, result)
	assert.Equal(t, true, msg["ok"])
	assert.Equal(t, "unsigned", msg["signatureStatus"])
	assert.NotContains(t, msg, "signers")
	st, found := active.Lookup("app-1", "")
	assert.True(t, found)
	assert.Equal(t, sigverify.StatusUnsigned, st)
}

// TestCommandHandler_Process_DisplayPlaylist_Inline_VerifiesMarshaledBytes
// pins WHICH bytes the inline branch verifies: the map re-marshal the JSON
// wrapper produced, not the typed struct. The typed playlist handed back by
// Unmarshal here is deliberately a stub that would never verify; the real
// signed bytes are what Marshal returns, and they must be what is judged.
func TestCommandHandler_Process_DisplayPlaylist_Inline_VerifiesMarshaledBytes(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	wireVerification(ts)

	command := inlineCast(ts, signedFixture(t), inlineTyped("stub"))
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	require.NoError(t, err)
	assert.Equal(t, "valid", replyMessage(t, result)["signatureStatus"])
}

// TestCommandHandler_Process_DisplayPlaylist_Inline_InvalidStillPlaysAndReports:
// phase 1 is observation only — a tampered document plays, but the reply
// names the failed signer and the sanitized reason.
func TestCommandHandler_Process_DisplayPlaylist_Inline_InvalidStillPlaysAndReports(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerification(ts)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(signedFixture(t), &doc))
	doc["title"] = "tampered"
	tampered, err := json.Marshal(doc)
	require.NoError(t, err)

	command := inlineCast(ts, tampered, inlineTyped("pl-tampered"))
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	require.NoError(t, err, "phase 1 never rejects")
	msg := replyMessage(t, result)
	assert.Equal(t, true, msg["ok"])
	assert.Equal(t, "invalid", msg["signatureStatus"])
	signers := msg["signers"].([]any)
	require.Len(t, signers, 1)
	signer := signers[0].(map[string]any)
	assert.Equal(t, false, signer["ok"])
	assert.Equal(t, sigverify.ReasonPayloadHashMismatch, signer["reason"])
	st, found := active.Lookup("pl-tampered", "")
	assert.True(t, found)
	assert.Equal(t, sigverify.StatusInvalid, st)
}

// TestCommandHandler_Process_DisplayPlaylist_LogsBoundedPlaylistID pins the
// log-field cap: a caster-sized playlist id (the open hub admits 4 MiB
// inline casts) must not reach the journal whole.
func TestCommandHandler_Process_DisplayPlaylist_LogsBoundedPlaylistID(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	core, logs := observer.New(zap.InfoLevel)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, nil, nil, nil, ts.mockJSON, zap.New(core))
	wireVerification(ts)

	hugeID := strings.Repeat("x", 1<<20)
	raw := []byte(`{"dpVersion":"1.1.0","id":"` + hugeID + `","title":"t","items":[]}`)
	command := inlineCast(ts, raw, inlineTyped(hugeID))
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	_, err := ts.handler.Process(ts.ctx, command)
	require.NoError(t, err)

	entries := logs.FilterMessage("displayPlaylist: playlist signature verdict").All()
	require.Len(t, entries, 1)
	logged, ok := entries[0].ContextMap()["playlist_id"]
	require.True(t, ok)
	assert.LessOrEqual(t, len(logged.(string)), logger.MAX_FIELD_LENGTH)
}

// TestCommandHandler_Process_DisplayDefaultPlaylist_ClearsCurrentKeepsPending:
// accepted default playback shows player-owned content controld never
// verified, so the attested verdict is dropped; the schedule's parked verdict
// survives because this command does not clear scheduler authority.
func TestCommandHandler_Process_DisplayDefaultPlaylist_ClearsCurrentKeepsPending(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerification(ts)
	active.Set("showing", "https://feed.example/p.json", sigverify.StatusValid)
	active.SetPending("scheduled", "", sigverify.StatusInvalid)

	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	_, err := ts.handler.Process(ts.ctx, commands.Command{Type: commands.CMD_DISPLAY_DEFAULT_PLAYLIST, Arguments: map[string]any{}})

	require.NoError(t, err)
	_, found := active.Lookup("showing", "https://feed.example/p.json")
	assert.False(t, found, "default playback replaced the attested document")
	active.Promote()
	st, found := active.Lookup("scheduled", "")
	assert.True(t, found, "the parked schedule verdict must survive")
	assert.Equal(t, sigverify.StatusInvalid, st)
}

// TestCommandHandler_Process_DisplayPlaylist_NotWired_ReplyUntouched: without
// SetSignatureVerification the reply is byte-for-byte the player's own, which
// is the shape old firmware has and what a disabled config must produce.
func TestCommandHandler_Process_DisplayPlaylist_NotWired_ReplyUntouched(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	command := inlineCast(ts, signedFixture(t), inlineTyped("pl-1"))
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	require.NoError(t, err)
	assert.Equal(t, playerOkResponse(), result)
}

// TestCommandHandler_Process_DisplayPlaylist_CachedFallback_NotVerified: the
// offline cached copy is a typed, hydrated re-marshal, not the signed bytes,
// so it carries NO verdict — the reply omits signatureStatus and the active
// slot is CLEARED, even though an earlier signed cast of the same URL had
// seeded it: player_status must not vouch for bytes nobody verified. The
// cached body here is byte-identical to the signed fixture precisely to
// prove the loader does not judge it: if it did, this would come back
// "valid".
func TestCommandHandler_Process_DisplayPlaylist_CachedFallback_NotVerified(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mockOfflineCache := mocks.NewMockOfflineCacheService(ts.ctrl)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, nil, nil, ts.mockJSON, ts.logger)
	active := wireVerification(ts)

	playlistURL := "https://feed.example/p.json"
	active.Set("cached-1", playlistURL, sigverify.StatusValid) // an earlier live cast of this URL
	cachedRaw := signedFixture(t)
	ts.mockDP1.EXPECT().ProcessPlaylistURLForCast(ts.ctx, playlistURL).Return(nil, errors.New("network unreachable")).Times(1)
	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).Return(json.RawMessage(cachedRaw), nil).Times(1)
	ts.mockJSON.EXPECT().
		Unmarshal(cachedRaw, gomock.Any()).
		DoAndReturn(func(_ []byte, v any) error {
			*(v.(**dp1.Playlist)) = inlineTyped("cached-1")
			return nil
		}).
		Times(1)
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.NoError(t, err)
	assert.Equal(t, playerOkResponse(), result, "no verdict keys on a cached-copy cast")
	_, found := active.Lookup("cached-1", playlistURL)
	assert.False(t, found, "the seeded verdict must not survive an unverified push of the same URL")
}

// TestCommandHandler_Process_DisplayPlaylist_Deferred_PendingUntilCutover: a
// future-only displayAt cast is accepted without reaching the player. The
// verdict is reported on the acceptance reply, the playlist still showing
// keeps its own verdict (same URL, different document), and the new verdict
// becomes visible only when the scheduler's push observer promotes it —
// the cutover that proves the cohort reached the screen.
func TestCommandHandler_Process_DisplayPlaylist_Deferred_PendingUntilCutover(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	mockClock.EXPECT().Now().Return(now).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()

	sched := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC }, logger)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, sched, mockJSON, logger)
	active := &sigverify.Active{}
	commandrouter.SetSignatureVerification(handler, commandrouter.SignatureVerificationOptions{Active: active}, logger)
	sched.SetPushObserver(active.Promote)

	playlistURL := "https://example.com/future.json"
	active.Set("showing", playlistURL, sigverify.StatusUnsigned) // the document on screen, same URL republished
	verdict := sigverify.Verify(signedFixture(t))
	mockDP1.EXPECT().ProcessPlaylistURLForCast(ctx, playlistURL).Return(&dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "future-1",
		Items: []dp1playlist.PlaylistItem{{ID: "future", Source: "https://example.com/future.html", DisplayAt: strPtr("2026-09-13T00:00:00Z")}},
	}, Verification: &verdict}, nil)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Times(0)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := handler.Process(ctx, displayPlaylistURLCommand(playlistURL))

	require.NoError(t, err)
	msg := replyMessage(t, result)
	assert.Equal(t, true, msg["ok"])
	assert.Equal(t, true, msg["deferred"])
	assert.Equal(t, "valid", msg["signatureStatus"])
	st, found := active.Lookup("showing", playlistURL)
	assert.True(t, found, "the playlist still on screen keeps its own verdict")
	assert.Equal(t, sigverify.StatusUnsigned, st)
	_, found = active.Lookup("future-1", "")
	assert.False(t, found, "the deferred document is not on screen yet")

	// The scheduler's cutover push is what promotes it (its observer is
	// exercised for real in playlistschedule's tests); here the promotion
	// itself is what is pinned.
	active.Promote()
	st, found = active.Lookup("future-1", playlistURL)
	assert.True(t, found)
	assert.Equal(t, sigverify.StatusValid, st)
}

// TestCommandHandler_Process_DisplayPlaylist_PublishesInsidePushLock pins the
// ordering rule: the slot is written inside the same WithPlayerPush critical
// section as the send. Cast A's player reply starts cast B concurrently; B
// can only send once A's closure has released the push lock, so if A's
// publication were outside the lock B could observe the slot before A wrote
// it. With the rule honored, B always sees A's verdict when B sends, and the
// slot ends describing B.
func TestCommandHandler_Process_DisplayPlaylist_PublishesInsidePushLock(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()
	mockExecutor := newRoutableExecutor(ctrl)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockClock.EXPECT().Now().Return(time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()

	sched := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC }, logger)
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, sched, mockJSON, logger)
	active := &sigverify.Active{}
	commandrouter.SetSignatureVerification(handler, commandrouter.SignatureVerificationOptions{Active: active}, logger)

	urlA, urlB := "https://feed.example/a.json", "https://feed.example/b.json"
	valid := sigverify.Verify(signedFixture(t))
	unsigned := sigverify.Verify([]byte(`{"dpVersion":"1.1.0","title":"b","items":[]}`))
	playlistFor := func(id string, v sigverify.Verdict) *dp1.Playlist {
		return &dp1.Playlist{Playlist: dp1playlist.Playlist{
			ID:    id,
			Items: []dp1playlist.PlaylistItem{{ID: "i", Source: "https://example.com/x"}},
		}, Verification: &v}
	}
	mockDP1.EXPECT().ProcessPlaylistURLForCast(ctx, urlA).Return(playlistFor("A", valid), nil).Times(1)
	mockDP1.EXPECT().ProcessPlaylistURLForCast(ctx, urlB).Return(playlistFor("B", unsigned), nil).Times(1)
	mockStatusPoller.EXPECT().ForceRefresh().Times(2)

	bDone := make(chan error, 1)
	var bSawA bool
	sends := 0
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(string, map[string]any) (any, error) {
		sends++
		switch sends {
		case 1: // A's send: kick off B, which must queue behind A's push lock.
			go func() {
				_, err := handler.Process(ctx, displayPlaylistURLCommand(urlB))
				bDone <- err
			}()
			time.Sleep(50 * time.Millisecond)
		case 2: // B's send: A's closure has finished, so A must already be published.
			_, bSawA = active.Lookup("A", urlA)
		}
		return playerOkResponse(), nil
	}).Times(2)

	_, err := handler.Process(ctx, displayPlaylistURLCommand(urlA))
	require.NoError(t, err)
	require.NoError(t, <-bDone)

	assert.True(t, bSawA, "B's send ran after A's push lock released, so A's verdict must already be in the slot")
	st, found := active.Lookup("B", urlB)
	assert.True(t, found)
	assert.Equal(t, sigverify.StatusUnsigned, st)
}
