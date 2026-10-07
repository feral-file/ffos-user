package mintpairing

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/overlay"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// setupRecorder stands in for setupui's transport: it records each setup kind
// the controller paints, in order.
type setupRecorder struct {
	mu     sync.Mutex
	events []string
}

func (r *setupRecorder) Show(_ context.Context, o overlay.Overlay) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "show:"+string(o.Kind))
	return nil
}

func (r *setupRecorder) Hide(_ context.Context, o overlay.Overlay) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, "hide:"+string(o.Kind))
	return nil
}

func (r *setupRecorder) snapshot() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

const kindClaimQR overlay.Kind = "setup:claim_qr"

// newSharedOverlayService builds a mint service and a controller it shares with
// a setup transport, the way main wires them.
func newSharedOverlayService(t *testing.T, starter brokerStarter, cdp *fakeCDP, setup *setupRecorder) (*service, *overlay.Controller) {
	t.Helper()
	state.GetState().Relayer.TopicID = "topic-1"
	t.Cleanup(state.ResetForTesting)
	s := newService(
		Options{
			Enabled:            true,
			BrokerBaseURL:      "https://broker.example",
			IdleTTL:            time.Minute,
			PlayerContractPath: writeValidPlayerContract(t),
		},
		starter,
		nil,
		nil,
		cdp,
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	ctrl := overlay.New(overlay.NewRouter(map[string]overlay.Painter{
		"setup:": setup,
		"mint:":  s.Painter(),
	}))
	s.SetController(ctrl)
	s.Start(context.Background())
	t.Cleanup(s.Stop)
	return s, ctrl
}

// Issue #382: an automatic refresh of an expired pairing code must not replace a
// claim QR the owner has on screen.
func TestAutomaticRefresh_DoesNotReplaceClaimQR(t *testing.T) {
	setup := &setupRecorder{}
	cdpClient := &fakeCDP{}
	starter := &fakeBrokerStarter{channel: &fakeBrokerChannel{pairingCode: "PAIR-A"}}
	s, ctrl := newSharedOverlayService(t, starter, cdpClient, setup)

	// The owner's pairing code is on screen, then the owner shows the claim QR.
	_, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	_, _, err = ctrl.Show(context.Background(), &setupRecorderListener{}, overlay.Overlay{Kind: kindClaimQR}, overlay.Owner)
	require.NoError(t, err)

	// The code expires on its own. The refresh must not put a code back over the QR.
	_, _ = s.CloseActivePairing(context.Background())
	s.refreshExpiredPairingCode()

	cur, ok := ctrl.Current()
	require.True(t, ok)
	assert.Equal(t, kindClaimQR, cur.Kind, "the claim QR must keep the screen")
	// Delivery is asynchronous (see the overlay package doc), so wait for the
	// claim QR to actually reach the recorder rather than asserting immediately.
	assert.Eventually(t, func() bool { return len(setup.snapshot()) > 0 }, time.Second, time.Millisecond)
	assert.Equal(t, []string{"show:" + string(kindClaimQR)}, setup.snapshot())
}

// Issue #382: the owner's claim QR replaces a pairing code, and the replaced
// session ends. Its later clear must not touch the claim QR.
func TestClaimQR_ReplacesPairingCodeAndEndsTheSession(t *testing.T) {
	setup := &setupRecorder{}
	cdpClient := &fakeCDP{}
	starter := &fakeBrokerStarter{channel: &fakeBrokerChannel{pairingCode: "PAIR-B"}}
	s, ctrl := newSharedOverlayService(t, starter, cdpClient, setup)

	_, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, s.DisplayActive(), "the pairing code is on screen")

	_, _, err = ctrl.Show(context.Background(), &setupRecorderListener{}, overlay.Overlay{Kind: kindClaimQR}, overlay.Owner)
	require.NoError(t, err)

	assert.Eventually(t, func() bool { return !s.DisplayActive() }, 2*time.Second, 10*time.Millisecond,
		"the replaced session must end")
	assertEventuallyDisplayObserved(t, cdpClient, "pairing_code", "PAIR-B", "")
	// The session's clear, once it runs, finds the claim QR current and sends nothing.
	s.Stop()
	for _, r := range cdpClient.displayRequestsSnapshot() {
		if r["state"] == "hidden" {
			t.Fatalf("a replaced session cleared the screen: %v", r)
		}
	}
}

