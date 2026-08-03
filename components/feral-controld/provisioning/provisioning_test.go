package provisioning

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/portal"
	"github.com/feral-file/ffos-user/components/feral-controld/softap"
	"github.com/feral-file/ffos-user/components/feral-controld/wifictl"
)

// recorder captures an ordered, cross-fake call log so tests can assert the
// hard AP-bounce/scan-cache sequencing.
type recorder struct {
	mu     sync.Mutex
	events []string
}

func (r *recorder) add(s string) {
	r.mu.Lock()
	r.events = append(r.events, s)
	r.mu.Unlock()
}

func (r *recorder) list() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.events...)
}

func (r *recorder) count(s string) int {
	n := 0
	for _, e := range r.list() {
		if e == s {
			n++
		}
	}
	return n
}

// indexOf returns the first index of s, or -1.
func indexOf(list []string, s string) int {
	for i, e := range list {
		if e == s {
			return i
		}
	}
	return -1
}

// --- fakes -------------------------------------------------------------------

type fakeAP struct {
	rec     *recorder
	upErr   error
	downErr error
	info    softap.Info
}

func (a *fakeAP) Up(context.Context) (softap.Info, error) {
	a.rec.add("ap.Up")
	if a.upErr != nil {
		return softap.Info{}, a.upErr
	}
	return a.info, nil
}
func (a *fakeAP) Down(context.Context) error {
	a.rec.add("ap.Down")
	return a.downErr
}
func (a *fakeAP) Status(context.Context) (softap.Status, error) {
	return softap.Status{}, nil
}

type fakeWifi struct {
	rec        *recorder
	mu         sync.Mutex
	hasProfile bool
	profileErr error
	joinErr    error // returned by Join
	// scanErrs is consumed one per RefreshScanCache call; nil entries and calls
	// beyond the script succeed.
	scanErrs []error
	// emptyScans makes that many RefreshScanCache calls return (nil, nil) — a
	// scan that succeeded but saw nothing, which the real controller reports
	// when NM's BSS list is still empty after an AP-mode flip.
	emptyScans int
	// panicNext makes the next HasSavedProfile call panic (once), injecting a
	// loop-goroutine panic for supervisor-recovery tests.
	panicNext bool
	// savedSSIDs/savedHidden/savedSSIDsErr script SavedWifiSSIDs. nil
	// savedSSIDs falls back to ["Net"] when hasProfile is true — an SSID
	// present in the default ScanAllSSIDs result — so a test that wires
	// BootAssessment without overriding these stays on the windowed path;
	// relocation tests override explicitly.
	savedSSIDs    []string
	savedHidden   bool
	savedSSIDsErr error
	// scanAll/scanAllErr script ScanAllSSIDs (the relocation check's uncapped
	// scan). nil scanAll falls back to ["Net"].
	scanAll    []string
	scanAllErr error
	// scanAllHook, when set, runs during each ScanAllSSIDs call (outside the
	// fake's lock) with the 1-based call number: final-gate tests use it to
	// flip the link state WHILE the scan is in flight.
	scanAllHook  func(call int)
	scanAllCalls int
}

func (w *fakeWifi) HasSavedProfile(context.Context) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.panicNext {
		w.panicNext = false
		panic("injected: saved-profile check exploded")
	}
	return w.hasProfile, w.profileErr
}

func (w *fakeWifi) armPanic() {
	w.mu.Lock()
	w.panicNext = true
	w.mu.Unlock()
}
func (w *fakeWifi) RefreshScanCache(context.Context) ([]string, error) {
	w.rec.add("wifi.RefreshScanCache")
	w.mu.Lock()
	var err error
	if len(w.scanErrs) > 0 {
		err = w.scanErrs[0]
		w.scanErrs = w.scanErrs[1:]
	}
	empty := w.emptyScans > 0
	if empty {
		w.emptyScans--
	}
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if empty {
		return nil, nil
	}
	return []string{"Net"}, nil
}
func (w *fakeWifi) CachedScan(context.Context) ([]string, error) {
	return []string{"Net"}, nil
}

func (w *fakeWifi) SavedWifiSSIDs(context.Context) ([]string, bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.savedSSIDsErr != nil {
		return nil, false, w.savedSSIDsErr
	}
	if w.savedSSIDs != nil {
		return append([]string(nil), w.savedSSIDs...), w.savedHidden, nil
	}
	if w.hasProfile {
		return []string{"Net"}, w.savedHidden, nil
	}
	return nil, w.savedHidden, nil
}

func (w *fakeWifi) ScanAllSSIDs(context.Context) ([]string, error) {
	w.rec.add("wifi.ScanAllSSIDs")
	w.mu.Lock()
	w.scanAllCalls++
	call := w.scanAllCalls
	hook := w.scanAllHook
	err := w.scanAllErr
	// nil scanAll means "unscripted" (falls back to ["Net"]); a non-nil EMPTY
	// scanAll is a scripted empty scan and must stay empty, so copy without
	// collapsing empty-to-nil.
	scripted := w.scanAll != nil
	res := make([]string, len(w.scanAll))
	copy(res, w.scanAll)
	w.mu.Unlock()
	if hook != nil {
		hook(call)
	}
	if err != nil {
		return nil, err
	}
	if scripted {
		return res, nil
	}
	return []string{"Net"}, nil
}
func (w *fakeWifi) Join(_ context.Context, ssid, _ string) error {
	w.rec.add("wifi.Join:" + ssid)
	return w.joinErr
}
func (w *fakeWifi) setProfile(v bool) {
	w.mu.Lock()
	w.hasProfile = v
	w.mu.Unlock()
}

type fakeConn struct {
	mu     sync.Mutex
	online bool
	err    error
	fn     func(bool)
}

func (c *fakeConn) Online(context.Context) (bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online, c.err
}
func (c *fakeConn) Subscribe(fn func(bool)) func() {
	c.mu.Lock()
	c.fn = fn
	c.mu.Unlock()
	return func() {}
}
func (c *fakeConn) setErr(err error) {
	c.mu.Lock()
	c.err = err
	c.mu.Unlock()
}
func (c *fakeConn) push(online bool) {
	c.mu.Lock()
	c.online = online
	fn := c.fn
	c.mu.Unlock()
	if fn != nil {
		fn(online)
	}
}

type fakePortal struct {
	rec      *recorder
	cfg      portal.Config
	started  bool
	startErr error
}

func (p *fakePortal) Start() error {
	p.rec.add("portal.Start")
	if p.startErr != nil {
		return p.startErr
	}
	p.started = true
	return nil
}
func (p *fakePortal) Stop(context.Context) error {
	p.rec.add("portal.Stop")
	p.started = false
	return nil
}

type fakeNotifier struct {
	mu    sync.Mutex
	rec   *recorder
	calls []struct {
		State  State
		Detail Detail
	}
}

func (n *fakeNotifier) OnStateChange(s State, d Detail) {
	n.mu.Lock()
	n.calls = append(n.calls, struct {
		State  State
		Detail Detail
	}{s, d})
	n.mu.Unlock()
	// Mirror into the shared recorder so tests can assert notify ordering
	// against AP/portal operations in one timeline.
	if n.rec != nil {
		n.rec.add("notify:" + string(s) + ":" + d.Reason)
	}
}
func (n *fakeNotifier) states() []State {
	n.mu.Lock()
	defer n.mu.Unlock()
	out := make([]State, len(n.calls))
	for i, c := range n.calls {
		out[i] = c.State
	}
	return out
}

// harness bundles a machine with its fakes.
type harness struct {
	m        *Machine
	rec      *recorder
	ap       *fakeAP
	wifi     *fakeWifi
	conn     *fakeConn
	clk      *fakeClock
	notifier *fakeNotifier
	portals  []*fakePortal
	// portalStartErr, when set, makes every NEW portal's Start fail with it.
	portalStartErr error
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	rec := &recorder{}
	h := &harness{
		rec:      rec,
		ap:       &fakeAP{rec: rec, info: softap.Info{SSID: "FF1-abc", PSK: "abc12345"}},
		wifi:     &fakeWifi{rec: rec},
		conn:     &fakeConn{},
		clk:      newFakeClock(),
		notifier: &fakeNotifier{rec: rec},
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
		NewPortal: func(cfg portal.Config) PortalServer {
			p := &fakePortal{rec: rec, cfg: cfg, startErr: h.portalStartErr}
			h.portals = append(h.portals, p)
			return p
		},
	})
	return h
}

