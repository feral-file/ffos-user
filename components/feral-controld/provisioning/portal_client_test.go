package provisioning

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/wifictl"
)

// drainPortalClientEvents pops every queued evPortalClient and reports how
// many there were. The harness never runs the loop, so the queue is the
// observable seam between the request-goroutine latch and the loop-side
// repaint.
func drainPortalClientEvents(t *testing.T, h *harness) int {
	t.Helper()
	n := 0
	for {
		select {
		case ev := <-h.m.events:
			require.Equal(t, evPortalClient, ev.kind, "unexpected event kind %v in the queue", ev.kind)
			n++
		default:
			return n
		}
	}
}

func attachedNotifies(h *harness) int {
	n := 0
	for _, c := range h.notifier.details() {
		if c.State == StateAPActive && c.Detail.ClientAttached {
			n++
		}
	}
	return n
}

// TestFirstPortalTrafficRepaintsOnce: the first portal request of a raise
// queues exactly one attached-client event; the loop-side handler then
// re-announces ap_active with ClientAttached and the raise's own
// credentials, once — however chatty the phone is afterwards.
func TestFirstPortalTrafficRepaintsOnce(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	// The link harness's AP omits the address by default; the repaint must
	// carry whatever the raise's post-bind lookup returned.
	h.ap.info.PortalURL = "http://10.42.0.1"
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	require.NotEmpty(t, h.portals)
	traffic := h.portals[len(h.portals)-1].cfg.TrafficObserved
	require.NotNil(t, traffic)

	for i := 0; i < 5; i++ {
		traffic()
	}
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "only the first request of a raise queues the repaint")

	before := len(h.notifier.details())
	h.m.applyPortalClientAttached()
	all := h.notifier.details()
	require.Len(t, all, before+1, "the handler announces exactly once")
	last := all[len(all)-1]
	assert.Equal(t, StateAPActive, last.State)
	assert.True(t, last.Detail.ClientAttached)
	assert.Equal(t, ReasonAPClientAttached, last.Detail.Reason)
	assert.Equal(t, "FF1-abc", last.Detail.SSID)
	assert.Equal(t, "abc12345", last.Detail.PSK, "the repaint must carry the live credentials for the manual-join line")
	assert.Equal(t, "http://10.42.0.1", last.Detail.PortalURL, "the repaint must carry the address the portal QR encodes")

	// A repaint is not a transition: state and reason are untouched.
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, "sustained-offline", h.m.Snapshot().Reason)

	// Later traffic stays silent for the rest of this raise.
	traffic()
	assert.Equal(t, 0, drainPortalClientEvents(t, h))
}

// TestPortalTrafficLatchReArmsOnReRaise: a failed join bounces the AP and the
// phone must re-associate, so the re-raise paints the join QR again and the
// next first request repaints the portal QR again.
func TestPortalTrafficLatchReArmsOnReRaise(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	h.portals[len(h.portals)-1].cfg.TrafficObserved()
	require.Equal(t, 1, drainPortalClientEvents(t, h))

	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "Home", "wrong", false)
	require.Equal(t, StateAPActive, h.m.State(), "a failed join re-raises the AP")
	require.Greater(t, len(h.portals), 1, "the re-raise builds a fresh portal")

	h.portals[len(h.portals)-1].cfg.TrafficObserved()
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "the re-raise re-arms the first-request latch")
}

// TestPortalClientRepaintSkipsTornDownAP: an event queued by a probe on a
// portal that is gone by the time the loop reads it must not announce
// credentials for an AP that is down.
func TestPortalClientRepaintSkipsTornDownAP(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	h.portals[len(h.portals)-1].cfg.TrafficObserved()
	require.Equal(t, 1, drainPortalClientEvents(t, h))

	fl.up = true
	h.m.onConnectivity(ctx, true, false)
	require.NotEqual(t, StateAPActive, h.m.State(), "an online transition tears the AP down")

	before := len(h.notifier.details())
	h.m.applyPortalClientAttached()
	assert.Len(t, h.notifier.details(), before, "no repaint for a torn-down AP")
	assert.Equal(t, 0, attachedNotifies(h))
}
