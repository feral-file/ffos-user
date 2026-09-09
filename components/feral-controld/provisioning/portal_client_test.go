package provisioning

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
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
		traffic(portal.ClientApple)
	}
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "only the first request of a raise queues the repaint")

	before := len(h.notifier.details())
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
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
	traffic(portal.ClientApple)
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
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))

	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "Home", "wrong", false)
	require.Equal(t, StateAPActive, h.m.State(), "a failed join re-raises the AP")
	require.Greater(t, len(h.portals), 1, "the re-raise builds a fresh portal")

	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "the re-raise re-arms the first-request latch")

	// The link harness's AP never learned its address: the handler retries
	// the lookup, still finds nothing, announces nothing, and latches the
	// attach as pending for the tick (the iOS Camera path sends no further
	// probe to retry on).
	before := len(h.notifier.details())
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	assert.Len(t, h.notifier.details(), before, "no attached repaint without a portal address")
	assert.True(t, h.m.apAttachPending, "the attach stays pending for the tick")
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	assert.Equal(t, 0, drainPortalClientEvents(t, h), "the latch is not handed back — the tick owns the retry")
}

// TestPendingAttachRetriesOnTheTick: with the address still unknown after
// the attach-time retry, the loop's tick keeps retrying the lookup and
// paints the attached phase once NetworkManager publishes the address —
// without any further request from the phone.
func TestPendingAttachRetriesOnTheTick(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx) // no address
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	require.True(t, h.m.apAttachPending)
	require.Equal(t, 0, attachedNotifies(h))

	h.tick(ctx) // still no address: stays pending, no repaint
	assert.True(t, h.m.apAttachPending)
	assert.Equal(t, 0, attachedNotifies(h))

	h.ap.info.PortalURL = "http://10.42.0.1"
	h.tick(ctx)
	assert.False(t, h.m.apAttachPending, "the tick's retry found the address")
	require.Equal(t, 1, attachedNotifies(h), "and painted the attached phase")
	all := h.notifier.details()
	assert.Equal(t, "http://10.42.0.1", all[len(all)-1].Detail.PortalURL)

	h.tick(ctx)
	assert.Equal(t, 1, attachedNotifies(h), "once")
}

// TestPendingAttachEndsWithTheRaise: a teardown clears the pending attach so
// the next raise starts on the join QR like any other.
func TestPendingAttachEndsWithTheRaise(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	require.True(t, h.m.apAttachPending)

	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "Home", "wrong", false)
	require.Equal(t, StateAPActive, h.m.State())
	assert.False(t, h.m.apAttachPending, "the re-raise starts clean")
	h.ap.info.PortalURL = "http://10.42.0.1"
	h.tick(ctx)
	assert.Equal(t, 0, attachedNotifies(h), "no repaint from a pending attach of a previous raise")
}

// TestAttachRetriesTheAddressLookup: a raise whose post-bind address lookup
// missed must not stay on the join QR for its lifetime — the attach retries
// the lookup and repaints once NetworkManager has published the address.
func TestAttachRetriesTheAddressLookup(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)           // raised with no address
	h.ap.info.PortalURL = "http://10.42.0.1" // NM publishes it afterwards
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	require.Equal(t, 1, attachedNotifies(h))
	all := h.notifier.details()
	assert.Equal(t, "http://10.42.0.1", all[len(all)-1].Detail.PortalURL)
}

// TestOldPortalCallbackCannotTouchTheNewRaise: a TrafficObserved callback
// retained from a torn-down portal (a request in flight across the bounded
// stop) must neither stamp traffic nor arm the latch of the re-raised
// hotspot, which no phone has joined.
func TestOldPortalCallbackCannotTouchTheNewRaise(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.ap.info.PortalURL = "http://10.42.0.1"
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	old := h.portals[len(h.portals)-1].cfg.TrafficObserved

	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "Home", "wrong", false)
	require.Equal(t, StateAPActive, h.m.State())
	require.Greater(t, len(h.portals), 1)

	h.clk.advance(time.Second)
	before := h.m.lastPortalTraffic
	old(portal.ClientApple)
	assert.Equal(t, 0, drainPortalClientEvents(t, h), "a stale callback must not arm the new raise")
	assert.Equal(t, before, h.m.lastPortalTraffic, "a stale callback must not count as traffic")

	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "the new raise's own callback still arms it")
}

