package mintpairing

import (
	"context"
	"time"
)

type pairingSessionStatus struct {
	OK              bool                   `json:"ok"`
	Status          string                 `json:"status"`
	Active          bool                   `json:"active"`
	Phase           activePairingPhase     `json:"phase,omitempty"`
	ChannelID       string                 `json:"channelID,omitempty"`
	ExpiresAt       string                 `json:"expiresAt,omitempty"`
	CodeAgeSeconds  *int64                 `json:"codeAgeSeconds,omitempty"`
	PendingApproval *pairingApprovalStatus `json:"pendingApproval"`
}

type pairingApprovalStatus struct {
	ApprovalRequestID string `json:"approvalRequestID"`
	ExpiresAt         string `json:"expiresAt"`
}

// HandleGetPairingSession reads only local session state. In particular it
// must not call currentActive: that accessor cancels expired sessions. The
// session worker, never a status probe, owns expiry and display cleanup.
func (s *service) HandleGetPairingSession(context.Context, map[string]any) (any, error) {
	if s == nil || !s.opts.Enabled {
		return commandError("disabled", "mint pairing is not enabled", false), nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	out := pairingSessionStatus{OK: true, Status: "none"}
	active := s.active
	if active == nil || (!active.joined && !active.expiresAt.IsZero() && !now.Before(active.expiresAt)) {
		if s.starting != nil && !s.starting.canceled {
			out.Status = "starting"
		}
		return out, nil
	}
	// A joined channel's broker poll can extend its deadline. Like the start
	// command, report it until its worker removes it rather than guessing from
	// a deadline that may be stale while that poll is in flight.
	out.Status = "active"
	out.Active = true
	out.Phase = active.phase
	out.ChannelID = active.channelID
	out.ExpiresAt = formatOptionalTime(active.expiresAt)
	if active.phase == activePairingPhasePairingCode && !active.codeCreatedAt.IsZero() {
		age := max(int64(0), int64(now.Sub(active.codeCreatedAt)/time.Second))
		out.CodeAgeSeconds = &age
	}
	for _, pending := range s.pending {
		if pending.channelID == active.channelID && pending.accepted == nil && pending.endedCode == "" && now.Before(pending.expiresAt) {
			out.PendingApproval = &pairingApprovalStatus{
				ApprovalRequestID: pending.approvalRequestID,
				ExpiresAt:         formatOptionalTime(pending.expiresAt),
			}
			break
		}
	}
	return out, nil
}
