package provisioning

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
)

// fakeLink is a settable ActiveLink guard: tests flip up/err mid-scenario to
// model a dropping association or a failing nmcli probe.
type fakeLink struct {
	up  bool
	err error
}

func (f *fakeLink) probe(context.Context) (bool, error) { return f.up, f.err }

// newLinkHarness builds a machine wired with the given fakeLink guard, reusing
// the shared package fakes.
func newLinkHarness(t *testing.T, fl *fakeLink) *harness {
	t.Helper()
	rec := &recorder{}
	h := &harness{
		rec:      rec,
		ap:       &fakeAP{rec: rec, info: softap.Info{SSID: "FF1-abc", PSK: "abc12345"}},
		wifi:     &fakeWifi{rec: rec},
		conn:     &fakeConn{},
		clk:      newFakeClock(),
		notifier: &fakeNotifier{},
	}
	h.m = New(Config{
		AP:            h.ap,
		Wifi:          h.wifi,
		Connectivity:  h.conn,
		Clock:         h.clk,
		Logger:        zap.NewNop(),
		Notifier:      h.notifier,
		OfflineWindow: 5 * time.Minute,
		CheckInterval: 15 * time.Second,
		PortalAddr:    "127.0.0.1:0",
		ActiveLink:    fl.probe,
		NewPortal: func(cfg portal.Config) PortalServer {
			p := &fakePortal{rec: rec, cfg: cfg}
			h.portals = append(h.portals, p)
			return p
		},
	})
	return h
}

// TestActiveLinkGuard covers the AP-suppression seam on the
// unprovisioned-immediate path: an offline unprovisioned device must NOT raise
// the setup AP while it has ANY confirmed local link (ethernet or an
// associated Wi-Fi station — either way it is reachable on its LAN and the AP
// cannot improve on that), must raise it immediately when link absence is
// CONFIRMED, and must defer — not raise — when the probe fails, because only a
// confirmed absence may authorize the raise.
func TestActiveLinkGuard(t *testing.T) {
	tests := []struct {
		name        string
		link        fakeLink
		wantState   State
		wantAPUps   int
		description string
	}{
		{
			name:        "active link + offline + unprovisioned -> no AP",
			link:        fakeLink{up: true},
			wantState:   StateUnprovisioned,
			wantAPUps:   0,
			description: "a linked-but-offline device must never pop the setup AP",
		},
		{
			name:        "confirmed no link + offline + unprovisioned -> AP",
			link:        fakeLink{up: false},
			wantState:   StateAPActive,
			wantAPUps:   1,
			description: "with confirmed link absence the device cannot self-heal, so raise the AP immediately",
		},
		{
			name:        "probe error + offline + unprovisioned -> defer, no AP",
			link:        fakeLink{err: errors.New("nmcli timeout")},
			wantState:   StateUnprovisioned,
			wantAPUps:   0,
			description: "a failed probe is unknown, and unknown never authorizes a raise",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fl := tt.link
			h := newLinkHarness(t, &fl)
			h.wifi.setProfile(false) // unprovisioned: no saved wifi profile
			ctx := context.Background()

			h.m.onConnectivity(ctx, false) // offline

			assert.Equal(t, tt.wantState, h.m.State(), tt.description)
			assert.Equal(t, tt.wantAPUps, h.rec.count("ap.Up"), tt.description)
		})
	}
}

