package mintpairing

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Issue #332: once the approval request is out, a page that abandons its
// channel must end the approval, not leave it for the owner to approve into
// nothing.

// startPendingApproval starts a device-initiated pairing whose browser has
// already sent its request, and returns once the approval request reached the
// controller.
func startPendingApproval(t *testing.T, ch *fakeBrokerChannel, sessions *countingSessionCreator) (*service, *fakeRelayer, relayer.Response) {
	t.Helper()
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 8)}
	s := newService(
		Options{
			Enabled:              true,
			BrokerBaseURL:        "https://broker.example",
			ApprovalTimeout:      time.Minute,
			PollInterval:         time.Millisecond,
			ChannelWatchInterval: time.Millisecond,
			RelayerBaseURL:       "https://relayer.example",
		},
		&fakeBrokerStarter{channel: ch},
		sessions,
		relayerClient,
		&fakeCDP{},
		wrapper.NewJSON(),
		zap.NewNop(),
	).(*service)
	s.Start(context.Background())
	t.Cleanup(s.Stop)

	result, err := s.HandleStartPairingSession(context.Background(), nil)
	require.NoError(t, err)
	require.True(t, result.(startPairingResponse).OK)

	request := receiveNotification(t, relayerClient)
	assertRelayerNotification(t, request, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_REQUEST)
	return s, relayerClient, request
}

func receiveNotification(t *testing.T, r *fakeRelayer) relayer.Response {
	t.Helper()
	select {
	case response := <-r.sent:
		return response
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a relayer notification")
		return relayer.Response{}
	}
}

func pendingRequestChannel() *fakeBrokerChannel {
	return &fakeBrokerChannel{
		pairingCode: "PAIR-123",
		expiresAt:   time.Now().Add(time.Minute),
		request: &minter.MintRequest{
			ChannelID:   "ch_1",
			MessageID:   "msg_1",
			Seq:         7,
			Origin:      "https://gallery.example",
			BrowserInfo: minter.BrowserInfo{Name: "Chrome"},
		},
	}
}

// countingSessionCreator wraps fakeSessionCreator to record whether a session
// was ever minted.
type countingSessionCreator struct {
	fakeSessionCreator
	created chan struct{}
}

func (c *countingSessionCreator) CreateEphemeralSession(ctx context.Context, topicID string, request minter.MintRequest, lifetime sessionLifetime) (minter.MintResult, error) {
	select {
	case c.created <- struct{}{}:
	default:
	}
	return c.fakeSessionCreator.CreateEphemeralSession(ctx, topicID, request, lifetime)
}

func approvalDecisionFor(t *testing.T, request relayer.Response) map[string]any {
	t.Helper()
	message := request.Message.(map[string]any)
	return validDecisionArgs(
		message["approvalRequestID"].(string),
		message["topicID"].(string),
		message["channelID"].(string),
		message["requestMessageID"].(string),
	)
}

func TestApprovalWait_BrowserClosingItsChannelCancelsTheApproval(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"closed by the page", http.StatusConflict, "closed"},
		{"swept", http.StatusNotFound, "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			ch := pendingRequestChannel()
			sessions := &countingSessionCreator{created: make(chan struct{}, 1)}
			s, relayerClient, request := startPendingApproval(t, ch, sessions)

			ch.SetPollAfterRequestErr(&minter.BrokerError{Method: http.MethodGet, Path: "/v1/channels/ch_1/messages", StatusCode: tc.status, Code: tc.code})

			outcome := receiveNotification(t, relayerClient)
			assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
			message := outcome.Message.(map[string]any)
			assert.Equal(t, approvalCancellationStatus, message["status"])
			assert.Equal(t, browserGoneReason, message["reason"])
			assert.Equal(t, request.Message.(map[string]any)["approvalRequestID"], message["approvalRequestID"])

			// The owner's approve, arriving after, is refused rather than
			// acknowledged into a session nobody will receive.
			result, err := s.HandleApprovalDecision(context.Background(), approvalDecisionFor(t, request))
			require.NoError(t, err)
			response := result.(approvalResponse)
			assert.False(t, response.OK)
			require.NotNil(t, response.Error)
			assert.Contains(t, []string{browserGoneReason, "not_found"}, response.Error.Code,
				"refused either by the fence or, once the worker unregistered, as unknown")

			// Wait for the worker's own cleanup to have run, so the close
			// assertion below observes its decision rather than a race.
			require.Eventually(t, func() bool {
				active, _, _ := s.currentActive()
				return active == nil
			}, 2*time.Second, time.Millisecond)
			select {
			case <-sessions.created:
				t.Fatal("no session may be minted for a browser that left")
			default:
			}
			assert.Empty(t, ch.RejectionReasons(), "no rejection can reach a gone channel")
			// clearActive runs at the top of the worker's defer, ahead of its
			// close decision, so hold the assertion over a window.
			assert.Never(t, func() bool { return ch.CloseCount() > 0 }, 100*time.Millisecond, 5*time.Millisecond,
				"a gone channel is not closed again")
		})
	}
}

