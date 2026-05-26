package metric

import (
	"errors"
	"testing"
)

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

	busy, ok := maxEngineBusyPercent(map[string]struct {
		Busy float64 `json:"busy"`
	}{
		"Render/3D": {Busy: 12.5},
		"Video":     {Busy: 3.0},
	})
	if !ok || busy != 12.5 {
		t.Fatalf("maxEngineBusyPercent() = (%v, %v), want (12.5, true)", busy, ok)
	}
}

func TestResolveGPUBusy(t *testing.T) {
	t.Parallel()

	busy, err := resolveGPUBusy(12.5, true, "")
	if err != nil || busy != 12.5 {
		t.Fatalf("engine busy prefer = (%v, %v), want (12.5, nil)", busy, err)
	}

	busy, err = resolveGPUBusy(0, false, "")
	if !errors.Is(err, errBestEffortMetricUnavailable) || busy != 0 {
		t.Fatalf("missing device path = (%v, %v), want (0, unavailable)", busy, err)
	}
}

func TestMaxEngineBusyPercent_PreservesZero(t *testing.T) {
	t.Parallel()

	busy, ok := maxEngineBusyPercent(map[string]struct {
		Busy float64 `json:"busy"`
	}{
		"Render/3D": {Busy: 0},
		"Video":     {Busy: 0},
	})
	if !ok {
		t.Fatal("expected busy data to be marked present")
	}
	if busy != 0 {
		t.Fatalf("maxEngineBusyPercent() = %v, want 0", busy)
	}
}
