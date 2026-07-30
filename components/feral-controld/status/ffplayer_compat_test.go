package status

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"github.com/feral-file/ffos-user/components/feral-controld/ffplayerfixtures"
)

// decodeFFPlayerFixture simulates the cdp client's decode of a
// Runtime.evaluate result that is itself a JSON string — see
// ffplayerfixtures' package doc.
func decodeFFPlayerFixture(t *testing.T, raw string) map[string]interface{} {
	t.Helper()
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture is not valid JSON: %v\nraw: %s", err, raw)
	}
	return decoded
}

// TestFFPlayerCompat_StampTransport drives the LITERAL checkStatus replies
// the shipped ff-player produces (ffplayerfixtures.CheckStatusReply*)
// through the real FetchPlayerStatus decode AND the stamp-observer plumbing
// pollPlayerStatus drives — the exact boundary
// playersession.Session.ObserveStatusStamp depends on to distinguish an old
// player (source unavailable, never bump) from a new player's present-but-
// possibly-empty stamp (docs/player-session-recovery.md §2.1's three-way
// contract).
func TestFFPlayerCompat_StampTransport(t *testing.T) {
	cases := []struct {
		name         string
		raw          string
		wantStampNil bool
		wantStamp    string
		wantPresent  bool
	}{
		{
			name:         "present, empty (fresh/unstamped document)",
			raw:          ffplayerfixtures.CheckStatusReplyStampPresentEmpty,
			wantStampNil: false,
			wantStamp:    "",
			wantPresent:  true,
		},
		{
			name:         "present, real value",
			raw:          ffplayerfixtures.CheckStatusReplyStampPresentValue,
			wantStampNil: false,
			wantStamp:    "42-1a2b3c",
			wantPresent:  true,
		},
		{
			name:         "absent (old player, predates the stamp contract)",
			raw:          ffplayerfixtures.CheckStatusReplyStampAbsent,
			wantStampNil: true,
			wantPresent:  false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mockCDP := &fakeCDP{
				pageNavigationURL: constants.WEBAPP_URL,
				noLogSendResult:   decodeFFPlayerFixture(t, tc.raw),
			}

			// --- FetchPlayerStatus: the Stamp *string field itself -----
			p := &poller{
				cdp:    mockCDP,
				json:   wrapper.NewJSON(),
				logger: zap.NewNop(),
			}
			ps, err := p.FetchPlayerStatus(context.Background())
			if err != nil {
				t.Fatalf("FetchPlayerStatus: %v", err)
			}
			if tc.wantStampNil {
				if ps.Stamp != nil {
					t.Fatalf("expected Stamp to be nil (absent), got %q", *ps.Stamp)
				}
			} else {
				if ps.Stamp == nil {
					t.Fatalf("expected Stamp to be present, got nil")
				}
				if *ps.Stamp != tc.wantStamp {
					t.Fatalf("Stamp = %q, want %q", *ps.Stamp, tc.wantStamp)
				}
			}

			// --- pollPlayerStatus: the stampObserver plumbing -----------
			mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
			mockWS := &fakeWS{}
			var observedStamp string
			var observedPresent bool
			var observedCalls int
			p2 := &poller{
				cdp:                     mockCDP,
				relayer:                 mockRelayer,
				ws:                      mockWS,
				json:                    wrapper.NewJSON(),
				logger:                  zap.NewNop(),
				lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
				lastWSStatusHashes:      make(map[relayer.NotificationType]string),
				stampObserver: func(stamp string, present bool) {
					observedCalls++
					observedStamp = stamp
					observedPresent = present
				},
			}
			p2.pollPlayerStatus(context.Background())

			if observedCalls != 1 {
				t.Fatalf("expected the stamp observer to be called once, got %d", observedCalls)
			}
			if observedPresent != tc.wantPresent {
				t.Fatalf("observed present = %v, want %v", observedPresent, tc.wantPresent)
			}
			if tc.wantPresent && observedStamp != tc.wantStamp {
				t.Fatalf("observed stamp = %q, want %q", observedStamp, tc.wantStamp)
			}
		})
	}
}

// TestFFPlayerCompat_StampStrippedFromNotificationPayload pins that the
// stamp field the fixtures above carry — an internal generation-tracking
// implementation detail — never leaks onto the relayer/websocket-facing
// player_status notification, using the SAME literal reply shape a real
// player produces.
func TestFFPlayerCompat_StampStrippedFromNotificationPayload(t *testing.T) {
	mockCDP := &fakeCDP{
		pageNavigationURL: constants.WEBAPP_URL,
		noLogSendResult:   decodeFFPlayerFixture(t, ffplayerfixtures.CheckStatusReplyStampPresentValue),
	}
	mockRelayer := &fakeRelayer{connectedResponses: []bool{true}}
	mockWS := &fakeWS{}

	p := &poller{
		cdp:                     mockCDP,
		relayer:                 mockRelayer,
		ws:                      mockWS,
		json:                    wrapper.NewJSON(),
		logger:                  zap.NewNop(),
		lastRelayerStatusHashes: make(map[relayer.NotificationType]string),
		lastWSStatusHashes:      make(map[relayer.NotificationType]string),
	}
	p.pollPlayerStatus(context.Background())

	if mockWS.sendAllCalls != 1 {
		t.Fatalf("expected one websocket send, got %d", mockWS.sendAllCalls)
	}
	payload, ok := mockWS.lastPayload.(map[string]interface{})
	if !ok {
		t.Fatalf("expected websocket payload map, got %T", mockWS.lastPayload)
	}
	message, ok := payload["message"].(*PlayerStatus)
	if !ok {
		t.Fatalf("expected payload message to be *PlayerStatus, got %T", payload["message"])
	}
	if message.Stamp != nil {
		t.Fatalf("expected stamp to be stripped from the notification payload, got %+v", *message.Stamp)
	}
}