// --- transition-table tests --------------------------------------------------

func TestTransientOutageBelowWindowDoesNotRaiseAP(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true) // provisioned
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // go offline
	assert.Equal(t, StateOfflineRetrying, h.m.State())

	// A router reboot: still offline but only 2 minutes elapsed.
	h.clk.advance(2 * time.Minute)
	h.m.onTick(ctx)

	assert.Equal(t, StateOfflineRetrying, h.m.State())
	assert.Equal(t, 0, h.rec.count("ap.Up"), "AP must not raise below the window")
}

func TestSustainedOfflineRaisesAP(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	h.clk.advance(6 * time.Minute) // past the 5-minute window
	h.m.onTick(ctx)

	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"))
	assert.Equal(t, 1, h.rec.count("portal.Start"))
	// Constraint 1: scan cache refreshed before the AP came up.
	list := h.rec.list()
	assert.Less(t, indexOf(list, "wifi.RefreshScanCache"), indexOf(list, "ap.Up"))
}

func TestRecoveryTakesAPDown(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	ctx := context.Background()

	// Drive it up first.
	h.m.onConnectivity(ctx, false, false)
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	require.Equal(t, StateAPActive, h.m.State())

	// Known network returns.
	h.m.onConnectivity(ctx, true, false)
	assert.Equal(t, StateOnline, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Down"))
	assert.Equal(t, 1, h.rec.count("portal.Stop"))
}

func TestUnprovisionedNoEthernetRaisesAPImmediately(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false) // no saved wifi profile
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // offline and no other connectivity

	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"), "unprovisioned + offline raises AP immediately")
}

func TestUnprovisionedWithEthernetDoesNotRaiseAP(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false) // no saved wifi profile
	ctx := context.Background()

	h.m.onConnectivity(ctx, true, false) // reachable via Ethernet

	assert.Equal(t, StateUnprovisioned, h.m.State())
	assert.Equal(t, 0, h.rec.count("ap.Up"), "Ethernet devices never raise the AP")
}

func TestAuthFailureReRaisesAPAndRecordsOutcome(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	// Get to APActive.
	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	// User submits a wrong password.
	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "HomeNet", "wrong-pw")

	// Back in APActive so the user can retry.
	assert.Equal(t, StateAPActive, h.m.State())

	// Outcome recorded for GET /status.
	st := h.m.Status()
	assert.Equal(t, portal.JoinFailed, st.State)
	assert.Equal(t, "auth-failure", st.Reason)
	assert.Equal(t, "HomeNet", st.SSID)
	assert.NotEmpty(t, st.Message)

	// Sequencing: initial Up, then Down before Join, then a re-raise Up.
	list := h.rec.list()
	assert.Equal(t, 2, h.rec.count("ap.Up"), "AP re-raised after auth failure")
	downIdx := indexOf(list, "ap.Down")
	joinIdx := indexOf(list, "wifi.Join:HomeNet")
	require.NotEqual(t, -1, downIdx)
	require.NotEqual(t, -1, joinIdx)
	assert.Less(t, downIdx, joinIdx, "Constraint 2: AP down before Join")
}

func TestSuccessfulJoinGoesOnlineAndLeavesAPDown(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	h.wifi.joinErr = nil
	// The join brings real reachability: the post-join assessment sees online.
	h.conn.online = true
	h.wifi.setProfile(true)
	h.m.applyJoin(ctx, "HomeNet", "correct-pw")

	assert.Equal(t, StateOnline, h.m.State())
	st := h.m.Status()
	assert.Equal(t, portal.JoinSucceeded, st.State)
	assert.Equal(t, "HomeNet", st.SSID)

	// AP came up once (for setup) and went down for the join; it is NOT re-raised.
	assert.Equal(t, 1, h.rec.count("ap.Up"))
	assert.Equal(t, 1, h.rec.count("ap.Down"))
	list := h.rec.list()
	assert.Less(t, indexOf(list, "ap.Down"), indexOf(list, "wifi.Join:HomeNet"))
}

// TestJoinAbortsWhenAPTeardownFails: constraint 2 requires the AP down before
// the station join; if softap.Down fails the join must NOT proceed (the hotspot
// may still hold the radio, and a success would strand the leftover profile).
// The machine reports the outcome on /status and re-raises the AP — softap.Up
// replaces the profile, self-healing the failed teardown — so a retry works.
func TestJoinAbortsWhenAPTeardownFails(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	h.ap.downErr = errors.New("nmcli connection delete timed out")
	scansBefore := h.rec.count("wifi.RefreshScanCache")
	h.m.applyJoin(ctx, "HomeNet", "pw")

	assert.Equal(t, 0, h.rec.count("wifi.Join:HomeNet"), "join must not run while the AP teardown failed")
	assert.Equal(t, StateAPActive, h.m.State(), "machine re-enters APActive for retry")
	st := h.m.Status()
	assert.Equal(t, portal.JoinFailed, st.State)
	assert.Equal(t, "ap-teardown-failed", st.Reason)
	assert.Equal(t, "HomeNet", st.SSID)
	// The re-raise is GATED on the pending teardown: while the old profile
	// cannot be deleted there must be no scan (the leftover AP may still own
	// the radio) and no second raise (duplicate-profile hazard).
	assert.Equal(t, 1, h.rec.count("ap.Up"), "no re-raise while the teardown is pending")
	assert.Equal(t, scansBefore, h.rec.count("wifi.RefreshScanCache"), "no scan while the teardown is pending")

	// Backend recovers: the tick deletes the leftover FIRST, then scans, then
	// re-raises — the ordering the single radio requires.
	h.ap.downErr = nil
	h.m.onTick(ctx)
	assert.Equal(t, 2, h.rec.count("ap.Up"), "recovered tick re-raises the AP")
	events := h.rec.list()
	lastIdx := func(s string) int {
		last := -1
		for i, e := range events {
			if e == s {
				last = i
			}
		}
		return last
	}
	assert.Less(t, lastIdx("ap.Down"), lastIdx("wifi.RefreshScanCache"),
		"pending deletion must complete before the pre-AP scan: %v", events)
	assert.Less(t, lastIdx("wifi.RefreshScanCache"), lastIdx("ap.Up"),
		"pre-AP scan must complete before the re-raise: %v", events)

	// And the user's retry now joins normally.
	h.conn.online = true
	h.wifi.setProfile(true)
	h.m.applyJoin(ctx, "HomeNet", "pw")
	assert.Equal(t, 1, h.rec.count("wifi.Join:HomeNet"))
	assert.Equal(t, StateOnline, h.m.State())
}

// TestRescanTeardownFailureGatesReRaise: the rescan bounce shares the gate —
// if the AP teardown fails mid-bounce, no scan and no re-raise may happen
// until the pending deletion succeeds (single radio; duplicate-profile
// hazard). The tick converges once the backend recovers.
func TestRescanTeardownFailureGatesReRaise(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	h.ap.downErr = errors.New("nmcli connection delete timed out")
	scansBefore := h.rec.count("wifi.RefreshScanCache")
	h.m.applyRescan(ctx)

	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"), "no re-raise while the teardown is pending")
	assert.Equal(t, scansBefore, h.rec.count("wifi.RefreshScanCache"), "no scan while the teardown is pending")

	h.ap.downErr = nil
	h.m.onTick(ctx)
	assert.Equal(t, 2, h.rec.count("ap.Up"), "recovered tick completes the bounce")
	assert.Greater(t, h.rec.count("wifi.RefreshScanCache"), scansBefore, "fresh scan ran before the re-raise")
}