// TestActiveLinkGuardWindowMeasuresLinkLoss pins the core issue-#233 property:
// OfflineWindow measures continuous LINK absence, not time since internet
// loss. A device that stays associated for most of the window and drops the
// link moments before expiry must NOT raise at that expiry (the pre-fix
// behavior raised after ~15s of link loss); the AP comes only after a full
// window with no link sighted. On Wi-Fi that raise is a one-way door — the
// hotspot takes the radio — which is why the early raise was a real hazard,
// not a rounding error.
func TestActiveLinkGuardWindowMeasuresLinkLoss(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true) // provisioned
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // WAN dies at t0; association still up
	assert.Equal(t, StateOfflineRetrying, h.m.State())

	h.clk.advance(4*time.Minute + 45*time.Second)
	h.m.onTick(ctx) // 4m45: link sighted -> window re-arms from here

	fl.up = false // association drops at ~4m59
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // 5m00: first CONFIRMED absence — starts the clock
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"a link lost moments before the internet-loss deadline must not raise the AP")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	h.clk.advance(5 * time.Minute)
	h.m.onTick(ctx) // 10m00: a full window of confirmed absence
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardSustainedLinkPresence covers the guard riding out an
// arbitrarily long WAN outage: every tick that sights the link disarms the
// window, so an associated-but-offline device (air-gapped LAN, ISP outage)
// never raises the AP no matter how long it stays offline.
func TestActiveLinkGuardSustainedLinkPresence(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false)
	for i := 0; i < 6; i++ { // 36+ minutes of link-up-but-offline
		h.clk.advance(6 * time.Minute)
		h.m.onTick(ctx)
	}
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"a live link must ride out any number of window expiries without the AP")
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardProbeErrorNeverRaises pins the failure bias: a failed
// probe (nmcli error/timeout) reads as UNKNOWN and disarms the window exactly
// like a sighted link — it must never satisfy the expiry, no matter how long
// the errors persist, because raising the AP off a transient NetworkManager
// hiccup would drop a possibly-healthy association. Only once a probe
// CONFIRMS absence for a full window does the AP raise.
func TestActiveLinkGuardProbeErrorNeverRaises(t *testing.T) {
	fl := &fakeLink{err: errors.New("nmcli timeout")}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false)
	for i := 0; i < 4; i++ { // 24 minutes of failing probes past the window
		h.clk.advance(6 * time.Minute)
		h.m.onTick(ctx)
	}
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"probe errors must defer the AP, never authorize it")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	fl.err = nil // probe recovers and confirms absence
	h.m.onTick(ctx)
	assert.Equal(t, 0, h.rec.count("ap.Up"),
		"first confirmed-absent reading starts the sustained window, not the AP")

	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardUnprovisionedCableUnplug covers the tick re-evaluation of
// an offline StateUnprovisioned device: parked there by a live ethernet link,
// it gets NO connectivity event when the cable is later unplugged (it is
// already offline, so monitord has no edge to emit). Confirmed link loss must
// raise the AP — but only after the same continuous-confirmed-absence window
// as the provisioned flavor, because once the AP is up nothing can lower it
// while the device stays offline; a probe error in between must defer.
func TestActiveLinkGuardUnprovisionedCableUnplug(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false) // unprovisioned
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // offline, wired: parked with AP down
	assert.Equal(t, StateUnprovisioned, h.m.State())

	fl.err = errors.New("nmcli busy") // probe flakes while still wired
	h.m.onTick(ctx)
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"a failed probe must not evict a parked unprovisioned device")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	fl.err = nil
	fl.up = false   // cable unplugged; no connectivity event will ever fire
	h.m.onTick(ctx) // first confirmed absence: starts the window
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"one absent sample must not raise — a transient switch reboot is not a lost link")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx) // a full window of confirmed absence
	assert.Equal(t, StateAPActive, h.m.State(),
		"sustained link loss on a parked unprovisioned device must raise the AP via the tick")
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardUnprovisionedWiredBlipNeverRaises pins the debounce the
// window buys: an air-gapped wired frame whose LAN switch reboots for one tick
// must NOT get the AP — the wire comes back, the window re-arms, and setup
// never appears. Without the window one confirmed-absent 15s sample raised the
// AP, and with no link-based exit from StateAPActive the frame would be
// permanently wedged in setup over a healthy wire.
func TestActiveLinkGuardUnprovisionedWiredBlipNeverRaises(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // air-gapped wired frame: offline forever
	assert.Equal(t, StateUnprovisioned, h.m.State())

	fl.up = false // switch reboots
	h.m.onTick(ctx)
	fl.up = true // switch back 15s later
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // link sighted: window re-arms

	fl.up = false // another blip much later
	h.clk.advance(30 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"a wired blip must never pop setup on an air-gapped frame")
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardUnprovisionedTickRechecksProfile pins the raise-boundary
// profile re-check: a device that acquired a saved profile out-of-band while
// parked (e.g. a hub-driven join) is the PROVISIONED flavor by the time the
// window expires — NM can retry the profile, so it must land in
// StateOfflineRetrying with a fresh window, not get an unprovisioned-style
// raise off stale state.
func TestActiveLinkGuardUnprovisionedTickRechecksProfile(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false)
	assert.Equal(t, StateUnprovisioned, h.m.State())

	fl.up = false           // link lost
	h.wifi.setProfile(true) // profile appears out-of-band while parked
	h.m.onTick(ctx)         // starts the absence window
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx) // window expires: must re-check the profile

	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"a profile acquired while parked must route to the provisioned resting state")
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardUnknownNeverEvictsFailedAPRaise covers the retry loop of
// a FAILED AP raise (state APActive, apUp false — the apUp short-circuit does
// not apply): a redundant offline reading with a flaking probe must keep the
// machine in StateAPActive retrying the raise, not bounce it through
// StateUnprovisioned — each such round trip would flap the on-screen setup
// narration at tick cadence.
func TestActiveLinkGuardUnknownNeverEvictsFailedAPRaise(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.ap.upErr = errors.New("nm busy") // the raise will fail
	h.m.onConnectivity(ctx, false)     // confirmed link-less: enter APActive
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 0, h.rec.count("portal.Start"), "raise failed: no portal")

	fl.err = errors.New("nmcli busy") // probe starts flaking
	h.m.onConnectivity(ctx, false)    // redundant offline reading
	assert.Equal(t, StateAPActive, h.m.State(),
		"an unknown link reading must not evict a failed-but-retrying AP raise")

	h.ap.upErr = nil // NM recovers; the retry tick must converge
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("portal.Start"),
		"the retried raise must converge to a served portal")
}

