package main

import (
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
)

// spyNarrationUI records the narration calls the notifier makes.
type spyNarrationUI struct {
	calls []string
}

func (s *spyNarrationUI) ShowScanning()                 { s.calls = append(s.calls, "scanning") }
func (s *spyNarrationUI) ShowSoftAPQR(ssid, psk string) { s.calls = append(s.calls, "softap") }
func (s *spyNarrationUI) ShowJoinFailed(reason string)  { s.calls = append(s.calls, "join_failed") }
func (s *spyNarrationUI) ShowJoining()                  { s.calls = append(s.calls, "joining") }
func (s *spyNarrationUI) Hide()                         { s.calls = append(s.calls, "hide") }

// TestSetupNotifierHidesOnlyOwnNarration is the G6 regression: a connectivity
// flap (OfflineRetrying→Online) while the EXECUTOR's claim QR owns the overlay
// must not Hide it — the provisioning notifier may only clear narration it
// painted itself.
func TestSetupNotifierHidesOnlyOwnNarration(t *testing.T) {
	spy := &spyNarrationUI{}
	n := &setupNotifier{ui: spy}

	// Flap with no prior provisioning narration: no Hide.
	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{})
	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	if len(spy.calls) != 0 {
		t.Fatalf("flap without narration produced calls %v; want none", spy.calls)
	}

	// A real provisioning cycle: AP up → joining → online must hide exactly once.
	n.OnStateChange(provisioning.StateAPActive, provisioning.Detail{SSID: "FF1-x", PSK: "p"})
	n.OnStateChange(provisioning.StateJoining, provisioning.Detail{})
	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	want := []string{"softap", "joining", "hide"}
	if len(spy.calls) != len(want) {
		t.Fatalf("calls = %v, want %v", spy.calls, want)
	}
	for i := range want {
		if spy.calls[i] != want[i] {
			t.Fatalf("calls = %v, want %v", spy.calls, want)
		}
	}

	// A second →Online (already hidden) must not hide again.
	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	if len(spy.calls) != len(want) {
		t.Fatalf("re-online hid again: calls = %v", spy.calls)
	}
}

// TestSetupNotifierNarratesScanning: the machine's pre-raise scanning
// announcement maps to ShowScanning, and it counts as this surface's own
// narration for the hide-guard (Online afterwards hides it).
func TestSetupNotifierNarratesScanning(t *testing.T) {
	spy := &spyNarrationUI{}
	n := &setupNotifier{ui: spy}

	n.OnStateChange(provisioning.StateAPActive,
		provisioning.Detail{Reason: "scanning", Message: "Looking for nearby Wi-Fi networks"})
	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})

	if len(spy.calls) != 2 || spy.calls[0] != "scanning" || spy.calls[1] != "hide" {
		t.Fatalf("calls = %v, want [scanning hide]", spy.calls)
	}
}
