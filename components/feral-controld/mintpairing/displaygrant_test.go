package mintpairing

import (
	"context"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeDisplayGrantIssuer stands in for the control authority.
type fakeDisplayGrantIssuer struct {
	mu        sync.Mutex
	err       error
	locked    bool
	onIssue   func()
	approvers []string
	issued    []control.DisplayGrantRequest
	revoked   []string
}

func (f *fakeDisplayGrantIssuer) IssueDisplayGrant(approver string, req control.DisplayGrantRequest) (control.DisplayGrant, error) {
	f.mu.Lock()
	f.approvers = append(f.approvers, approver)
	f.issued = append(f.issued, req)
	onIssue := f.onIssue
	f.mu.Unlock()
	if onIssue != nil {
		onIssue()
	}
	g := control.DisplayGrant{Ctl: req.Sig.Thumbprint(), Sig: req.Sig, Enc: req.Enc, Origin: req.Origin,
		OriginAttested: req.OriginAttested, ApprovedBy: approver, ExpiresAt: req.ExpiresAt}
	return g, f.err
}

func (f *fakeDisplayGrantIssuer) RevokeDisplayGrant(ctl string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.revoked = append(f.revoked, ctl)
	return nil
}

func (f *fakeDisplayGrantIssuer) Status() control.Status {
	if f.locked {
		return control.Status{V: 1, State: control.StateLocked}
	}
	return control.Status{V: 1, State: control.StateEnrolled, Profile: &control.Profile{V: 1, Dev: "FF1-TEST0001", Gen: "gen"}}
}

func (f *fakeDisplayGrantIssuer) revokes() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.revoked...)
}

// displayGrantKeysJSON builds a site's displayGrant member and the ctl the
// device derives from it.
func displayGrantKeysJSON(t *testing.T) (json.RawMessage, string) {
	t.Helper()
	sig, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	enc, err := ecdh.P256().GenerateKey(rand.Reader)
	require.NoError(t, err)
	sigPub, err := control.PublicJWKFromECDSA(&sig.PublicKey)
	require.NoError(t, err)
	encPub, err := control.PublicJWKFromECDH(enc.PublicKey())
	require.NoError(t, err)
	sigJSON, _ := json.Marshal(sigPub)
	encJSON, _ := json.Marshal(encPub)
	return json.RawMessage(`{"v":1,"sig":` + string(sigJSON) + `,"enc":` + string(encJSON) + `}`), sigPub.Thumbprint()
}

type displayRig struct {
	s       *service
	ch      *fakeBrokerChannel
	creator *recordingSessionCreator
	issuer  *fakeDisplayGrantIssuer
	relayer *fakeRelayer
}

func newDisplayRig(t *testing.T) *displayRig {
	t.Helper()
	r := &displayRig{ch: &fakeBrokerChannel{}, creator: &recordingSessionCreator{}, issuer: &fakeDisplayGrantIssuer{},
		relayer: &fakeRelayer{sent: make(chan relayer.Response, 4)}}
	r.s = newService(Options{RelayerBaseURL: "https://relayer.example"}, nil, r.creator, r.relayer, nil, wrapper.NewJSON(), zap.NewNop()).(*service)
	r.s.SetDisplayGrants(r.issuer, "wss://relay.example")
	return r
}

func (r *displayRig) complete(t *testing.T, request minter.MintRequest, decision approvalDecisionRequest) (bool, error) {
	t.Helper()
	request.ChannelID, request.MessageID = "ch_1", "msg_1"
	decision.ApprovalRequestID, decision.TopicID, decision.ChannelID, decision.RequestMessageID, decision.Decision = "mpa_1", "topic-1", "ch_1", "msg_1", "approve"
	return r.s.completeDecision(context.Background(), r.ch, request, topicGuard{topicID: "topic-1"}, "mpa_1", decision)
}

func (r *displayRig) outcome(t *testing.T) map[string]any {
	t.Helper()
	outcome := <-r.relayer.sent
	assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
	return outcome.Message.(map[string]any)
}

