package status

// player_status's controld-owned overlay annotation (issue #381): whatever
// overlay feral-controld's shared overlay.Controller currently has on the
// player — mint pairing's or setupui's — is a CDP evaluation painted over
// the player page, not a navigation, so nothing else on this reply reflects
// it. Filled from the overlay source when wired and something is showing,
// omitted otherwise, and stable across polls so it never defeats the
// notification dedupe — same contract as SignatureStatus
// (signature_status_test.go), reusing its test harness.

import (
	"context"
	"testing"
	"time"
)

func TestPollPlayerStatus_AttachesMintOverlay(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	expiresAt := time.Unix(1700000300, 0).UTC()
	p.SetOverlaySource(func() *Overlay {
		return &Overlay{
			Owner:       OverlayOwnerMint,
			State:       "pairing_code",
			ChannelID:   "ch_abc123",
			PairingCode: "436729",
			ExpiresAt:   &expiresAt,
		}
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.Overlay == nil {
		t.Fatal("expected overlay to be attached")
	}
	if got.Overlay.Owner != OverlayOwnerMint || got.Overlay.State != "pairing_code" ||
		got.Overlay.ChannelID != "ch_abc123" || got.Overlay.PairingCode != "436729" {
		t.Fatalf("unexpected overlay: %+v", got.Overlay)
	}
	if got.Overlay.ExpiresAt == nil || !got.Overlay.ExpiresAt.Equal(expiresAt) {
		t.Fatalf("expected expiresAt %v, got %v", expiresAt, got.Overlay.ExpiresAt)
	}
}

// TestPollPlayerStatus_AttachesMintOverlay_CreatingToken: the third named
// mint wire state carries only Owner/State/ChannelID, same as
// request_received — no code is on screen once minting has started. This
// layer is state-agnostic (it forwards whatever the source returns), but
// round-4 pass-3 review found creating_token untested on every surface;
// this pins it on this one too.
func TestPollPlayerStatus_AttachesMintOverlay_CreatingToken(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	p.SetOverlaySource(func() *Overlay {
		return &Overlay{Owner: OverlayOwnerMint, State: "creating_token", ChannelID: "ch_abc123"}
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.Overlay == nil {
		t.Fatal("expected overlay to be attached")
	}
	if got.Overlay.State != "creating_token" || got.Overlay.ChannelID != "ch_abc123" {
		t.Fatalf("unexpected overlay: %+v", got.Overlay)
	}
	if got.Overlay.PairingCode != "" || got.Overlay.ExpiresAt != nil {
		t.Fatalf("expected no code left to report, got %+v", got.Overlay)
	}
}

// TestPollPlayerStatus_AttachesSetupOverlay: the setup/claim-QR sibling —
// Owner "setup", no ChannelID/PairingCode/ExpiresAt (those are
// mint-pairing-specific).
func TestPollPlayerStatus_AttachesSetupOverlay(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	p.SetOverlaySource(func() *Overlay {
		return &Overlay{Owner: OverlayOwnerSetup, State: "claim_qr"}
	})

	p.pollPlayerStatus(context.Background())

	got := sentPlayerStatus(t, ws)
	if got.Overlay == nil {
		t.Fatal("expected overlay to be attached")
	}
	if got.Overlay.Owner != OverlayOwnerSetup || got.Overlay.State != "claim_qr" {
		t.Fatalf("unexpected overlay: %+v", got.Overlay)
	}
	if got.Overlay.ChannelID != "" || got.Overlay.PairingCode != "" || got.Overlay.ExpiresAt != nil {
		t.Fatalf("setup overlay must carry no mint-specific fields, got %+v", got.Overlay)
	}
}

// TestPollPlayerStatus_OmitsOverlayWhenNothingShowing: a wired source that
// reports nothing showing (nil) must omit the field, the same as an unwired
// one — this is how the real seam in main.go reports "not showing".
func TestPollPlayerStatus_OmitsOverlayWhenNothingShowing(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)
	p.SetOverlaySource(func() *Overlay { return nil })

	p.pollPlayerStatus(context.Background())

	if got := sentPlayerStatus(t, ws); got.Overlay != nil {
		t.Fatalf("expected no overlay when nothing is showing, got %+v", got.Overlay)
	}
}

func TestPollPlayerStatus_NoOverlaySourceWired_OmitsOverlay(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":          true,
		"castCommand": "displayPlaylist",
		"index":       0,
	}, ws)

	p.pollPlayerStatus(context.Background())

	if got := sentPlayerStatus(t, ws); got.Overlay != nil {
		t.Fatalf("expected no overlay without a source, got %+v", got.Overlay)
	}
}

// TestPollPlayerStatus_DropsPlayerSuppliedOverlay: the field is
// controld-owned. A value the player put in its reply must not survive, on
// a nil source or when no source is wired.
func TestPollPlayerStatus_DropsPlayerSuppliedOverlay(t *testing.T) {
	for _, wired := range []bool{false, true} {
		ws := &fakeWS{}
		p := signatureTestPoller(map[string]any{
			"ok":      true,
			"index":   0,
			"overlay": map[string]any{"owner": "mint", "state": "pairing_code", "pairingCode": "SPOOFED"},
		}, ws)
		if wired {
			p.SetOverlaySource(func() *Overlay { return nil })
		}

		p.pollPlayerStatus(context.Background())

		if got := sentPlayerStatus(t, ws); got.Overlay != nil {
			t.Fatalf("wired=%v: player-supplied overlay leaked: %+v", wired, got.Overlay)
		}
	}
}

// TestPollPlayerStatus_OverlayDoesNotDefeatDedupe: a stable overlay on an
// unchanged reply must hash identically, so the second poll sends nothing —
// the annotation must never turn every poll into a notification.
func TestPollPlayerStatus_OverlayDoesNotDefeatDedupe(t *testing.T) {
	ws := &fakeWS{}
	p := signatureTestPoller(map[string]any{
		"ok":    true,
		"index": 0,
	}, ws)
	p.SetOverlaySource(func() *Overlay {
		return &Overlay{Owner: OverlayOwnerMint, State: "request_received", ChannelID: "ch_abc123"}
	})

	p.pollPlayerStatus(context.Background())
	p.pollPlayerStatus(context.Background())

	if ws.sendAllCalls != 1 {
		t.Fatalf("expected one websocket send across two identical polls, got %d", ws.sendAllCalls)
	}
}

// TestCombineOverlaySources pins the precedence, the setup wrapping, and the
// nil fallback of the one closure BOTH status surfaces are wired with. Before
// this existed the three-branch decision was a closure inside initializeApp,
// reachable from no test at all: reversing the precedence, dropping a source,
// or breaking the nil fallback shipped green (round 2 review, F2) — which is
// exactly the class of bug issue #381 is about.
func TestCombineOverlaySources(t *testing.T) {
	mintShowing := &Overlay{Owner: OverlayOwnerMint, State: "pairing_code", ChannelID: "ch-1", PairingCode: "ABCD"}
	mint := func(o *Overlay) func() *Overlay { return func() *Overlay { return o } }
	setup := func(state string, ok bool) func() (string, bool) {
		return func() (string, bool) { return state, ok }
	}

	tests := []struct {
		name  string
		mint  func() *Overlay
		setup func() (string, bool)
		want  *Overlay
	}{{
		name:  "nothing showing on either source is no overlay at all",
		mint:  mint(nil),
		setup: setup("", false),
		want:  nil,
	}, {
		name:  "mint's own overlay is reported with its richer fields intact",
		mint:  mint(mintShowing),
		setup: setup("", false),
		want:  mintShowing,
	}, {
		name:  "setupui's narration state is reported as owner setup",
		mint:  mint(nil),
		setup: setup("claim_qr", true),
		want:  &Overlay{Owner: OverlayOwnerSetup, State: "claim_qr"},
	}, {
		name:  "any setupui state is reported, not a curated subset",
		mint:  mint(nil),
		setup: setup("finalizing", true),
		want:  &Overlay{Owner: OverlayOwnerSetup, State: "finalizing"},
	}, {
		// The shared overlay.Controller makes this unreachable in production
		// (one owner holds the screen); pinned so the ORDER cannot be
		// reversed silently if that ever stops holding.
		name:  "mint wins when both report, and carries no setup state",
		mint:  mint(mintShowing),
		setup: setup("claim_qr", true),
		want:  mintShowing,
	}, {
		name:  "a true setupui answer with an empty state still reports owner setup",
		mint:  mint(nil),
		setup: setup("", true),
		want:  &Overlay{Owner: OverlayOwnerSetup, State: ""},
	}, {
		name:  "an unwired mint probe falls through to setupui",
		mint:  nil,
		setup: setup("scanning", true),
		want:  &Overlay{Owner: OverlayOwnerSetup, State: "scanning"},
	}, {
		name:  "an unwired setupui probe reports mint alone",
		mint:  mint(mintShowing),
		setup: nil,
		want:  mintShowing,
	}, {
		name:  "two unwired probes are no overlay, never a panic",
		mint:  nil,
		setup: nil,
		want:  nil,
	}}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CombineOverlaySources(tt.mint, tt.setup)()
			if tt.want == nil {
				if got != nil {
					t.Fatalf("expected no overlay, got %+v", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected overlay %+v, got nil", tt.want)
			}
			if *got != *tt.want {
				t.Fatalf("expected overlay %+v, got %+v", *tt.want, *got)
			}
		})
	}
}