// TestPortalClientRepaintDropsStaleGeneration: an event queued under one
// raise must not repaint a later raise — its hotspot has no phone on it yet.
func TestPortalClientRepaintDropsStaleGeneration(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.ap.info.PortalURL = "http://10.42.0.1"
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	stale := h.m.apRaiseGen
	require.Equal(t, 1, drainPortalClientEvents(t, h))

	// Bounce the AP through a failed join before the event is handled.
	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "Home", "wrong", false)
	require.Equal(t, StateAPActive, h.m.State())
	require.NotEqual(t, stale, h.m.apRaiseGen, "a re-raise advances the generation")

	before := len(h.notifier.details())
	h.m.applyPortalClientAttached(ctx, stale)
	assert.Len(t, h.notifier.details(), before, "a stale event repaints nothing")
	assert.Equal(t, 0, attachedNotifies(h))
}

// TestAttachedPhaseRearmsOnPortalSilence: under a session with no re-raise
// (the out-of-box raise is unbounded), a phone that left must not pin the
// screen on the portal-address QR — after attachedIdleReset of silence the
// join QR is painted again and the next first request re-attaches.
func TestAttachedPhaseRearmsOnPortalSilence(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.ap.info.PortalURL = "http://10.42.0.1"
	// Out-of-box: no saved profile and no link raises immediately under the
	// UNBOUNDED session — the one with no blink to repaint the join QR (the
	// recheck cadence's blink already covers the link-absent raises).
	h.wifi.setProfile(false)
	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 1, countReason(h, StateAPActive, "unprovisioned"))
	require.NotEmpty(t, h.portals)
	traffic := h.portals[len(h.portals)-1].cfg.TrafficObserved
	traffic(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	require.Equal(t, 1, attachedNotifies(h))

	// Chatty phone: silence never accumulates, no reverse repaint.
	for i := 0; i < 8; i++ {
		traffic(portal.ClientApple)
		h.tick(ctx)
	}
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientIdle))

	// Phone gone: the reset fires once the silence crosses the threshold.
	ticks := int(attachedIdleReset/(15*time.Second)) + 1
	h.tickN(ctx, ticks)
	require.Equal(t, 1, countReason(h, StateAPActive, ReasonAPClientIdle), "one reverse repaint after the idle window")
	all := h.notifier.details()
	last := all[len(all)-1]
	assert.Equal(t, ReasonAPClientIdle, last.Detail.Reason)
	assert.False(t, last.Detail.ClientAttached)
	assert.Equal(t, "abc12345", last.Detail.PSK, "the join QR repaint carries the credentials")
	assert.Equal(t, StateAPActive, h.m.State())

	// Still silent: no repeat. A returning phone re-attaches as a first request.
	h.tickN(ctx, 4)
	assert.Equal(t, 1, countReason(h, StateAPActive, ReasonAPClientIdle))
	traffic(portal.ClientApple)
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "the idle reset re-armed the latch")
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
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))

	fl.up = true
	h.m.onConnectivity(ctx, true, false)
	require.NotEqual(t, StateAPActive, h.m.State(), "an online transition tears the AP down")

	before := len(h.notifier.details())
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	assert.Len(t, h.notifier.details(), before, "no repaint for a torn-down AP")
	assert.Equal(t, 0, attachedNotifies(h))
}

// TestNonAppleTrafficNeverArmsTheRepaint: an Android (or unknown) client's
// probes stamp lastPortalTraffic but never queue the portal-address QR —
// on Android the link would be dead (cellular stays the default route) and
// nothing is looking at the screen. A later Apple client still arms it.
func TestNonAppleTrafficNeverArmsTheRepaint(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.ap.info.PortalURL = "http://10.42.0.1"
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	traffic := h.portals[len(h.portals)-1].cfg.TrafficObserved

	before := h.clk.Now()
	h.clk.advance(time.Second)
	traffic(portal.ClientUnknown)
	traffic(portal.ClientUnknown)
	assert.Equal(t, 0, drainPortalClientEvents(t, h), "non-Apple traffic never queues the repaint")
	h.m.mu.Lock()
	stamped := h.m.lastPortalTraffic.After(before)
	h.m.mu.Unlock()
	assert.True(t, stamped, "non-Apple traffic still counts as an attached device")

	traffic(portal.ClientApple)
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "an Apple client arms it")
}

