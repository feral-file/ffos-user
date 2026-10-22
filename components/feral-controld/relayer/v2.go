package relayer

import (
	"context"
	"errors"
	"regexp"
	"time"

	"go.uber.org/zap"

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

// retryV2 keeps an optional endpoint outage local, including configuration/404
// failures. It never exits controld or disables a healthy legacy/LAN carrier.
func (r *relayer) retryV2(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := r.Connect(ctx)
		if err == nil || errors.Is(err, ErrAlreadyConnected) {
			return nil
		}
		r.logger.Warn("Optional v2 connection unavailable; retrying", zap.Error(err))
		r.Lock()
		done := r.done
		r.Unlock()
		timer := time.NewTimer(5 * time.Second)
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
}
