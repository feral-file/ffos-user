package main

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
)

// The ladder's "backend" is the v2 relay host when one is configured: once the
// legacy relayer is retired its host may not resolve, and probing it would
// classify every outage as backend-only-down.
func TestNetlogProbeEndpointPrefersRelayV2(t *testing.T) {
	for _, tc := range []struct{ v2, legacy, wantHost string }{
		{"wss://relayer.feralfile.io", "wss://tv-cast-coordination.autonomy-system.workers.dev", "relayer.feralfile.io"},
		{"", "wss://tv-cast-coordination.autonomy-system.workers.dev", "tv-cast-coordination.autonomy-system.workers.dev"},
		{"", "", "support-logs.feralfile.com"},
	} {
		got := netlogBackendHost(netlogProbeEndpoint(tc.v2, tc.legacy), zap.NewNop())
		assert.Equal(t, tc.wantHost, got)
	}
}
