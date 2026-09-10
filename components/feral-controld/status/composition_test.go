package status

import (
	"encoding/json"
	"strings"
	"testing"
)

// Exercise the typed decode and lightweight notification path: an omitted
// field here silently disappears even though checkStatus reported it.
func TestCompositionRoundTrip(t *testing.T) {
	for _, margin := range []string{`"12%"`, `24`, `0`} {
		t.Run(margin, func(t *testing.T) {
			raw := []byte(`{"ok":true,"index":0,"items":[],"deviceSettings":{
				"showingKey":"71f1273d-4d1d-4a9a-b487-41a4a2275b03",
				"compositionRevision":3,"scaling":"fill","margin":` + margin + `,"background":"#AABBCC"}}`)
			var status PlayerStatus
			if err := json.Unmarshal(raw, &status); err != nil {
				t.Fatal(err)
			}
			encoded, err := json.Marshal((&poller{}).lightweightPlayerStatus(&status))
			if err != nil {
				t.Fatal(err)
			}
			var before, after map[string]json.RawMessage
			if err := json.Unmarshal(raw, &before); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(encoded, &after); err != nil {
				t.Fatal(err)
			}
			var expected, actual map[string]any
			if err := json.Unmarshal(before["deviceSettings"], &expected); err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(after["deviceSettings"], &actual); err != nil {
				t.Fatal(err)
			}
			for key, value := range expected {
				if actual[key] != value {
					t.Errorf("deviceSettings.%s = %v, want %v", key, actual[key], value)
				}
			}
		})
	}
}

func TestAbsentCompositionStaysAbsent(t *testing.T) {
	var status PlayerStatus
	if err := json.Unmarshal([]byte(`{"deviceSettings":{"scaling":"fit"}}`), &status); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(status.DeviceSettings)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"scaling":"fit"}` {
		t.Fatalf("invented composition fields: %s", encoded)
	}
}

// The player generates a source-free UUID before status crosses the typed
// bridge. Notification trimming must also remove the signed item source.
func TestCompositionNotificationDoesNotExposeSource(t *testing.T) {
	const signedSource = "https://cdn.example.com/work.png?signature=private-access-token"
	raw := []byte(`{"items":[{"source":"` + signedSource + `"}],"deviceSettings":{
        "showingKey":"71f1273d-4d1d-4a9a-b487-41a4a2275b03","margin":"10%"}}`)
	var status PlayerStatus
	if err := json.Unmarshal(raw, &status); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal((&poller{}).lightweightPlayerStatus(&status))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), signedSource) || strings.Contains(string(encoded), "private-access-token") {
		t.Fatalf("notification exposes source credentials: %s", encoded)
	}
	if status.DeviceSettings.ShowingKey == nil || *status.DeviceSettings.ShowingKey != "71f1273d-4d1d-4a9a-b487-41a4a2275b03" {
		t.Fatal("notification dropped the opaque showing UUID")
	}
}
