package controlgate

import (
	"context"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// V2Peer exposes the routing handle so tests can stand in fake relay peers.
type V2Peer = v2Peer

// SetSource replaces how a request context names its relay peer.
func (r *RelayV2) SetSource(fn func(context.Context) (V2Peer, bool)) { r.source = fn }

// GrantsChanged runs the watching hook directly.
func (r *RelayV2) GrantsChanged(ctx context.Context, adm *control.Admission) {
	r.grantsChanged(ctx, adm)
}

// Admitted runs the gate's admission hook directly.
func (r *RelayV2) Admitted(ctx context.Context, adm *control.Admission) { r.admitted(ctx, adm) }

// Bound returns the peers currently bound to ctl.
func (r *RelayV2) Bound(ctl string) []V2Peer {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []V2Peer
	for p, b := range r.bindings {
		if b.ctl == ctl {
			out = append(out, p)
		}
	}
	return out
}

// ReleaseAfterReply runs what Handle does once a reply has gone out.
func (r *RelayV2) ReleaseAfterReply(p V2Peer) { r.releaseAfterReply(p) }