func TestCompleteDecision_DisplayRequestIsIssuedAGrantAndNoSession(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"
	keys, ctl := displayGrantKeysJSON(t)
	r := newDisplayRig(t)

	before := time.Now()
	terminalSent, err := r.complete(t, minter.MintRequest{Origin: "https://gallery.example", DisplayGrant: keys, RequestedExpiresInSeconds: 600},
		approvalDecisionRequest{Approver: "owner-ctl"})
	require.NoError(t, err)
	assert.True(t, terminalSent)
	assert.Zero(t, r.creator.calls, "a 0.5 request never mints a session")
	assert.Empty(t, r.ch.DeliveredSessions())

	require.Equal(t, []string{"owner-ctl"}, r.issuer.approvers)
	issued := r.issuer.issued[0]
	assert.Equal(t, "https://gallery.example", issued.Origin)
	assert.False(t, issued.OriginAttested, "a device-initiated channel carries the site's own origin claim")
	assert.InDelta(t, before.Add(600*time.Second).Unix(), issued.ExpiresAt, 2)

	delivered := r.ch.DeliveredGrants()
	require.Len(t, delivered, 1)
	assert.Equal(t, 1, delivered[0].Version)
	assert.Equal(t, ctl, delivered[0].Ctl)
	assert.Equal(t, "wss://relay.example", delivered[0].RelayV2Endpoint)
	assert.Equal(t, "FF1-TEST0001", delivered[0].Profile.Dev)
	assert.Equal(t, time.Unix(issued.ExpiresAt, 0).UTC().Format(time.RFC3339), delivered[0].ExpiresAt)

	outcome := r.outcome(t)
	assert.Equal(t, "completed", outcome["status"])
	assert.Equal(t, "timed", outcome["lifetime"])
	assert.Equal(t, ctl, outcome["ctl"])
	assert.Empty(t, r.issuer.revokes())
}

func TestCompleteDecision_DisplayKeepPairedIsAKeptGrant(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"
	keys, _ := displayGrantKeysJSON(t)
	// Kept, requester capable: no expiry. Kept, requester incapable: the
	// longest timed grant, named as the fallback.
	for _, tc := range []struct {
		supports bool
		lifetime string
	}{{true, "persistent"}, {false, "timed_fallback_requester"}} {
		r := newDisplayRig(t)
		_, err := r.complete(t, minter.MintRequest{Origin: "https://gallery.example", DisplayGrant: keys, SupportsPersistentSessions: tc.supports},
			approvalDecisionRequest{KeepPaired: true, Approver: "owner-ctl"})
		require.NoError(t, err)
		issued := r.issuer.issued[0]
		if tc.supports {
			assert.Zero(t, issued.ExpiresAt)
			assert.Empty(t, r.ch.DeliveredGrants()[0].ExpiresAt)
		} else {
			assert.InDelta(t, time.Now().Add(maxSessionTTLSeconds*time.Second).Unix(), issued.ExpiresAt, 2)
		}
		assert.Equal(t, tc.lifetime, r.outcome(t)["lifetime"])
	}
}

func TestCompleteDecision_DisplayLegacyApproverIsEmpty(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"
	keys, ctl := displayGrantKeysJSON(t)
	// A real legacy authority: no owner, so the decision arrives in
	// plaintext with no principal, and the grant records no approver.
	a, err := control.New(control.Config{Dir: "/s/control", DeviceID: "FF1-TEST0001", FS: control.NewMemFS()})
	require.NoError(t, err)
	r := newDisplayRig(t)
	r.s.SetDisplayGrants(a, "wss://relay.example")
	_, err = r.complete(t, minter.MintRequest{Origin: "https://gallery.example", DisplayGrant: keys}, approvalDecisionRequest{})
	require.NoError(t, err)
	g, ok := a.Store().DisplayGrant(ctl)
	require.True(t, ok)
	assert.Equal(t, "", g.ApprovedBy)
	assert.Equal(t, "gallery.example", g.Label)
	assert.Equal(t, a.Status().Profile.Gen, r.ch.DeliveredGrants()[0].Profile.Gen)
}

