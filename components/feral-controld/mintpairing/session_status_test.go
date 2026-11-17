package mintpairing

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHandleGetPairingSession_ReadOnly(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		phase                     activePairingPhase
		expired, joined, starting bool
		wantStatus                string
	}{
		{name: "idle", wantStatus: "none"},
		{name: "starting", starting: true, wantStatus: "starting"},
		{name: "code", phase: activePairingPhasePairingCode, wantStatus: "active"},
		{name: "approval", phase: activePairingPhasePendingApproval, wantStatus: "active"},
		{name: "joined", phase: activePairingPhaseJoined, joined: true, wantStatus: "active"},
		{name: "joined deadline may extend", phase: activePairingPhaseJoined, joined: true, expired: true, wantStatus: "active"},
		{name: "expired code", phase: activePairingPhasePairingCode, expired: true, wantStatus: "none"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newTestService()
			s.opts.Enabled = true
			var canceled bool
			if tc.phase != "" {
				deadline := time.Now().Add(time.Minute)
				if tc.expired {
					deadline = time.Now().Add(-time.Minute)
				}
				s.active = &activePairing{channelID: "channel", pairingCode: "secret-code", phase: tc.phase,
					expiresAt: deadline, codeCreatedAt: time.Now().Add(-10 * time.Second), joined: tc.joined,
					cancel: func() { canceled = true }}
			}
			if tc.starting {
				s.starting = &startingPairing{}
			}
			before := s.active
			for range 3 {
				result, err := s.HandleGetPairingSession(context.Background(), nil)
				require.NoError(t, err)
				got := result.(pairingSessionStatus)
				assert.Equal(t, tc.wantStatus, got.Status)
				assert.Equal(t, tc.wantStatus == "active", got.Active)
				if got.Active {
					assert.Equal(t, "channel", got.ChannelID)
					assert.Equal(t, tc.phase, got.Phase)
					if tc.phase == activePairingPhasePairingCode {
						require.NotNil(t, got.CodeAgeSeconds)
						assert.GreaterOrEqual(t, *got.CodeAgeSeconds, int64(10))
					} else {
						assert.Nil(t, got.CodeAgeSeconds)
					}
				}
				raw, err := json.Marshal(got)
				require.NoError(t, err)
				assert.NotContains(t, string(raw), "secret-code")
				assert.True(t, before == s.active)
				assert.False(t, canceled)
			}
			// No broker, painter, player manifest, topic or running service was
			// supplied: any accidental I/O or start would fail these probes.
		})
	}
}

func TestHandleGetPairingSession_PendingApproval(t *testing.T) {
	s := newTestService()
	s.opts.Enabled = true
	s.active = &activePairing{channelID: "channel", phase: activePairingPhasePendingApproval, joined: true}
	p := &pendingApproval{channelID: "channel", approvalRequestID: "approval", expiresAt: time.Now().Add(time.Minute)}
	s.pending["approval"] = p
	result, err := s.HandleGetPairingSession(context.Background(), nil)
	require.NoError(t, err)
	require.Equal(t, "approval", result.(pairingSessionStatus).PendingApproval.ApprovalRequestID)
	for _, tc := range []struct {
		name   string
		change func()
	}{
		{"accepted", func() { p.accepted = &approvalDecisionRequest{} }},
		{"ended", func() { p.endedCode = "browser_gone" }},
		{"expired", func() { p.expiresAt = time.Now().Add(-time.Minute) }},
		{"other channel", func() { p.channelID = "other" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			saved := *p
			tc.change()
			result, err := s.HandleGetPairingSession(context.Background(), nil)
			require.NoError(t, err)
			assert.Nil(t, result.(pairingSessionStatus).PendingApproval)
			*p = saved
		})
	}
}

func TestHandleGetPairingSession_Disabled(t *testing.T) {
	for _, s := range []*service{nil, newTestService()} {
		result, err := s.HandleGetPairingSession(context.Background(), nil)
		require.NoError(t, err)
		assertCommandError(t, result, "disabled", false)
	}
}