func TestApprovalWait_BrokerExpiryReportsExpiredNotBrowserGone(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := pendingRequestChannel()
	s, relayerClient, request := startPendingApproval(t, ch, &countingSessionCreator{created: make(chan struct{}, 1)})

	ch.SetPollAfterRequestErr(brokerStatus(http.StatusGone))

	outcome := receiveNotification(t, relayerClient)
	message := outcome.Message.(map[string]any)
	assert.Equal(t, "expired", message["status"])
	assert.Nil(t, message["reason"])

	result, err := s.HandleApprovalDecision(context.Background(), approvalDecisionFor(t, request))
	require.NoError(t, err)
	response := result.(approvalResponse)
	assert.False(t, response.OK)
	require.NotNil(t, response.Error)
	assert.Contains(t, []string{"expired", "not_found"}, response.Error.Code)
}

func TestApprovalWait_TransientPollFailuresKeepTheApprovalLive(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{"broker 5xx", &minter.BrokerError{Method: http.MethodGet, Path: "/v1/channels/ch_1/messages", StatusCode: http.StatusServiceUnavailable}},
		{"own credential refused", &minter.BrokerError{Method: http.MethodGet, Path: "/v1/channels/ch_1/messages", StatusCode: http.StatusUnauthorized}},
		{"network", errors.New("dial tcp: connection refused")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer state.ResetForTesting()
			state.GetState().Relayer.TopicID = "topic-1"

			ch := pendingRequestChannel()
			ch.successSent = make(chan struct{}, 1)
			sessions := &countingSessionCreator{created: make(chan struct{}, 1)}
			s, relayerClient, request := startPendingApproval(t, ch, sessions)

			ch.SetPollAfterRequestErr(tc.err)
			// Let the watcher see several failures before the owner decides.
			polls := ch.PollCount()
			require.Eventually(t, func() bool { return ch.PollCount() >= polls+3 }, 2*time.Second, time.Millisecond)

			result, err := s.HandleApprovalDecision(context.Background(), approvalDecisionFor(t, request))
			require.NoError(t, err)
			assert.True(t, result.(approvalResponse).OK)

			select {
			case <-ch.successSent:
			case <-time.After(2 * time.Second):
				t.Fatal("the approved session was never delivered")
			}
			outcome := receiveNotification(t, relayerClient)
			assert.Equal(t, "completed", outcome.Message.(map[string]any)["status"])
		})
	}
}

func TestApprovalWait_WatcherReadsPastTheHandledRequest(t *testing.T) {
	defer state.ResetForTesting()
	state.GetState().Relayer.TopicID = "topic-1"

	ch := pendingRequestChannel()
	_, _, _ = startPendingApproval(t, ch, &countingSessionCreator{created: make(chan struct{}, 1)})

	require.Eventually(t, func() bool { return ch.PollCount() >= 3 }, 2*time.Second, time.Millisecond)
	ch.mu.Lock()
	defer ch.mu.Unlock()
	// The first poll found the request; every watcher poll after it starts
	// past the request's seq, so the handled request is never re-read.
	for _, afterSeq := range ch.pollAfterSeqs[1:] {
		assert.Equal(t, int64(7), afterSeq)
	}
}

// goneApprovalFixture registers a pending approval on a service whose
// relayer records outcomes, for exercising endApprovalOnGoneChannel directly:
// the worker's select cannot be steered into the gone case while a decision is
// also waiting, so the "decision landed first" branches are pinned here.
func goneApprovalFixture(t *testing.T) (*service, *pendingApproval, *fakeRelayer, *countingSessionCreator) {
	t.Helper()
	state.GetState().Relayer.TopicID = "topic-1"
	relayerClient := &fakeRelayer{sent: make(chan relayer.Response, 4)}
	sessions := &countingSessionCreator{created: make(chan struct{}, 1)}
	s := newService(
		Options{Enabled: true, ApprovalTimeout: time.Minute, PollInterval: time.Millisecond},
		nil, sessions, relayerClient, nil, wrapper.NewJSON(), zap.NewNop(),
	).(*service)
	pending := &pendingApproval{
		approvalRequestID: "mpa_1",
		guard:             currentTopicGuard(),
		channelID:         "ch_1",
		requestMessageID:  "msg_1",
		expiresAt:         time.Now().Add(time.Minute),
		decisionCh:        make(chan approvalDecisionRequest, 1),
	}
	s.registerPending(pending)
	return s, pending, relayerClient, sessions
}

var goneRequest = minter.MintRequest{ChannelID: "ch_1", MessageID: "msg_1"}

func brokerStatus(status int) error {
	code := map[int]string{http.StatusNotFound: "not_found", http.StatusConflict: "closed", http.StatusGone: "expired"}[status]
	return &minter.BrokerError{Method: http.MethodGet, Path: "/v1/channels/ch_1/messages", StatusCode: status, Code: code}
}

