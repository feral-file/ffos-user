package mdns

import (
	"strings"
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

	// An unnamed device advertises its serial as the name rather than dropping
	// the key. Before owner-set names existed the name was ALWAYS the hostname,
	// so this keeps the record resolvers have always seen: clearing a custom
	// name must return the frame to its serial label, not remove the label.
	txt = txtRecords(DeviceInfo{ID: "ff1-abc", Claimed: true})
	assert.Equal(t, []string{"id=ff1-abc", "name=ff1-abc", "claimed=true", "api=2"}, txt)

	// Even a degenerate record carries the claim flag and the api gate.
	txt = txtRecords(DeviceInfo{})
	assert.Equal(t, []string{"claimed=false", "api=2"}, txt)
}

// TestInstanceLabel pins the DNS-SD label bound. The limit is 63 OCTETS on one
// label, and the rename path stops the existing advertisement before
// re-registering — so a label the resolver rejects does not merely fail to
// apply a name, it leaves the frame undiscoverable while the command reports
// success.
func TestInstanceLabel(t *testing.T) {
	assert.Equal(t, "Living Room", instanceLabel(DeviceInfo{ID: "ff1-abc", Name: "Living Room"}))

	// An unnamed unit labels itself with its serial.
	assert.Equal(t, "ff1-abc", instanceLabel(DeviceInfo{ID: "ff1-abc"}))

	// 32 accented runes are inside the display limit and outside the DNS one
	// (64 octets). The serial takes the label; the full name still rides TXT.
	accented := strings.Repeat("é", 32)
	assert.Greater(t, len(accented), maxInstanceLabelOctets)
	assert.Equal(t, "ff1-abc", instanceLabel(DeviceInfo{ID: "ff1-abc", Name: accented}))
	assert.Contains(t, txtRecords(DeviceInfo{ID: "ff1-abc", Name: accented}), "name="+accented)

	// Exactly at the ceiling still belongs to the owner.
	atLimit := strings.Repeat("a", maxInstanceLabelOctets)
	assert.Equal(t, atLimit, instanceLabel(DeviceInfo{ID: "ff1-abc", Name: atLimit}))

	// Nothing usable at all still registers something rather than failing.
	assert.Equal(t, "FF1", instanceLabel(DeviceInfo{}))
}
