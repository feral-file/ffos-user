package metric

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"
)

func TestGPUFrequencyPercent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		current float64
		max     float64
		want    float64
	}{
		{name: "zero max", current: 2200, max: 0, want: 0},
		{name: "amd qr case", current: 2200, max: 2200, want: 100},
		{name: "legacy hardcoded max", current: 2200, max: 2000, want: 110},
		{name: "idle artwork", current: 400, max: 2200, want: 400.0 / 2200.0 * 100},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := gpuFrequencyPercent(tt.current, tt.max)
			if diff := got - tt.want; diff < -0.001 || diff > 0.001 {
				t.Fatalf("gpuFrequencyPercent() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseAMDMaxSclkMHz(t *testing.T) {
	t.Parallel()

	const sample = `0: 200Mhz 
1: 700Mhz 
2: 2200Mhz *
`

	maxMHz, err := parseAMDMaxSclkMHz(sample)
	if err != nil {
		t.Fatalf("parseAMDMaxSclkMHz() error = %v", err)
	}
	if maxMHz != 2200 {
		t.Fatalf("parseAMDMaxSclkMHz() = %v, want 2200", maxMHz)
	}
}

func TestMaxEngineBusyPercent(t *testing.T) {
	t.Parallel()

	busy := maxEngineBusyPercent(map[string]struct {
		Busy float64 `json:"busy"`
	}{
		"Render/3D": {Busy: 12.5},
		"Video":     {Busy: 3.0},
	})
	if busy != 12.5 {
		t.Fatalf("maxEngineBusyPercent() = %v, want 12.5", busy)
	}
}

func TestResolveGPUBusy(t *testing.T) {
	t.Parallel()

	busy, err := resolveGPUBusy(12.5, "")
	if err != nil || busy != 12.5 {
		t.Fatalf("engine busy prefer = (%v, %v), want (12.5, nil)", busy, err)
	}

	busy, err = resolveGPUBusy(0, "")
	if !errors.Is(err, errBestEffortMetricUnavailable) || busy != 0 {
		t.Fatalf("missing device path = (%v, %v), want (0, unavailable)", busy, err)
	}
}

func TestApplyGPUDerivedPercentages(t *testing.T) {
	t.Parallel()

	monitor := NewSysResMonitor(context.Background(), zap.NewNop())
	monitor.lastMetrics.GPU.CurrentFrequency = 900
	monitor.lastMetrics.GPU.MaxFrequency = 1800

	monitor.applyGPUDerivedPercentages()

	if monitor.lastMetrics.GPU.FrequencyPercent != 50 {
		t.Fatalf("FrequencyPercent = %v, want 50", monitor.lastMetrics.GPU.FrequencyPercent)
	}
}
