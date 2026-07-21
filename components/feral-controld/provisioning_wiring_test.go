package main

import (
	"context"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/hub"
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

// TestSetupNotifierTriggersAutoClaimWhenReachable: Online and Unprovisioned
// both fire the executor's auto claim flow (the launcher-ui replacement) on a
// separate goroutine; the flow itself guards claimed/topic-less cases.
func TestSetupNotifierTriggersAutoClaimWhenReachable(t *testing.T) {
	spy := &spyNarrationUI{}
	fired := make(chan struct{}, 2)
	n := &setupNotifier{
		ui:       spy,
		claimCtx: context.Background(),
		claim:    func(context.Context) { fired <- struct{}{} },
	}

	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("auto claim not triggered on StateOnline")
	}

	n.OnStateChange(provisioning.StateUnprovisioned, provisioning.Detail{})
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("auto claim not triggered on StateUnprovisioned")
	}

	// AP states must not trigger it.
	n.OnStateChange(provisioning.StateAPActive, provisioning.Detail{Reason: "scanning"})
	select {
	case <-fired:
		t.Fatal("auto claim must not fire on StateAPActive")
	case <-time.After(50 * time.Millisecond):
	}
}

// stubHubStatusBase is a minimal hub.StatusProvider for wrapper tests.
type stubHubStatusBase struct{ info hub.StatusInfo }

func (s stubHubStatusBase) Status(context.Context) hub.StatusInfo { return s.info }

// TestProvisioningStatusProviderSuppliesInternet: the wrapper must overlay the
// live internet signal (claim-QR parity) onto the base payload, and leave it
// at the zero value when no probe is wired (test/default path).
func TestProvisioningStatusProviderSuppliesInternet(t *testing.T) {
	base := stubHubStatusBase{info: hub.StatusInfo{DeviceID: "ff1-abc", Branch: "develop"}}

	wired := &provisioningStatusProvider{
		base:     base,
		internet: func(context.Context) bool { return true },
	}
	got := wired.Status(context.Background())
	if !got.Internet {
		t.Fatal("wired probe must set Internet")
	}
	if got.Branch != "develop" {
		t.Fatalf("base fields must pass through, got branch %q", got.Branch)
	}

	unwired := &provisioningStatusProvider{base: base}
	if unwired.Status(context.Background()).Internet {
		t.Fatal("no probe wired: Internet must stay false")
	}
}