// The owner's Browser Pairing tap replaces the claim QR, as the owner asked.
func TestBrowserPairingTap_ReplacesClaimQR(t *testing.T) {
	setup := &setupRecorder{}
	cdpClient := &fakeCDP{}
	starter := &fakeBrokerStarter{channel: &fakeBrokerChannel{pairingCode: "PAIR-C"}}
	s, ctrl := newSharedOverlayService(t, starter, cdpClient, setup)

	_, _, err := ctrl.Show(context.Background(), &setupRecorderListener{}, overlay.Overlay{Kind: kindClaimQR}, overlay.Owner)
	require.NoError(t, err)

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, result.(startPairingResponse).OK)

	cur, ok := ctrl.Current()
	require.True(t, ok)
	assert.Equal(t, KindPairingCode, cur.Kind)
}

// TestClaimQR_OverridesPairingCodeDuringItsInitialPaint_EndsTheSessionWithoutPublishing
// pins round 5 review's F1: the claim QR can replace the pairing code's
// overlay WHILE the code's own initial CDP send is still in flight —
// showPairingCode's decision commits (and so can be overridden) well before
// startPairing ever publishes s.active, which only happens after
// showPairingCode's result.Wait returns. Before the fix, endOverridden
// checked only s.active == active, which was still nil at that moment, so the
// override was silently dropped: the start went on to publish anyway and
// spawn a worker for a session that had already lost the screen. onDisplay
// fires synchronously from inside the pairing code's own CDP send — before
// result.Wait, and so before the publish — reproducing the race
// deterministically rather than by timing.
func TestClaimQR_OverridesPairingCodeDuringItsInitialPaint_EndsTheSessionWithoutPublishing(t *testing.T) {
	setup := &setupRecorder{}
	cdpClient := &fakeCDP{}
	starter := &fakeBrokerStarter{channel: &fakeBrokerChannel{pairingCode: "PAIR-RACE"}}
	s, ctrl := newSharedOverlayService(t, starter, cdpClient, setup)

	cdpClient.onDisplay = func(state string) {
		if state != "pairing_code" {
			return
		}
		cdpClient.mu.Lock()
		cdpClient.onDisplay = nil // once
		cdpClient.mu.Unlock()
		_, _, err := ctrl.Show(context.Background(), &setupRecorderListener{}, overlay.Overlay{Kind: kindClaimQR}, overlay.Owner)
		assert.NoError(t, err)
	}

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	assertCommandError(t, result, "pairing_closed", false)

	s.mu.Lock()
	active := s.active
	s.mu.Unlock()
	assert.Nil(t, active, "an overridden start must not publish")

	cur, ok := ctrl.Current()
	require.True(t, ok)
	assert.Equal(t, kindClaimQR, cur.Kind, "the claim QR must keep the screen")
}

// setupRecorderListener is the setup side's listener: it is never overridden in
// these tests, so it needs no behavior.
type setupRecorderListener struct{}

func (setupRecorderListener) OnOverride(overlay.Overlay) {}
func (setupRecorderListener) OnClose()                   {}

// Round 2 review, F-noise: an Automatic refresh correctly rejected because the
// claim QR legitimately owns the screen is the designed outcome, not a
// display fault — it must not log at Warn, the level that means "something
// is actually broken" to on-call triage.
func TestAutomaticRefresh_RejectedByClaimQRLogsNoWarning(t *testing.T) {
	core, logs := observer.New(zap.WarnLevel)
	setup := &setupRecorder{}
	cdpClient := &fakeCDP{}
	starter := &fakeBrokerStarter{channel: &fakeBrokerChannel{pairingCode: "PAIR-D"}}

	state.GetState().Relayer.TopicID = "topic-1"
	t.Cleanup(state.ResetForTesting)
	s := newService(
		Options{
			Enabled:            true,
			BrokerBaseURL:      "https://broker.example",
			IdleTTL:            time.Minute,
			PlayerContractPath: writeValidPlayerContract(t),
		},
		starter,
		nil,
		nil,
		cdpClient,
		wrapper.NewJSON(),
		zap.New(core),
	).(*service)
	ctrl := overlay.New(overlay.NewRouter(map[string]overlay.Painter{
		"setup:": setup,
		"mint:":  s.Painter(),
	}))
	s.SetController(ctrl)
	s.Start(context.Background())
	t.Cleanup(s.Stop)

	_, _, err := ctrl.Show(context.Background(), &setupRecorderListener{}, overlay.Overlay{Kind: kindClaimQR}, overlay.Owner)
	require.NoError(t, err)

	s.refreshExpiredPairingCode()

	cur, ok := ctrl.Current()
	require.True(t, ok)
	assert.Equal(t, kindClaimQR, cur.Kind, "the claim QR must keep the screen")
	assert.Zero(t, logs.Len(), "a correctly-rejected Automatic refresh must not log at Warn")
}
