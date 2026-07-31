package playersession

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/ffplayerfixtures"
)

// decodeCheckStatusStamp extracts the (stamp, present) pair the status
// poller's stampObserver would report for a LITERAL checkStatus reply, by
// applying the exact rule status.poller.pollPlayerStatus applies: present
// iff the "message" object carries a "stamp" key at all, regardless of its
// value.
func decodeCheckStatusStamp(t *testing.T, raw string) (stamp string, present bool) {
	t.Helper()
	var decoded struct {
		Message struct {
			Stamp *string `json:"stamp"`
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture is not valid JSON: %v\nraw: %s", err, raw)
	}
	if decoded.Message.Stamp == nil {
		return "", false
	}
	return *decoded.Message.Stamp, true
}

// TestFFPlayerCompat_StampObserverDrivesGenerationBump closes the full
// stamp-transport loop end to end with LITERAL ff-player checkStatus reply
// shapes: decode the fixture exactly as status.poller would (present iff the
// key exists), then feed the result into the REAL ObserveStatusStamp and
// assert the generation-bump decision the boot-recovery/park machinery
// ultimately depends on (docs/player-session-recovery.md §2.1).
func TestFFPlayerCompat_StampObserverDrivesGenerationBump(t *testing.T) {
	// newReadySession builds a fresh, independent session per subtest
	// (already past its own bump, with a real non-empty baseline stamp) so
	// each subtest's generation-count assertions aren't coupled to bumps a
	// PRIOR subtest made against a shared session.
	newReadySession := func(t *testing.T) (*Session, uint64, string) {
		t.Helper()
		f := newFakeCDP()
		f.handlerInstalled = true
		s := newTestSession(t, f)
		s.OnConnect()
		waitFor(t, time.Second, func() bool { return s.StageReady(StageHandler) })
		gen := s.Generation()
		baseline := s.currentGenerationStampForTest()
		require.NotEmpty(t, baseline)
		return s, gen, baseline
	}

	t.Run("absent (old player) never bumps", func(t *testing.T) {
		s, gen, _ := newReadySession(t)
		stamp, present := decodeCheckStatusStamp(t, ffplayerfixtures.CheckStatusReplyStampAbsent)
		require.False(t, present)
		s.ObserveStatusStamp(stamp, present)
		assert.Equal(t, gen, s.Generation())
	})

	t.Run("present, real value, different from baseline: bumps (foreign document)", func(t *testing.T) {
		s, gen, baseline := newReadySession(t)
		stamp, present := decodeCheckStatusStamp(t, ffplayerfixtures.CheckStatusReplyStampPresentValue)
		require.True(t, present)
		require.NotEqual(t, baseline, stamp, "fixture value must not accidentally equal the real baseline")
		s.ObserveStatusStamp(stamp, present)
		waitFor(t, time.Second, func() bool { return s.Generation() == gen+1 })
	})

	t.Run("present, empty against a non-empty baseline: bumps (foreign document)", func(t *testing.T) {
		s, gen, _ := newReadySession(t)
		stamp, present := decodeCheckStatusStamp(t, ffplayerfixtures.CheckStatusReplyStampPresentEmpty)
		require.True(t, present)
		require.Empty(t, stamp)
		s.ObserveStatusStamp(stamp, present)
		waitFor(t, time.Second, func() bool { return s.Generation() == gen+1 })
	})
}

// fixtureStatusCDP is a minimal CDPSender whose window.__ffosPlayerStatus
// probe answers with a LITERAL ff-player fixture, decoded once the same way
// the real cdp client decodes a JSON-string Runtime.evaluate result (see
// ffplayerfixtures' package doc). Everything else (Page.navigate,
// handler-presence probes) behaves as an always-ready, always-succeeding
// player, so these tests isolate the status-probe decode path this package
// depends on (rawPlayerStatus / currentRoute / isErrorPage) rather than
// re-exercising the navigation machinery session_test.go already covers.
type fixtureStatusCDP struct {
	statusPayload map[string]any
}

func newFixtureStatusCDP(t *testing.T, raw string) *fixtureStatusCDP {
	t.Helper()
	var decoded map[string]any
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		t.Fatalf("fixture is not valid JSON: %v\nraw: %s", err, raw)
	}
	return &fixtureStatusCDP{statusPayload: decoded}
}

func (f *fixtureStatusCDP) Initialized() bool { return true }

func (f *fixtureStatusCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	expr, _ := params["expression"].(string)
	switch {
	case expr == `window.__ffosPlayerStatus ? window.__ffosPlayerStatus() : null`:
		return f.statusPayload, nil
	case method == "Page.navigate":
		return map[string]any{}, nil
	default:
		// Nonce stamp writes, the barrier stamp write, and the StageHandler
		// probe all take this catch-all: report ready so navigation-gate
		// tests only need to control the status payload above.
		return map[string]any{"ready": true, "stamped": true, "nonce": "x"}, nil
	}
}

// TestFFPlayerCompat_ErrorPageGate drives the LITERAL __ffosPlayerStatus
// payloads a shipped ff-player produces through NavigateHomeInline's
// pre-navigation gates, pinning the [NV2] error-page gate's real behavior:
// it must fire on route=="/error" REGARDLESS of the protocol field (a
// future protocol bump this package cannot otherwise decode must not
// silently disable "never navigate over /error" — see
// docs/player-session-recovery.md §3.1), and must NOT fire for a healthy
// /playlist payload.
func TestFFPlayerCompat_ErrorPageGate(t *testing.T) {
	cases := []struct {
		name        string
		raw         string
		wantOutcome NavOutcome
	}{
		{
			name:        "route=/error, protocol=1",
			raw:         ffplayerfixtures.PlayerStatusRouteError,
			wantOutcome: NavSkippedOverlay,
		},
		{
			name:        "route=/error, protocol mismatch",
			raw:         ffplayerfixtures.PlayerStatusProtocolMismatchRouteError,
			wantOutcome: NavSkippedOverlay,
		},
		{
			name:        "route=/playlist, healthy",
			raw:         ffplayerfixtures.PlayerStatusBootHydrationOK,
			wantOutcome: NavExecuted,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cdp := newFixtureStatusCDP(t, tc.raw)
			s := New(context.Background(), cdp, nil, nil, zap.NewNop())
			s.pollInterval = 5 * time.Millisecond // in case verification needs to poll

			res := s.navigateAndVerify(context.Background(), NavOptions{})
			assert.Equal(t, tc.wantOutcome, res.Outcome)
			if tc.wantOutcome == NavExecuted {
				assert.NoError(t, res.Err)
			}
		})
	}
}