// TestJoinSucceedsButStillOfflineArmsRecovery: a successful nmcli association
// to a network with no upstream must NOT park the machine in StateOnline —
// sys-monitord emits no change event (offline before, offline after), so
// nothing would ever correct it. The post-join assessment files the device as
// provisioned-but-offline, and the sustained-offline window brings the AP
// back if the network stays dark.
func TestJoinSucceedsButStillOfflineArmsRecovery(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	// Association succeeds; reachability stays false; the join persisted a
	// profile.
	h.wifi.joinErr = nil
	h.conn.online = false
	h.wifi.setProfile(true)
	h.m.applyJoin(ctx, "DeadUplink", "pw")

	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"association without reachability must land in provisioned-offline, not Online")
	st := h.m.Status()
	assert.Equal(t, portal.JoinSucceeded, st.State, "the association itself DID succeed")
	assert.Equal(t, 1, h.rec.count("ap.Up"), "AP stays down while NM retries")

	// The network stays dark past the sustained-offline window: the setup AP
	// must come back.
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 2, h.rec.count("ap.Up"), "recovery AP re-raised after the window")

	// And if reachability arrives instead, the machine settles Online.
	h.m.onConnectivity(ctx, true, false)
	assert.Equal(t, StateOnline, h.m.State())
}

// TestOnlineRecoveryRetriesFailedAPTeardown: a Down failure while leaving
// APActive (e.g. connectivity restored) must not orphan the persisted hotspot
// profile until the next daemon boot — apDownPending makes every subsequent
// reconcile retry the deletion until it succeeds.
func TestOnlineRecoveryRetriesFailedAPTeardown(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	// Reachability returns while the AP teardown fails.
	h.ap.downErr = errors.New("nmcli connection delete timed out")
	h.wifi.setProfile(true)
	h.m.onConnectivity(ctx, true, false)
	require.Equal(t, StateOnline, h.m.State())
	downsAfterFailure := h.rec.count("ap.Down")
	require.GreaterOrEqual(t, downsAfterFailure, 1)

	// While the failure persists, ticks keep retrying the profile deletion.
	h.m.onTick(ctx)
	assert.Greater(t, h.rec.count("ap.Down"), downsAfterFailure, "tick must retry the failed deletion")

	// Backend recovers: one more retry deletes the profile, then retries stop.
	h.ap.downErr = nil
	h.m.onTick(ctx)
	settled := h.rec.count("ap.Down")
	h.m.onTick(ctx)
	assert.Equal(t, settled, h.rec.count("ap.Down"), "no further Down calls once the deletion succeeded")
}

func TestJoinIgnoredWhenNotAPActive(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, true, false) // Online, AP down
	require.Equal(t, StateOnline, h.m.State())

	h.m.applyJoin(ctx, "HomeNet", "pw") // stray submission
	assert.Equal(t, StateOnline, h.m.State())
	assert.Equal(t, 0, h.rec.count("wifi.Join:HomeNet"))
}

func TestPortalSeamsWireToMachine(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	require.Len(t, h.portals, 1)

	cfg := h.portals[0].cfg
	assert.Equal(t, "FF1-abc", cfg.APSSID)
	require.NotNil(t, cfg.Scan)
	require.NotNil(t, cfg.Join)
	require.NotNil(t, cfg.Status)

	// StatusFunc reads live machine status.
	assert.Equal(t, portal.JoinIdle, cfg.Status().State)

	// JoinFunc validates empty SSID.
	assert.Error(t, cfg.Join("", "pw"))
	// A valid submission is accepted (enqueued) without blocking.
	assert.NoError(t, cfg.Join("HomeNet", "pw"))
}

func TestNotifierReceivesStateChanges(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // OfflineRetrying
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)                      // APActive
	h.m.onConnectivity(ctx, true, false) // Online

	states := h.notifier.states()
	assert.Contains(t, states, StateOfflineRetrying)
	assert.Contains(t, states, StateAPActive)
	assert.Contains(t, states, StateOnline)
}

// --- lifecycle test through the real loop ------------------------------------

func TestStartStopDrivesInitialAssessmentAndTearsDown(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.conn.online = false // unprovisioned + offline -> AP on start

	ctx := context.Background()
	h.m.Start(ctx)

	// The loop's initial assessment should raise the AP.
	require.Eventually(t, func() bool {
		return h.m.State() == StateAPActive && h.rec.count("ap.Up") == 1
	}, 2*time.Second, 5*time.Millisecond)

	h.m.Stop()
	// Stop tears the AP down.
	assert.GreaterOrEqual(t, h.rec.count("ap.Down"), 1)
}

func TestConnectivityRecoveryThroughLoop(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.conn.online = false

	ctx := context.Background()
	h.m.Start(ctx)
	defer h.m.Stop()

	require.Eventually(t, func() bool {
		return h.m.State() == StateAPActive
	}, 2*time.Second, 5*time.Millisecond)

	// Provision + reachability restored, pushed via the connectivity signal.
	h.wifi.setProfile(true)
	h.conn.push(true)

	require.Eventually(t, func() bool {
		return h.m.State() == StateOnline && h.rec.count("ap.Down") >= 1
	}, 2*time.Second, 5*time.Millisecond)
}

