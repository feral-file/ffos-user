package commandrouter_test

// DP-1 signature verification on the displayPlaylist path
// (feral-file/ffos-user#307), phase 1: verify-and-report. Every cast still
// plays; the verdict rides the reply and the active-verdict slot. The
// verifier itself is covered in sigverify; these cover the handler's wiring:
// which bytes it verifies, where the verdict lands, and when the slot moves.

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"github.com/display-protocol/dp1-go/sign"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/playlistschedule"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
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

// wireVerificationWithMode is wireVerification with the owner's mode fixed.
func wireVerificationWithMode(ts *testSetup, mode sigverify.Mode) *sigverify.Active {
	active := &sigverify.Active{}
	commandrouter.SetSignatureVerification(ts.handler, commandrouter.SignatureVerificationOptions{
		Active: active,
		Mode:   func() sigverify.Mode { return mode },
	}, ts.logger)
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

// TestCommandHandler_Process_DisplayPlaylist_Inline_VerifiesWireTokenNotRemarshal
// pins the ingress contract: when the command arrived over the wire, the
// caller's own dp1_call token is what is decoded and verified — the map is
// never re-marshaled. The document here is signed and dense with `&`: a
// re-marshal would HTML-escape it sixfold past the verifier's size bound
// and report an honest document as invalid.
func TestCommandHandler_Process_DisplayPlaylist_Inline_VerifiesWireTokenNotRemarshal(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerification(ts)

	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	doc := map[string]any{
		"dpVersion": "1.1.0",
		"id":        "amp-1",
		"title":     strings.Repeat("&", sigverify.MaxDocumentBytes/5),
		"items":     []any{map[string]any{"id": "i", "source": "https://example.com/a", "duration": 10, "license": "open"}},
	}
	unsignedBytes, err := json.Marshal(doc) // encoding/json escapes: NOT the wire form
	require.NoError(t, err)
	// Build the wire form by hand: unescaped, as a client would send it.
	// (The escape sequence is assembled from bytes so no tooling can
	// helpfully collapse it into a literal ampersand.)
	jsonAmpEscape := string([]byte{'\\', 'u', '0', '0', '2', '6'})
	require.Contains(t, string(unsignedBytes), jsonAmpEscape, "encoding/json escapes & on marshal")
	wireDoc := []byte(strings.ReplaceAll(string(unsignedBytes), jsonAmpEscape, "&"))
	entry, err := sign.SignMultiEd25519(wireDoc, priv, dp1playlist.RoleFeed, "2026-09-12T00:00:00Z")
	require.NoError(t, err)
	entryJSON, err := json.Marshal(entry)
	require.NoError(t, err)
	signedWire := []byte(strings.TrimSuffix(string(wireDoc), "}") + `,"signatures":[` + string(entryJSON) + `]}`)
	require.Less(t, len(signedWire), sigverify.MaxDocumentBytes)
	require.Equal(t, sigverify.StatusValid, sigverify.Verify(signedWire).Status, "fixture sanity")

	var command commands.Command
	require.NoError(t, json.Unmarshal([]byte(`{"command":"displayPlaylist","request":{"dp1_call":`+string(signedWire)+`}}`), &command))
	// No Marshal expectation: a re-marshal is a test failure. Decode from
	// the wire token, which must be byte-identical to what was sent.
	ts.mockJSON.EXPECT().Unmarshal(gomock.Any(), gomock.Any()).DoAndReturn(func(b []byte, v any) error {
		assert.Equal(t, signedWire, b)
		*(v.(**dp1.Playlist)) = inlineTyped("amp-1")
		return nil
	}).Times(1)
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	require.NoError(t, err)
	assert.Equal(t, "valid", replyMessage(t, result)["signatureStatus"])
	st, ok := active.Lookup("amp-1", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st)
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

// Strict mode (#307 phase 3): the one mode in which a verdict changes what
// plays. Everything not proven valid is refused before the preflight, the
// playback lock, and the scheduler snapshot, with nothing to restore.

// TestCommandHandler_Process_Strict_UnsignedInlineRejected is today's app
// cast under strict: unsigned, so refused — no CDP send, no ForceRefresh,
// the slot untouched, and the playback-failure accounting fired via err.
func TestCommandHandler_Process_Strict_UnsignedInlineRejected(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerificationWithMode(ts, sigverify.ModeStrict)
	active.Set("showing", "", sigverify.StatusValid)

	raw := []byte(`{"dpVersion":"1.1.0","id":"app-1","title":"t","items":[{"id":"i","source":"https://example.com/a","duration":10,"license":"open"}]}`)
	command := inlineCast(ts, raw, inlineTyped("app-1"))
	// No Send, no ForceRefresh expectations: either is an unexpected call.
	failuresBefore := status.PlaybackStartFailures()

	result, err := ts.handler.Process(ts.ctx, command)

	require.Error(t, err)
	assert.True(t, commandrouter.IsSigInvalid(err))
	assert.Equal(t, "sigInvalid: playlist rejected by strict signature verification (unsigned)", err.Error())
	assert.Nil(t, result)
	assert.Equal(t, failuresBefore+1, status.PlaybackStartFailures(), "err must be assigned so the failure accounting fires")
	st, ok := active.Lookup("showing", "")
	assert.True(t, ok, "the previous artwork keeps playing and keeps its verdict")
	assert.Equal(t, sigverify.StatusValid, st)
}

// TestCommandHandler_Process_Strict_InvalidURLRejectedSanitized: a tampered
// document by URL is refused with the role and reason vocabulary only.
func TestCommandHandler_Process_Strict_InvalidURLRejectedSanitized(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	wireVerificationWithMode(ts, sigverify.ModeStrict)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(signedFixture(t), &doc))
	doc["title"] = "tampered"
	tampered, err := json.Marshal(doc)
	require.NoError(t, err)
	verdict := sigverify.Verify(tampered)
	require.Equal(t, sigverify.StatusInvalid, verdict.Status)
	playlistURL := "https://feed.example/secret-token-abc/p.json"
	ts.mockDP1.EXPECT().ProcessPlaylistURLForCast(ts.ctx, playlistURL).Return(&dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "pl-tampered",
		Items: []dp1playlist.PlaylistItem{{ID: "i", Source: "https://example.com/a"}},
	}, Verification: &verdict}, nil).Times(1)

	_, err = ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSigInvalid(err))
	assert.Equal(t, "sigInvalid: playlist rejected by strict signature verification (signature invalid: payload_hash mismatch)", err.Error())
	assert.NotContains(t, err.Error(), "secret-token")
	assert.NotContains(t, err.Error(), "did:key")
	assert.NotContains(t, err.Error(), "feed", "the document's own role string never reaches a caller")
}

