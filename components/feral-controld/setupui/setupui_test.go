package setupui

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/overlay"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

type fakePlayer struct {
	mu       sync.Mutex
	err      error
	requests []map[string]any
	calls    int
	gate     chan struct{}
	signal   chan struct{}
}

func newFakePlayer() *fakePlayer { return &fakePlayer{signal: make(chan struct{}, 64)} }
func (f *fakePlayer) SetOverlay(ctx context.Context, owner, state string, fields map[string]any) error {
	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if owner != "setup" {
		return errors.New("wrong owner")
	}
	raw, _ := json.Marshal(fields)
	var req map[string]any
	_ = json.Unmarshal(raw, &req)
	req["state"] = state
	f.mu.Lock()
	f.calls++
	f.requests = append(f.requests, req)
	err := f.err
	f.mu.Unlock()
	select {
	case f.signal <- struct{}{}:
	default:
	}
	return err
}
func (f *fakePlayer) callCount() int { f.mu.Lock(); defer f.mu.Unlock(); return f.calls }
func (f *fakePlayer) lastRequest() map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) == 0 {
		return nil
	}
	return f.requests[len(f.requests)-1]
}
func (f *fakePlayer) waitForCalls(t *testing.T, n int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for f.callCount() < n {
		select {
		case <-f.signal:
		case <-deadline:
			t.Fatalf("waited for %d calls, got %d", n, f.callCount())
		}
	}
}

type fakeListener struct{}

func (fakeListener) OnOverride(overlay.Overlay) {}
func (fakeListener) OnClose()                   {}
func TestTypedMethodsEmitContractPayloads(t *testing.T) {
	tests := []struct {
		name       string
		call       func(*Service)
		wantState  string
		wantFields map[string]any
		absent     []string
	}{
		{
			name:      "softap qr with password",
			call:      func(s *Service) { s.ShowSoftAPQR("FF1-abc", "secret123", "http://10.42.0.1", false) },
			wantState: stateSoftAPQR,
			wantFields: map[string]any{
				"ssid": "FF1-abc", "password": "secret123", "portal_url": "http://10.42.0.1",
			},
		},
		{
			name:       "softap qr omits blank optional fields",
			call:       func(s *Service) { s.ShowSoftAPQR("FF1-abc", "", "", false) },
			wantState:  stateSoftAPQR,
			wantFields: map[string]any{"ssid": "FF1-abc"},
			absent:     []string{"password", "portal_url", "client_attached"},
		},
		{
			name: "softap retry qr carries the failure reason",
			call: func(s *Service) {
				s.ShowSoftAPQRRetry("FF1-abc", "secret123", "http://10.42.0.1", "Wrong Wi-Fi password.")
			},
			wantState: stateSoftAPQR,
			wantFields: map[string]any{
				"ssid": "FF1-abc", "password": "secret123", "portal_url": "http://10.42.0.1",
				"reason": "Wrong Wi-Fi password.",
			},
			absent: []string{"client_attached"},
		},
		{
			name:      "softap portal qr flags the attached phase",
			call:      func(s *Service) { s.ShowSoftAPPortalQR("FF1-abc", "secret123", "http://10.42.0.1") },
			wantState: stateSoftAPQR,
			wantFields: map[string]any{
				"ssid": "FF1-abc", "password": "secret123", "portal_url": "http://10.42.0.1",
				"client_attached": true,
			},
		},
		{
			name:      "scanning",
			call:      func(s *Service) { s.ShowScanning(false) },
			wantState: stateScanning,
			absent:    []string{"reason", "ssid", "url", "progress"},
		},
		{
			name:      "joining",
			call:      func(s *Service) { s.ShowJoining() },
			wantState: stateJoining,
			absent:    []string{"reason", "ssid", "url", "progress"},
		},
		{
			name:       "join failed with reason",
			call:       func(s *Service) { s.ShowJoinFailed("bad_password") },
			wantState:  stateJoinFailed,
			wantFields: map[string]any{"reason": "bad_password"},
		},
		{
			name:      "join failed omits blank reason",
			call:      func(s *Service) { s.ShowJoinFailed("") },
			wantState: stateJoinFailed,
			absent:    []string{"reason"},
		},
		{
			name:       "updating carries progress",
			call:       func(s *Service) { s.ShowUpdating(42) },
			wantState:  stateUpdating,
			wantFields: map[string]any{"progress": float64(42)},
		},
		{
			name:      "finalizing",
			call:      func(s *Service) { s.ShowFinalizing() },
			wantState: stateFinalizing,
			absent:    []string{"reason", "ssid", "url", "progress"},
		},
		{
			name: "claim qr carries url and device name",
			call: func(s *Service) {
				s.ShowClaimQR("https://link.feralfile.com/device_connect/xyz", "FF1-8EVTK3RE")
			},
			wantState: stateClaimQR,
			wantFields: map[string]any{
				"url":         "https://link.feralfile.com/device_connect/xyz",
				"device_name": "FF1-8EVTK3RE",
			},
		},
		{
			name:       "claim qr omits blank device name",
			call:       func(s *Service) { s.ShowClaimQR("https://link.feralfile.com/device_connect/xyz", " ") },
			wantState:  stateClaimQR,
			wantFields: map[string]any{"url": "https://link.feralfile.com/device_connect/xyz"},
			absent:     []string{"device_name"},
		},
		{
			name:      "ready",
			call:      func(s *Service) { s.ShowReady() },
			wantState: stateReady,
		},
		{
			name:      "factory reset",
			call:      func(s *Service) { s.ShowFactoryReset() },
			wantState: stateFactoryReset,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sender := newFakePlayer()
			svc := New(sender, nil)

			tt.call(svc)
			sender.waitForCalls(t, 1)

			req := sender.lastRequest()
			require.NotNil(t, req)
			assert.Equal(t, tt.wantState, req["state"])
			for k, v := range tt.wantFields {
				assert.Equal(t, v, req[k], "field %q", k)
			}
			for _, k := range tt.absent {
				_, present := req[k]
				assert.False(t, present, "field %q should be absent", k)
			}
		})
	}
}

