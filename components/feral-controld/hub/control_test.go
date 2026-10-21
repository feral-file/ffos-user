package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// stubGate is a ControlGate with a fixed state and a recorded envelope.
type stubGate struct {
	gen       uint64
	state     control.State
	envelope  json.RawMessage
	origin    control.Origin
	protected bool
}

func (s *stubGate) HandleEnvelope(_ context.Context, raw json.RawMessage, origin control.Origin) (*controlgate.Wire, *control.PublicError) {
	s.envelope, s.origin = raw, origin
	if string(raw) == `{"v":1,"jwe":"bad"}` {
		return nil, &control.PublicError{Code: control.CodeUnknownController}
	}
	return &controlgate.Wire{V: 1, JWE: "reply"}, nil
}
func (s *stubGate) Plaintext(route control.Route, cmd commands.Command) (control.Decision, commands.Command) {
	return control.Policy(s.state, route, string(cmd.Type), cmd.RawArguments), cmd
}
func (s *stubGate) ScreenProtected() bool        { return s.protected }
func (s *stubGate) InvitationGeneration() uint64 { return s.gen }
func (s *stubGate) State() control.State         { return s.state }
func (s *stubGate) Status() control.Status       { return control.Status{V: 1, State: s.state} }

func serve(mux *http.ServeMux, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// With control disabled (no gate) an ff1Envelope cast is just an unknown
// command for the router, exactly as before the protocol existed, and the
// status payload has no control key.
func TestControlDisabledIsUnchanged(t *testing.T) {
	mux, mockCmd, _ := newControlHub(t, unclaimed, "")
	mockCmd.EXPECT().Process(gomock.Any(), gomock.Any()).DoAndReturn(func(_ context.Context, c commands.Command) (any, error) {
		assert.Equal(t, commands.Type(control.CmdEnvelope), c.Type)
		return map[string]any{"ok": false}, nil
	})
	rec := serve(mux, castRequest(control.CmdEnvelope, nil))
	assert.Equal(t, http.StatusOK, rec.Code)

	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111/api/v2/status", nil)
	rec = serve(mux, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.NotContains(t, rec.Body.String(), `"control"`)
	assert.Contains(t, rec.Body.String(), `"topic_id":"`+testTopic+`"`)
}

// newControlHub is newGateHub plus a stub control gate in state st.
func newControlHub(t *testing.T, claim state.ClaimInfo, st control.State) (*http.ServeMux, *mocks.MockCommandHandler, *stubGate) {
	t.Helper()
	ctrl := gomock.NewController(t)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()
	provider := fixedStatus{TopicID: claim.TopicID, Claimed: claim.Claimed}
	h := New(context.Background(), mocks.NewMockWS(ctrl), mockCmd, provider, &fakeScreenshotCapturer{image: &screenshot.Image{Data: []byte("png"), Width: 1, Height: 1}}, mockServer,
		wrapper.NewJSON(), zap.NewNop()).(*hub)
	h.claimSnapshot = func() state.ClaimInfo { return claim }
	if st == "" {
		return mux, mockCmd, nil // control disabled
	}
	g := &stubGate{state: st}
	h.SetControl(g)
	return mux, mockCmd, g
}

type fixedStatus StatusInfo

func (f fixedStatus) Status(context.Context) StatusInfo { return StatusInfo(f) }

func TestControlEnvelopeRoute(t *testing.T) {
	mux, _, g := newControlHub(t, claimedWithTopic, control.StateEnrolled)
	body := `{"command":"ff1Envelope","request":{"v":1,"jwe":"ok"}}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1111/api/cast", strings.NewReader(body))
	rec := serve(mux, req)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.JSONEq(t, `{"v":1,"jwe":"reply"}`, rec.Body.String())
	assert.Equal(t, `{"v":1,"jwe":"ok"}`, string(g.envelope))

	body = `{"command":"ff1Envelope","request":{"v":1,"jwe":"bad"}}`
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1111/api/cast", strings.NewReader(body))
	rec = serve(mux, req)
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.JSONEq(t, `{"error":{"code":"unknown_controller"}}`, rec.Body.String())

	// No request object at all.
	req = httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1111/api/cast", strings.NewReader(`{"command":"ff1Envelope"}`))
	assert.Equal(t, http.StatusUnauthorized, serve(mux, req).Code)
}

func TestControlPlaintextPolicyOnLAN(t *testing.T) {
	// Legacy: policy allows, and the phase-0 topic gate still guards
	// sshAccess on a claimed device.
	mux, mockCmd, _ := newControlHub(t, claimedWithTopic, control.StateLegacy)
	rec := serve(mux, castRequest("sshAccess", nil))
	assert.Equal(t, http.StatusUnauthorized, rec.Code)
	assert.Contains(t, rec.Body.String(), "topic_required")
	mockCmd.EXPECT().Process(gomock.Any(), gomock.Any()).Return(map[string]any{"ok": true}, nil)
	assert.Equal(t, http.StatusOK, serve(mux, castRequest("displayPlaylist", nil)).Code)

	for _, st := range []control.State{control.StateEnrolled, control.StateEnforced, control.StateLocked} {
		mux, _, _ := newControlHub(t, claimedWithTopic, st)
		rec := serve(mux, castRequest("displayPlaylist", strPtr(testTopic)))
		want := control.CodeEnrollmentRequired
		status := http.StatusForbidden
		if st == control.StateLocked {
			want, status = control.CodeLocked, http.StatusUnauthorized
		}
		assert.Equal(t, status, rec.Code, st)
		assert.JSONEq(t, `{"error":{"code":"`+want+`"}}`, rec.Body.String(), st)
	}
}

func TestControlStatusAndScreenshot(t *testing.T) {
	for _, st := range []control.State{control.StateLegacy, control.StateEnrolled} {
		mux, _, _ := newControlHub(t, unclaimed, st)
		rec := serve(mux, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111/api/v2/status", nil))
		require.Equal(t, http.StatusOK, rec.Code)
		assert.Contains(t, rec.Body.String(), `"control":{"v":1,"state":"`+string(st)+`"}`)
		if st == control.StateLegacy {
			assert.Contains(t, rec.Body.String(), `"topic_id":"`+testTopic+`"`)
		} else {
			// An owner exists before the relayer claim: the topic is withheld.
			assert.Contains(t, rec.Body.String(), `"topic_id":""`)
		}
	}
	mux, _, _ := newControlHub(t, unclaimed, control.StateEnrolled)
	req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111/api/screenshot", nil)
	rec := serve(mux, req)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":{"code":"enrollment_required"}}`, rec.Body.String())
}

// Review F1: in legacy, a live invitation closes /api/screenshot (an
// unclaimed device has no topic gate, and the claim QR carries the
// invitation).
func TestScreenshotRefusedWhileInvitationLive(t *testing.T) {
	mux, _, g := newControlHub(t, unclaimed, control.StateLegacy)
	g.protected = true
	rec := serve(mux, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111/api/screenshot", nil))
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.JSONEq(t, `{"error":{"code":"screen_protected"}}`, rec.Body.String())
	g.protected = false
	rec = serve(mux, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111/api/screenshot", nil))
	assert.Equal(t, http.StatusOK, rec.Code, "no invitation: phase-0 rules (unclaimed = open)")
}

// Review F4: the hub passes the LAN peer's IP (not authority) for the
// per-host pending cap.
func TestEnvelopeOriginIsTheLANPeer(t *testing.T) {
	mux, _, g := newControlHub(t, claimedWithTopic, control.StateEnrolled)
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1111/api/cast",
		strings.NewReader(`{"command":"ff1Envelope","request":{"v":1,"jwe":"ok"}}`))
	req.RemoteAddr = "192.168.1.77:51000"
	serve(mux, req)
	assert.Equal(t, control.Origin{Route: control.RouteLAN, Remote: "192.168.1.77", Known: true}, g.origin)
}

// raceCapturer mints an invitation (moves the stub's generation) while the
// capture is in flight.
type raceCapturer struct {
	g    *stubGate
	live bool
}

func (c *raceCapturer) Capture(context.Context, screenshot.Bounds) (*screenshot.Image, error) {
	c.g.gen++
	c.g.protected = c.live
	return &screenshot.Image{Data: []byte("png"), Width: 1, Height: 1}, nil
}

// Review N1: an invitation minted (or still live) during the capture
// discards the image.
func TestScreenshotDiscardedWhenInvitationRacesCapture(t *testing.T) {
	for _, live := range []bool{true, false} {
		ctrl := gomock.NewController(t)
		mockServer := mocks.NewMockHTTPServer(ctrl)
		mux := http.NewServeMux()
		mockServer.EXPECT().Handler().Return(mux).AnyTimes()
		g := &stubGate{state: control.StateLegacy}
		h := New(context.Background(), mocks.NewMockWS(ctrl), mocks.NewMockCommandHandler(ctrl), fixedStatus{},
			&raceCapturer{g: g, live: live}, mockServer, wrapper.NewJSON(), zap.NewNop()).(*hub)
		h.claimSnapshot = func() state.ClaimInfo { return unclaimed }
		h.SetControl(g)
		rec := serve(mux, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111/api/screenshot", nil))
		assert.Equal(t, http.StatusForbidden, rec.Code, "live=%v", live)
		assert.JSONEq(t, `{"error":{"code":"screen_protected"}}`, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), "png")
	}
}