// TestCommandHandler_Process_Strict_ValidPlays: strict only bites on
// non-valid verdicts — a signed feed playlist plays and is reported as usual.
func TestCommandHandler_Process_Strict_ValidPlays(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerificationWithMode(ts, sigverify.ModeStrict)

	verdict := sigverify.Verify(signedFixture(t))
	playlistURL := "https://feed.example/p.json"
	expectDisplayPlaylistSuccess(ts, playlistURL, &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "pl-ok",
		Items: []dp1playlist.PlaylistItem{{ID: "i", Source: "https://example.com/a"}},
	}, Verification: &verdict})

	result, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.NoError(t, err)
	assert.Equal(t, "valid", replyMessage(t, result)["signatureStatus"])
	_, ok := active.Lookup("pl-ok", playlistURL)
	assert.True(t, ok)
}

// TestCommandHandler_Process_Strict_CachedCopyRejected: the offline cached
// copy carries no verdict, and a strict device does not guess.
func TestCommandHandler_Process_Strict_CachedCopyRejected(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	mockOfflineCache := mocks.NewMockOfflineCacheService(ts.ctrl)
	ts.handler = commandrouter.New(ts.mockExecutor, ts.mockCDP, ts.mockDP1, ts.mockStatusPoller, nil, mockOfflineCache, nil, nil, ts.mockJSON, ts.logger)
	wireVerificationWithMode(ts, sigverify.ModeStrict)

	playlistURL := "https://feed.example/p.json"
	cachedRaw := signedFixture(t)
	ts.mockDP1.EXPECT().ProcessPlaylistURLForCast(ts.ctx, playlistURL).Return(nil, errors.New("network unreachable")).Times(1)
	mockOfflineCache.EXPECT().CachedPlaylistForURL(playlistURL).Return(json.RawMessage(cachedRaw), nil).Times(1)
	ts.mockJSON.EXPECT().Unmarshal(cachedRaw, gomock.Any()).DoAndReturn(func(_ []byte, v any) error {
		*(v.(**dp1.Playlist)) = inlineTyped("cached-1")
		return nil
	}).Times(1)

	_, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSigInvalid(err))
	assert.Contains(t, err.Error(), "cached copy carries no verdict")
}

