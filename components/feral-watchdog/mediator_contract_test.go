package main

import (
	"encoding/json"
	"testing"
)

func TestSysMetricsPayloadPreservesGpuBusy(t *testing.T) {
	t.Parallel()

	const payload = `{"gpu":{"max_frequency":2200,"current_frequency":2200,"current_temperature":42,"max_temperature":95,"gpu_busy":7.5}}`

	var metrics SysMetrics
	if err := json.Unmarshal([]byte(payload), &metrics); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if metrics.GPU.GPUBusy != 7.5 {
		t.Fatalf("decoded GPU busy = %v, want 7.5", metrics.GPU.GPUBusy)
	}
	if metrics.GPU.CurrentFrequency != 2200 {
		t.Fatalf("decoded GPU current frequency = %v, want 2200", metrics.GPU.CurrentFrequency)
	}
	if metrics.GPU.MaxFrequency != 2200 {
		t.Fatalf("decoded GPU max frequency = %v, want 2200", metrics.GPU.MaxFrequency)
	}
}
