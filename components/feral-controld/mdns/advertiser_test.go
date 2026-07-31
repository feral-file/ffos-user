package mdns

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestTXTRecords pins the advertised TXT contract. The `api` key is the
// discovery-time firmware gate: the pairing app requires it before treating a
// device as LAN-pairable, so its presence/value is load-bearing — as is
// `claimed` always being published (resolvers rely on the key's presence,
// never inferring from absence).
func TestTXTRecords(t *testing.T) {
	txt := txtRecords(DeviceInfo{ID: "ff1-abc", Name: "Living Room", Claimed: false})
	assert.Equal(t, []string{"id=ff1-abc", "name=Living Room", "claimed=false", "api=2"}, txt)

	txt = txtRecords(DeviceInfo{ID: "ff1-abc", Claimed: true})
	assert.Equal(t, []string{"id=ff1-abc", "claimed=true", "api=2"}, txt)

	// Even a degenerate record carries the claim flag and the api gate.
	txt = txtRecords(DeviceInfo{})
	assert.Equal(t, []string{"claimed=false", "api=2"}, txt)
}