func TestDownPlayerDoesNotBlockOrPanic(t *testing.T) {
	sender := newFakePlayer()
	sender.err = errors.New("cdp down")
	svc := New(sender, nil)

	// Each push must return promptly even though every send errors.
	done := make(chan struct{})
	go func() {
		svc.ShowJoining()
		svc.ShowJoinFailed("dhcp_timeout")
		svc.ShowUpdating(10)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Show* calls blocked on a down player socket")
	}

	// The last state change must still have been attempted against player socket.
	sender.waitForCalls(t, 1)

	// A subsequent change re-pushes (a new send attempt) despite prior failures.
	// Wait on the CONTENT, not a call count: the worker may still be draining
	// the earlier queued states, so the next send is not necessarily the ready
	// push.
	svc.ShowReady()
	assert.Eventually(t, func() bool {
		last := sender.lastRequest()
		return last != nil && last["state"] == stateReady
	}, 2*time.Second, 10*time.Millisecond, "ready state never reached player socket")
}

func TestResyncRepushesLastStateWhenPlayerReturns(t *testing.T) {
	sender := newFakePlayer()
	sender.err = errors.New("cdp down")
	svc := New(sender, nil)

	svc.ShowUpdating(75)
	sender.waitForCalls(t, 1) // failed attempt

	// player socket comes back.
	sender.mu.Lock()
	sender.err = nil
	sender.mu.Unlock()

	before := sender.callCount()
	svc.Resync()
	sender.waitForCalls(t, before+1)

	last := sender.lastRequest()
	require.NotNil(t, last)
	assert.Equal(t, stateUpdating, last["state"])
	assert.Equal(t, float64(75), last["progress"])
}

func TestResyncBeforeAnyStateIsNoop(t *testing.T) {
	sender := newFakePlayer()
	svc := New(sender, nil)

	svc.Resync()
	// Give any erroneously-spawned worker a chance to call player socket.
	time.Sleep(50 * time.Millisecond)
	assert.Equal(t, 0, sender.callCount())
}

