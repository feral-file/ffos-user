package controlgate

import (
	"context"
	"encoding/json"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

// Publisher sends sealed notifications on both channels (§9): the relayer
// notification channel (when connected) and every LAN notification WebSocket.
type Publisher struct {
	relayer relayer.Relayer
	ws      ws.WS
	logger  *zap.Logger
}

// NewPublisher builds a publisher over the RAW (unwrapped) handles.
func NewPublisher(r relayer.Relayer, w ws.WS, logger *zap.Logger) *Publisher {
	return &Publisher{relayer: r, ws: w, logger: logger}
}

// PublishAdmission sends the notifications an admission produced: the
// relayer gets the authority's sealed copies, the LAN WebSocket a separately
// sealed copy of each (§9: one seal, and one seq, per channel; review L1).
func (p *Publisher) PublishAdmission(ctx context.Context, auth *control.Authority, adm *control.Admission) {
	if p.relayer != nil && p.relayer.IsConnected() {
		for _, s := range adm.Notifications {
			if wire, err := s.Wire(); err == nil {
				if err := p.relayer.Send(ctx, json.RawMessage(wire)); err != nil {
					p.logger.Warn("Failed to send sealed notification via relayer", zap.Error(err))
				}
			}
		}
	}
	if p.ws == nil {
		return
	}
	for _, n := range adm.Notices {
		s, err := auth.Seal(n.Ctl, n.Type, n.Body)
		if err != nil {
			continue
		}
		if wire, err := s.Wire(); err == nil {
			if err := p.ws.SendAll(json.RawMessage(wire)); err != nil {
				p.logger.Warn("Failed to send sealed notification via websocket", zap.Error(err))
			}
		}
	}
}

// notification extracts the type and message of a notification envelope as
// the producers build it (status poller and offline cache: a map; mint
// pairing: relayer.Response). ok is false for anything else (RPC replies).
type notificationMeta struct {
	Type               string          `json:"type"`
	NotificationType   string          `json:"notification_type"`
	Message            json.RawMessage `json:"message"`
	MessageID          string          `json:"messageID"`
	PersistRecordCount int             `json:"persist_record_count"`
}

func notification(data any) (notificationMeta, bool) {
	b, err := json.Marshal(data)
	if err != nil {
		return notificationMeta{}, false
	}
	var n notificationMeta
	if json.Unmarshal(b, &n) != nil || n.Type != "notification" || n.NotificationType == "" {
		return notificationMeta{}, false
	}
	return n, true
}

// channelPlan says, for the current state, whether plaintext still goes out
// on a channel and whether sealed copies go out on it (§9 table). A locked
// device cannot seal (no readable grants), so it keeps today's plaintext.
func channelPlan(st control.State, relayerChannel bool) (plaintext, sealed bool) {
	switch st {
	case control.StateEnrolled:
		return relayerChannel, true // relayer keeps plaintext for old apps; LAN WS stops
	case control.StateEnforced:
		return false, true
	default: // legacy, locked
		return true, false
	}
}

// SealingRelayer wraps the relayer handle given to notification producers.
// RPC replies and every non-notification pass through untouched.
type SealingRelayer struct {
	relayer.Relayer
	auth   *control.Authority
	logger *zap.Logger
}

// WrapRelayer returns r wrapped for notification sealing.
func WrapRelayer(r relayer.Relayer, auth *control.Authority, logger *zap.Logger) *SealingRelayer {
	return &SealingRelayer{Relayer: r, auth: auth, logger: logger}
}

// Send applies the §9 plan to notifications.
func (s *SealingRelayer) Send(ctx context.Context, data any) error {
	n, ok := notification(data)
	if !ok {
		return s.Relayer.Send(ctx, data)
	}
	plain, sealed := channelPlan(s.auth.State(), true)
	var firstErr error
	if plain {
		firstErr = s.Relayer.Send(ctx, data)
	}
	if sealed {
		for _, w := range sealAll(s.auth, n, s.logger) {
			if err := s.Relayer.Send(ctx, w); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// SealingWS wraps the WebSocket handle given to notification producers.
type SealingWS struct {
	ws.WS
	auth   *control.Authority
	logger *zap.Logger
}

// WrapWS returns w wrapped for notification sealing.
func WrapWS(w ws.WS, auth *control.Authority, logger *zap.Logger) *SealingWS {
	return &SealingWS{WS: w, auth: auth, logger: logger}
}

// SendAll applies the §9 plan to notifications broadcast on the LAN WS.
func (s *SealingWS) SendAll(message any) error {
	n, ok := notification(message)
	if !ok {
		return s.WS.SendAll(message)
	}
	plain, sealed := channelPlan(s.auth.State(), false)
	var firstErr error
	if plain {
		firstErr = s.WS.SendAll(message)
	}
	if sealed {
		for _, w := range sealAll(s.auth, n, s.logger) {
			if err := s.WS.SendAll(w); err != nil && firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func sealAll(auth *control.Authority, n notificationMeta, logger *zap.Logger) []json.RawMessage {
	msg := n.Message
	if len(msg) == 0 {
		msg = json.RawMessage("null")
	}
	sealed, err := auth.SealAll(n.NotificationType, msg)
	if err != nil {
		logger.Warn("Failed to seal notification", zap.String("notification_type", n.NotificationType), zap.Error(err))
		return nil
	}
	out := make([]json.RawMessage, 0, len(sealed))
	for _, s := range sealed {
		if w, err := s.WireWith(n.PersistRecordCount, n.MessageID); err == nil {
			out = append(out, w)
		}
	}
	return out
}