// TestJoinWithFailedPostJoinQueryRecoversOnTick: sys-monitord briefly
// unavailable during a successful association must not become a permanent
// offline verdict — the boot-time connUnknown discipline applies to the
// post-join assessment too, so a tick re-query (no connectivity event needed)
// settles the machine Online instead of raising the AP over a working uplink
// after the offline window.
func TestJoinWithFailedPostJoinQueryRecoversOnTick(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	// Association succeeds; the uplink genuinely works, but the post-join
	// query fails (monitord blip).
	h.wifi.joinErr = nil
	h.conn.online = true
	h.conn.setErr(errors.New("monitord momentarily unavailable"))
	h.wifi.setProfile(true)
	h.m.applyJoin(ctx, "HomeNet", "pw")

	require.Equal(t, StateOfflineRetrying, h.m.State(), "failed query is assumed offline for now")
	require.Equal(t, portal.JoinSucceeded, h.m.Status().State)

	// monitord recovers: the next tick's re-query must settle Online with no
	// connectivity event, and the AP must never come back.
	h.conn.setErr(nil)
	h.m.onTick(ctx)
	assert.Equal(t, StateOnline, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"), "no AP re-raise over a working uplink")
}

// TestOnlineBootWithFailedInitialQueryRecoversOnTick: controld starts before
// sys-monitord, so the boot-time reachability query can fail; the machine then
// assumes offline and raises the AP. monitord may resolve its initial state
// before its signal emitters register, so no connectivity_change event ever
// corrects the guess — ticks must re-query until one succeeds, and an actually
// online device must drop the wrongly raised AP.
func TestOnlineBootWithFailedInitialQueryRecoversOnTick(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.conn.online = true                           // actually reachable (e.g. ethernet)
	h.conn.err = errors.New("monitord not up yet") // ...but the query fails at boot

	h.m.Start(context.Background())
	defer h.m.Stop()

	// The failed query is assumed offline: unprovisioned + offline raises the AP.
	require.Eventually(t, func() bool {
		return h.m.State() == StateAPActive && h.rec.count("ap.Up") == 1
	}, 2*time.Second, 5*time.Millisecond, "assumed-offline boot must raise the AP")

	// monitord comes up: the next tick's re-query gets the real answer and the
	// machine converges to the online truth (unprovisioned + reachable =
	// Unprovisioned, AP down).
	h.conn.setErr(nil)
	h.clk.tick <- time.Time{}

	require.Eventually(t, func() bool {
		return h.m.State() == StateUnprovisioned && h.rec.count("portal.Stop") >= 1
	}, 2*time.Second, 5*time.Millisecond, "tick re-query must correct the assumption and drop the AP")
}

// TestRecoveredPanicRebuildsActiveAP: the supervisor reruns loop after a panic
// with whatever in-memory state the previous run left. With a stale apUp=true
// the restarted run's boot sweep would delete the real hotspot while every
// ensureAPUp early-returns on the flag — a dead AP the machine believes is up.
// The loop-entry reset must stop the leftover portal, clear the AP bookkeeping,
// and rebuild AP + portal from scratch.
func TestRecoveredPanicRebuildsActiveAP(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.conn.online = false // unprovisioned + offline -> AP on start

	h.m.Start(context.Background())
	defer h.m.Stop()

	require.Eventually(t, func() bool {
		return h.m.State() == StateAPActive && h.rec.count("portal.Start") == 1
	}, 2*time.Second, 5*time.Millisecond)

	// Panic on the loop goroutine while the AP is up, triggered by the next
	// connectivity event.
	h.wifi.armPanic()
	h.conn.push(false)

	// The recovered run must tear down the stale portal and re-raise the pair;
	// a run that trusts the stale apUp would never call ap.Up or portal.Start
	// again. (Assert via the recorder only: h.portals is appended on the loop
	// goroutine.)
	require.Eventually(t, func() bool {
		return h.m.RestartCount() == 1 &&
			h.rec.count("portal.Stop") >= 1 &&
			h.rec.count("ap.Up") >= 2 &&
			h.rec.count("portal.Start") >= 2
	}, 2*time.Second, 5*time.Millisecond, "restarted loop must rebuild AP+portal, got: %v", h.rec.list())
	assert.Equal(t, StateAPActive, h.m.State())
}

// TestPortalBindFailureTearsAPBackDown is the orphaned-AP regression: if the
// captive portal cannot bind, the just-raised radio hotspot must be torn back
// down in the same ensureAPUp — apUp is still false at that point, so a later
// ensureAPDown would no-op and the WPA2 setup AP (PSK derivable from the
// advertised SSID) would keep broadcasting with nothing behind it, surviving
// daemon shutdown as a persisted NM profile. The next tick then retries the
// whole scan→AP→portal sequence cleanly.
func TestPortalBindFailureTearsAPBackDown(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false) // unprovisioned
	h.portalStartErr = errors.New("listen tcp :80: address already in use")
	ctx := context.Background()

	// Offline + unprovisioned + no wire → APActive → reconcile → portal fails.
	h.m.onConnectivity(ctx, false, false)

	events := h.rec.list()
	iUp := indexOf(events, "ap.Up")
	iStart := indexOf(events, "portal.Start")
	iDown := indexOf(events, "ap.Down")
	require.NotEqual(t, -1, iUp, "AP must be raised: %v", events)
	require.NotEqual(t, -1, iStart, "portal start must be attempted: %v", events)
	require.NotEqual(t, -1, iDown, "AP must be torn back down after the portal failed to bind: %v", events)
	assert.Less(t, iStart, iDown, "teardown must follow the failed portal start: %v", events)
	assert.Equal(t, StateAPActive, h.m.State(), "machine stays in APActive so the tick retries")

	// Retry with the bind conflict resolved: the full constraint-1 sequence
	// (scan refresh before ap.Up) must run again and converge.
	h.portalStartErr = nil
	h.m.onTick(ctx)

	events = h.rec.list()
	assert.Equal(t, 2, h.rec.count("ap.Up"), "retry must re-raise the AP: %v", events)
	assert.Equal(t, 2, h.rec.count("portal.Start"), "retry must re-start the portal: %v", events)
	require.Len(t, h.portals, 2)
	assert.True(t, h.portals[1].started, "second portal must be running")
	// The retry's scan refresh must precede its ap.Up (radio back in station mode).
	last := events[indexOf(events[iDown:], "wifi.RefreshScanCache")+iDown:]
	assert.Less(t, indexOf(last, "wifi.RefreshScanCache"), indexOf(last, "ap.Up"),
		"retry must scan before re-raising the AP: %v", events)
}

// TestFreshAPRaiseResetsStaleJoinStatus: a join outcome from a past setup must
// not survive onto a portal raised weeks later for sustained-offline — the
// user mid-re-setup would see a "Connected to X" success banner.
func TestFreshAPRaiseResetsStaleJoinStatus(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	// Full successful setup: the join brings real reachability.
	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	h.conn.online = true
	h.wifi.setProfile(true)
	h.m.applyJoin(ctx, "HomeNet", "pw")
	require.Equal(t, StateOnline, h.m.State())
	require.Equal(t, portal.JoinSucceeded, h.m.Status().State)

	// Weeks later: provisioned but sustained-offline past the window.
	h.conn.online = false
	h.m.onConnectivity(ctx, false, false)
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	require.Equal(t, StateAPActive, h.m.State())

	// The re-raised portal must greet the user fresh, not with the old outcome.
	assert.Equal(t, portal.JoinIdle, h.m.Status().State)
}

// TestRedundantOfflineKeepsJoinFailureStatus: the unprovisioned-offline branch
// is level-triggered, so a redundant offline event while the AP is already up
// (wlan churn during the post-failure re-raise) must not wipe the join-failure
// outcome the re-associated phone polls /status for.
func TestRedundantOfflineKeepsJoinFailureStatus(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	// User submits a wrong password; the failure outcome must be recorded.
	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "HomeNet", "wrong-pw")
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, portal.JoinFailed, h.m.Status().State)

	// Redundant offline event with the AP already up: status must survive.
	h.m.onConnectivity(ctx, false, false)
	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, portal.JoinFailed, h.m.Status().State)
}

// details returns a copy of every recorded notification.
func (n *fakeNotifier) details() []struct {
	State  State
	Detail Detail
} {
	n.mu.Lock()
	defer n.mu.Unlock()
	return append([]struct {
		State  State
		Detail Detail
	}(nil), n.calls...)
}

// TestAPRaiseScanRetriesUntilComplete: the AP must not come up until a scan
// pass completes — transient pre-AP scan failures are retried first, because
// the portal picker can only ever offer what that scan saw.
func TestAPRaiseScanRetriesUntilComplete(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.wifi.scanErrs = []error{errors.New("busy"), errors.New("busy")}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 3, h.rec.count("wifi.RefreshScanCache"), "two failures then a success")
	assert.Equal(t, 1, h.rec.count("ap.Up"))
	// The successful (last) scan still precedes the raise.
	list := h.rec.list()
	lastScan := -1
	for i, e := range list {
		if e == "wifi.RefreshScanCache" {
			lastScan = i
		}
	}
	assert.Less(t, lastScan, indexOf(list, "ap.Up"))
}

// TestAPRaiseProceedsAfterScanRetriesExhausted: a persistently failing scan
// must not strand the user — the AP still comes up and the portal falls back
// to manual SSID entry.
func TestAPRaiseProceedsAfterScanRetriesExhausted(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.wifi.scanErrs = repeatErrs(preAPScanAttempts, "busy")
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, preAPScanAttempts, h.rec.count("wifi.RefreshScanCache"))
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// TestAPRaiseRetriesEmptyScan: a scan that SUCCEEDS with zero networks is
// retried like a failure. Right after the rescan bounce NM's BSS list is still
// empty while the radio finishes flipping out of AP mode, and accepting that
// first empty answer is what blanked the portal picker.
func TestAPRaiseRetriesEmptyScan(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.wifi.emptyScans = 2
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 3, h.rec.count("wifi.RefreshScanCache"), "two empty scans then a populated one")
	assert.Equal(t, 1, h.rec.count("ap.Up"))
	// The populated (last) scan still precedes the raise.
	list := h.rec.list()
	lastScan := -1
	for i, e := range list {
		if e == "wifi.RefreshScanCache" {
			lastScan = i
		}
	}
	assert.Less(t, lastScan, indexOf(list, "ap.Up"))
}

// TestAPRaiseProceedsAfterEmptyScansExhausted: a genuinely empty environment
// must still get an AP — the retries bound the wait, they do not gate the raise.
func TestAPRaiseProceedsAfterEmptyScansExhausted(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.wifi.emptyScans = preAPScanAttempts
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, preAPScanAttempts, h.rec.count("wifi.RefreshScanCache"))
	assert.Equal(t, 1, h.rec.count("ap.Up"))
}

