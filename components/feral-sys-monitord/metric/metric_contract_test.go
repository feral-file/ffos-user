package metric

import (
	"encoding/json"
	"testing"
	"time"
)

func TestSysMetricsDBusPreservesGpuContractFields(t *testing.T) {
	t.Parallel()

	metrics := SysMetrics{
		GPU: GPUMetrics{
			CurrentFrequency:   2200,
			MaxFrequency:       2200,
			CurrentTemperature: 42,
			MaxTemperature:     95,
			GPUBusy:            0,
		},
		Timestamp: time.Unix(123, 456000000),
	}

	payload, err := json.Marshal(metrics)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}

	var decoded SysMetrics
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded.GPU.CurrentFrequency != 2200 {
		t.Fatalf("decoded GPU current frequency = %v, want 2200", decoded.GPU.CurrentFrequency)
	}
	if decoded.GPU.MaxFrequency != 2200 {
		t.Fatalf("decoded GPU max frequency = %v, want 2200", decoded.GPU.MaxFrequency)
	}
	if decoded.GPU.GPUBusy != 0 {
		t.Fatalf("decoded GPU busy = %v, want 0", decoded.GPU.GPUBusy)
	}
}

func TestSysMetricsDBusAcceptsMissingGpuBusyField(t *testing.T) {
	t.Parallel()

	const payload = `{"gpu":{"max_frequency":2200,"current_frequency":2200,"current_temperature":42,"max_temperature":95}}`

	var decoded SysMetrics
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatalf("json.Unmarshal() error = %v", err)
	}

	if decoded.GPU.GPUBusy != 0 {
		t.Fatalf("decoded GPU busy = %v, want 0 for omitted field", decoded.GPU.GPUBusy)
	}
	if decoded.GPU.CurrentFrequency != 2200 {
		t.Fatalf("decoded GPU current frequency = %v, want 2200", decoded.GPU.CurrentFrequency)
	}
}