func TestCompleteDecision_DisplayRefusals(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"
	keys, ctl := displayGrantKeysJSON(t)
	otherKeys, _ := displayGrantKeysJSON(t)
	for name, tc := range map[string]struct {
		setup   func(*displayRig)
		request minter.MintRequest
		reason  string
		issued  bool
		revoked bool
	}{
		"cap":            {func(r *displayRig) { r.issuer.err = control.ErrTooManyDisplayGrants }, minter.MintRequest{DisplayGrant: keys}, reasonTooManySites, true, false},
		"durability":     {func(r *displayRig) { r.issuer.err = control.ErrDurability }, minter.MintRequest{DisplayGrant: keys}, reasonDisplayGrantUnavailable, true, true},
		"not owner":      {func(r *displayRig) { r.issuer.err = control.ErrNotOwner }, minter.MintRequest{DisplayGrant: keys}, reasonDisplayGrantUnavailable, true, false},
		"no authority":   {func(r *displayRig) { r.s.SetDisplayGrants(nil, "wss://relay.example") }, minter.MintRequest{DisplayGrant: keys}, reasonDisplayGrantUnavailable, false, false},
		"no v2 endpoint": {func(r *displayRig) { r.s.SetDisplayGrants(r.issuer, "") }, minter.MintRequest{DisplayGrant: keys}, reasonDisplayGrantUnavailable, false, false},
		"locked":         {func(r *displayRig) { r.issuer.locked = true }, minter.MintRequest{DisplayGrant: keys}, reasonDisplayGrantUnavailable, false, false},
		"malformed keys": {nil, minter.MintRequest{DisplayGrant: json.RawMessage(`{"v":1}`)}, reasonDisplayGrantUnavailable, false, false},
		"private key":    {nil, minter.MintRequest{DisplayGrant: json.RawMessage(`{"v":1,"sig":{"kty":"EC","crv":"P-256","x":"AA","y":"AA","d":"AA"},"enc":{}}`)}, reasonDisplayGrantUnavailable, false, false},
		"announced keys differ": {func(r *displayRig) { r.ch.joinedRequest = &minter.MintRequest{DisplayGrant: otherKeys} },
			minter.MintRequest{DisplayGrant: keys, Seq: 3}, reasonDisplayGrantUnavailable, false, false},
		"announced copy only": {func(r *displayRig) { r.ch.joinedRequest = &minter.MintRequest{DisplayGrant: keys} },
			minter.MintRequest{Seq: 3}, reasonDisplayGrantUnavailable, false, false},
		"encrypted copy only": {func(r *displayRig) { r.ch.joinedRequest = &minter.MintRequest{} },
			minter.MintRequest{DisplayGrant: keys, Seq: 3}, reasonDisplayGrantUnavailable, false, false},
	} {
		t.Run(name, func(t *testing.T) {
			r := newDisplayRig(t)
			if tc.setup != nil {
				tc.setup(r)
			}
			terminalSent, err := r.complete(t, tc.request, approvalDecisionRequest{Approver: "owner-ctl"})
			require.Error(t, err)
			assert.True(t, terminalSent)
			assert.Zero(t, r.creator.calls, "a refused 0.5 request gets no session either")
			assert.Empty(t, r.ch.DeliveredGrants())
			assert.Equal(t, []string{tc.reason}, r.ch.RejectionReasons())
			assert.Equal(t, []bool{name == "durability"}, r.ch.rejectionRetryable, "only a durability failure is worth a retry")
			outcome := r.outcome(t)
			assert.Equal(t, "failed", outcome["status"])
			assert.Equal(t, tc.reason, outcome["reason"])
			assert.Equal(t, tc.issued, len(r.issuer.issued) == 1)
			if tc.revoked {
				assert.Equal(t, []string{ctl}, r.issuer.revokes())
			} else {
				assert.Empty(t, r.issuer.revokes())
			}
		})
	}
}

func TestCompleteDecision_DisplayAnnouncedKeysAreUsedAlone(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"
	keys, ctl := displayGrantKeysJSON(t)
	otherKeys, _ := displayGrantKeysJSON(t)
	// The announced request itself (Seq 0) is what the device answers on the
	// #375 path; it is never compared with itself. Matching copies pass too.
	r := newDisplayRig(t)
	r.ch.joinedRequest = &minter.MintRequest{DisplayGrant: otherKeys}
	_, err := r.complete(t, minter.MintRequest{DisplayGrant: otherKeys, Seq: 0}, approvalDecisionRequest{Approver: "o"})
	require.NoError(t, err)
	r = newDisplayRig(t)
	r.ch.joinedRequest = &minter.MintRequest{DisplayGrant: keys}
	_, err = r.complete(t, minter.MintRequest{DisplayGrant: keys, Seq: 3}, approvalDecisionRequest{Approver: "o"})
	require.NoError(t, err)
	assert.Equal(t, ctl, r.ch.DeliveredGrants()[0].Ctl)
}