func TestResync_DoesNotReclaimAfterConcurrentOwner(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)

	svc.ShowFinalizing()
	fake.waitForCalls(t, 1)

	// A browser-pairing request takes the screen before the reconnect fires
	// Resync.
	other := overlay.Overlay{Kind: "mint:pairing_code"}
	otherListener := &fakeListener{}
	_, _, err := svc.ctrl.Show(context.Background(), otherListener, other, overlay.Owner)
	require.NoError(t, err)

	svc.Resync()
	time.Sleep(20 * time.Millisecond)

	cur, has := svc.ctrl.Current()
	assert.True(t, has)
	assert.Equal(t, other.Kind, cur.Kind, "Resync must not reclaim the screen from a newer owner")
}

func TestResync_ReplayedHiddenIntentRoutesThroughTheProductionRouter(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)
	svc.SetController(overlay.New(overlay.NewRouter(map[string]overlay.Painter{
		"setup:": setupPainter{s: svc},
	})))

	svc.ShowReady()
	svc.Hide()
	fake.waitForCalls(t, 2)

	before := fake.callCount()
	svc.Resync()
	fake.waitForCalls(t, before+1)
	assert.Equal(t, stateHidden, fake.lastRequest()["state"])
}

func TestResync_ReplaysHiddenIntentAfterHide(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)

	svc.ShowReady()
	svc.Hide()
	fake.waitForCalls(t, 2)

	before := fake.callCount()
	svc.Resync()
	fake.waitForCalls(t, before+1)
	assert.Equal(t, stateHidden, fake.lastRequest()["state"])
}

func TestResync_HiddenIntentDoesNotClobberANewerOwner(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)

	svc.ShowReady()
	svc.Hide()
	fake.waitForCalls(t, 2)

	// A different listener takes the screen after setupui hid itself. Asserted
	// on the controller's own decision (Current), not a player socket call count: this
	// test's svc.ctrl has no router splitting owners onto separate painters
	// the way production wiring does, so any Show on it — including this
	// other listener's — reaches the same fake sender setupui's own narration
	// does, making a call-count assertion race against that unrelated send.
	other := overlay.Overlay{Kind: "other:kind"}
	otherListener := &fakeListener{}
	_, _, err := svc.ctrl.Show(context.Background(), otherListener, other, overlay.Owner)
	require.NoError(t, err)

	svc.Resync()
	time.Sleep(20 * time.Millisecond)

	cur, has := svc.ctrl.Current()
	assert.True(t, has)
	assert.Equal(t, other.Kind, cur.Kind, "a newer owner's overlay must survive a stale hidden-intent replay")
}

func TestResync_ReEnqueuesLastWhenPendingEmpty(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)

	svc.ShowReady()
	fake.waitForCalls(t, 1) // queue drains empty

	svc.Resync()
	fake.waitForCalls(t, 2)
	assert.Equal(t, stateReady, fake.lastRequest()["state"])
}

func TestReadyThenHideDeliversBoth(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)

	svc.ShowReady()
	svc.Hide()
	fake.waitForCalls(t, 2)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.requests, 2)
	assert.Equal(t, stateReady, fake.requests[0]["state"])
	assert.Equal(t, stateHidden, fake.requests[1]["state"])
}

func TestShowClaimQRAutomatic_NotStarvedByItsOwnEarlierFinalizing(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)

	svc.ShowFinalizing()
	fake.waitForCalls(t, 1)

	svc.ShowClaimQRAutomatic("https://claim.example/x", "FF1-8EVTK3RE")
	fake.waitForCalls(t, 2)

	fake.mu.Lock()
	defer fake.mu.Unlock()
	require.Len(t, fake.requests, 2)
	assert.Equal(t, stateFinalizing, fake.requests[0]["state"])
	assert.Equal(t, stateClaimQR, fake.requests[1]["state"], "the claim QR must not be starved by this service's own earlier overlay")
}

func TestHide_NothingCurrentSendsNothing(t *testing.T) {
	fake := newFakePlayer()
	svc := New(fake, nil)

	svc.Hide()

	time.Sleep(20 * time.Millisecond)
	assert.Zero(t, fake.callCount(), "nothing was shown, so there is nothing to clear")
}

