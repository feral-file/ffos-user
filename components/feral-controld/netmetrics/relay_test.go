package netmetrics

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gathered returns name -> (label value or "") -> value for the families asked.
func gathered(t *testing.T, names ...string) map[string]map[string]float64 {
	t.Helper()
	families, err := registry.Gather()
	require.NoError(t, err)
	want := map[string]bool{}
	for _, n := range names {
		want[n] = true
	}
	out := map[string]map[string]float64{}
	for _, f := range families {
		if !want[f.GetName()] {
			continue
		}
		out[f.GetName()] = map[string]float64{}
		for _, m := range f.GetMetric() {
			label := ""
			if len(m.GetLabel()) > 0 {
				label = m.GetLabel()[0].GetValue()
			}
			out[f.GetName()][label] = m.GetGauge().GetValue()
		}
	}
	return out
}

// The retirement gauges read live state at scrape time, and a changed label
// (control state, reset target) replaces its series rather than adding one.
func TestRegisterDeviceStateExportsRetirementGauges(t *testing.T) {
	retired, topic, claimed := true, false, true
	ctlState, resetTarget := "legacy", "unknown"
	RegisterDeviceState(DeviceStateSources{
		LegacyRetired:      func() bool { return retired },
		LegacyTopicPresent: func() bool { return topic },
		Claimed:            func() bool { return claimed },
		ControlState:       func() string { return ctlState },
		ResetTargetVersion: func() string { return resetTarget },
	})
	SetRelayV2Connected(true)
	defer SetRelayV2Connected(false)
	assert.True(t, RelayV2Connected())

	names := []string{"control_state", "device_claimed", "legacy_topic_present", "relay_v2_connected",
		"relayer_legacy_retired", "reset_target_version_info"}
	got := gathered(t, names...)
	assert.Equal(t, map[string]float64{"legacy": 1}, got["control_state"])
	assert.Equal(t, map[string]float64{"unknown": 1}, got["reset_target_version_info"])
	assert.Equal(t, map[string]float64{"": 1}, got["relayer_legacy_retired"])
	assert.Equal(t, map[string]float64{"": 0}, got["legacy_topic_present"])
	assert.Equal(t, map[string]float64{"": 1}, got["device_claimed"])
	assert.Equal(t, map[string]float64{"": 1}, got["relay_v2_connected"])

	ctlState, resetTarget, topic = "enrolled", "2.0.14", true
	got = gathered(t, names...)
	assert.Equal(t, map[string]float64{"enrolled": 1}, got["control_state"], "the old state's series must not linger")
	assert.Equal(t, map[string]float64{"2.0.14": 1}, got["reset_target_version_info"])
	assert.Equal(t, map[string]float64{"": 1}, got["legacy_topic_present"])
}
