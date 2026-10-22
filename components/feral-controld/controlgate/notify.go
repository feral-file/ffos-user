package controlgate

import (
	"context"
	"encoding/json"
	"time"

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
	v2      *RelayV2
}

// NewPublisher builds a publisher over the RAW (unwrapped) handles.
func NewPublisher(r relayer.Relayer, w ws.WS, logger *zap.Logger) *Publisher {
	return &Publisher{relayer: r, ws: w, logger: logger}
}

// SetRelayV2 adds the independent targeted carrier before serving.
func (p *Publisher) SetRelayV2(v2 *RelayV2) { p.v2 = v2 }

// publishTimeout bounds the relayer sends of one admission's notifications.
const publishTimeout = 10 * time.Second

// PublishAdmission sends the notifications an admission produced: the
// relayer gets the authority's sealed copies, the LAN WebSocket a separately
// sealed copy of each (§9: one seal, and one seq, per channel; review L1).
//
// The sends deliberately do not use the requester's context. A relay that
// routes by a source carried in the context (the v2 device session replies
// to the peer a request came from) would otherwise deliver the owners'
// approval requests to the requester instead of the owners.
func (p *Publisher) PublishAdmission(_ context.Context, auth *control.Authority, adm *control.Admission) {
	if p.v2 != nil {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
		for _, n := range adm.Notices {
			p.v2.Notify(ctx, n.Type, n.Body, n.Ctl)
		}
	}
	if p.relayer != nil && p.relayer.IsConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), publishTimeout)
		defer cancel()
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
	NotifyV2    func(context.Context, string, any)
	V2Connected func() bool
	auth        *control.Authority
	logger      *zap.Logger
}

// WrapRelayer returns r wrapped for notification sealing.
func WrapRelayer(r relayer.Relayer, auth *control.Authority, logger *zap.Logger) *SealingRelayer {
	return &SealingRelayer{Relayer: r, auth: auth, logger: logger}
}

// IsConnected keeps notification producers active when either transport works.
func (s *SealingRelayer) IsConnected() bool {
	return s.Relayer.IsConnected() || (s.V2Connected != nil && s.V2Connected())
}

// Send applies the §9 plan to notifications.
func (s *SealingRelayer) Send(ctx context.Context, data any) error {
	n, ok := notification(data)
	if !ok {
		return s.Relayer.Send(ctx, data)
	}
	if s.NotifyV2 != nil {
		s.NotifyV2(ctx, n.NotificationType, n.Message)
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
		// Sealed copies are never persisted (§9): every copy shares the one
		// relayer type "sealed", and the relayer keeps the newest N rows per
		// type, so stored rows would be whichever controller's copy came
		// last. A reconnecting controller refetches state by envelope.
		if w, err := s.WireWith(0, n.MessageID); err == nil {
			out = append(out, w)
		}
	}
	return out
}