// TestActiveLinkGuardOnlineUnprovisionedSkipsProbe proves the tick never
// probes (and so never raises off) an ONLINE unprovisioned device — the
// ethernet steady state. Losing that link surfaces as a connectivity_change
// event, so the tick has no business running nmcli every 15s for it, and a
// spurious absent reading while online must not exist to raise the AP.
func TestActiveLinkGuardOnlineUnprovisionedSkipsProbe(t *testing.T) {
	fl := &fakeLink{up: false} // probe would scream "no link" if consulted
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, true) // online via ethernet, no wifi profile
	assert.Equal(t, StateUnprovisioned, h.m.State())

	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"an online unprovisioned device must never raise the AP from a tick probe")
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardIgnoresOwnAP pins the machine-level self-link exclusion:
// even with a naive guard that reports ANY wifi device in "connected" state —
// which matches the machine's own hotspot while the setup AP is up — a
// redundant offline reading arriving with the AP raised (monitord restarts
// re-emit their first probe unconditionally; the connUnknown re-query path
// feeds one in after an assumed-offline boot) must NOT take the link-present
// branch and tear the AP down mid-setup — nothing would ever re-raise it. The
// production probe (ExternalLink) additionally excludes the hotspot by profile
// name; this test deliberately wires the naive flavor so the apUp
// short-circuit is what is under test.
func TestActiveLinkGuardIgnoresOwnAP(t *testing.T) {
	h := newLinkHarness(t, &fakeLink{})
	h.m.activeLink = func(context.Context) (bool, error) {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return h.m.apUp, nil // nmcli sees the hotspot as a connected wifi device
	}
	h.wifi.setProfile(false) // unprovisioned, and truly link-less
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // offline: raises the AP
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))

	h.m.onConnectivity(ctx, false) // redundant offline while the AP is up

	assert.Equal(t, StateAPActive, h.m.State(),
		"our own hotspot must not count as a link and suppress the AP it belongs to")
	assert.Equal(t, 1, h.rec.count("ap.Up"), "AP must not bounce")
	assert.Equal(t, 0, h.rec.count("ap.Down"), "AP must not be torn down mid-setup")
}