func TestSweepStaleOverlay(t *testing.T) {
	t.Run("no intent yet: sweeps the previous life's overlay", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.SweepStaleOverlay()
		sender.waitForCalls(t, 1)
		assert.Equal(t, stateHidden, sender.lastRequest()["state"])
	})

	t.Run("narration first, sweep delayed: narration stays authoritative", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowUpdating(40)
		sender.waitForCalls(t, 1)
		svc.SweepStaleOverlay() // maximally delayed sweep: must no-op

		assert.True(t, svc.Narrating(), "sweep must not clear live narration intent")
		assert.Equal(t, stateUpdating, sender.lastRequest()["state"])
		assert.Equal(t, 1, sender.callCount(), "the sweep must not deliver a hide")
	})

	t.Run("sweep first, narration racing in behind: screen ends on narration", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.SweepStaleOverlay()
		svc.ShowUpdating(10) // queued behind the hide on the same lane
		sender.waitForCalls(t, 2)
		assert.Equal(t, stateUpdating, sender.lastRequest()["state"])
		assert.True(t, svc.Narrating())
	})
}

func TestSweepStaleOverlay_DoesNotExploitSameListenerExemptionDuringTheNarratedRace(t *testing.T) {
	sender := newFakePlayer()
	svc := New(sender, nil)

	live := overlay.Overlay{
		Kind:    setupKind(stateUpdating),
		Payload: map[string]any{"state": stateUpdating},
	}
	_, result, err := svc.ctrl.ShowIf(context.Background(), svc, live, overlay.Owner, nil, nil)
	require.NoError(t, err)
	require.NoError(t, result.Wait(context.Background()))
	sender.waitForCalls(t, 1)
	svc.mu.Lock()
	narratedStillFalse := !svc.narrated
	svc.mu.Unlock()
	require.True(t, narratedStillFalse, "the race window requires narrated to still read false (committed outside showOwned, which alone flips it)")

	svc.SweepStaleOverlay()
	time.Sleep(20 * time.Millisecond) // give a (wrongly) accepted sweep time to deliver

	cur, has := svc.ctrl.Current()
	require.True(t, has, "the live overlay must still be current")
	assert.Equal(t, live.Kind, cur.Kind, "the sweep must not replace a live overlay committed outside showOwned's narrated flip")
	assert.Equal(t, 1, sender.callCount(), "no hide may be delivered while the live overlay stands")
}

func TestShowOwnedAndHideIf_ConcurrentDecisionsKeepHiddenIntentConsistent(t *testing.T) {
	sender := newFakePlayer()
	svc := New(sender, nil)

	const workers = 8
	const rounds = 50
	for round := 0; round < rounds; round++ {
		var wg sync.WaitGroup
		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					svc.ShowUpdating(i)
				} else {
					svc.HideIfShowing(stateUpdating)
				}
			}(i)
		}
		wg.Wait()

		cur, has := svc.ctrl.Current()
		svc.mu.Lock()
		hiddenIntent := svc.hiddenIntent
		svc.mu.Unlock()
		if has {
			assert.False(t, hiddenIntent, "round %d: %v is current; hiddenIntent must reflect the later show, not a stale hide", round, cur.Kind)
		} else {
			assert.True(t, hiddenIntent, "round %d: nothing is current; hiddenIntent must reflect the later hide, not a stale show", round)
		}
		// Reset to a known state before the next round's race.
		svc.HideIfShowing(stateUpdating)
	}
}

func TestHideIfShowing(t *testing.T) {
	t.Run("hides its own narration", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowFinalizing()
		svc.HideIfShowing(StateFinalizing)
		sender.waitForCalls(t, 2)
		assert.Equal(t, stateHidden, sender.lastRequest()["state"])
		assert.False(t, svc.Narrating())
	})

	t.Run("yields to a concurrent narrator", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowFinalizing()
		svc.ShowUpdating(20) // another narrator took the screen mid-flow
		svc.HideIfShowing(StateFinalizing)
		sender.waitForCalls(t, 2)
		assert.True(t, svc.Narrating(), "updating must survive the finalizing owner's clear")
		assert.Equal(t, stateUpdating, sender.lastRequest()["state"])
		assert.Equal(t, 2, sender.callCount(), "no hide may be delivered")
	})

	t.Run("no intent: no spurious hide", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.HideIfShowing(StateFinalizing)
		assert.Equal(t, 0, sender.callCount())
		assert.False(t, svc.Narrating())
	})
}