// TestCommandHandler_Process_Notify_InvalidStillPlays pins that only strict
// rejects: under notify the tampered document plays and is reported.
func TestCommandHandler_Process_Notify_InvalidStillPlays(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	wireVerificationWithMode(ts, sigverify.ModeNotify)

	var doc map[string]any
	require.NoError(t, json.Unmarshal(signedFixture(t), &doc))
	doc["title"] = "tampered"
	tampered, err := json.Marshal(doc)
	require.NoError(t, err)
	command := inlineCast(ts, tampered, inlineTyped("pl-tampered"))
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	result, err := ts.handler.Process(ts.ctx, command)

	require.NoError(t, err)
	assert.Equal(t, "invalid", replyMessage(t, result)["signatureStatus"])
}

// TestCommandHandler_Process_Strict_ScheduledRejectedBeforeSnapshot: a
// displayAt cast is judged before the scheduler is touched, so a rejected
// schedule never arms a timer or replaces the cache.
func TestCommandHandler_Process_Strict_ScheduledRejectedBeforeSnapshot(t *testing.T) {
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
	mockClock.EXPECT().Now().Return(time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() },
	).AnyTimes()

	sched := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC }, logger)
	defer sched.Stop()
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, sched, mockJSON, logger)
	commandrouter.SetSignatureVerification(handler, commandrouter.SignatureVerificationOptions{
		Active: &sigverify.Active{},
		Mode:   func() sigverify.Mode { return sigverify.ModeStrict },
	}, logger)

	unsigned := sigverify.Verify([]byte(`{"dpVersion":"1.1.0","title":"t","items":[]}`))
	playlistURL := "https://example.com/future.json"
	mockDP1.EXPECT().ProcessPlaylistURLForCast(ctx, playlistURL).Return(&dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "future-1",
		Items: []dp1playlist.PlaylistItem{{ID: "future", Source: "https://example.com/future.html", DisplayAt: strPtr("2026-09-14T00:00:00Z")}},
	}, Verification: &unsigned}, nil)

	_, err := handler.Process(ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.True(t, commandrouter.IsSigInvalid(err))
	assert.False(t, sched.HasCache(), "a rejected schedule must not reach the scheduler")
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
	sched.SetPushObserver(func(p playlistschedule.PushPhase) {
		if p == playlistschedule.PushAccepted {
			active.Promote()
		}
	})

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

// TestCommandHandler_Process_ScheduledCast_SurvivesReconnectRepush: a
// displayAt cast whose active cohort is non-empty is displayed at once AND
// cached by the scheduler. After a player reload the reconnect reconciler
// clears current and the scheduler re-pushes the same schedule; its
// promotion must restore this cast's verdict — a static inline schedule
// has no refresher pass that would restage it.
func TestCommandHandler_Process_ScheduledCast_SurvivesReconnectRepush(t *testing.T) {
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
	defer sched.Stop()
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, sched, mockJSON, logger)
	active := &sigverify.Active{}
	commandrouter.SetSignatureVerification(handler, commandrouter.SignatureVerificationOptions{Active: active}, logger)
	sched.SetPushObserver(func(p playlistschedule.PushPhase) {
		switch p {
		case playlistschedule.PushStarting:
			active.ClearCurrent()
		case playlistschedule.PushAccepted:
			active.Promote()
		}
	})

	// Static inline schedule: one cohort active now, one tomorrow.
	raw := []byte(`{"dpVersion":"1.1.0","id":"sched-1","title":"t","items":[]}`)
	typed := &dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID: "sched-1",
		Items: []dp1playlist.PlaylistItem{
			{ID: "now", Source: "https://example.com/now", DisplayAt: strPtr("2026-09-10T00:00:00Z")},
			{ID: "later", Source: "https://example.com/later", DisplayAt: strPtr("2026-09-13T00:00:00Z")},
		},
	}}
	playlistMap := map[string]any{"marker": "inline"}
	mockJSON.EXPECT().Marshal(playlistMap).Return(raw, nil).Times(1)
	mockJSON.EXPECT().Unmarshal(raw, gomock.Any()).DoAndReturn(func(_ []byte, v any) error {
		*(v.(**dp1.Playlist)) = typed
		return nil
	}).Times(1)
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	_, err := handler.Process(ctx, commands.Command{Type: commands.CMD_DISPLAY_PLAYLIST, Arguments: map[string]any{"dp1_call": playlistMap}})
	require.NoError(t, err)
	st, ok := active.Lookup("sched-1", "")
	require.True(t, ok)
	assert.Equal(t, sigverify.StatusUnsigned, st)

	// Player reloads: the reconnect reconciler clears current, then the
	// scheduler's recompute re-pushes the cached schedule (observer → Promote).
	active.ClearCurrent()
	_, ok = active.Lookup("sched-1", "")
	require.False(t, ok)
	mockCDP.EXPECT().Initialized().Return(true).AnyTimes()
	mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).Return(playerOkResponse(), nil).Times(1)
	sched.RecomputeNow(ctx)

	st, ok = active.Lookup("sched-1", "")
	assert.True(t, ok, "the re-pushed schedule restores its own verdict")
	assert.Equal(t, sigverify.StatusUnsigned, st)
}

