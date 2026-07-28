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