func TestRefreshClaimQRName(t *testing.T) {
	resolveTo := func(name string, ran *bool) func() string {
		return func() string {
			*ran = true
			return name
		}
	}

	t.Run("rewrites the name of a showing claim QR", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowClaimQR("https://claim.example/x", "FF1-8EVTK3RE")
		// Let the paint reach player socket first: a refresh queued behind an unsent
		// claim_qr coalesces into it (trailing same-state rule), which is
		// also correct but would make the second delivery unobservable.
		sender.waitForCalls(t, 1)
		ran := false
		svc.RefreshClaimQRName(resolveTo("Living Room", &ran))
		sender.waitForCalls(t, 2)

		assert.True(t, ran)
		last := sender.lastRequest()
		assert.Equal(t, stateClaimQR, last["state"])
		assert.Equal(t, "https://claim.example/x", last["url"], "the url is carried over, not re-derived")
		assert.Equal(t, "Living Room", last["device_name"])
	})

	t.Run("carries the url a concurrent repaint installed", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowClaimQR("https://claim.example/old-topic", "FF1-8EVTK3RE")
		sender.waitForCalls(t, 1)
		// A topic reassignment repaints the claim QR with a new url while
		// the rename is resolving its name — the refresh must commit that
		// url, not the one it saw before resolving.
		svc.RefreshClaimQRName(func() string {
			svc.ShowClaimQR("https://claim.example/new-topic", "FF1-8EVTK3RE")
			return "Living Room"
		})
		assert.Eventually(t, func() bool {
			last := sender.lastRequest()
			return last != nil && last["device_name"] == "Living Room"
		}, 2*time.Second, 10*time.Millisecond, "refreshed name never reached player socket")

		last := sender.lastRequest()
		assert.Equal(t, stateClaimQR, last["state"])
		assert.Equal(t, "https://claim.example/new-topic", last["url"])
	})

	t.Run("yields to a concurrent narrator", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowClaimQR("https://claim.example/x", "FF1-8EVTK3RE")
		svc.ShowUpdating(20) // OTA took the screen before the rename landed
		ran := false
		svc.RefreshClaimQRName(resolveTo("Living Room", &ran))
		sender.waitForCalls(t, 2)

		assert.False(t, ran, "the resolver must not run with no claim QR to refresh")
		assert.Equal(t, stateUpdating, sender.lastRequest()["state"])
		assert.Equal(t, 2, sender.callCount(), "no claim_qr may be re-delivered over updating")
	})

	t.Run("no intent: no push, no resolve", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		ran := false
		svc.RefreshClaimQRName(resolveTo("Living Room", &ran))

		assert.False(t, ran)
		assert.Equal(t, 0, sender.callCount())
		assert.False(t, svc.Narrating())
	})
}