func TestCompleteDecision_DisplayGrantIsRevokedOnEveryAbandonPath(t *testing.T) {
	keys, ctl := displayGrantKeysJSON(t)
	t.Run("broker refused the delivery", func(t *testing.T) {
		defer state.ResetForTesting()
		state.GetState().Relayer.TopicID = "topic-1"
		r := newDisplayRig(t)
		r.ch.successErr = errors.New("broker POST /v1/channels/ch_1/messages failed with status 410")
		terminalSent, err := r.complete(t, minter.MintRequest{DisplayGrant: keys}, approvalDecisionRequest{Approver: "o"})
		require.Error(t, err)
		assert.False(t, terminalSent)
		assert.Equal(t, []string{ctl}, r.issuer.revokes())
		assert.Equal(t, "failed", r.outcome(t)["status"])
	})
	t.Run("indeterminate delivery keeps the grant", func(t *testing.T) {
		defer state.ResetForTesting()
		state.GetState().Relayer.TopicID = "topic-1"
		r := newDisplayRig(t)
		r.ch.successErr = errors.New("broker POST failed with status 503")
		_, err := r.complete(t, minter.MintRequest{DisplayGrant: keys}, approvalDecisionRequest{Approver: "o"})
		require.Error(t, err)
		assert.Empty(t, r.issuer.revokes(), "the site may hold the grant")
		r.outcome(t)
	})
	t.Run("topic changed after issuance", func(t *testing.T) {
		defer state.ResetForTesting()
		state.GetState().Relayer.TopicID = "topic-1"
		r := newDisplayRig(t)
		r.issuer.onIssue = func() { state.GetState().Relayer.TopicID = "topic-2" }
		terminalSent, err := r.complete(t, minter.MintRequest{DisplayGrant: keys}, approvalDecisionRequest{Approver: "o"})
		require.Error(t, err)
		assert.True(t, terminalSent)
		assert.Empty(t, r.ch.DeliveredGrants())
		assert.Equal(t, []string{"topic_changed"}, r.ch.RejectionReasons())
		assert.Equal(t, []string{ctl}, r.issuer.revokes())
		r.outcome(t)
	})
	t.Run("topic changed during delivery", func(t *testing.T) {
		defer state.ResetForTesting()
		state.GetState().Relayer.TopicID = "topic-1"
		r := newDisplayRig(t)
		r.ch.onSend = func() { state.GetState().Relayer.TopicID = "topic-2" }
		terminalSent, err := r.complete(t, minter.MintRequest{DisplayGrant: keys}, approvalDecisionRequest{Approver: "o"})
		require.NoError(t, err)
		assert.True(t, terminalSent)
		require.Len(t, r.ch.DeliveredGrants(), 1, "the browser's message is not taken back")
		assert.Equal(t, []string{ctl}, r.issuer.revokes())
		assert.Equal(t, "completed", r.outcome(t)["status"])
	})
	t.Run("pairing superseded after issuance", func(t *testing.T) {
		defer state.ResetForTesting()
		state.GetState().Relayer.TopicID = "topic-1"
		r := newDisplayRig(t)
		active := &activePairing{channel: r.ch, channelID: "ch_1", joined: true}
		r.s.active = active
		r.issuer.onIssue = func() {
			r.s.mu.Lock()
			r.s.active = nil
			r.s.mu.Unlock()
		}
		terminalSent, err := r.s.completeDecisionFor(context.Background(), active, r.ch, minter.MintRequest{ChannelID: "ch_1", MessageID: "msg_1", DisplayGrant: keys},
			topicGuard{topicID: "topic-1"}, "mpa_1", approvalDecisionRequest{ApprovalRequestID: "mpa_1", TopicID: "topic-1", ChannelID: "ch_1", RequestMessageID: "msg_1", Decision: "approve", Approver: "o"})
		require.Error(t, err)
		assert.True(t, terminalSent)
		assert.Empty(t, r.ch.DeliveredGrants())
		assert.True(t, r.issuer.issued[0].OriginAttested, "a joined channel's origin is broker-attested")
		assert.Equal(t, []string{ctl}, r.issuer.revokes())
		r.outcome(t)
	})
}