// TestIdleResetCancelsAPendingAttach: a pending attach (address unknown)
// whose phone went silent is dropped by the idle reset together with the
// latch; an address appearing afterwards must not paint the address QR for
// a phone that is gone, with no idle reset left to undo it.
func TestIdleResetCancelsAPendingAttach(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(false) // unbounded out-of-box raise: no blink to rescue it
	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	require.True(t, h.m.apAttachPending)

	h.tickN(ctx, int(attachedIdleReset/(15*time.Second))+1)
	assert.False(t, h.m.apAttachPending, "the idle reset drops the pending attach")
	assert.False(t, h.m.apClientSeen)

	h.ap.info.PortalURL = "http://10.42.0.1"
	h.tickN(ctx, 3)
	assert.Equal(t, 0, attachedNotifies(h), "an address arriving after the reset paints nothing")

	// A returning phone starts over as a first request and gets the repaint.
	h.portals[len(h.portals)-1].cfg.TrafficObserved(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	assert.Equal(t, 1, attachedNotifies(h))
}

// TestReRaiseAfterFailedJoinCarriesTheReason: the AP-up announcement after a
// wrong password carries the failure's user-facing message, and a raise with
// no failed outcome carries none.
func TestReRaiseAfterFailedJoinCarriesTheReason(t *testing.T) {
	ctx := context.Background()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.wifi.setProfile(true)
	driveSustainedRaise(t, h, ctx)
	first := h.notifier.details()
	assert.Equal(t, "", first[len(first)-1].Detail.JoinFailure, "a clean raise has no failure line")

	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "Home", "wrong", false)
	require.Equal(t, StateAPActive, h.m.State())
	all := h.notifier.details()
	var reraise *Detail
	for i := len(all) - 1; i >= 0; i-- {
		if all[i].State == StateAPActive && all[i].Detail.PSK != "" {
			d := all[i].Detail
			reraise = &d
			break
		}
	}
	require.NotNil(t, reraise, "the re-raise announces the AP with credentials")
	assert.Equal(t, "Wrong Wi-Fi password. Please check it and try again.", reraise.JoinFailure)
}

// attachAppleClient raises the out-of-box AP on a link harness, lands the
// raise's first Apple request, and runs the loop-side repaint — the state
// every station-poll test starts from.
func attachAppleClient(ctx context.Context, t *testing.T) (*harness, func(portal.ClientKind)) {
	t.Helper()
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	h.ap.info.PortalURL = "http://10.42.0.1"
	h.wifi.setProfile(false)
	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	require.NotEmpty(t, h.portals)
	traffic := h.portals[len(h.portals)-1].cfg.TrafficObserved
	traffic(portal.ClientApple)
	require.Equal(t, 1, drainPortalClientEvents(t, h))
	h.m.applyPortalClientAttached(ctx, h.m.apRaiseGen)
	require.Equal(t, 1, attachedNotifies(h))
	return h, traffic
}

// TestStationPollSwapsBackWhenThePhoneLeaves: with the portal-address QR up,
// stationLeftPolls consecutive empty station reads put the join QR back
// (ReasonAPClientLeft, ClientLeft set, credentials carried), hand the latch
// back so the next first request re-attaches, and a single empty read is not
// enough.
func TestStationPollSwapsBackWhenThePhoneLeaves(t *testing.T) {
	ctx := context.Background()
	h, traffic := attachAppleClient(ctx, t)
	gen := h.m.apRaiseGen
	require.True(t, h.m.stationWatchWantedLocked(), "an attached Apple client wants the poll")

	// Phone present: nothing happens, however long.
	for i := 0; i < 5; i++ {
		h.m.applyStationPoll(gen, 1, true)
	}
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientLeft))

	// One empty read is a transient, not a verdict.
	h.m.applyStationPoll(gen, 0, true)
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientLeft))
	// A station reappearing resets the streak.
	h.m.applyStationPoll(gen, 1, true)
	h.m.applyStationPoll(gen, 0, true)
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientLeft))

	// The second consecutive empty read is the verdict.
	h.m.applyStationPoll(gen, 0, true)
	require.Equal(t, 1, countReason(h, StateAPActive, ReasonAPClientLeft), "one swap-back after stationLeftPolls empty reads")
	all := h.notifier.details()
	last := all[len(all)-1]
	assert.True(t, last.Detail.ClientLeft)
	assert.False(t, last.Detail.ClientAttached)
	assert.Equal(t, "abc12345", last.Detail.PSK, "the join QR repaint carries the credentials")
	assert.Equal(t, "http://10.42.0.1", last.Detail.PortalURL)
	assert.Equal(t, StateAPActive, h.m.State())
	assert.False(t, h.m.stationWatchWantedLocked(), "the latch is handed back, so the poll stops")

	// Further readings after the latch cleared are dropped.
	h.m.applyStationPoll(gen, 0, true)
	assert.Equal(t, 1, countReason(h, StateAPActive, ReasonAPClientLeft))

	// A returning phone re-attaches as a first request, from a clean streak.
	traffic(portal.ClientApple)
	assert.Equal(t, 1, drainPortalClientEvents(t, h), "the swap-back re-armed the latch")
	assert.Equal(t, 0, h.m.apStationZero)
}

