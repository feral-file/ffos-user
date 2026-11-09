package netmetrics

import (
	"os"
	"path/filepath"
	"regexp"
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

// testDeviceState backs the retirement-gauge readers for every test in this
// package. RegisterDeviceState is a sync.Once over the shared registry, so
// whichever test registers first owns the readers for the whole run; routing
// all of them through these variables keeps the tests order-independent
// (each sets the fields it asserts on, then calls registerTestDeviceState).
var testDeviceState struct {
	retired, topic, claimed bool
	ctlState, resetTarget   string
}

func registerTestDeviceState() {
	RegisterDeviceState(DeviceStateSources{
		LegacyRetired:      func() bool { return testDeviceState.retired },
		LegacyTopicPresent: func() bool { return testDeviceState.topic },
		Claimed:            func() bool { return testDeviceState.claimed },
		ControlState:       func() string { return testDeviceState.ctlState },
		ResetTargetVersion: func() string { return testDeviceState.resetTarget },
	})
}

// The retirement gauges read live state at scrape time, and a changed label
// (control state, reset target) replaces its series rather than adding one.
func TestRegisterDeviceStateExportsRetirementGauges(t *testing.T) {
	testDeviceState.retired, testDeviceState.topic, testDeviceState.claimed = true, false, true
	testDeviceState.ctlState, testDeviceState.resetTarget = "legacy", "unknown"
	registerTestDeviceState()
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

	testDeviceState.ctlState, testDeviceState.resetTarget, testDeviceState.topic = "enrolled", "2.0.14", true
	got = gathered(t, names...)
	assert.Equal(t, map[string]float64{"enrolled": 1}, got["control_state"], "the old state's series must not linger")
	assert.Equal(t, map[string]float64{"2.0.14": 1}, got["reset_target_version_info"])
	assert.Equal(t, map[string]float64{"": 1}, got["legacy_topic_present"])
}

// vmagentRemoteWriteLabelNames reads the label names run-vmagent.sh stamps on
// every pushed sample, so reservedRemoteWriteLabels cannot drift from the
// script that defines them (the script ships on the other release rail).
func vmagentRemoteWriteLabelNames(t *testing.T) []string {
	t.Helper()
	script, err := os.ReadFile(filepath.Join("..", "..", "..", "users", "feralfile", "scripts", "run-vmagent.sh"))
	require.NoError(t, err)
	var names []string
	for _, m := range regexp.MustCompile(`-remoteWrite\.label="([a-z_]+)=`).FindAllStringSubmatch(string(script), -1) {
		names = append(names, m[1])
	}
	return names
}

// vmagent's -remoteWrite.label set replaces same-name labels on the backend
// (VictoriaMetrics appendExtraLabels), so a gauge label named like one of them
// is exported correctly on the device and silently wrong in the fleet store.
// reset_target_version_info{version} shipped that way in 3.0.0.
//
// Gather() only returns families that hold a series, so every labeled vec is
// populated first; this must therefore run after TestExportAbsenceContract
// (metrics_test.go sorts before this file), which asserts their absence.
func TestNoGaugeLabelCollidesWithRemoteWriteLabels(t *testing.T) {
	assert.Equal(t, vmagentRemoteWriteLabelNames(t), reservedRemoteWriteLabels,
		"reservedRemoteWriteLabels must mirror run-vmagent.sh")

	testDeviceState.ctlState, testDeviceState.resetTarget = "enrolled", "3.0.0"
	registerTestDeviceState()
	SetLinkState(LinkUp, LinkDown)
	SetWifiStation(50, "00:11:22:33:44:55", "6")
	defer ClearWifiStation()
	RecordRelayerCloseCode(1006)

	families, err := registry.Gather()
	require.NoError(t, err)
	reserved := map[string]bool{}
	for _, l := range reservedRemoteWriteLabels {
		reserved[l] = true
	}
	labelsOf := map[string][]string{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			for _, l := range m.GetLabel() {
				labelsOf[f.GetName()] = append(labelsOf[f.GetName()], l.GetName())
				assert.Falsef(t, reserved[l.GetName()], "%s uses label %q, which vmagent overwrites on remote write", f.GetName(), l.GetName())
			}
		}
	}
	// Every labeled family must have been gathered, or the check above
	// silently covered less than it claims.
	for _, name := range []string{"net_link_state", "net_wifi_link_info", "control_state", "reset_target_version_info"} {
		assert.NotEmptyf(t, labelsOf[name], "%s was not gathered; the reserved-label check did not see it", name)
	}
	assert.Equal(t, []string{"target_version"}, labelsOf["reset_target_version_info"])
}