func TestEndApprovalOnGoneChannel(t *testing.T) {
	for _, tc := range []struct {
		name          string
		decision      string // "" = the owner had not decided
		goneStatus    int
		wantStatus    string
		wantReason    any
		wantRefusal   string
		wantNoRefusal bool
	}{
		{name: "page closed, undecided", goneStatus: http.StatusConflict, wantStatus: approvalCancellationStatus, wantReason: browserGoneReason, wantRefusal: browserGoneReason},
		{name: "swept, undecided", goneStatus: http.StatusNotFound, wantStatus: approvalCancellationStatus, wantReason: browserGoneReason, wantRefusal: browserGoneReason},
		// Review F1: a broker expiry is the owner not answering in time, not
		// the site leaving — on a joined pairing the two deadlines coincide.
		{name: "broker expiry, undecided", goneStatus: http.StatusGone, wantStatus: "expired", wantReason: nil, wantRefusal: "expired"},
		{name: "approve landed first", decision: "approve", goneStatus: http.StatusConflict, wantStatus: "failed", wantReason: nil, wantNoRefusal: true},
		{name: "reject landed first", decision: "reject", goneStatus: http.StatusConflict, wantStatus: "rejected", wantReason: nil, wantNoRefusal: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer state.ResetForTesting()
			s, pending, relayerClient, sessions := goneApprovalFixture(t)
			args := validDecisionArgs("mpa_1", "topic-1", "ch_1", "msg_1")
			if tc.decision != "" {
				args["decision"] = tc.decision
				result, err := s.HandleApprovalDecision(context.Background(), args)
				require.NoError(t, err)
				require.True(t, result.(approvalResponse).OK)
			}

			skipClose := s.endApprovalOnGoneChannel(&activePairing{channelID: "ch_1"}, goneRequest, pending.guard, pending, brokerStatus(tc.goneStatus))
			assert.True(t, skipClose, "a gone channel is never closed again")

			outcome := receiveNotification(t, relayerClient)
			assertRelayerNotification(t, outcome, relayer.NOTIFICATION_TYPE_MINT_PAIRING_APPROVAL_OUTCOME)
			message := outcome.Message.(map[string]any)
			assert.Equal(t, tc.wantStatus, message["status"])
			assert.Equal(t, tc.wantReason, message["reason"])

			select {
			case <-sessions.created:
				t.Fatal("nothing may be minted for a channel already known to be gone")
			default:
			}

			result, err := s.HandleApprovalDecision(context.Background(), args)
			require.NoError(t, err)
			response := result.(approvalResponse)
			if tc.wantNoRefusal {
				// The landed decision stands; a retry of it is not refused.
				assert.True(t, response.OK)
				return
			}
			assert.False(t, response.OK)
			require.NotNil(t, response.Error)
			assert.Equal(t, tc.wantRefusal, response.Error.Code)
			assert.False(t, response.Error.Retryable)
			select {
			case <-pending.decisionCh:
				t.Fatal("a refused decision must not reach the worker")
			default:
			}
		})
	}
}

func TestEndApprovalOnGoneChannel_AClaimThatIsGoneHearsNothing(t *testing.T) {
	defer state.ResetForTesting()
	s, pending, relayerClient, _ := goneApprovalFixture(t)
	// A factory reset and re-claim between the request and the gone answer:
	// same topic id, new generation.
	if _, err := state.ClearClaim(); err != nil {
		t.Logf("ClearClaim save failed as expected in tests: %v", err)
	}
	if _, err := state.SetRelayerTopicID("topic-1"); err != nil {
		t.Logf("SetRelayerTopicID save failed as expected in tests: %v", err)
	}

	skipClose := s.endApprovalOnGoneChannel(&activePairing{channelID: "ch_1"}, goneRequest, pending.guard, pending, brokerStatus(http.StatusConflict))
	assert.True(t, skipClose)
	select {
	case response := <-relayerClient.sent:
		t.Fatalf("a superseded claim must hear nothing, got %v", response.NotificationType)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestIsChannelGone(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want bool
	}{
		{"404 from the broker", &minter.BrokerError{StatusCode: http.StatusNotFound, Code: "not_found"}, true},
		{"bare 404 (proxy, misroute)", &minter.BrokerError{StatusCode: http.StatusNotFound}, false},
		{"404 as a formatted line only", errors.New("broker GET /v1/channels/x/messages failed with status 404"), false},
		{"409 closed", &minter.BrokerError{StatusCode: http.StatusConflict}, true},
		{"410 expired", &minter.BrokerError{StatusCode: http.StatusGone}, true},
		{"401 own credential", &minter.BrokerError{StatusCode: http.StatusUnauthorized}, false},
		{"500", &minter.BrokerError{StatusCode: http.StatusInternalServerError}, false},
		{"wrapped 409", errors.Join(errors.New("poll"), &minter.BrokerError{StatusCode: http.StatusConflict}), true},
		{"formatted status line", errors.New("broker GET /v1/channels/x/messages failed with status 410"), true},
		{"network", errors.New("connection reset"), false},
		{"nil", nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, isChannelGone(tc.err))
		})
	}
}
