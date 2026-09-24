package status

import (
	"encoding/json"
	"testing"
)

func TestDeviceFramingRoundTrip(t *testing.T) {
	for _, mode := range []string{"artwork", "fit", "fill"} {
		t.Run(mode, func(t *testing.T) {
			var status PlayerStatus
			if err := json.Unmarshal([]byte(`{"deviceSettings":{"scaling":"fit","framing":"`+mode+`"}}`), &status); err != nil {
				t.Fatal(err)
			}
			if status.DeviceSettings.Framing == nil || *status.DeviceSettings.Framing != mode {
				t.Fatal("saved framing lost during player status decode")
			}
			encoded, err := json.Marshal((&poller{}).lightweightPlayerStatus(&status))
			if err != nil {
				t.Fatal(err)
			}
			var wire struct {
				DeviceSettings map[string]any `json:"deviceSettings"`
			}
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			if wire.DeviceSettings["framing"] != mode || wire.DeviceSettings["scaling"] != "fit" {
				t.Fatalf("saved preference must stay separate from rendering: %s", encoded)
			}
		})
	}
}

func TestUnsupportedFramingStaysAbsent(t *testing.T) {
	var status PlayerStatus
	if err := json.Unmarshal([]byte(`{"deviceSettings":{"scaling":"fit"}}`), &status); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status.DeviceSettings)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"scaling":"fit"}` {
		t.Fatalf("unexpected framing capability: %s", encoded)
	}
}