// TestActiveLinkGuardRedundantOfflineKeepsProvisionedAP is the provisioned
// twin of TestActiveLinkGuardIgnoresOwnAP: once the sustained link-loss window
// has raised the AP on a PROVISIONED device, a redundant offline reading must
// not tear it back down. The unprovisioned flavor is protected by probeLink's
// apUp short-circuit; the provisioned branch never consulted the probe at all
// and fell straight through to StateOfflineRetrying, whose reconcile calls
// ensureAPDown — dropping the portal out from under a phone mid-setup and
// costing another full five-minute window before it returned. Such readings
// are routine (a sys-monitord restart re-emits its first probe
// unconditionally), so the AP must survive them.
func TestActiveLinkGuardRedundantOfflineKeepsProvisionedAP(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true) // provisioned
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // WAN and link both gone
	h.m.onTick(ctx)                // first confirmed absence arms the window
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx) // sustained absence: AP up
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 1, h.rec.count("ap.Up"))

	h.m.onConnectivity(ctx, false) // monitord restart re-emits offline

	assert.Equal(t, StateAPActive, h.m.State(),
		"a redundant offline reading must not evict the AP the window just raised")
	assert.Equal(t, 0, h.rec.count("ap.Down"), "the portal must not drop mid-setup")
	assert.Equal(t, 1, h.rec.count("ap.Up"), "AP must not bounce")
}

// TestActiveLinkGuardWindowStartsAtFirstConfirmedAbsence pins the other half of
// the "continuous confirmed absence" contract: the clock must not start at the
// connectivity event. Arming there gave the window a head start of one tick —
// a device offline AND link-less from the event raised the AP after only
// 4m45s of confirmed absence, since the first probe runs 15s in. The window is
// armed by the first linkAbsent probe instead, so every raise is backed by a
// full window of readings that actually confirmed the link was gone.
func TestActiveLinkGuardWindowStartsAtFirstConfirmedAbsence(t *testing.T) {
	fl := &fakeLink{up: false} // link already absent when the WAN reading lands
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // t0: no probe has run yet
	require.Equal(t, StateOfflineRetrying, h.m.State())

	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // t0+15s: FIRST confirmed absence — the clock starts here

	h.clk.advance(4*time.Minute + 45*time.Second)
	h.m.onTick(ctx) // t0+5m, but only 4m45s of confirmed absence
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"the window must measure confirmed absence, not time since the internet reading")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // t0+5m15s: a full window since the first confirmed absence
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardRedundantOfflineRespectsUnprovisionedWindow pins that a
// redundant offline reading (a sys-monitord restart re-emits its first probe
// unconditionally) landing on a device parked in StateUnprovisioned must NOT
// take the immediate-raise path and jump the continuous-confirmed-absence
// window the tick is measuring: an air-gapped wired frame mid-LAN-switch-reboot
// would get setup popped by a coincidental re-emission, and with no link-based
// exit from StateAPActive it would stay wedged there. The reading counts as one
// confirmed absence — no more — and the raise still needs the full window.
func TestActiveLinkGuardRedundantOfflineRespectsUnprovisionedWindow(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // air-gapped wired frame: parked with AP down
	require.Equal(t, StateUnprovisioned, h.m.State())

	fl.up = false   // LAN switch reboots
	h.m.onTick(ctx) // first confirmed absence arms the window

	h.clk.advance(5 * time.Second)
	h.m.onConnectivity(ctx, false) // monitord restart re-emits offline mid-blip
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"a redundant offline reading must not jump the sustained-absence window")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	fl.up = true // switch back after ~20s: the blip must never raise the AP
	h.clk.advance(10 * time.Second)
	h.m.onTick(ctx)
	h.clk.advance(30 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, 0, h.rec.count("ap.Up"),
		"the wired blip must ride out entirely, re-emission or not")

	fl.up = false // now a REAL sustained loss, noticed first by a re-emission
	h.m.onConnectivity(ctx, false)
	assert.Equal(t, 0, h.rec.count("ap.Up"),
		"the re-emission arms the window; it must not raise by itself")
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx) // a full window of confirmed absence: NOW the AP raises
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardOnlineWiredEdgeGetsWindow pins that the online→offline
// edge is windowed too: an ONLINE wired unprovisioned frame whose LAN switch
// reboots loses link and internet in the same instant, so the connectivity
// edge arrives with a confirmed-absent probe — and must NOT raise immediately.
// Flashing the setup QR over artwork for the length of every router reboot is
// exactly the transient-blip hazard #233 exists to prevent; only the boot
// assessment (a fresh link-less device) keeps the immediate raise.
func TestActiveLinkGuardOnlineWiredEdgeGetsWindow(t *testing.T) {
	fl := &fakeLink{up: false} // the switch is already down when the edge lands
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, true) // online via ethernet: the wired steady state
	require.Equal(t, StateUnprovisioned, h.m.State())

	h.m.onConnectivity(ctx, false) // switch reboots: link + internet die together
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"the online→offline edge must arm the window, not raise immediately")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	fl.up = true // switch back ~15s later: the blip rides out
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // sighting disarms the window
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	fl.up = false // the wire is now REALLY gone; only ticks will notice
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // first confirmed absence arms a fresh window
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State(),
		"a full window of confirmed absence must still raise the AP")
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardRedundantOfflinePreservesProvisionedWindow pins that a
// redundant offline reading landing mid-window in StateOfflineRetrying leaves
// the armed window untouched: onConnectivity's provisioned branch must neither
// reset the clock (which would let a sys-monitord restart loop postpone the AP
// forever on a genuinely dead link) nor clear it (which the unprovisioned
// branches do on sightings). The guard-wired entry path deliberately does not
// arm — only the first confirmed-absent probe does — so this is the one
// invariant keeping the window continuous across re-emissions.
func TestActiveLinkGuardRedundantOfflinePreservesProvisionedWindow(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // offline reading; nothing armed yet
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // first confirmed absence arms the window

	h.clk.advance(4 * time.Minute)
	h.m.onConnectivity(ctx, false) // monitord restart re-emits offline mid-window
	assert.Equal(t, StateOfflineRetrying, h.m.State())
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	// 5m10s since arming: if the re-emission had reset (or re-armed) the
	// window, this tick would still be minutes short and the assert fails.
	h.clk.advance(70 * time.Second)
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State(),
		"the re-emission must not have reset the confirmed-absence clock")
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardJoinWithoutInternetKeepsAPDown pins the post-join
// re-assessment with the guard wired: joining a network that associates but
// has no upstream (air-gapped LAN, dead WAN) parks the machine in
// StateOfflineRetrying with the AP down, and the AP never returns while the
// association survives — every tick sights the link and disarms the window.
// This is the accepted #233 trade-off (re-submitting credentials rejoins the
// same dead network; recovery is physical), previously pinned only by prose in
// docs/setup-flow.md.
func TestActiveLinkGuardJoinWithoutInternetKeepsAPDown(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // boot: link-less unprovisioned -> AP up
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 1, h.rec.count("ap.Up"))

	h.wifi.setProfile(true) // the join saves a profile...
	fl.up = true            // ...and brings the association up
	h.m.applyJoin(ctx, "Net", "pw")

	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"associated-but-offline must park provisioned with the AP down")
	assert.Equal(t, portal.JoinSucceeded, h.m.Status().State,
		"the association DID succeed; that is the portal's contract")

	for i := 0; i < 4; i++ { // 24+ minutes of ticks, well past the window
		h.clk.advance(6 * time.Minute)
		h.m.onTick(ctx)
	}
	assert.Equal(t, StateOfflineRetrying, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"),
		"the AP must never return over a live association")
}