func TestHandleApprovalDecision_RecordsTheApprover(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"
	s := newTestService()
	pending := &pendingApproval{approvalRequestID: "mpa_1", guard: topicGuard{topicID: "topic-1"}, channelID: "ch_1",
		requestMessageID: "msg_1", expiresAt: time.Now().Add(time.Minute), decisionCh: make(chan approvalDecisionRequest, 1)}
	s.registerPending(pending)
	ctx := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "owner-ctl", Role: control.RoleOwner})
	_, err := s.HandleApprovalDecision(ctx, validDecisionArgs("mpa_1", "topic-1", "ch_1", "msg_1"))
	require.NoError(t, err)
	decision := <-pending.decisionCh
	assert.Equal(t, "owner-ctl", decision.Approver)
	// The same decision in plaintext (a legacy device) is the same decision.
	result, err := s.HandleApprovalDecision(context.Background(), validDecisionArgs("mpa_1", "topic-1", "ch_1", "msg_1"))
	require.NoError(t, err)
	assert.Equal(t, "already_accepted", result.(approvalResponse).Status)
	accepted, ok := s.acceptedDecision(pending)
	require.True(t, ok)
	assert.Equal(t, "owner-ctl", accepted.Approver, "the first decision's approver is kept")
}

func TestSendApprovalRequest_MarksDisplayRequests(t *testing.T) {
	keys, _ := displayGrantKeysJSON(t)
	for _, tc := range []struct {
		request minter.MintRequest
		flag    any
	}{
		{minter.MintRequest{ChannelID: "ch_1", DisplayGrant: keys}, true},
		{minter.MintRequest{ChannelID: "ch_1"}, nil},
	} {
		relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 1)}
		s := newService(Options{}, nil, nil, relayerClient, nil, wrapper.NewJSON(), zap.NewNop()).(*service)
		display := isDisplayRequest(&fakeBrokerChannel{}, tc.request)
		require.NoError(t, s.sendApprovalRequest(context.Background(), "mpa_1", "topic-1", tc.request, minter.PublicJWK{}, time.Now().Add(time.Minute), display))
		msg := (<-relayerClient.sent).Message.(map[string]any)
		assert.Equal(t, tc.flag, msg["grantsDisplayAccess"])
	}
}

// A 0.5 request the device cannot honor is refused before the owner is
// asked: no approval request goes out, no session is minted.
func TestWaitForBrowserAndApproval_RefusesAnUnservableDisplayRequestBeforeAskingTheOwner(t *testing.T) {
	keys, _ := displayGrantKeysJSON(t)
	for name, tc := range map[string]struct {
		keys json.RawMessage
		wire func(*service)
	}{
		"no authority":   {keys, func(*service) {}},
		"malformed keys": {json.RawMessage(`{"v":1}`), func(s *service) { s.SetDisplayGrants(&fakeDisplayGrantIssuer{}, "wss://relay.example") }},
	} {
		t.Run(name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"
			ch := &fakeBrokerChannel{pairingCode: "PAIR-123", rejectionSent: make(chan struct{}, 1),
				request: &minter.MintRequest{ChannelID: "ch_1", MessageID: "msg_1", Origin: "https://gallery.example", DisplayGrant: tc.keys}}
			creator := &recordingSessionCreator{}
			relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
			s := newService(Options{Enabled: true, BrokerBaseURL: "https://broker.example", ApprovalTimeout: time.Minute, PollInterval: time.Millisecond},
				&fakeBrokerStarter{channel: ch}, creator, relayerClient, &fakeCDP{}, wrapper.NewJSON(), zap.NewNop()).(*service)
			tc.wire(s)
			s.Start(context.Background())
			defer s.Stop()
			result, err := s.HandleStartPairingSession(context.Background(), nil)
			require.NoError(t, err)
			require.True(t, result.(startPairingResponse).OK)
			select {
			case <-ch.rejectionSent:
			case <-time.After(2 * time.Second):
				t.Fatal("no rejection reached the browser")
			}
			assert.Equal(t, []string{reasonDisplayGrantUnavailable}, ch.RejectionReasons())
			assert.Zero(t, creator.calls)
			select {
			case n := <-relayerClient.sent:
				t.Fatalf("owner was asked: %+v", n)
			case <-time.After(100 * time.Millisecond):
			}
		})
	}
}

