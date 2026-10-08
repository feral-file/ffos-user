package status

// player_status's controld-owned mintPairing annotation (issue #381): the
// browser-pairing mint overlay is a CDP evaluation painted over the player
// page, not a navigation, so nothing else on this reply reflects it. Filled
// from the mint pairing source when wired and something is showing, omitted
// otherwise, and stable across polls so it never defeats the notification
// dedupe — same contract as SignatureStatus (signature_status_test.go),
// reusing its test harness.

import (
	"context"
	"testing"
	"time"
)

func TestPollPlayerStatus_AttachesMintPairingOverlay(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	expiresAt := time.Unix(1700000300, 0).UTC()
	p.SetMintPairingOverlaySource(func() *MintPairingOverlay {
		return &MintPairingOverlay{
			State:       "pairing_code",
			ChannelID:   "ch_abc123",
			PairingCode: "436729",
			ExpiresAt:   &expiresAt,
		}
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.MintPairing == nil {
		t.Fatal("expected mintPairing to be attached")
	}
	if got.MintPairing.State != "pairing_code" || got.MintPairing.ChannelID != "ch_abc123" || got.MintPairing.PairingCode != "436729" {
		t.Fatalf("unexpected mintPairing: %+v", got.MintPairing)
	}
	if got.MintPairing.ExpiresAt == nil || !got.MintPairing.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expected expiresAt %v, got %v", expiresAt, got.MintPairing.ExpiresAt)
	}
}

// TestPollPlayerStatus_AttachesMintPairingOverlay_CreatingToken: the third
// named wire state (status.go's MintPairingOverlay.State doc) carries only
// State/ChannelID, same as request_received — no code is on screen once
// minting has started. This layer is state-agnostic (it forwards whatever
// the source returns), but round-4 pass-3 review found creating_token
// untested on every surface; this pins it on this one too.
func TestPollPlayerStatus_AttachesMintPairingOverlay_CreatingToken(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	p.SetMintPairingOverlaySource(func() *MintPairingOverlay {
		return &MintPairingOverlay{State: "creating_token", ChannelID: "ch_abc123"}
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.MintPairing == nil {
		t.Fatal("expected mintPairing to be attached")
	}
	if got.MintPairing.State != "creating_token" || got.MintPairing.ChannelID != "ch_abc123" {
		t.Fatalf("unexpected mintPairing: %+v", got.MintPairing)
	}
	if got.MintPairing.PairingCode != "" || got.MintPairing.ExpiresAt != nil {
		t.Fatalf("expected no code left to report, got %+v", got.MintPairing)
	}
}

// TestPollPlayerStatus_OmitsMintPairingWhenNothingShowing: a wired source
// that reports nothing showing (nil) must omit the field, the same as an
// unwired one — this is how the real seam in main.go reports "not showing".
func TestPollPlayerStatus_OmitsMintPairingWhenNothingShowing(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	p.SetMintPairingOverlaySource(func() *MintPairingOverlay { return nil })

	p.pollPlayerStatus(context.Background())

	if got := sentPlayerStatus(t, ws); got.MintPairing != nil {
		t.Fatalf("expected no mintPairing when nothing is showing, got %+v", got.MintPairing)
	}
}

func TestPollPlayerStatus_NoMintPairingSourceWired_OmitsMintPairing(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)

	p.pollPlayerStatus(context.Background())

	if got := sentPlayerStatus(t, ws); got.MintPairing != nil {
		t.Fatalf("expected no mintPairing without a source, got %+v", got.MintPairing)
	}
}

// TestPollPlayerStatus_DropsPlayerSuppliedMintPairing: the field is
// controld-owned. A value the player put in its reply must not survive, on a
// nil source or when no source is wired.
func TestPollPlayerStatus_DropsPlayerSuppliedMintPairing(t *testing.T) {
	for _, wired := range []bool{false, true} {
		ws := &fakeWS{}
		p := signatureTestPoller(map[string]any{
			"ok":          true,
			"index":       0,
			"mintPairing": map[string]any{"state": "pairing_code", "pairingCode": "SPOOFED"},
		}, ws)
		if wired {
			p.SetMintPairingOverlaySource(func() *MintPairingOverlay { return nil })
		}

		p.pollPlayerStatus(context.Background())

		if got := sentPlayerStatus(t, ws); got.MintPairing != nil {
			t.Fatalf("wired=%v: player-supplied mintPairing leaked: %+v", wired, got.MintPairing)
		}
	}
}

// TestPollPlayerStatus_MintPairingDoesNotDefeatDedupe: a stable overlay on an
// unchanged reply must hash identically, so the second poll sends nothing —
// the annotation must never turn every poll into a notification.
func TestPollPlayerStatus_MintPairingDoesNotDefeatDedupe(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":    true,
		"index": 0,
	}, ws)
	p.SetMintPairingOverlaySource(func() *MintPairingOverlay {
		return &MintPairingOverlay{State: "request_received", ChannelID: "ch_abc123"}
	})

	p.pollPlayerStatus(context.Background())
	p.pollPlayerStatus(context.Background())

	if ws.sendAllCalls != 1 {
		t.Fatalf("expected one websocket send across two identical polls, got %d", ws.sendAllCalls)
	}
}