// TestActiveLinkGuardRecoveredLinkExitsFailedRaise pins the failed-raise
// recovery path on a PROVISIONED device: after the sustained window raises and
// softap.Up FAILS (apUp false), the station can re-associate while NM keeps
// refusing the raise — and on an air-gapped LAN no connectivity event will
// ever arrive. The tick probe must exit StateAPActive to StateOfflineRetrying
// on the confirmed sighting, and the later NM recovery must NOT raise the AP
// over the recovered association; a fresh raise needs a fresh full window.
func TestActiveLinkGuardRecoveredLinkExitsFailedRaise(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.ap.upErr = errors.New("nm busy") // every raise attempt fails
	h.m.onConnectivity(ctx, false)
	h.m.onTick(ctx) // first confirmed absence arms the window
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx) // window expires: the raise is attempted and FAILS
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 0, h.rec.count("portal.Start"), "raise failed: no portal")

	fl.up = true // router returns; NM re-associates while the raise is pending
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx)
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"a confirmed link sighting must exit a failed raise")

	h.ap.upErr = nil         // NM recovers — the retry must never fire now
	for i := 0; i < 4; i++ { // 24+ minutes of link-up-but-offline ticks
		h.clk.advance(6 * time.Minute)
		h.m.onTick(ctx)
	}
	assert.Equal(t, StateOfflineRetrying, h.m.State())
	assert.Equal(t, 0, h.rec.count("portal.Start"),
		"the recovered association must never be dropped by a late successful raise")
}