// TestHandleJoinPairingChannel_NoTopicWithDisplayGrantsProceeds: a device that
// can issue display grants pairs without a legacy topic (it may never get one
// once the legacy relayer is retired); without display grants the topic is
// still required (docs/relay-v2-only-g3.md §3.2).
func TestHandleJoinPairingChannel_NoTopicWithDisplayGrantsProceeds(t *testing.T) {
	joiner := &fakeBrokerJoiner{joined: joinedFor(&fakeBrokerChannel{}, "ch_site")}
	s := newDisplayJoinTestService(t, joiner, nil)

	result, err := s.HandleJoinPairingChannel(context.Background(), map[string]any{"shortCode": "123456"})
	require.NoError(t, err)
	if resp, ok := result.(map[string]any); ok {
		if errPayload, ok := resp["error"].(map[string]any); ok {
			assert.NotEqual(t, "topic_not_ready", errPayload["code"])
		}
	}
	assert.Eventually(t, func() bool { return len(joiner.Requests()) > 0 }, 2*time.Second, 10*time.Millisecond,
		"the join must reach the broker")
}

// TestCompleteDecision_DisplayRequestWithoutTopic: with no legacy topic the
// approval carries an empty topicID, and the display grant is still issued.
func TestCompleteDecision_DisplayRequestWithoutTopic(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = ""
	keys, _ := displayGrantKeysJSON(t)
	r := newDisplayRig(t)

	request := minter.MintRequest{Origin: "https://gallery.example", DisplayGrant: keys, ChannelID: "ch_1", MessageID: "msg_1"}
	decision := approvalDecisionRequest{Approver: "owner-ctl", ApprovalRequestID: "mpa_1", ChannelID: "ch_1", RequestMessageID: "msg_1", Decision: "approve"}
	terminalSent, err := r.s.completeDecision(context.Background(), r.ch, request, topicGuard{}, "mpa_1", decision)
	require.NoError(t, err)
	assert.True(t, terminalSent)
	assert.Len(t, r.ch.DeliveredGrants(), 1)
	assert.Zero(t, r.creator.calls)
}

// TestJoinedPairing_SessionRequestWithoutTopicIsRefused: a 0.4 (session)
// request on a device with no legacy topic is refused before the owner is
// asked, and no relayer session is ever requested with an empty topic.
func TestJoinedPairing_SessionRequestWithoutTopicIsRefused(t *testing.T) {
	ch := &fakeBrokerChannel{
		channelID:     "ch_site",
		request:       &minter.MintRequest{ChannelID: "ch_site", MessageID: "msg_1", Origin: testSiteOrigin},
		rejectionSent: make(chan struct{}, 1),
	}
	joiner := &fakeBrokerJoiner{joined: joinedFor(ch, "ch_site")}
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	s := newDisplayJoinTestService(t, joiner, relayerClient)

	_, err := s.HandleJoinPairingChannel(context.Background(), map[string]any{"shortCode": "123456"})
	require.NoError(t, err)
	select {
	case <-ch.rejectionSent:
	case <-time.After(3 * time.Second):
		t.Fatal("expected the session request to be refused")
	}
	ch.mu.Lock()
	reasons := append([]string(nil), ch.rejectionReasons...)
	ch.mu.Unlock()
	assert.Equal(t, []string{"topic_not_ready"}, reasons)
	select {
	case msg := <-relayerClient.sent:
		t.Fatalf("no approval request may reach the owner, got %v", msg)
	default:
	}
}

// newDisplayJoinTestService is newJoinTestService with display grants wired
// BEFORE Start (SetDisplayGrants is a wiring-time setter), and with the global
// state reset registered first so it runs after the service's Stop cleanup:
// a reset while the worker still reads state is a data race.
func newDisplayJoinTestService(t *testing.T, joiner *fakeBrokerJoiner, relayerClient relayer.Relayer) *service {
	t.Helper()
	t.Cleanup(state.ResetForTesting)
	state.GetState().Relayer.TopicID = ""
	s := newService(
		Options{
			Enabled:         true,
			BrokerBaseURL:   "https://broker.example",
			ApprovalTimeout: 2 * time.Second,
			PollInterval:    time.Millisecond,
			IdleTTL:         time.Minute,
			RelayerBaseURL:  "https://relayer.example",
		},
		nil,
		fakeSessionCreator{},
		relayerClient,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.joiner = joiner
	s.SetDisplayGrants(&fakeDisplayGrantIssuer{}, "wss://relay.example")
	s.Start(context.Background())
	t.Cleanup(s.Stop)
	return s
}
