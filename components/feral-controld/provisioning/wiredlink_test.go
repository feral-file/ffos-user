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

// newWiredHarness builds a machine wired with a WiredLink guard returning the
// given fixed value, reusing the shared package fakes.
func newWiredHarness(t *testing.T, wired bool) *harness {
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
		WiredLink:     func(context.Context) bool { return wired },
		NewPortal: func(cfg portal.Config) PortalServer {
			p := &fakePortal{rec: rec, cfg: cfg}
			h.portals = append(h.portals, p)
			return p
		},
	})
	return h
}

// TestWiredLinkGuard covers the AP-suppression seam: an unprovisioned device that
// is offline must NOT raise the setup AP while it has an active ETHERNET link
// (ethernet is its intended path), but must raise it when no such wired link
// exists — including the wifi-link-up-but-offline case (broken credentials), for
// which the wired-only guard reports false.
func TestWiredLinkGuard(t *testing.T) {
	tests := []struct {
		name        string
		wiredLink   bool
		wantState   State
		wantAPUps   int
		description string
	}{
		{
			name:        "ethernet link + offline + unprovisioned -> no AP",
			wiredLink:   true,
			wantState:   StateUnprovisioned,
			wantAPUps:   0,
			description: "a wired-but-offline device must never pop the setup AP",
		},
		{
			name:        "no wired link + offline + unprovisioned -> AP",
			wiredLink:   false,
			wantState:   StateAPActive,
			wantAPUps:   1,
			description: "with no wired link the device cannot self-heal, so raise the AP (also the wifi-link-up-but-offline broken-credentials case, which the wired-only guard reports as no wired link)",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newWiredHarness(t, tt.wiredLink)
			h.wifi.setProfile(false) // unprovisioned: no saved wifi profile
			ctx := context.Background()

			h.m.onConnectivity(ctx, false) // offline

			assert.Equal(t, tt.wantState, h.m.State(), tt.description)
			assert.Equal(t, tt.wantAPUps, h.rec.count("ap.Up"), tt.description)
		})
	}
}

// TestWiredLinkGuardNilDefaultsToNoSuppression proves the guard is opt-in: with no
// WiredLink configured (the newHarness default), an unprovisioned offline device
// raises the AP exactly as before, so the seam cannot silently change behavior on
// callers that do not wire it.
func TestWiredLinkGuardNilDefaultsToNoSuppression(t *testing.T) {
	h := newHarness(t) // no WiredLink configured
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false)

	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}