func TestShowConnectingIfShowing(t *testing.T) {
	t.Run("paints over the named state", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowFinalizing()
		sender.waitForCalls(t, 1)
		svc.ShowConnectingIfShowing("Connected by cable, but no internet. (FF1-TEST)", StateFinalizing)
		sender.waitForCalls(t, 2)

		req := sender.lastRequest()
		require.NotNil(t, req)
		assert.Equal(t, stateConnecting, req["state"])
		assert.Equal(t, "Connected by cable, but no internet. (FF1-TEST)", req["reason"])
	})

	t.Run("a concurrently painted softap_qr survives", func(t *testing.T) {
		sender := newFakePlayer()
		svc := New(sender, nil)

		svc.ShowSoftAPQR("FF1-TEST", "12345678", "http://10.42.0.1", false)
		sender.waitForCalls(t, 1)
		svc.ShowConnectingIfShowing("Connected by cable, but no internet.", StateFinalizing)

		time.Sleep(50 * time.Millisecond)
		assert.Equal(t, 1, sender.callCount(),
			"the conditional paint must not overwrite another narrator's overlay")
		assert.Equal(t, stateSoftAPQR, sender.lastRequest()["state"])
	})
}
func TestStatusRequiresHostObservation(t *testing.T) {
	sender := newFakePlayer()
	s := New(sender, nil)
	sender.err = errors.New("host absent")
	s.ShowClaimQR("https://example.org/claim", "FF1")
	sender.waitForCalls(t, 1)
	state, ok := s.CurrentNarrationState()
	require.False(t, ok)
	require.Empty(t, state)
	require.True(t, s.Narrating(), "intent remains for replay")
	s.Observe(playerlink.Snapshot{Overlay: &playerlink.Overlay{Owner: "setup", State: "claim_qr"}})
	state, ok = s.CurrentNarrationState()
	require.True(t, ok)
	require.Equal(t, "claim_qr", state)
	s.Observe(playerlink.Snapshot{Overlay: &playerlink.Overlay{Owner: "pairing", State: "pairing_code"}})
	_, ok = s.CurrentNarrationState()
	require.False(t, ok)
}
func TestSlowHostDoesNotBlockProvisioning(t *testing.T) {
	sender := newFakePlayer()
	sender.gate = make(chan struct{})
	s := New(sender, nil)
	done := make(chan struct{})
	go func() { s.ShowConnecting("Checking the network"); s.Hide(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("provisioning blocked behind display")
	}
	close(sender.gate)
	sender.waitForCalls(t, 2)
}

type scriptedWatch struct {
	connected  chan func(playerlink.SnapshotDelivery)
	disconnect chan struct{}
}

func (w *scriptedWatch) Watch(ctx context.Context, deliver func(playerlink.SnapshotDelivery)) error {
	select {
	case w.connected <- deliver:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-w.disconnect:
		return errors.New("host disconnected")
	case <-ctx.Done():
		return ctx.Err()
	}
}
func TestHostReconnectReplaysIntentAndClearsUnobservedStatus(t *testing.T) {
	sender := newFakePlayer()
	s := New(sender, nil)
	s.ShowConnecting("Checking the network")
	sender.waitForCalls(t, 1)
	watcher := &scriptedWatch{connected: make(chan func(playerlink.SnapshotDelivery)), disconnect: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { s.Run(ctx, watcher); close(done) }()
	callback := <-watcher.connected
	callback(playerlink.SnapshotDelivery{Initial: true, Snapshot: playerlink.Snapshot{Overlay: &playerlink.Overlay{Owner: "setup", State: "connecting"}}})
	sender.waitForCalls(t, 2)
	_, visible := s.CurrentNarrationState()
	require.True(t, visible)
	watcher.disconnect <- struct{}{}
	require.Eventually(t, func() bool { _, showing := s.CurrentNarrationState(); return !showing }, time.Second, time.Millisecond)
	select {
	case callback = <-watcher.connected:
	case <-time.After(2 * time.Second):
		t.Fatal("watch did not reconnect")
	}
	callback(playerlink.SnapshotDelivery{Initial: true, Snapshot: playerlink.Snapshot{}})
	sender.waitForCalls(t, 3)
	require.Equal(t, "connecting", sender.lastRequest()["state"])
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("watch failed to stop")
	}
}

func TestPassiveSetupRetriesOnceOnOlderHost(t *testing.T) {
	p := newFakePlayer()
	p.err = &playerlink.RemoteError{Code: "invalid_params", Message: "unknown passive flag"}
	s := New(p, nil)
	s.version = "3.2.0"
	req := map[string]any{"state": stateScanning, "passive": true}
	err := s.trySend(t.Context(), req)
	require.ErrorIs(t, err, p.err)
	require.Len(t, p.requests, 2)
	require.Equal(t, true, p.requests[0]["passive"])
	require.NotContains(t, p.requests[1], "passive")
	require.Equal(t, "3.2.0", p.requests[1]["version"])
	require.Equal(t, true, req["passive"])
}
