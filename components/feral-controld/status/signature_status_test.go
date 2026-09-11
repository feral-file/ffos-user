package status

// player_status's controld-owned signatureStatus annotation
// (feral-file/ffos-user#307): filled from the verification lookup when the
// reply's playlist id or URL matches, omitted otherwise, and stable across
// polls so it never defeats the notification dedupe.

import (
	"context"
	"testing"

	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

func signatureTestPoller(reply map[string]any, ws *fakeWS) *poller {
	return &poller{
		cdp: &fakeCDP{
			pageNavigationURL: constants.WEBAPP_URL,
			noLogSendResult:   map[string]any{"message": reply},
		},
		relayer:                 &fakeRelayer{connectedResponses: []bool{true}},
		ws:                      ws,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}
}

func sentPlayerStatus(t *testing.T, ws *fakeWS) *PlayerStatus {
	t.Helper()
	payload, ok := ws.lastPayload.(map[string]any)
	if !ok {
		t.Fatalf("expected websocket payload map, got %T", ws.lastPayload)
	}
	message, ok := payload["message"].(*PlayerStatus)
	if !ok {
		t.Fatalf("expected payload message to be *PlayerStatus, got %T", payload["message"])
	}
	return message
}

func TestPollPlayerStatus_AnnotatesSignatureStatusByPlaylistID(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
		"playlist":    map[string]any{"id": "pl-1", "items": []any{}},
	}, ws)
	var askedID, askedURL string
	p.SetVerificationLookup(func(id, url string) (string, bool) {
		askedID, askedURL = id, url
		if id == "pl-1" {
			return "invalid", true
		}
		return "", false
	})

	p.pollPlayerStatus(context.Background())

	if askedID != "pl-1" || askedURL != "" {
		t.Fatalf("lookup asked with id=%q url=%q", askedID, askedURL)
	}
	got := sentPlayerStatus(t, ws)
	if got.SignatureStatus == nil || *got.SignatureStatus != "invalid" {
		t.Fatalf("expected signatureStatus invalid, got %v", got.SignatureStatus)
	}
}

func TestPollPlayerStatus_AnnotatesSignatureStatusByURL(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
		"playlistURL": "https://feed.example/p.json",
	}, ws)
	p.SetVerificationLookup(func(id, url string) (string, bool) {
		if url == "https://feed.example/p.json" {
			return "valid", true
		}
		return "", false
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.SignatureStatus == nil || *got.SignatureStatus != "valid" {
		t.Fatalf("expected signatureStatus valid, got %v", got.SignatureStatus)
	}
}

// TestPollPlayerStatus_OmitsSignatureStatusOnLookupMiss: the player-fetched
// default playlist, or anything controld did not verify, carries no field —
// never a guessed one.
func TestPollPlayerStatus_OmitsSignatureStatusOnLookupMiss(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayDefaultPlaylist",
		"index":       0,
		"playlist":    map[string]any{"id": "default", "items": []any{}},
	}, ws)
	p.SetVerificationLookup(func(string, string) (string, bool) { return "", false })

	p.pollPlayerStatus(context.Background())

	if got := sentPlayerStatus(t, ws); got.SignatureStatus != nil {
		t.Fatalf("expected no signatureStatus on a miss, got %q", *got.SignatureStatus)
	}
}

func TestPollPlayerStatus_NoLookupWired_OmitsSignatureStatus(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":       true,
		"index":    0,
		"playlist": map[string]any{"id": "pl-1", "items": []any{}},
	}, ws)

	p.pollPlayerStatus(context.Background())

	if got := sentPlayerStatus(t, ws); got.SignatureStatus != nil {
		t.Fatalf("expected no signatureStatus without a lookup, got %q", *got.SignatureStatus)
	}
}

// TestPollPlayerStatus_SignatureStatusDoesNotDefeatDedupe: a stable verdict
// on an unchanged reply must hash identically, so the second poll sends
// nothing — the annotation must never turn every poll into a notification.
func TestPollPlayerStatus_SignatureStatusDoesNotDefeatDedupe(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":       true,
		"index":    0,
		"playlist": map[string]any{"id": "pl-1", "items": []any{}},
	}, ws)
	p.SetVerificationLookup(func(string, string) (string, bool) { return "unsigned", true })

	p.pollPlayerStatus(context.Background())
	p.pollPlayerStatus(context.Background())

	if ws.sendAllCalls != 1 {
		t.Fatalf("expected one websocket send across two identical polls, got %d", ws.sendAllCalls)
	}
}
