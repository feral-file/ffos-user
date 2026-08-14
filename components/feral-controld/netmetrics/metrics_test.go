package netmetrics

import (
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatherFamily returns the series of one metric family, or nil when the
// family is not exported at all (absence is part of the contract under test).
func gatherFamily(t *testing.T, name string) []*dto.Metric {
	t.Helper()
	families, err := Gatherer().Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() == name {
			return mf.GetMetric()
		}
	}
	return nil
}

func gaugeValue(t *testing.T, name string) float64 {
	t.Helper()
	series := gatherFamily(t, name)
	require.Len(t, series, 1, "expected exactly one series for %s", name)
	return series[0].GetGauge().GetValue()
}

// TestExportAbsenceContract pins which gauges are eagerly exported and which
// stay absent until a real value lands (docs/wan-outage-observability.md
// stage 0: absence must mean "never measured", never alias with 0). It runs
// first in this package's test order, which is what makes absence assertable
// against the shared registry.
func TestExportAbsenceContract(t *testing.T) {
	assert.Nil(t, gatherFamily(t, "net_link_state"),
		"net_link_state must be absent before the first probe")
	assert.Nil(t, gatherFamily(t, "net_wifi_signal_percent"),
		"net_wifi_signal_percent must be absent before the first association")
	assert.Nil(t, gatherFamily(t, "net_wifi_link_info"),
		"net_wifi_link_info must be absent before the first association")
	assert.Nil(t, gatherFamily(t, "relayer_last_close_code"),
		"relayer_last_close_code must be absent until a close frame is observed")

	// relayer_connected is the deliberate exception: 0 at start is the truth.
	assert.Equal(t, 0.0, gaugeValue(t, "relayer_connected"))
	require.Len(t, gatherFamily(t, "relayer_disconnects_total"), 1)
	assert.Equal(t, 0.0, gatherFamily(t, "relayer_disconnects_total")[0].GetCounter().GetValue())
}

func TestRelayerSetters(t *testing.T) {
	SetRelayerConnected(true)
	assert.Equal(t, 1.0, gaugeValue(t, "relayer_connected"))

	RecordRelayerCloseCode(1006)
	assert.Equal(t, 1006.0, gaugeValue(t, "relayer_last_close_code"))

	before := gatherFamily(t, "relayer_disconnects_total")[0].GetCounter().GetValue()
	RecordRelayerDisconnect()
	SetRelayerConnected(false)
	assert.Equal(t, 0.0, gaugeValue(t, "relayer_connected"))
	assert.Equal(t, before+1, gatherFamily(t, "relayer_disconnects_total")[0].GetCounter().GetValue())
}

// TestWifiStationSeriesLifecycle: exactly one info series may be live (a BSS
// roam replaces, never accumulates), and clearing drops the series entirely.
func TestWifiStationSeriesLifecycle(t *testing.T) {
	SetWifiStation(70, "AA:BB:CC:DD:EE:FF", "36")
	assert.Equal(t, 70.0, gaugeValue(t, "net_wifi_signal_percent"))
	info := gatherFamily(t, "net_wifi_link_info")
	require.Len(t, info, 1)
	firstHash := labelValue(t, info[0], "bssid_hash")
	assert.Len(t, firstHash, 8)
	assert.NotEqual(t, "AA:BB:CC:DD:EE:FF", firstHash, "raw BSSID must never be exported")
	assert.Equal(t, "36", labelValue(t, info[0], "channel"))

	// Roam to a different BSS: the old series must be gone, not siblinged.
	SetWifiStation(41, "11:22:33:44:55:66", "11")
	info = gatherFamily(t, "net_wifi_link_info")
	require.Len(t, info, 1, "a roam must replace the info series, not accumulate")
	assert.NotEqual(t, firstHash, labelValue(t, info[0], "bssid_hash"))

	// Same input must hash identically across calls (the timeline correlates
	// on it).
	SetWifiStation(70, "AA:BB:CC:DD:EE:FF", "36")
	assert.Equal(t, firstHash, labelValue(t, gatherFamily(t, "net_wifi_link_info")[0], "bssid_hash"))

	ClearWifiStation()
	assert.Nil(t, gatherFamily(t, "net_wifi_signal_percent"))
	assert.Nil(t, gatherFamily(t, "net_wifi_link_info"))
}

func labelValue(t *testing.T, m *dto.Metric, name string) string {
	t.Helper()
	for _, lp := range m.GetLabel() {
		if lp.GetName() == name {
			return lp.GetValue()
		}
	}
	t.Fatalf("label %s not found", name)
	return ""
}