// TestStationPollGivesUpOnUnknownReads: stationUnknownGiveUp consecutive
// unknown readings switch the poll off for the raise without repainting;
// the portal-silence re-arm still brings the join QR back, and the next
// raise polls again.
func TestStationPollGivesUpOnUnknownReads(t *testing.T) {
	ctx := context.Background()
	h, _ := attachAppleClient(ctx, t)
	gen := h.m.apRaiseGen
	for i := 0; i < stationUnknownGiveUp-1; i++ {
		h.m.applyStationPoll(gen, 0, false)
	}
	assert.True(t, h.m.stationWatchWantedLocked(), "still polling one short of the give-up")
	// A known read in between resets the unknown streak.
	h.m.applyStationPoll(gen, 1, true)
	assert.Equal(t, 0, h.m.apStationUnknown)
	for i := 0; i < stationUnknownGiveUp; i++ {
		h.m.applyStationPoll(gen, 0, false)
	}
	assert.False(t, h.m.stationWatchWantedLocked(), "the poll is off for this raise")
	assert.True(t, h.m.apClientSeen, "giving up keeps the attached phase painted")
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientLeft))
	// Empty reads after the give-up are dropped, not counted.
	h.m.applyStationPoll(gen, 0, true)
	h.m.applyStationPoll(gen, 0, true)
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientLeft))

	// The silence backstop is untouched.
	ticks := int(attachedIdleReset/(15*time.Second)) + 1
	h.tickN(ctx, ticks)
	assert.Equal(t, 1, countReason(h, StateAPActive, ReasonAPClientIdle))
}

// TestStationPollIgnoresOtherRaisesAndTornDownAPs: a reading from a
// previous raise's goroutine, or one landing after the AP went down, must
// not repaint anything or touch the counters.
func TestStationPollIgnoresOtherRaisesAndTornDownAPs(t *testing.T) {
	ctx := context.Background()
	h, _ := attachAppleClient(ctx, t)
	gen := h.m.apRaiseGen
	h.m.applyStationPoll(gen, 0, true)
	require.Equal(t, 1, h.m.apStationZero)

	// Stale generation: dropped.
	h.m.applyStationPoll(gen-1, 0, true)
	assert.Equal(t, 1, h.m.apStationZero)
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientLeft))

	// AP torn down under the poll: the counters reset with the latch and a
	// late reading is dropped.
	h.m.ensureAPDown(ctx)
	assert.Equal(t, 0, h.m.apStationZero)
	assert.False(t, h.m.stationWatchWantedLocked())
	h.m.applyStationPoll(gen, 0, true)
	h.m.applyStationPoll(gen, 0, true)
	assert.Equal(t, 0, countReason(h, StateAPActive, ReasonAPClientLeft))
}

// TestStationWatcherStartsOncePerRaiseAndStops: ensureStationWatcher starts
// one goroutine for the attached raise, is idempotent while it runs, and the
// goroutine exits — clearing its generation — once the latch is handed back.
// The counter is scripted empty, so the goroutine's own readings drive the
// swap-back end to end through the event queue.
func TestStationWatcherStartsOncePerRaiseAndStops(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h, _ := attachAppleClient(ctx, t)
	gen := h.m.apRaiseGen
	h.stations.set(0, true)

	h.m.ensureStationWatcher(ctx)
	h.m.ensureStationWatcher(ctx)
	assert.Equal(t, gen, h.m.apStationWatchGen, "one watcher, bound to this raise")

	// Drain the goroutine's readings into the loop-side handler until the
	// swap-back lands (2 s cadence, so a few seconds at most).
	deadline := time.After(15 * time.Second)
	for countReason(h, StateAPActive, ReasonAPClientLeft) == 0 {
		select {
		case ev := <-h.m.events:
			require.Equal(t, evStationPoll, ev.kind)
			h.m.applyStationPoll(ev.gen, ev.stations, ev.known)
		case <-deadline:
			t.Fatal("the watcher's readings never produced the swap-back")
		}
	}
	require.Eventually(t, func() bool {
		h.m.mu.Lock()
		defer h.m.mu.Unlock()
		return h.m.apStationWatchGen == 0
	}, 10*time.Second, 50*time.Millisecond, "the goroutine exits once the raise no longer wants it")
}
