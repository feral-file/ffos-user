package relayer

import (
	"context"
	"errors"
	"regexp"
	"time"

	"go.uber.org/zap"

	"github.com/gorilla/websocket"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// V2Eligible deliberately judges actual authority, not its config switch. Old
// package-only images omit the endpoint and must retain their legacy connection.
func V2Eligible(endpoint, deviceID string, authorityPresent bool) bool {
	return endpoint != "" && authorityPresent && regexp.MustCompile(`^FF1-[A-Z0-9]{8}$`).MatchString(deviceID)
}

// NewV2 creates an independent carrier. The caller supplies only RouteRelayV2
// gate handlers; no legacy system/topic messages may reach their state handlers.
func NewV2(endpoint, deviceID string, dialer wrapper.WebSocketDialer, randomizer wrapper.Randomizer,
	clock wrapper.Clock, os wrapper.OS, json wrapper.JSON, logger *zap.Logger) Relayer {
	r := New(endpoint, "", dialer, randomizer, clock, os, json, logger).(*relayer)
	r.deviceID = deviceID
	return r
}

// Peer identifies a source in one socket generation. It is routing context only;
// the security gate alone may associate it with an admitted controller.
type Peer struct{ source relaySource }

// SourcePeer exposes the source carried through request dispatch.
func SourcePeer(ctx context.Context) (Peer, bool) {
	s, ok := ctx.Value(relaySourceKey{}).(relaySource)
	return Peer{s}, ok
}

// Live rejects disconnected peers and replaced socket generations.
func (p Peer) Live() bool {
	if p.source.session == nil {
		return false
	}
	select {
	case <-p.source.session.done:
		return false
	default:
	}
	p.source.session.mu.Lock()
	defer p.source.session.mu.Unlock()
	return p.source.session.peers[p.source.peer] != nil
}

// Send targets exactly this peer and never redirects an old response on reconnect.
func (p Peer) Send(ctx context.Context, payload []byte) error {
	if !p.Live() {
		return ErrNotConnected
	}
	return p.source.session.sendPeer(ctx, p.source.peer, payload)
}

// SetPeerDisconnectHandler clears endpoint bindings when a peer or generation
// leaves. Install before connecting; the handler must not call the relayer.
func (r *relayer) SetPeerDisconnectHandler(fn func(Peer)) { r.peerDisconnected = fn }

// v2 redial backoff. §10: a refusal at connect or admission (HTTP 429, close
// code 1008) is transient, and so, for an optional carrier, is everything
// else, a malformed endpoint included; none of them may become a tight loop.
const (
	v2RetryFloor   = 5 * time.Second
	v2RetryCeiling = 5 * time.Minute
	// v2StableAfter is how long a session must have lasted for the next
	// redial to start the backoff over. A relay that accepts and then closes
	// at once (1008) keeps backing off.
	v2StableAfter = time.Minute
)

// v2RetryDelay is the base wait before dial attempt n since the last stable
// session: none for the first, then doubling from v2RetryFloor up to v2RetryCeiling.
func v2RetryDelay(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}
	d := v2RetryFloor
	for i := 1; i < attempt && d < v2RetryCeiling; i++ {
		d *= 2
	}
	return min(d, v2RetryCeiling)
}

// retryV2 keeps an optional endpoint outage local, including configuration/404
// failures. It never exits controld or disables a healthy legacy/LAN carrier.
func (r *relayer) retryV2(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		r.Lock()
		if !r.v2ConnectedAt.IsZero() {
			if time.Since(r.v2ConnectedAt) >= v2StableAfter {
				r.v2Attempts = 0
			}
			r.v2ConnectedAt = time.Time{}
		}
		delay := v2RetryDelay(r.v2Attempts)
		r.v2Attempts++
		done := r.done
		r.Unlock()
		if delay > 0 {
			// Jitter so devices refused together do not redial together.
			delay = r.randomizer.Duration(delay/2, delay)
			r.logger.Info("Waiting before v2 redial", zap.Duration("delay", delay))
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-done:
				timer.Stop()
				return ErrNotConnected
			case <-timer.C:
			}
		}
		err := r.Connect(ctx)
		if err == nil || errors.Is(err, ErrAlreadyConnected) {
			return nil
		}
		r.logger.Warn("Optional v2 connection unavailable; retrying", zap.Error(err))
	}
}

// §10 liveness: the relay closes a connection that shows no frame and no
// ping for 90 s, so the device sends the text frame "ping" well inside 30 s,
// jittered so a fleet reconnected together does not ping together. The relay
// auto-responds "pong", which the read loop drops.
const (
	v2PingFloor   = 20 * time.Second
	v2PingCeiling = 28 * time.Second
)

// v2Keepalive pings one socket generation until it, or the relayer, stops.
func (r *relayer) v2Keepalive(ctx context.Context, done, pingDone <-chan struct{}, s *deviceSession) {
	for {
		timer := time.NewTimer(r.randomizer.Duration(v2PingFloor, v2PingCeiling))
		select {
		case <-ctx.Done():
		case <-done:
		case <-pingDone:
		case <-s.done:
		case <-timer.C:
			if err := s.write(websocket.TextMessage, []byte("ping")); err != nil {
				r.logger.Debug("V2 keepalive ping failed", zap.Error(err))
			}
			continue
		}
		timer.Stop()
		return
	}
}