// repeatErrs builds an n-long scanErrs script of identical failures.
func repeatErrs(n int, msg string) []error {
	errs := make([]error, n)
	for i := range errs {
		errs[i] = errors.New(msg)
	}
	return errs
}

// TestScanningNarrationPrecedesAPRaise: the "scanning" announcement fires
// before the credential-bearing AP-up announcement, so the screen can show a
// scan-in-progress state until the hotspot is actually ready.
func TestScanningNarrationPrecedesAPRaise(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	scanIdx, apIdx := -1, -1
	for i, c := range h.notifier.details() {
		if c.State != StateAPActive {
			continue
		}
		if c.Detail.Reason == "scanning" && scanIdx == -1 {
			scanIdx = i
		}
		if c.Detail.PSK != "" && apIdx == -1 {
			apIdx = i
		}
	}
	require.NotEqual(t, -1, scanIdx, "scanning announcement must fire")
	require.NotEqual(t, -1, apIdx, "credential-bearing AP-up announcement must fire")
	assert.Less(t, scanIdx, apIdx)
}

// --- portal rescan bounce ----------------------------------------------------

// TestRescanBouncesAPAndRunsFreshScan: the portal's "search again" request
// tears the AP down, runs a fresh station-mode scan, and re-raises — in that
// order — while the machine stays in StateAPActive throughout.
func TestRescanBouncesAPAndRunsFreshScan(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	require.Equal(t, 1, h.rec.count("ap.Up"))

	h.m.applyRescan(ctx)

	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Down"))
	assert.Equal(t, 2, h.rec.count("ap.Up"))
	assert.Equal(t, 1, h.rec.count("portal.Stop"))
	assert.Equal(t, 2, h.rec.count("portal.Start"))
	assert.Equal(t, 2, h.rec.count("wifi.RefreshScanCache"))

	// After the bounce: down first, then the fresh scan, then the re-raise.
	list := h.rec.list()
	down := indexOf(list, "ap.Down")
	require.NotEqual(t, -1, down)
	tail := list[down:]
	assert.Less(t, indexOf(tail, "wifi.RefreshScanCache"), indexOf(tail, "ap.Up"),
		"fresh scan must precede the re-raise: %v", list)
}

// TestRescanClearsStaleJoinOutcome: the bounce starts a fresh portal session,
// so a pre-bounce join failure must not greet the re-associated phone.
func TestRescanClearsStaleJoinOutcome(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	h.wifi.joinErr = &wifictl.JoinError{Kind: wifictl.JoinErrAuth, Output: "secrets were required"}
	h.m.applyJoin(ctx, "HomeNet", "wrong-pw")
	require.Equal(t, portal.JoinFailed, h.m.Status().State)

	h.m.applyRescan(ctx)

	assert.Equal(t, portal.JoinIdle, h.m.Status().State)
}

// TestRescanIgnoredOutsideAPActive: an online device has no picker to refresh;
// the request must not touch the AP.
func TestRescanIgnoredOutsideAPActive(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, true, false)
	require.Equal(t, StateOnline, h.m.State())

	h.m.applyRescan(ctx)

	assert.Equal(t, 0, h.rec.count("ap.Down"))
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestRescanNarratesScanningThenQR: the on-screen state must follow the bounce
// — scanning first, then the QR once the AP is back.
func TestRescanNarratesScanningThenQR(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())
	before := len(h.notifier.details())

	h.m.applyRescan(ctx)

	scanIdx, qrIdx := -1, -1
	for i, c := range h.notifier.details()[before:] {
		if c.Detail.Reason == "scanning" && scanIdx == -1 {
			scanIdx = i
		}
		if c.Detail.PSK != "" && qrIdx == -1 {
			qrIdx = i
		}
	}
	require.NotEqual(t, -1, scanIdx, "rescan must narrate scanning")
	require.NotEqual(t, -1, qrIdx, "rescan must re-narrate the QR after the bounce")
	assert.Less(t, scanIdx, qrIdx)
}

// TestRescanFlipsToScanningBeforeTeardown: the button press must repaint the
// screen to "scanning" BEFORE the seconds-long AP teardown, not after it —
// otherwise the stale join QR (or, on players without the scanning renderer,
// a blank overlay) reads as a stalled press.
func TestRescanFlipsToScanningBeforeTeardown(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	h.m.applyRescan(ctx)

	// Between the initial raise (first ap.Up) and the bounce's ap.Down there
	// must be a scanning announcement — the immediate button-press repaint.
	list := h.rec.list()
	firstUp := indexOf(list, "ap.Up")
	down := indexOf(list, "ap.Down")
	require.NotEqual(t, -1, firstUp)
	require.NotEqual(t, -1, down)
	seg := list[firstUp:down]
	assert.NotEqual(t, -1, indexOf(seg, "notify:ap_active:scanning"),
		"rescan must flip to scanning before teardown: %v", list)
}

// TestBootSweepsLeftoverAPBeforeFirstScan: an ungraceful previous exit leaves
// the persisted ff1-softap profile behind while this boot starts with
// apUp=false, so ensureAPDown would never touch it. The loop must sweep it
// unconditionally at startup, BEFORE the first pre-AP scan (the leftover AP
// would otherwise hold the radio through that scan).
func TestBootSweepsLeftoverAPBeforeFirstScan(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	h.conn.online = false // unprovisioned + offline -> AP raise after the sweep

	h.m.Start(context.Background())
	defer h.m.Stop()

	require.Eventually(t, func() bool {
		return h.rec.count("ap.Up") == 1
	}, 2*time.Second, 5*time.Millisecond)

	list := h.rec.list()
	sweep := indexOf(list, "ap.Down")
	require.NotEqual(t, -1, sweep, "boot must sweep the leftover AP: %v", list)
	assert.Less(t, sweep, indexOf(list, "wifi.RefreshScanCache"),
		"sweep must precede the first pre-AP scan: %v", list)
	assert.Less(t, sweep, indexOf(list, "ap.Up"))
}

// TestBootWhileOnlineNotifiesInitialState: a daemon restart on a healthy,
// online device must still notify the initial StateOnline — the auto claim
// trigger hangs off that notification, and initializing the machine AT
// StateOnline used to swallow it (Online→Online, changed=false).
func TestBootWhileOnlineNotifiesInitialState(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	h.conn.online = true

	h.m.Start(context.Background())
	defer h.m.Stop()

	require.Eventually(t, func() bool {
		for _, s := range h.notifier.states() {
			if s == StateOnline {
				return true
			}
		}
		return false
	}, 2*time.Second, 5*time.Millisecond,
		"boot-while-online must notify StateOnline (the claim trigger depends on it)")
}

// --- offline entry-edge narration + boot relocation (F-01 / M-0 / M-1) -------

// lastDetail returns the most recent notification, failing the test when none
// was recorded.
func (n *fakeNotifier) lastDetail(t *testing.T) (State, Detail) {
	t.Helper()
	all := n.details()
	require.NotEmpty(t, all, "expected at least one notification")
	last := all[len(all)-1]
	return last.State, last.Detail
}

// markBoot wires the harness machine as a device-boot process start
// (Config.BootAssessment true, as latched by New), the precondition for every
// boot-entry narration and the relocation check.
func markBoot(h *harness) {
	h.m.startedAtBoot = true
}

// TestBootOfflineEntryNarratesAndKeepsWindow (M-0a): a provisioned device
// booting with its network out of reach must say so on screen immediately —
// the previous behavior was a black screen for the whole sustained-offline
// window — while the window itself still governs the AP raise.
func TestBootOfflineEntryNarratesAndKeepsWindow(t *testing.T) {
	h := newHarness(t)
	markBoot(h)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // boot assessment: state is StateStarting

	require.Equal(t, StateOfflineRetrying, h.m.State())
	st, d := h.notifier.lastDetail(t)
	assert.Equal(t, StateOfflineRetrying, st)
	assert.Equal(t, ReasonBootOffline, d.Reason)
	assert.Contains(t, d.Message, "Setup mode will start",
		"the narration must set the expectation the window creates")
	assert.Equal(t, 0, h.rec.count("ap.Up"), "narration alone must not shortcut the window")

	// The window is untouched: the AP still rises only after it elapses.
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State())
}

