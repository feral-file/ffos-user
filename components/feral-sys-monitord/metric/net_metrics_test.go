package metric

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gatherGaugeValue scans the package registry for a family and returns its
// single-series gauge value. found=false means the family is not exported at
// all — the "absent until first probe" contract, distinct from value 0.
func gatherGaugeValue(t *testing.T, name string) (value float64, found bool) {
	t.Helper()
	families, err := MetricsGatherer().Gather()
	require.NoError(t, err)
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		require.Len(t, mf.GetMetric(), 1, "expected exactly one series for %s", name)
		return mf.GetMetric()[0].GetGauge().GetValue(), true
	}
	return 0, false
}

// TestInternetReachabilityGauges pins the stage-0 export contract
// (docs/wan-outage-observability.md): the connectivity gauges are ABSENT from
// the registry until the first applied probe — a scrape racing daemon startup
// must not read a fabricated "offline" sample — and thereafter mirror each
// applied verdict. Runs before Set is called anywhere else in this package's
// tests, which is what makes the absence half assertable.
func TestInternetReachabilityGauges(t *testing.T) {
	_, found := gatherGaugeValue(t, "net_internet_reachable")
	assert.False(t, found, "net_internet_reachable must be absent before the first applied probe")
	_, found = gatherGaugeValue(t, "net_internet_probe_duration_seconds")
	assert.False(t, found, "net_internet_probe_duration_seconds must be absent before the first applied probe")

	SetInternetReachability(true, 1500*time.Millisecond)
	v, found := gatherGaugeValue(t, "net_internet_reachable")
	require.True(t, found)
	assert.Equal(t, 1.0, v)
	v, found = gatherGaugeValue(t, "net_internet_probe_duration_seconds")
	require.True(t, found)
	assert.Equal(t, 1.5, v)

	SetInternetReachability(false, 5*time.Second)
	v, _ = gatherGaugeValue(t, "net_internet_reachable")
	assert.Equal(t, 0.0, v)
	v, _ = gatherGaugeValue(t, "net_internet_probe_duration_seconds")
	assert.Equal(t, 5.0, v)
}