// TestCommandHandler_Process_DisplayPlaylist_InvalidatesBeforeSend pins the
// pre-send invalidation: at the moment the CDP send runs — when the player
// may already be showing the new document — the previous document's slot
// (same URL) must already be gone, so a concurrent status round can only
// omit, never re-attest the old verdict for the new document.
func TestCommandHandler_Process_DisplayPlaylist_InvalidatesBeforeSend(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()
	active := wireVerification(ts)

	playlistURL := "https://feed.example/p.json"
	active.Set("", playlistURL, sigverify.StatusValid) // the previous document, URL-only identity
	unsigned := sigverify.Verify([]byte(`{"dpVersion":"1.1.0","title":"t","items":[]}`))
	ts.mockDP1.EXPECT().ProcessPlaylistURLForCast(ts.ctx, playlistURL).Return(&dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "",
		Items: []dp1playlist.PlaylistItem{{ID: "i", Source: "https://example.com/a"}},
	}, Verification: &unsigned}, nil).Times(1)
	var atSend bool
	ts.mockCDP.EXPECT().Send(cdp.METHOD_EVALUATE, gomock.Any()).DoAndReturn(func(string, map[string]any) (any, error) {
		_, atSend = active.Lookup("", playlistURL)
		return playerOkResponse(), nil
	}).Times(1)
	ts.mockStatusPoller.EXPECT().ForceRefresh().Times(1)

	_, err := ts.handler.Process(ts.ctx, displayPlaylistURLCommand(playlistURL))

	require.NoError(t, err)
	assert.False(t, atSend, "the previous verdict must be gone by the time the send runs")
	st, ok := active.Lookup("", playlistURL)
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusUnsigned, st, "and the new one published after acceptance")
}

// TestCommandHandler_Process_DisplayPlaylist_PublishesInsidePushLock pins the
// ordering rule: the slot is written inside the same WithPlayerPush critical
// section as the send. Cast A's player reply starts cast B concurrently; B
// can only send once A's closure has released the push lock. Two things
// must then hold: at B's send the slot carries NO attestation (B's own
// pre-send invalidation cleared A's — the window in which the player may
// already show B is an omission), and the slot ends describing B. If A's
// publication ran outside the lock it could land after B's and leave the
// slot describing A.
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
	var bSawAttestation bool
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
		case 2: // B's send: A's closure has finished and B invalidated before sending.
			_, bSawAttestation = active.Lookup("A", urlA)
		}
		return playerOkResponse(), nil
	}).Times(2)

	_, err := handler.Process(ctx, displayPlaylistURLCommand(urlA))
	require.NoError(t, err)
	require.NoError(t, <-bDone)

	assert.False(t, bSawAttestation, "at B's send the slot must carry no attestation: B invalidated before sending")
	st, found := active.Lookup("B", urlB)
	assert.True(t, found)
	assert.Equal(t, sigverify.StatusUnsigned, st)
}