// TestDaemonRestartMidOutageStaysSilent (C-1 regression): controld is
// Restart=always, so StateStarting alone does NOT mean a device boot. Without
// the BootAssessment probe (nil, or false), a restart during an
// exhibition-long outage must take the generic un-narrated path — no boot
// narration over playing offline-cache artwork, no relocation scan, no AP.
func TestDaemonRestartMidOutageStaysSilent(t *testing.T) {
	h := newHarness(t) // startedAtBoot deliberately left false
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet"} // would confirm relocation IF consulted
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateOfflineRetrying, h.m.State())
	_, d := h.notifier.lastDetail(t)
	assert.Equal(t, "offline", d.Reason,
		"a mid-life restart must keep the reason the notifier leaves un-narrated")
	assert.Equal(t, 0, h.rec.count("wifi.ScanAllSSIDs"), "no relocation scan outside a device boot")

	// Ticks keep the plain window semantics: no relocation raise ever.
	h.m.onTick(ctx)
	h.m.onTick(ctx)
	assert.Equal(t, 0, h.rec.count("wifi.ScanAllSSIDs"))
	assert.Equal(t, StateOfflineRetrying, h.m.State())
}

// TestBootClassificationLatchedAtConstruction (review P1): boot-vs-restart is
// a property of PROCESS START, so New must latch it at construction (wiring
// time, moments after exec) instead of re-reading /proc/uptime at the initial
// offline assessment — which runs after the boot AP sweep and the initial
// connectivity query and can therefore land past the boot window's edge on a
// slow boot (large offline cache, monitord D-Bus timeout). A probe that is
// true at New but false by assessment time is exactly that shape, and it must
// still take the narrated boot path.
func TestBootClassificationLatchedAtConstruction(t *testing.T) {
	rec := &recorder{}
	wifi := &fakeWifi{rec: rec}
	wifi.setProfile(true)
	notifier := &fakeNotifier{rec: rec}
	probeCalls := 0
	m := New(Config{
		AP:           &fakeAP{rec: rec, info: softap.Info{SSID: "FF1-abc", PSK: "abc12345"}},
		Wifi:         wifi,
		Connectivity: &fakeConn{},
		Clock:        newFakeClock(),
		Logger:       zap.NewNop(),
		Notifier:     notifier,
		PortalAddr:   "127.0.0.1:0",
		BootAssessment: func() bool {
			probeCalls++
			return probeCalls == 1 // inside the window at New, outside ever after
		},
	})
	require.Equal(t, 1, probeCalls, "New must evaluate the probe exactly once, at construction")

	m.onConnectivity(context.Background(), false, false) // the (late) boot assessment

	require.Equal(t, StateOfflineRetrying, m.State())
	st, d := notifier.lastDetail(t)
	assert.Equal(t, StateOfflineRetrying, st)
	assert.Equal(t, ReasonBootOffline, d.Reason,
		"a process that started at boot must narrate even when the assessment itself lands past the window")
	assert.Equal(t, 1, probeCalls, "the assessment must consume the latched value, never re-read the probe")
}

// TestRelocationFinalGateLinkRestoredDuringLastScanDefersToWindow (review P1):
// the top-of-tick link probe predates the multi-second final scan, and a link
// that comes back while it runs (the ethernet escape hatch plugged in; an AP
// whose beacons the scan pass missed but the supplicant then caught) has its
// event queued BEHIND this tick — invisible before the raise. The raise is a
// one-way door on Wi-Fi, so the machine must re-probe immediately before it
// and treat anything short of confirmed absence as a sighting.
func TestRelocationFinalGateLinkRestoredDuringLastScanDefersToWindow(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet"}
	// The link recovers WHILE the third (confirming) scan is in flight.
	h.wifi.scanAllHook = func(call int) {
		if call == 3 {
			fl.up = true
		}
	}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // scan 1 (boot assessment)
	h.m.onTick(ctx)                       // scan 2
	h.m.onTick(ctx)                       // scan 3 — would raise without the gate

	assert.Equal(t, StateOfflineRetrying, h.m.State(), "a link sighted by the final gate must veto the raise")
	assert.Equal(t, 0, h.rec.count("ap.Up"))

	// Sighting semantics: the ladder is disarmed for good (arming is
	// boot-assessment-only), so later absent ticks fall back to the plain
	// window and never scan again.
	fl.up = false
	h.m.onTick(ctx)
	assert.Equal(t, 3, h.rec.count("wifi.ScanAllSSIDs"), "ladder must stay disarmed after the veto")
	assert.Equal(t, StateOfflineRetrying, h.m.State())
}

// TestRelocationFinalGateLinkUnknownDuringLastScanDefersToWindow: the gate's
// unknown flavor — a probe that FAILS during the final scan cannot confirm
// absence, and unknown never authorizes a raise (the same bias as the window
// path and hasProfile).
func TestRelocationFinalGateLinkUnknownDuringLastScanDefersToWindow(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet"}
	h.wifi.scanAllHook = func(call int) {
		if call == 3 {
			fl.err = errors.New("injected: nmcli timed out")
		}
	}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	h.m.onTick(ctx)
	h.m.onTick(ctx)

	assert.Equal(t, StateOfflineRetrying, h.m.State(), "an unknown link at the final gate must veto the raise")
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestBootNarrationUpgradesWhenLinkAppears (review P1): the boot entry's
// "setup will start in a few minutes" promise is painted off a confirmed-
// absent link; when a link then returns, the AP is correctly suppressed —
// but the same-state transition dedupe repaints nothing, so on an air-gapped
// LAN the false promise would sit on screen forever. A linkPresent tick must
// repaint the no-internet wording, exactly once.
func TestBootNarrationUpgradesWhenLinkAppears(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	_, d := h.notifier.lastDetail(t)
	require.Equal(t, ReasonBootOffline, d.Reason)

	fl.up = true
	h.m.onTick(ctx)
	st, d := h.notifier.lastDetail(t)
	assert.Equal(t, StateOfflineRetrying, st)
	assert.Equal(t, ReasonBootNoInternet, d.Reason,
		"a sighted link must replace the setup promise with the no-internet wording")
	assert.Contains(t, d.Message, "no internet access")

	// Further link-present ticks must not spam the narration surface.
	n := len(h.notifier.details())
	h.m.onTick(ctx)
	assert.Len(t, h.notifier.details(), n)
}

// TestBootNarrationHedgesWhenProbeTurnsUnknown (review P1): an unknown probe
// disarms the window — setup is deferred until a FUTURE confirmed absence —
// so the "setup will start in a few minutes" promise painted at a
// confirmed-absent boot entry stops being true, indefinitely so under a
// persistently failing probe. The tick must repaint the same hedge the entry
// table would have chosen ("Checking the network connection…"), exactly once,
// and restore the promise when absence is confirmed again.
func TestBootNarrationHedgesWhenProbeTurnsUnknown(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	_, d := h.notifier.lastDetail(t)
	require.Equal(t, ReasonBootOffline, d.Reason)

	fl.err = errors.New("injected: nmcli timed out")
	h.m.onTick(ctx)
	_, d = h.notifier.lastDetail(t)
	assert.Equal(t, ReasonBootLinkUnknown, d.Reason,
		"an unknown probe defers setup, so the promise must give way to the hedge")
	assert.Contains(t, d.Message, "Checking the network connection")

	// Further unknown ticks must not spam the narration surface.
	n := len(h.notifier.details())
	h.m.onTick(ctx)
	assert.Len(t, h.notifier.details(), n)

	// A confirmed absence re-arms the window and restores the promise.
	fl.err = nil
	h.m.onTick(ctx)
	_, d = h.notifier.lastDetail(t)
	assert.Equal(t, ReasonBootOffline, d.Reason)
	assert.Contains(t, d.Message, "Setup mode will start")
}

// TestBootNarrationDowngradesWhenLinkDropsAgain: the upgrade's mirror. An
// entry (or upgrade) that asserted "connected, no internet" stops being true
// when the link is confirmed gone, and the setup promise becomes accurate
// again — the window is arming below it.
func TestBootNarrationDowngradesWhenLinkDropsAgain(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // boot with a live link: no-internet wording
	_, d := h.notifier.lastDetail(t)
	require.Equal(t, ReasonBootNoInternet, d.Reason)

	fl.up = false
	h.m.onTick(ctx)
	_, d = h.notifier.lastDetail(t)
	assert.Equal(t, ReasonBootOffline, d.Reason,
		"a confirmed-lost link must restore the setup promise")
	assert.Contains(t, d.Message, "Setup mode will start")
}

// TestExhibitionOutageLinkSightingStaysSilent: the narration-truthfulness
// ticks are gated on the boot marker — a mid-life restart's un-narrated
// offline entry must stay silent through link comings and goings.
func TestExhibitionOutageLinkSightingStaysSilent(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl) // startedAtBoot deliberately left false
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	n := len(h.notifier.details()) // the entry transition (generic, un-painted)

	fl.up = true
	h.m.onTick(ctx)
	fl.up = false
	h.m.onTick(ctx)
	assert.Len(t, h.notifier.details(), n, "no narration repaints outside a boot-narrated episode")
}

