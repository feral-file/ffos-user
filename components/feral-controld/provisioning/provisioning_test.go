package provisioning

import (
	"context"
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
}

func (w *fakeWifi) HasSavedProfile(context.Context) (bool, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.hasProfile, w.profileErr
}
func (w *fakeWifi) RefreshScanCache(context.Context) ([]string, error) {
	w.rec.add("wifi.RefreshScanCache")
	return []string{"Net"}, nil
}
func (w *fakeWifi) CachedScan(context.Context) ([]string, error) {
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
	rec     *recorder
	cfg     portal.Config
	started bool
}

func (p *fakePortal) Start() error {
	p.rec.add("portal.Start")
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
		NewPortal: func(cfg portal.Config) PortalServer {
			p := &fakePortal{rec: rec, cfg: cfg}
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

	h.m.onConnectivity(ctx, false) // go offline
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

	h.m.onConnectivity(ctx, false)
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
	h.m.onConnectivity(ctx, false)
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)
	require.Equal(t, StateAPActive, h.m.State())

	// Known network returns.
	h.m.onConnectivity(ctx, true)
	assert.Equal(t, StateOnline, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Down"))
	assert.Equal(t, 1, h.rec.count("portal.Stop"))
}

func TestUnprovisionedNoEthernetRaisesAPImmediately(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false) // no saved wifi profile
	ctx := context.Background()

	h.m.onConnectivity(ctx, false) // offline and no other connectivity

	assert.Equal(t, StateAPActive, h.m.State())
	assert.Equal(t, 1, h.rec.count("ap.Up"), "unprovisioned + offline raises AP immediately")
}

func TestUnprovisionedWithEthernetDoesNotRaiseAP(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false) // no saved wifi profile
	ctx := context.Background()

	h.m.onConnectivity(ctx, true) // reachable via Ethernet

	assert.Equal(t, StateUnprovisioned, h.m.State())
	assert.Equal(t, 0, h.rec.count("ap.Up"), "Ethernet devices never raise the AP")
}

func TestAuthFailureReRaisesAPAndRecordsOutcome(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	// Get to APActive.
	h.m.onConnectivity(ctx, false)
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

	h.m.onConnectivity(ctx, false)
	require.Equal(t, StateAPActive, h.m.State())

	h.wifi.joinErr = nil
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

func TestJoinIgnoredWhenNotAPActive(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(true)
	ctx := context.Background()

	h.m.onConnectivity(ctx, true) // Online, AP down
	require.Equal(t, StateOnline, h.m.State())

	h.m.applyJoin(ctx, "HomeNet", "pw") // stray submission
	assert.Equal(t, StateOnline, h.m.State())
	assert.Equal(t, 0, h.rec.count("wifi.Join:HomeNet"))
}

func TestPortalSeamsWireToMachine(t *testing.T) {
	h := newHarness(t)
	h.wifi.setProfile(false)
	ctx := context.Background()

	h.m.onConnectivity(ctx, false)
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

	h.m.onConnectivity(ctx, false) // OfflineRetrying
	h.clk.advance(6 * time.Minute)
	h.m.onTick(ctx)               // APActive
	h.m.onConnectivity(ctx, true) // Online

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