// TestStrictPushGate is the scheduler-side half of strict: the mode is read
// when the gate is asked, so a schedule accepted under notify is refused at
// its cutover once the owner has switched to strict, and allowed again when
// they switch back. A verdict-less document is refused under strict.
func TestStrictPushGate(t *testing.T) {
	mode := sigverify.ModeNotify
	gate := commandrouter.StrictPushGate(func() sigverify.Mode { return mode })
	unsigned := &dp1.Playlist{Verification: &sigverify.Verdict{Status: sigverify.StatusUnsigned}}
	valid := &dp1.Playlist{Verification: &sigverify.Verdict{Status: sigverify.StatusValid}}
	tampered := &dp1.Playlist{Verification: &sigverify.Verdict{
		Status:  sigverify.StatusInvalid,
		Reason:  "https://evil.example/role signature invalid: payload_hash mismatch",
		Signers: []sigverify.Signer{{Role: "https://evil.example/role", Reason: sigverify.ReasonPayloadHashMismatch}},
	}}
	noVerdict := &dp1.Playlist{}

	assert.NoError(t, gate(unsigned), "notify never refuses")
	assert.NoError(t, gate(noVerdict))

	mode = sigverify.ModeStrict
	err := gate(unsigned)
	require.Error(t, err)
	assert.Equal(t, "strict signature verification: unsigned", err.Error())
	assert.NoError(t, gate(valid))
	err = gate(tampered)
	require.Error(t, err)
	assert.Equal(t, "strict signature verification: signature invalid: payload_hash mismatch", err.Error())
	assert.NotContains(t, err.Error(), "evil")
	err = gate(noVerdict)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no verdict")
	assert.Error(t, gate(nil))

	mode = sigverify.ModeSilent
	assert.NoError(t, gate(unsigned))
	assert.NoError(t, commandrouter.StrictPushGate(nil)(unsigned), "unwired mode reader never refuses")
}

// TestCommandHandler_Process_DisplayPlaylist_RefusedWhenResetStagesDuringResolution:
// a cast admitted before a factory reset staged must abort inside the
// push-lock section (where the reset narration write is serialized) rather
// than repaint over the reset screen (feral-file/ffos-user#307 review round 7).
func TestCommandHandler_Process_DisplayPlaylist_RefusedWhenResetStagesDuringResolution(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockExecutor := mocks.NewMockExecutor(ctrl)
	// Admitted at accept time (first check false), then the reset stages
	// before the push-lock section (every later check true).
	gomock.InOrder(
		mockExecutor.EXPECT().ResetStaged().Return(false),
		mockExecutor.EXPECT().ResetStaged().Return(true).AnyTimes(),
	)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockClock.EXPECT().Now().Return(time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() }).AnyTimes()

	sched := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC }, logger)
	defer sched.Stop()
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, sched, mockJSON, logger)

	playlistURL := "https://feed.example/p.json"
	mockDP1.EXPECT().ProcessPlaylistURLForCast(ctx, playlistURL).Return(&dp1.Playlist{Playlist: dp1playlist.Playlist{
		ID:    "pl-inflight",
		Items: []dp1playlist.PlaylistItem{{ID: "i", Source: "https://example.com/a"}},
	}}, nil).Times(1)
	// No CDP Send: the push-lock recheck must abort before any write.

	_, err := handler.Process(ctx, displayPlaylistURLCommand(playlistURL))

	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory reset in progress")
	assert.False(t, sched.HasCache(), "a reset-aborted cast must not commit scheduler state")
}

// TestCommandHandler_Process_DisplayDefaultPlaylist_RefusedWhenResetStagesFirst:
// the default-playlist path shares the same push lock, so a default cast
// admitted before a reset staged must also drop inside the lock (#307).
func TestCommandHandler_Process_DisplayDefaultPlaylist_RefusedWhenResetStagesFirst(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockExecutor := mocks.NewMockExecutor(ctrl)
	gomock.InOrder(
		mockExecutor.EXPECT().ResetStaged().Return(false),
		mockExecutor.EXPECT().ResetStaged().Return(true).AnyTimes(),
	)
	mockCDP := mocks.NewMockCDP(ctrl)
	mockDP1 := mocks.NewMockDP1(ctrl)
	mockStatusPoller := mocks.NewMockStatusPoller(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockClock := mocks.NewMockClock(ctrl)
	mockClock.EXPECT().Now().Return(time.Date(2026, 9, 12, 12, 0, 0, 0, time.UTC)).AnyTimes()
	mockClock.EXPECT().SleepContext(gomock.Any(), gomock.Any()).DoAndReturn(
		func(c context.Context, _ time.Duration) error { <-c.Done(); return c.Err() }).AnyTimes()

	sched := playlistschedule.New(ctx, mockCDP, mockClock, func() *time.Location { return time.UTC }, logger)
	defer sched.Stop()
	handler := commandrouter.New(mockExecutor, mockCDP, mockDP1, mockStatusPoller, nil, nil, nil, sched, mockJSON, logger)
	// No CDP Send: the push-lock recheck must abort before any write.

	_, err := handler.Process(ctx, commands.Command{Type: commands.CMD_DISPLAY_DEFAULT_PLAYLIST, Arguments: map[string]any{}})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "factory reset in progress")
}