// TestBootNarrationDoesNotLeakIntoLaterOutage: any transition away from
// offline_retrying clears the marker, so a later mid-exhibition outage
// (deliberately silent) can never inherit the boot narration's repaints.
func TestBootNarrationDoesNotLeakIntoLaterOutage(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // narrated boot entry
	h.m.onConnectivity(ctx, true, false)  // recovery: leaving the state clears the marker
	h.m.onConnectivity(ctx, false, false) // mid-exhibition outage: silent entry
	n := len(h.notifier.details())

	fl.up = true
	h.m.onTick(ctx)
	assert.Len(t, h.notifier.details(), n, "the boot marker must not survive an online transition")
}

// TestBootRelocationConfirmedByRepeatedScansRaisesAP (M-0b): the moved-frame
// shape — saved profile, confirmed absent link, and relocConfirmScans
// consecutive full scans with no saved SSID. The AP must rise only after the
// LAST confirming scan (~30-45s), never off the single boot-time sample: one
// scan at t≈0 cannot tell a moved frame from a router still booting after the
// same power cut, and the raise is a one-way door.
func TestBootRelocationConfirmedByRepeatedScansRaisesAP(t *testing.T) {
	fl := &fakeLink{up: false} // wired guard, REAL confirmed absence
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet", "NeighborNet"}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // sample 1 (boot)
	require.Equal(t, StateOfflineRetrying, h.m.State(), "no raise off a single scan")
	_, d := h.notifier.lastDetail(t)
	assert.Equal(t, ReasonBootOffline, d.Reason, "the boot narration paints before/despite the scan")

	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // sample 2
	require.Equal(t, StateOfflineRetrying, h.m.State(), "still confirming")

	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // sample 3 → confirmed
	assert.Equal(t, StateAPActive, h.m.State(), "three consecutive positive scans confirm relocation")
	assert.Equal(t, 3, h.rec.count("wifi.ScanAllSSIDs"))
	assert.Equal(t, 1, h.rec.count("ap.Up"))
	found := false
	for _, c := range h.notifier.details() {
		if c.State == StateAPActive && c.Detail.Reason == ReasonRelocated {
			found = true
		}
	}
	assert.True(t, found, "the raise must carry the relocated reason")
}

// TestBootRelocationLiveLinkNeverArms (M-0b ⚠, pins the linkAbsent conjunct):
// a wired frame with a stale Wi-Fi profile boots offline with a LIVE link and
// a scan that shows none of the saved SSIDs — the relocation check must not
// even arm: the link is direct counter-evidence, and raising would drop it.
func TestBootRelocationLiveLinkNeverArms(t *testing.T) {
	fl := &fakeLink{up: true}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet"}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateOfflineRetrying, h.m.State())
	assert.Equal(t, 0, h.rec.count("wifi.ScanAllSSIDs"), "a live link must skip the relocation scan entirely")
	_, d := h.notifier.lastDetail(t)
	assert.Equal(t, ReasonBootNoInternet, d.Reason)

	h.m.onTick(ctx)
	assert.Equal(t, 0, h.rec.count("wifi.ScanAllSSIDs"))
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestBootRelocationDisconfirmedMidwayFallsBackToWindow (M-0b / C-4): the
// saved SSID reappears on the second sample (router finished booting) — the
// confirmation must disarm permanently for this boot and leave the plain
// sustained-offline window in charge.
func TestBootRelocationDisconfirmedMidwayFallsBackToWindow(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet"}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // sample 1: positive
	h.clk.advance(15 * time.Second)
	h.wifi.mu.Lock()
	h.wifi.scanAll = []string{"CafeNet", "HomeNet"} // router came back
	h.wifi.mu.Unlock()
	h.m.onTick(ctx) // sample 2: disconfirmed → disarm
	require.Equal(t, StateOfflineRetrying, h.m.State())

	h.wifi.mu.Lock()
	h.wifi.scanAll = []string{"CafeNet"} // even if it vanishes again…
	h.wifi.mu.Unlock()
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx)
	scans := h.rec.count("wifi.ScanAllSSIDs")
	assert.Equal(t, 2, scans, "…the disarmed check must not keep scanning")
	assert.Equal(t, StateOfflineRetrying, h.m.State())

	// The plain window still recovers the device.
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State())
}

// TestBootRelocationInconclusiveEvidenceNeverArms (M-0b ⚠): every
// inconclusive input — scan error, empty scan (radio still settling), an
// unreadable profile list, or a saved profile targeting a HIDDEN network
// (whose SSID can never appear in a scan) — must keep the full window.
func TestBootRelocationInconclusiveEvidenceNeverArms(t *testing.T) {
	cases := []struct {
		name string
		mut  func(w *fakeWifi)
	}{
		{"scan error", func(w *fakeWifi) { w.scanAllErr = errors.New("nmcli scan failed") }},
		{"empty scan", func(w *fakeWifi) { w.scanAll = []string{} }},
		{"profile list error", func(w *fakeWifi) { w.savedSSIDsErr = errors.New("nmcli list failed") }},
		{"hidden saved network", func(w *fakeWifi) { w.savedHidden = true }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fl := &fakeLink{up: false}
			h := newLinkHarness(t, fl)
			markBoot(h)
			h.wifi.setProfile(true)
			h.wifi.savedSSIDs = []string{"HomeNet"}
			h.wifi.scanAll = []string{"CafeNet"}
			tc.mut(h.wifi)
			ctx := context.Background()

			h.m.onConnectivity(ctx, false, false)
			h.clk.advance(15 * time.Second)
			h.m.onTick(ctx)
			h.clk.advance(15 * time.Second)
			h.m.onTick(ctx)

			assert.Equal(t, StateOfflineRetrying, h.m.State())
			assert.Equal(t, 0, h.rec.count("ap.Up"))
			_, d := h.notifier.lastDetail(t)
			assert.Equal(t, ReasonBootOffline, d.Reason)
		})
	}
}

// TestBootOfflineLinkUnknownHedges (M-0a honesty): a failed link probe knows
// nothing — the narration must hedge, not assert a missing network or promise
// setup mode.
func TestBootOfflineLinkUnknownHedges(t *testing.T) {
	fl := &fakeLink{err: errors.New("nmcli probe timeout")}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateOfflineRetrying, h.m.State())
	_, d := h.notifier.lastDetail(t)
	assert.Equal(t, ReasonBootLinkUnknown, d.Reason)
	assert.Contains(t, d.Message, "Checking")
	assert.Equal(t, 0, h.rec.count("wifi.ScanAllSSIDs"), "an unknown link must not arm the relocation check")
}

