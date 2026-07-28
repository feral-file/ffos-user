package provisioning

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
)

// newLinkHarness builds a machine wired with an ActiveLink guard reading the
// given flag (dereferenced per call, so tests can flip it mid-scenario),
// reusing the shared package fakes.
func newLinkHarness(t *testing.T, link *bool) *harness {
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
		ActiveLink:    func(context.Context) bool { return *link },
		NewPortal: func(cfg portal.Config) PortalServer {
			p := &fakePortal{rec: rec, cfg: cfg}
			h.portals = append(h.portals, p)
			return p
		},
	})
	return h
}

// TestActiveLinkGuard covers the AP-suppression seam: an unprovisioned device
// that is offline must NOT raise the setup AP while it has ANY active local
// link (ethernet or an associated Wi-Fi station — either way it is reachable
// on its LAN and the AP cannot improve on that), but must raise it immediately
// when no link exists at all, because a link-less device cannot self-heal.
func TestActiveLinkGuard(t *testing.T) {
	tests := []struct {
		name        string
		link        bool
		wantState   State
		wantAPUps   int
		description string
	}{
		{
			name:        "active link + offline + unprovisioned -> no AP",
			link:        true,
			wantState:   StateUnprovisioned,
			wantAPUps:   0,
			description: "a linked-but-offline device must never pop the setup AP",
		},
		{
			name:        "no link + offline + unprovisioned -> AP",
			link:        false,
			wantState:   StateAPActive,
			wantAPUps:   1,
			description: "with no link at all the device cannot self-heal, so raise the AP immediately",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			link := tt.link
			h := newLinkHarness(t, &link)
			h.wifi.setProfile(false) // unprovisioned: no saved wifi profile
			ctx := context.Background()

			h.m.onConnectivity(ctx, false) // offline

			assert.Equal(t, tt.wantState, h.m.State(), tt.description)
			assert.Equal(t, tt.wantAPUps, h.rec.count("ap.Up"), tt.description)
		})
	}
}

// TestActiveLinkGuardProvisionedSustainedOffline covers the guard's PROVISIONED
// flavor with the issue-#233 acceptance scenario: a provisioned device whose
// Wi-Fi association is up but whose WAN is dead must ride out the sustained
// -offline window indefinitely without raising the AP — raising would drop the
// station link on single-radio hardware and cannot fix an upstream outage. The
// window is re-armed fresh at each guarded expiry, so losing the link (e.g. the
// router's Wi-Fi password changed and NM dropped the association) starts a new
// sustained window rather than raising the AP instantly off a stale one.
func TestActiveLinkGuardProvisionedSustainedOffline(t *testing.T) {
	link := true
	h := newLinkHarness(t, &link)
	h.wifi.setProfile(true) // provisioned
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // WAN dead: arms the window
	assert.Equal(t, StateOfflineRetrying, h.m.State())

	// 30+ minutes of link-up-but-offline: every window expiry is guarded.
	for i := 0; i < 6; i++ {
		h.clk.advance(6 * time.Minute)
		h.m.onTick(ctx)
	}
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"a live link must ride out any number of window expiries without the AP")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	link = false // association dropped (password changed / SSID gone), still offline
	h.m.onTick(ctx)
	assert.Equal(t, 0, h.rec.count("ap.Up"),
		"link loss must not raise instantly off a stale window (window was re-armed)")

	h.clk.advance(6 * time.Minute) // a fresh sustained window elapses link-less
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestActiveLinkGuardIgnoresOwnAP pins the self-link exclusion: the production
// guard is an nmcli probe that reports ANY wifi device in "connected" state,
// which matches the machine's own hotspot while the setup AP is up. A redundant
// offline reading arriving with the AP raised (monitord restarts re-emit their
// first probe unconditionally; the connUnknown re-query path feeds one in after
// an assumed-offline boot) must NOT take the link-present branch and tear the
// AP down mid-setup — nothing would ever re-raise it. The guard here returns
// exactly what nmcli would: link present iff our AP is up.
func TestActiveLinkGuardIgnoresOwnAP(t *testing.T) {
	// The harness's flag-backed guard is replaced below with one correlated to
	// AP state; the placeholder is never read.
	h := newLinkHarness(t, new(bool))
	h.m.activeLink = func(context.Context) bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return h.m.apUp // nmcli sees the hotspot as a connected wifi device
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