// TestActiveLinkGuardRecoveredLinkExitsFailedRaiseOnRedundantReading is the
// connectivity-event flavor of the failed-raise recovery: a redundant offline
// re-emission (monitord restart) arriving after the link recovered must take
// the probe-gated exit, not blindly reconcile the pending raise. A RAISED AP
// is unaffected — probeLink short-circuits to linkAbsent while apUp is true —
// which TestActiveLinkGuardRedundantOfflineKeepsProvisionedAP pins.
func TestActiveLinkGuardRecoveredLinkExitsFailedRaiseOnRedundantReading(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.ap.upErr = errors.New("nm busy")
	h.m.onConnectivity(ctx, false)
	h.m.onTick(ctx)
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx) // raise attempted and failed
	require.Equal(t, StateAPActive, h.m.State())

	fl.up = true                   // link recovers between retry ticks
	h.m.onConnectivity(ctx, false) // monitord restart re-emits offline
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"a redundant reading with a recovered link must exit the pending raise")
	assert.Equal(t, 0, h.rec.count("portal.Start"))
}

// TestActiveLinkGuardRecoveredWireExitsFailedUnprovisionedRaise covers the
// unprovisioned twin via the tick: an air-gapped wired frame whose raise is
// failing gets its cable back — no connectivity event fires — and must park
// back in StateUnprovisioned instead of letting the retry eventually raise
// the AP over the healthy wire.
func TestActiveLinkGuardRecoveredWireExitsFailedUnprovisionedRaise(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.ap.upErr = errors.New("nm busy")
	h.m.onConnectivity(ctx, false) // boot: link-less unprovisioned -> raise fails
	require.Equal(t, StateAPActive, h.m.State())

	fl.up = true // cable plugged back in; the device stays offline (air-gapped)
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx)
	assert.Equal(t, StateUnprovisioned, h.m.State(),
		"a recovered wire must park the failed raise back in unprovisioned")
	assert.Equal(t, 0, h.rec.count("portal.Start"))
}

// TestActiveLinkGuardNilDefaultsToNoSuppression proves the guard is opt-in: with
// no ActiveLink configured (the newHarness default), an unprovisioned offline
// device raises the AP exactly as before, so the seam cannot silently change
// behavior on callers that do not wire it.
func TestActiveLinkGuardNilDefaultsToNoSuppression(t *testing.T) {
	h := newHarness(t) // no ActiveLink configured
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false)

	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestWiredOfflineBootNotifiesWhenWANArrives pins the notification edge every
// online-triggered hook (claim QR, startup OTA gate, boot player recovery)
// depends on: a wired device that boots offline parks on StateUnprovisioned's
// link-present leg, and when the WAN probe later succeeds, onConnectivity
// re-targets the SAME state with ReasonUnprovisioned. A state-change-only
// notify swallows that edge and no hook ever learns the device came online
// this boot. The reason change must notify; a repeated same-reason reading
// must stay suppressed.
func TestWiredOfflineBootNotifiesWhenWANArrives(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false) // no Wi-Fi profile: wired device
	ctx := context.Background()

	// Boot assessment: offline with a live wired link parks without the AP.
	h.m.onConnectivity(ctx, false)
	require.Equal(t, StateUnprovisioned, h.m.State())
	require.NotEmpty(t, h.notifier.calls)
	require.Equal(t, "link-present", h.notifier.calls[len(h.notifier.calls)-1].Detail.Reason)

	// WAN becomes reachable: same state, online leg — MUST notify.
	h.m.onConnectivity(ctx, true)
	last := h.notifier.calls[len(h.notifier.calls)-1]
	require.Equal(t, StateUnprovisioned, last.State)
	require.Equal(t, ReasonUnprovisioned, last.Detail.Reason,
		"WAN arrival on a wired-offline boot must notify the online leg")
	notified := len(h.notifier.calls)

	// A re-emitted online reading (sys-monitord restart, periodic re-query) is
	// the same state AND reason: suppressed.
	h.m.onConnectivity(ctx, true)
	require.Equal(t, notified, len(h.notifier.calls),
		"same-state same-reason re-emission must not re-notify")
}