// TestJoinOfflineEdgeNarratesNoInternet (M-1a): the F-01 shape — the join
// ASSOCIATED but the upstream is dead. The machine already refused to park in
// StateOnline (TestJoinSucceedsButStillOfflineArmsRecovery); now the entry
// into offline_retrying must also SAY what happened instead of leaving the
// "Connecting to X" screen up forever.
func TestJoinOfflineEdgeNarratesNoInternet(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	h.conn.online = false
	h.wifi.setProfile(true) // the join persisted a profile
	h.m.applyJoin(ctx, "DeadUplink", "pw")

	require.Equal(t, StateOfflineRetrying, h.m.State())
	_, d := h.notifier.lastDetail(t)
	assert.Equal(t, ReasonJoinedNoInternet, d.Reason)
	assert.Equal(t, "DeadUplink", d.SSID)
	assert.Contains(t, d.Message, "no internet access")
	assert.Contains(t, d.Message, "DeadUplink")
	assert.Equal(t, portal.JoinSucceeded, h.m.Status().State,
		"the portal contract is untouched: the association DID succeed")
}

// TestJoinOfflineQueryFailureHedges (M-1a ⚠): when the post-join reachability
// QUERY fails, offline is an assumption — the narration must hedge, never
// assert a dead network (the network may be fine and sys-monitord briefly
// down). The hedge is the leg's terminal narration by design (same-state
// re-notifications are deduped), pinned here so a future "improvement" that
// asserts no-internet off a failed query fails this test.
func TestJoinOfflineQueryFailureHedges(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateAPActive, h.m.State())

	h.conn.setErr(errors.New("monitord unavailable"))
	h.wifi.setProfile(true)
	h.m.applyJoin(ctx, "MaybeFineNet", "pw")

	require.Equal(t, StateOfflineRetrying, h.m.State())
	_, d := h.notifier.lastDetail(t)
	assert.Equal(t, ReasonJoinedConnUnknown, d.Reason)
	assert.Contains(t, d.Message, "Checking internet access")
	assert.NotContains(t, d.Message, "no internet",
		"a failed query must not assert a dead network")

	// The assumption self-heals: the next tick re-queries, and a real online
	// reading settles the machine (connUnknown ownership moved into
	// onConnectivity with the assumed parameter — this pins it end to end).
	h.conn.setErr(nil)
	h.conn.online = true
	h.m.onTick(ctx)
	assert.Equal(t, StateOnline, h.m.State())
}

// TestExhibitionOfflineEdgeKeepsGenericReason (M-0 ⚠): the online→offline edge
// on a playing device keeps the un-narrated generic reason — flashing a setup
// overlay over artwork on a WAN blip is exactly what the notifier's silent
// default exists to prevent.
func TestExhibitionOfflineEdgeKeepsGenericReason(t *testing.T) {
	h := newHarness(t)
	markBoot(h) // even on a boot-window process, the ONLINE→offline edge is not a boot entry
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, true, false)
	require.Equal(t, StateOnline, h.m.State())

	h.m.onConnectivity(ctx, false, false)

	require.Equal(t, StateOfflineRetrying, h.m.State())
	_, d := h.notifier.lastDetail(t)
	assert.Equal(t, "offline", d.Reason,
		"the exhibition edge must keep the reason the notifier leaves un-narrated")
}

// TestBootRelocationEmptyFirstScanArmsOnLaterTick (R-1): the first scan runs
// moments after the boot AP sweep flips the radio to station mode — the
// documented moment an empty (inconclusive) result is expected. That must
// consume arming budget, not forfeit the feature: a later clean positive
// within the budget still arms, and the ladder completes.
func TestBootRelocationEmptyFirstScanArmsOnLaterTick(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{} // radio still settling at the assessment
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // attempt 1: inconclusive
	require.Equal(t, StateOfflineRetrying, h.m.State())

	h.wifi.mu.Lock()
	h.wifi.scanAll = []string{"CafeNet"} // radio settled; still no saved SSID
	h.wifi.mu.Unlock()
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // attempt 2: positive → armed (confirm 1)
	require.Equal(t, StateOfflineRetrying, h.m.State())
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // confirm 2
	require.Equal(t, StateOfflineRetrying, h.m.State())
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // confirm 3 → raise
	assert.Equal(t, StateAPActive, h.m.State(),
		"an empty first scan must not render the relocation check inert")
	assert.Equal(t, 4, h.rec.count("wifi.ScanAllSSIDs"))
}

// TestBootRelocationArmingBudgetExhausts (R-1 bound): persistent inconclusive
// scans may only burn relocArmTries attempts; after that the check goes quiet
// (no further scans) and the plain window owns recovery.
func TestBootRelocationArmingBudgetExhausts(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAllErr = errors.New("nmcli keeps failing")
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // attempt 1
	for i := 0; i < 4; i++ {
		h.clk.advance(15 * time.Second)
		h.m.onTick(ctx) // attempts 2..3, then no-ops
	}
	assert.Equal(t, relocArmTries, h.rec.count("wifi.ScanAllSSIDs"),
		"scanning must stop once the arming budget is spent")
	require.Equal(t, StateOfflineRetrying, h.m.State())

	// The plain window still recovers the device.
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	assert.Equal(t, StateAPActive, h.m.State())
}

// TestBootRelocationStaleArmDoesNotSurviveLaterEpisode pins the clearOffline
// coupling: a check armed at boot, resolved by the device coming ONLINE, must
// be fully disarmed — hours later a fresh offline episode with an absent link
// must go through the plain window, never resume a stale confirmation ladder
// and raise the AP over artwork after ~30s.
func TestBootRelocationStaleArmDoesNotSurviveLaterEpisode(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet"}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // boot: armed (confirm 1)
	require.Equal(t, 1, h.rec.count("wifi.ScanAllSSIDs"))

	h.m.onConnectivity(ctx, true, false) // internet arrives → resolved
	require.Equal(t, StateOnline, h.m.State())

	// Hours later: an exhibition offline episode with the link genuinely gone.
	h.clk.advance(3 * time.Hour)
	h.m.onConnectivity(ctx, false, false)
	require.Equal(t, StateOfflineRetrying, h.m.State())
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx)
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx)

	assert.Equal(t, 1, h.rec.count("wifi.ScanAllSSIDs"),
		"a resolved boot check must never scan again in a later episode")
	assert.Equal(t, StateOfflineRetrying, h.m.State(),
		"the later episode is owned by the plain window, not a stale ladder")
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}

// TestBootRelocationLinkUnknownMidConfirmationForfeits: an inconclusive link
// probe between confirmations disarms via the sighting path (clearOffline) —
// the ladder is consecutive clean probe+scan pairs by design, and the forfeit
// is permanent for this boot (arming is boot-assessment-only).
func TestBootRelocationLinkUnknownMidConfirmationForfeits(t *testing.T) {
	fl := &fakeLink{up: false}
	h := newLinkHarness(t, fl)
	markBoot(h)
	h.wifi.setProfile(true)
	h.wifi.savedSSIDs = []string{"HomeNet"}
	h.wifi.scanAll = []string{"CafeNet"}
	ctx := context.Background()

	h.m.onConnectivity(ctx, false, false) // boot: armed (confirm 1)

	fl.err = errors.New("nmcli probe flake")
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx) // linkUnknown → clearOffline → disarm

	fl.err = nil // link reads absent again; scans stay positive
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx)
	h.clk.advance(15 * time.Second)
	h.m.onTick(ctx)

	assert.Equal(t, 1, h.rec.count("wifi.ScanAllSSIDs"),
		"a probe flake mid-confirmation must forfeit the check for this boot")
	assert.Equal(t, StateOfflineRetrying, h.m.State())
	assert.Equal(t, 0, h.rec.count("ap.Up"))
}
