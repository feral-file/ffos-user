package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/hub"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/provisioning"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// spyNarrationUI records the narration calls the notifier makes.
type spyNarrationUI struct {
	calls []string
	// joinFailedReasons records ShowJoinFailed bodies in order: the offline
	// entry-edge narration reuses join_failed and the tests must assert the
	// machine's prose (not a reason code) is what reaches the player.
	joinFailedReasons []string
}

func (s *spyNarrationUI) ShowScanning()                 { s.calls = append(s.calls, "scanning") }
func (s *spyNarrationUI) ShowSoftAPQR(ssid, psk string) { s.calls = append(s.calls, "softap") }
func (s *spyNarrationUI) ShowJoinFailed(reason string) {
	s.calls = append(s.calls, "join_failed")
	s.joinFailedReasons = append(s.joinFailedReasons, reason)
}
func (s *spyNarrationUI) ShowJoining() { s.calls = append(s.calls, "joining") }
func (s *spyNarrationUI) Hide()        { s.calls = append(s.calls, "hide") }

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

// TestSetupNotifierTriggersStartupGateWhenReachable: the claimed-device boot
// OTA gate mirrors the auto-claim trigger exactly — Online and Unprovisioned
// fire it (on its own goroutine, the Notifier must not block), AP states must
// not: while the setup AP is up there is no route to the distributor, and the
// gate would only burn its bounded version-check attempts.
func TestSetupNotifierTriggersStartupGateWhenReachable(t *testing.T) {
	spy := &spyNarrationUI{}
	fired := make(chan struct{}, 2)
	n := &setupNotifier{
		ui:          spy,
		claimCtx:    context.Background(),
		startupGate: func(context.Context) { fired <- struct{}{} },
	}

	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("startup OTA gate not triggered on StateOnline")
	}

	n.OnStateChange(provisioning.StateUnprovisioned, provisioning.Detail{Reason: provisioning.ReasonUnprovisioned})
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("startup OTA gate not triggered on StateUnprovisioned")
	}

	// AP states must not trigger it.
	n.OnStateChange(provisioning.StateAPActive, provisioning.Detail{Reason: "scanning"})
	select {
	case <-fired:
		t.Fatal("startup OTA gate must not fire on StateAPActive")
	case <-time.After(50 * time.Millisecond):
	}

	// The OFFLINE legs of StateUnprovisioned (WAN probe failed; local link
	// present, unknown, or lost) must not fire it either: a version check
	// there provably cannot succeed and would burn the bounded attempt
	// budget. The guard is a positive match on ReasonUnprovisioned, so all
	// three — and any future offline leg — fail closed.
	for _, reason := range []string{"link-present", "link-unknown", "link-lost"} {
		n.OnStateChange(provisioning.StateUnprovisioned, provisioning.Detail{Reason: reason})
		select {
		case <-fired:
			t.Fatalf("startup OTA gate must not fire on the offline %s leg", reason)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// TestSetupNotifierStartupGateContextObservesShutdown: the gate's waits
// (version-check ladder, retry backoff) run on the context the notifier hands
// it; cancellation must reach the spawned goroutine so daemon shutdown is not
// held up by a backoff sleep.
func TestSetupNotifierStartupGateContextObservesShutdown(t *testing.T) {
	spy := &spyNarrationUI{}
	ctx, cancel := context.WithCancel(context.Background())
	unblocked := make(chan struct{})
	n := &setupNotifier{
		ui:       spy,
		claimCtx: ctx,
		startupGate: func(c context.Context) {
			<-c.Done()
			close(unblocked)
		},
	}

	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	cancel()

	select {
	case <-unblocked:
	case <-time.After(2 * time.Second):
		t.Fatal("startup OTA gate did not observe daemon-context cancellation")
	}
}

// TestSetupNotifierTriggersPlayerRecoveryWhenReachable: the boot-online
// player recovery fires on the same reachable transitions as the other two
// hooks and never on AP states (no route to anything worth recovering for).
func TestSetupNotifierTriggersPlayerRecoveryWhenReachable(t *testing.T) {
	spy := &spyNarrationUI{}
	fired := make(chan struct{}, 2)
	n := &setupNotifier{
		ui:             spy,
		claimCtx:       context.Background(),
		playerRecovery: func(context.Context) { fired <- struct{}{} },
	}

	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("player recovery not triggered on StateOnline")
	}

	n.OnStateChange(provisioning.StateUnprovisioned, provisioning.Detail{Reason: provisioning.ReasonUnprovisioned})
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("player recovery not triggered on StateUnprovisioned")
	}

	n.OnStateChange(provisioning.StateAPActive, provisioning.Detail{Reason: "scanning"})
	select {
	case <-fired:
		t.Fatal("player recovery must not fire on StateAPActive")
	case <-time.After(50 * time.Millisecond):
	}

	// The OFFLINE legs of StateUnprovisioned must not fire it: a recovery
	// there burns the one-shot latch on fetches that cannot succeed, and the
	// real online transition then finds nothing left to recover with (the
	// Ethernet-only boot regression the positive-match guard prevents).
	for _, reason := range []string{"link-present", "link-unknown", "link-lost"} {
		n.OnStateChange(provisioning.StateUnprovisioned, provisioning.Detail{Reason: reason})
		select {
		case <-fired:
			t.Fatalf("player recovery must not fire on the offline %s leg", reason)
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// fakeBootFlows satisfies both boot-scoped flow interfaces for wiring tests.
type fakeBootFlows struct {
	probe      func() bool
	entryProbe func() bool
}

func (f *fakeBootFlows) MaybeRunStartupOTAGateOnOnline(context.Context) {}
func (f *fakeBootFlows) MaybeRecoverPlayerOnBootOnline(context.Context) {}
func (f *fakeBootFlows) CompletePendingBootPlayerRecovery()             {}
func (f *fakeBootFlows) SetBootLifecycleProbe(probe func() bool)        { f.probe = probe }
func (f *fakeBootFlows) SetStartupOTAGateEntryProbe(probe func() bool)  { f.entryProbe = probe }

// TestWireBootLifecycleHooksGatesOnBootWindow: BOTH boot-scoped hooks — the
// startup OTA gate and the player recovery — must stay unwired for a process
// that started outside the kernel boot window. feral-controld.service is
// Restart=always, so a mid-exhibition crash-restart re-delivers the initial
// online state; an ungated OTA hook would let that restart spring a required
// update (and reboot) on a healthy playing device. Regression: the OTA hook
// was originally wired unconditionally.
func TestWireBootLifecycleHooksGatesOnBootWindow(t *testing.T) {
	midLife := &setupNotifier{ui: &spyNarrationUI{}, claimCtx: context.Background()}
	wireBootLifecycleHooks(midLife, &fakeBootFlows{}, func() bool { return false }, func() bool { return false })
	if midLife.startupGate != nil {
		t.Fatal("startup OTA gate must not be wired on a mid-life (non-boot) restart")
	}
	if midLife.playerRecovery != nil {
		t.Fatal("player recovery must not be wired on a mid-life (non-boot) restart")
	}

	boot := &setupNotifier{ui: &spyNarrationUI{}, claimCtx: context.Background()}
	flows := &fakeBootFlows{}
	wireBootLifecycleHooks(boot, flows, func() bool { return true }, func() bool { return true })
	if boot.startupGate == nil {
		t.Fatal("startup OTA gate must be wired on a boot-window start")
	}
	if boot.playerRecovery == nil {
		t.Fatal("player recovery must be wired on a boot-window start")
	}
	if flows.probe == nil {
		t.Fatal("the boot-window probe must be handed to the recovery flow so a parked recovery can expire")
	}
	if flows.entryProbe == nil {
		t.Fatal("the OTA gate's own entry probe must be handed over so its wider window actually applies")
	}
}

// TestUptimeWithin pins the boot-lifecycle gate: only a readable,
// parseable /proc/uptime below the window arms the boot player recovery;
// everything else fails CLOSED (a spurious mid-exhibition reload is worse
// than a missed boot recovery).
func TestUptimeWithin(t *testing.T) {
	cases := []struct {
		name    string
		content string
		readErr error
		want    bool
	}{
		{name: "fresh boot", content: "12.34 45.67\n", want: true},
		{name: "just inside window", content: "119.9 200.0\n", want: true},
		{name: "past window", content: "120.1 300.0\n", want: false},
		{name: "long-running system", content: "864000.00 1700000.00\n", want: false},
		{name: "unreadable fails closed", readErr: errors.New("no procfs"), want: false},
		{name: "garbage fails closed", content: "not-a-number\n", want: false},
		{name: "empty fails closed", content: "", want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			readFile := func(string) ([]byte, error) {
				if tc.readErr != nil {
					return nil, tc.readErr
				}
				return []byte(tc.content), nil
			}
			got := uptimeWithin(bootLifecycleWindow, readFile, zap.NewNop())
			if got != tc.want {
				t.Errorf("uptimeWithin = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSetupNotifierClaimContextObservesShutdown: the claim flow's waits (topic,
// OTA gate, retry backoff) run on the context the notifier hands it; that must
// be a cancelable daemon-lifetime context, so cancellation reaches the spawned
// goroutine. The regression was claimCtx being a never-canceled Background.
func TestSetupNotifierClaimContextObservesShutdown(t *testing.T) {
	spy := &spyNarrationUI{}
	ctx, cancel := context.WithCancel(context.Background())
	unblocked := make(chan struct{})
	n := &setupNotifier{
		ui:       spy,
		claimCtx: ctx,
		claim: func(c context.Context) {
			<-c.Done()
			close(unblocked)
		},
	}

	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	cancel()

	select {
	case <-unblocked:
	case <-time.After(2 * time.Second):
		t.Fatal("claim flow did not observe daemon-context cancellation")
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

// TestExternalLinkProbeExcludesOwnHotspot pins the production wiring for the
// provisioning ActiveLink guard: the adapter must pass softap.ProfileName so
// the machine's own hotspot — including a leftover from a failed teardown —
// never counts as an external link. Mutating the excluded profile name (e.g.
// to "") silently reverts that fix; this is the only test that joins the
// adapter to the constant.
func TestExternalLinkProbeExcludesOwnHotspot(t *testing.T) {
	ctrl := gomock.NewController(t)
	cmd := mocks.NewMockExecCmd(ctrl)
	cmd.EXPECT().Output().
		Return([]byte("GENERAL.DEVICE:wlan0\nGENERAL.TYPE:wifi\nGENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:ff1-softap\n"+
			"GENERAL.DEVICE:eth0\nGENERAL.TYPE:ethernet\nGENERAL.STATE:20 (unavailable)\nGENERAL.CONNECTION:\n"), nil).
		AnyTimes()
	exec := mocks.NewMockExec(ctrl)
	exec.EXPECT().
		CommandContext(gomock.Any(), "nmcli", "-t", "-f",
			"GENERAL.DEVICE,GENERAL.TYPE,GENERAL.STATE,GENERAL.CONNECTION", "device", "show").
		Return(cmd).
		AnyTimes()

	probe := externalLinkProbe(status.NewLinkChecker(exec, zap.NewNop()))
	up, err := probe(context.Background())
	if err != nil {
		t.Fatalf("probe returned error: %v", err)
	}
	if up {
		t.Fatal("the adapter must exclude the ff1-softap hotspot from external-link detection")
	}
}

// fakeOTAOnlyFlow satisfies the startup-OTA interface and the probe sink but
// NOT bootPlayerRecoveryFlow, discriminating where fakeBootFlows cannot.
type fakeOTAOnlyFlow struct {
	probe func() bool
}

func (f *fakeOTAOnlyFlow) MaybeRunStartupOTAGateOnOnline(context.Context) {}
func (f *fakeOTAOnlyFlow) SetBootLifecycleProbe(p func() bool)            { f.probe = p }

// TestWireBootLifecycleHooksProbeInjectionIsIndependent enforces the "neither
// hook can end up wired but unguarded" invariant structurally: a flow that
// implements only the OTA gate (no player recovery) must still receive the
// boot-window probe. Regression: injecting the probe inside the
// bootPlayerRecoveryFlow assertion would leave exactly this shape wired with
// a nil probe — an OTA hook that never expires, finding 2 all over again.
func TestWireBootLifecycleHooksProbeInjectionIsIndependent(t *testing.T) {
	n := &setupNotifier{ui: &spyNarrationUI{}, claimCtx: context.Background()}
	flow := &fakeOTAOnlyFlow{}
	wireBootLifecycleHooks(n, flow, func() bool { return true }, func() bool { return true })
	if n.startupGate == nil {
		t.Fatal("the OTA hook must be wired for an OTA-only flow")
	}
	if flow.probe == nil {
		t.Fatal("the OTA gate must never be wired without its boot-window guard")
	}
	if n.playerRecovery != nil {
		t.Fatal("an OTA-only flow must not have a player recovery wired")
	}
}

// TestSetupNotifierNarratesOfflineEntryEdges (M-0/M-1): the narrated
// StateOfflineRetrying entry edges paint join_failed with the machine's
// Message as the body; the generic exhibition edge ("offline") stays silent —
// that default is what keeps a WAN blip from covering artwork with a setup
// overlay. The narration counts as owned, so a later Online must hide it.
func TestSetupNotifierNarratesOfflineEntryEdges(t *testing.T) {
	spy := &spyNarrationUI{}
	n := &setupNotifier{ui: spy}

	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{
		Reason: provisioning.ReasonBootOffline, Message: "Unable to connect to your Wi-Fi network. Setup mode will start in a few minutes if the connection does not return."})
	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{
		Reason: provisioning.ReasonJoinedNoInternet, Message: "Connected to X, but this network has no internet access"})
	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{
		Reason: provisioning.ReasonJoinedConnUnknown, Message: "Connected to X. Checking internet access…"})
	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{
		Reason: provisioning.ReasonBootNoInternet, Message: "Connected to the network, but there is no internet access. Retrying…"})
	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{
		Reason: provisioning.ReasonBootLinkUnknown, Message: "Checking the network connection…"})
	// The exhibition edge and future unknown reasons must fail silent.
	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{
		Reason: "offline", Message: "Reconnecting to Wi-Fi"})
	n.OnStateChange(provisioning.StateOfflineRetrying, provisioning.Detail{
		Reason: "link-present", Message: "Reconnecting to Wi-Fi"})

	if got := len(spy.joinFailedReasons); got != 5 {
		t.Fatalf("join_failed narrations = %d (%v); want 5 — un-narrated reasons must stay silent",
			got, spy.calls)
	}
	for i, want := range []string{
		"Unable to connect to your Wi-Fi network. Setup mode will start in a few minutes if the connection does not return.",
		"Connected to X, but this network has no internet access",
		"Connected to X. Checking internet access…",
		"Connected to the network, but there is no internet access. Retrying…",
		"Checking the network connection…",
	} {
		if spy.joinFailedReasons[i] != want {
			t.Fatalf("narration %d = %q, want the machine's Message %q", i, spy.joinFailedReasons[i], want)
		}
	}

	// The narration is owned: Online clears it exactly once.
	n.OnStateChange(provisioning.StateOnline, provisioning.Detail{})
	if spy.calls[len(spy.calls)-1] != "hide" {
		t.Fatalf("expected Online to hide the owned narration; calls = %v", spy.calls)
	}
}
