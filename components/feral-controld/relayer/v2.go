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
	r.v2Wake = make(chan struct{}, 1)
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

// v2 redial backoff. §10: a refusal at connect time (HTTP 429 at the upgrade:
// the relay's `rate limited` and `capacity` answers) is transient, and so, for
// an optional carrier, is everything else, a malformed endpoint included; none
// of them may become a tight loop.
const (
	v2RetryFloor   = 5 * time.Second
	v2RetryCeiling = 5 * time.Minute
	// v2StableAfter is how long a session must have lasted for the next
	// redial to start the backoff over. A relay that accepts and then closes
	// at once keeps backing off.
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

// WakeV2 restarts the backoff at the floor and ends the current redial wait. The
// mediator calls it on an offline→online connectivity edge, the same edge that
// redials the legacy socket: the count grown while offline says nothing about
// the network the device just joined.
//
// The reset happens even while a socket is up: in the F2 shape the edge for
// the new network arrives while the socket dialed on the old one still looks
// connected, and when that socket dies the reset must still apply. The dial a
// wake releases counts as the first, so a failure there waits the floor. The
// token is sent only while no socket is up (a waiting loop is the only reader);
// retryV2 drains a token left by a wake that raced a dial. A wake racing the
// v2Up flag itself can still leave one, which skips one floor wait: accepted.
//
// Never blocks: it takes only v2Mu, which no dial holds (Connect holds the
// relayer mutex across the handshake), and the buffered token coalesces
// bursts, so it is safe from the mediator's signal handler. A no-op on the
// legacy instance.
func (r *relayer) WakeV2() {
	if r.v2Wake == nil {
		return
	}
	r.v2Mu.Lock()
	r.v2Attempts = 1
	r.v2Mu.Unlock()
	if r.v2Up.Load() {
		return
	}
	select {
	case r.v2Wake <- struct{}{}:
	default:
	}
}

// drainV2Wake drops a wake token that is no longer for a waiting loop.
func (r *relayer) drainV2Wake() {
	select {
	case <-r.v2Wake:
	default:
	}
}

// retryV2 keeps an optional endpoint outage local, including configuration/404
// failures. It never exits controld or disables a healthy legacy/LAN carrier.
func (r *relayer) retryV2(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		// v2ConnectedAt lives under the relayer mutex (Connect writes it), the
		// counter under v2Mu (WakeV2 writes it and must never wait on a dial).
		// Never hold both.
		r.Lock()
		stable := !r.v2ConnectedAt.IsZero() && r.clock.Now().Sub(r.v2ConnectedAt) >= v2StableAfter
		r.v2ConnectedAt = time.Time{}
		done := r.done
		r.Unlock()
		r.v2Mu.Lock()
		if stable {
			r.v2Attempts = 0
		}
		delay := v2RetryDelay(r.v2Attempts)
		r.v2Attempts++
		r.v2Mu.Unlock()
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
			case <-r.v2Wake:
				timer.Stop()
				r.logger.Info("Connectivity restored; redialing v2 now")
			case <-timer.C:
			}
		}
		err := r.Connect(ctx)
		if err == nil || errors.Is(err, ErrAlreadyConnected) {
			// A wake that raced this dial has done its job (the reset);
			// its token must not skip the wait after this session ends.
			r.drainV2Wake()
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
