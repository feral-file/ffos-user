package offlinecache

import "testing"

// TestTransport_BoundsHeaderMemoryAcrossAnAdmittedWave pins the response-header
// ceiling against the whole wave the storm gate can admit, not one response.
//
// The gate runs MaxConcurrent 16 with casting at Weight 4, so four casts are
// admitted together, and each preflight probes classifyConcurrency sources
// concurrently. Every probe holds its completed headers while the body stalls
// out the request timeout, so the resident worst case is
// casts x probes x MaxResponseHeaderBytes — reachable by an unauthenticated
// LAN caller against an OOM-sensitive device.
//
// The bound is asserted rather than the constant so that raising
// classifyConcurrency or the gate's weights fails here instead of quietly
// raising the ceiling with them.
func TestTransport_BoundsHeaderMemoryAcrossAnAdmittedWave(t *testing.T) {
	const (
		gateMaxConcurrent = 16 // commandrouter.defaultGateConfig
		castWeight        = 4  // commandrouter "heavy" policy
		maxWaveBytes      = 8 << 20
	)

	concurrentCasts := gateMaxConcurrent / castWeight
	limit := sourceGuard{}.transport().MaxResponseHeaderBytes
	worstCase := int64(concurrentCasts) * int64(classifyConcurrency) * limit

	if limit <= 0 {
		t.Fatalf("MaxResponseHeaderBytes must be set; Go's 10 MiB default is far too high here")
	}
	if worstCase > maxWaveBytes {
		t.Fatalf("a full admitted wave can hold %d bytes of attacker-chosen headers (%d casts x %d probes x %d); keep it under %d",
			worstCase, concurrentCasts, classifyConcurrency, limit, maxWaveBytes)
	}
}
