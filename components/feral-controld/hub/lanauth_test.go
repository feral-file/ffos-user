package hub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const testTopic = "topic-0123456789abcdef"

var (
	claimedWithTopic = state.ClaimInfo{DeviceID: "phone-1", Claimed: true, TopicID: testTopic, TopicReady: true}
	claimedNoTopic   = state.ClaimInfo{DeviceID: "phone-1", Claimed: true}
	unclaimed        = state.ClaimInfo{TopicID: testTopic, TopicReady: true}
)

// newGateHub builds a hub on a real mux with real JSON, pinned to claim, so
// requests run the whole route: middleware seam, single body decode, the
// cast-level gate, and the command router mock.
func newGateHub(t *testing.T, claim state.ClaimInfo) (*http.ServeMux, *mocks.MockCommandHandler, *fakeScreenshotCapturer) {
	t.Helper()
	ctrl := gomock.NewController(t)
	t.Cleanup(ctrl.Finish)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()
	capturer := &fakeScreenshotCapturer{image: &screenshot.Image{Data: []byte("png"), Width: 1, Height: 1}}

	h := New(context.Background(), mocks.NewMockWS(ctrl), mockCmd, nil, capturer, mockServer, wrapper.NewJSON(), logger).(*hub)
	h.claimSnapshot = func() state.ClaimInfo { return claim }
	return mux, mockCmd, capturer
}

func castRequest(command string, topicHeader *string) *http.Request {
	body := `{"command":"` + command + `","request":{}}`
	req := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:1111/api/cast", strings.NewReader(body))
	req.RemoteAddr = "192.168.1.10:41000"
	if topicHeader != nil {
		req.Header.Set(TopicHeader, *topicHeader)
	}
	return req
}

func strPtr(s string) *string { return &s }

// TestCastTopicGate pins the feral-file#3551 phase-0 policy on /api/cast:
// on a claimed device sshAccess needs X-FF1-Topic equal to the current topic
// (no topic yet = refuse); unclaimed devices and every other command —
// connect included, which the released app sends before every LAN cast
// without the header — are untouched. A refusal never reaches the command router — the
// mock carries no expectation, so any call through fails the test.
func TestCastTopicGate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		claim   state.ClaimInfo
		command commands.Type
		header  *string
		allowed bool
	}{
		{"claimed sshAccess without header", claimedWithTopic, commands.CMD_SSH_ACCESS, nil, false},
		{"claimed sshAccess with empty header", claimedWithTopic, commands.CMD_SSH_ACCESS, strPtr(""), false},
		{"claimed sshAccess with wrong header", claimedWithTopic, commands.CMD_SSH_ACCESS, strPtr("topic-wrong"), false},
		{"claimed sshAccess with topic prefix", claimedWithTopic, commands.CMD_SSH_ACCESS, strPtr(testTopic[:len(testTopic)-1]), false},
		{"claimed sshAccess with right header", claimedWithTopic, commands.CMD_SSH_ACCESS, strPtr(testTopic), true},
		{"claimed connect without header stays open (released app casts)", claimedWithTopic, commands.CMD_CONNECT, nil, true},
		{"claimed connect with wrong header stays open", claimedWithTopic, commands.CMD_CONNECT, strPtr("topic-wrong"), true},
		{"claimed but no topic yet refuses sshAccess even with a header", claimedNoTopic, commands.CMD_SSH_ACCESS, strPtr(testTopic), false},
		{"claimed with no topic yet leaves connect open", claimedNoTopic, commands.CMD_CONNECT, nil, true},
		{"claimed but no topic yet refuses an empty header", claimedNoTopic, commands.CMD_SSH_ACCESS, strPtr(""), false},
		{"unclaimed sshAccess stays open", unclaimed, commands.CMD_SSH_ACCESS, nil, true},
		{"unclaimed connect stays open (first claim)", unclaimed, commands.CMD_CONNECT, nil, true},
		{"claimed getDeviceStatus is not gated", claimedWithTopic, commands.CMD_DEVICE_STATUS, nil, true},
		{"claimed factoryReset stays open (phase-1 residual)", claimedWithTopic, commands.CMD_FACTORY_RESET, nil, true},
		{"claimed showPairingQRCode stays open", claimedWithTopic, commands.CMD_SHOW_PAIRING_QR_CODE, nil, true},
		{"claimed displayPlaylist ignores a wrong header", claimedWithTopic, commands.CMD_DISPLAY_PLAYLIST, strPtr("topic-wrong"), true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mux, mockCmd, _ := newGateHub(t, tt.claim)
			if tt.allowed {
				mockCmd.EXPECT().
					Process(gomock.Any(), gomock.Any()).
					DoAndReturn(func(_ context.Context, cmd commands.Command) (any, error) {
						assert.Equal(t, tt.command, cmd.Type)
						return map[string]bool{"ok": true}, nil
					}).
					Times(1)
			}

			w := httptest.NewRecorder()
			mux.ServeHTTP(w, castRequest(string(tt.command), tt.header))

			if tt.allowed {
				assert.Equal(t, http.StatusOK, w.Code)
				return
			}
			assertTopicRequired(t, w)
		})
	}
}

// TestScreenshotTopicGate: the screen can show the claim QR (any LAN caller
// can put it up with showPairingQRCode), so a screenshot of a claimed device
// needs the topic too; otherwise it would leak what status now withholds.
func TestScreenshotTopicGate(t *testing.T) {
	for _, tt := range []struct {
		name    string
		claim   state.ClaimInfo
		header  *string
		allowed bool
	}{
		{"claimed without header", claimedWithTopic, nil, false},
		{"claimed with wrong header", claimedWithTopic, strPtr("topic-wrong"), false},
		{"claimed with right header", claimedWithTopic, strPtr(testTopic), true},
		{"claimed but no topic yet", claimedNoTopic, strPtr(testTopic), false},
		{"unclaimed stays open", unclaimed, nil, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			mux, _, capturer := newGateHub(t, tt.claim)
			req := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111/api/screenshot", nil)
			if tt.header != nil {
				req.Header.Set(TopicHeader, *tt.header)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, req)

			if tt.allowed {
				assert.Equal(t, http.StatusOK, w.Code)
				assert.Equal(t, 1, capturer.calls)
				return
			}
			assertTopicRequired(t, w)
			assert.Zero(t, capturer.calls, "a refused screenshot must never capture")
		})
	}
}

// TestTopicGateLeavesOtherRoutesOpen: phase 0 gates only the listed surfaces;
// status on a claimed device still answers (without the topic) and needs no
// header, so the released app keeps discovering and reading claimed frames.
func TestTopicGateLeavesOtherRoutesOpen(t *testing.T) {
	mux, _, _ := newGateHub(t, claimedWithTopic)
	for _, path := range []string{"/api/status", "/api/v2/status", "/metrics"} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "http://127.0.0.1:1111"+path, nil))
		assert.Equal(t, http.StatusOK, w.Code, path)
	}
}

func assertTopicRequired(t *testing.T, w *httptest.ResponseRecorder) {
	t.Helper()
	require.Equal(t, http.StatusUnauthorized, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.JSONEq(t, `{"error":{"code":"topic_required"}}`, w.Body.String())
	assert.NotContains(t, w.Body.String(), testTopic, "a refusal must never echo the topic")
}
