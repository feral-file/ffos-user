package wifictl

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// scriptedExec fakes wrapper.Exec: every nmcli invocation is recorded (argv
// plus the ctx's liveness at call time) and answered by reply.
type scriptedExec struct {
	mu      sync.Mutex
	calls   [][]string
	ctxErrs []error
	reply   func(argv []string) ([]byte, error)
}

type scriptedCmd struct {
	out []byte
	err error
}

func (c scriptedCmd) String() string                  { return "scripted" }
func (c scriptedCmd) Run() error                      { return c.err }
func (c scriptedCmd) Start() error                    { return c.err }
func (c scriptedCmd) Wait() error                     { return c.err }
func (c scriptedCmd) Output() ([]byte, error)         { return c.out, c.err }
func (c scriptedCmd) CombinedOutput() ([]byte, error) { return c.out, c.err }

func (e *scriptedExec) CommandContext(ctx context.Context, name string, arg ...string) wrapper.ExecCmd {
	argv := append([]string{name}, arg...)
	e.mu.Lock()
	e.calls = append(e.calls, argv)
	e.ctxErrs = append(e.ctxErrs, ctx.Err())
	e.mu.Unlock()
	out, err := e.reply(argv)
	return scriptedCmd{out: out, err: err}
}

func (e *scriptedExec) recorded() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

func (e *scriptedExec) recordedCtxErrs() []error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]error(nil), e.ctxErrs...)
}

// fakeExitError is an error that reports a process exit code, matching the
// exitCoder contract *os/exec.ExitError satisfies in production.
type fakeExitError struct {
	code int
	msg  string
}

func (e fakeExitError) Error() string { return e.msg }
func (e fakeExitError) ExitCode() int { return e.code }

// fakeClock is a settable wrapper.Clock.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}
func (c *fakeClock) Sleep(time.Duration) {}

// SleepContext advances the fake clock by d so deadline-bounded retry loops
// (waitForSSID) terminate deterministically in tests.
func (c *fakeClock) SleepContext(ctx context.Context, d time.Duration) error {
	c.advance(d)
	return ctx.Err()
}
func (c *fakeClock) NewTicker(time.Duration) wrapper.Ticker { panic("unused") }

func newController(reply func(argv []string) ([]byte, error)) (*Controller, *scriptedExec, *fakeClock) {
	exec := &scriptedExec{reply: reply}
	clock := &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	return New(exec, clock, zap.NewNop(), ""), exec, clock
}

// --- classifyJoin: each error class ------------------------------------------

func TestClassifyJoin(t *testing.T) {
	cases := []struct {
		name string
		code int
		out  string
		want JoinErrorKind
	}{
		{"ssid not found by exit 10", 10, "", JoinErrSSIDNotFound},
		{"ssid not found by text", 1, "Error: No network with SSID 'Home' found.", JoinErrSSIDNotFound},
		{"timeout by exit 3", 3, "", JoinErrTimeout},
		{"timeout by text", 1, "Error: Timeout expired (90 seconds)", JoinErrTimeout},
		{"auth by exit 4", 4, "Error: Connection activation failed.", JoinErrAuth},
		{"auth by secrets text", 1, "Error: Secrets were required, but not provided.", JoinErrAuth},
		{"auth by 802-11 text", 1, "reason: 802-11-wireless-security key mgmt", JoinErrAuth},
		{"unknown", 1, "Error: NetworkManager is not running", JoinErrUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyJoin(tc.code, tc.out))
		})
	}
}

// --- Join: happy path + classified failure + cleanup -------------------------

// joinScanAware answers the post-bounce visibility scan with ssids and every
// other call with reply, so Join tests can script scan and connect separately.
func joinScanAware(ssids string, reply func(argv []string) ([]byte, error)) func(argv []string) ([]byte, error) {
	return func(argv []string) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "device wifi list") {
			return []byte(ssids), nil
		}
		return reply(argv)
	}
}

func TestJoinSuccess(t *testing.T) {
	c, exec, _ := newController(joinScanAware("Home\nOther", func(argv []string) ([]byte, error) {
		if len(argv) > 2 && argv[len(argv)-2] == "connection" && argv[len(argv)-1] == "show" {
			return nil, nil // no saved profiles: nothing to pre-delete
		}
		return []byte("Device 'wlan0' successfully activated"), nil
	}))
	err := c.Join(context.Background(), "Home", "supersecret")
	require.NoError(t, err)

	// Pre-delete listing (no matches, so no delete), visibility scan, connect.
	calls := exec.recorded()
	require.Len(t, calls, 3)
	assert.Equal(t, []string{"nmcli", "-t", "-f", "UUID,TYPE,NAME", "connection", "show"}, calls[0])
	assert.Contains(t, strings.Join(calls[1], " "), "device wifi list --rescan yes")
	assert.Contains(t, strings.Join(calls[2], " "), "device wifi connect Home password supersecret")
}

func TestJoinAuthFailureCleansUpProfile(t *testing.T) {
	// The first profile listing (pre-delete) is empty; after the failed connect
	// the half-created wifi profile shows up and must be deleted BY UUID.
	listings := 0
	c, exec, _ := newController(joinScanAware("Home", func(argv []string) ([]byte, error) {
		if len(argv) > 2 && argv[len(argv)-2] == "connection" && argv[len(argv)-1] == "show" {
			listings++
			if listings == 1 {
				return nil, nil
			}
			return []byte("half-created-uuid:802-11-wireless:Home\n"), nil
		}
		if len(argv) >= 4 && argv[1] == "device" && argv[2] == "wifi" && argv[3] == "connect" {
			return []byte("Error: Connection activation failed: (7) Secrets were required."),
				fakeExitError{code: 4, msg: "exit status 4"}
		}
		return nil, nil
	}))

	err := c.Join(context.Background(), "Home", "wrongpass")
	require.Error(t, err)

	var je *JoinError
	require.ErrorAs(t, err, &je)
	assert.Equal(t, JoinErrAuth, je.Kind)

	// Pre-delete listing, visibility scan, connect (fails), cleanup listing,
	// UUID-scoped delete of the half-created broken profile.
	calls := exec.recorded()
	require.Len(t, calls, 5)
	assert.Equal(t, []string{"nmcli", "connection", "delete", "uuid", "half-created-uuid"}, calls[4])
}

// TestJoinCleanupSparesNonWifiProfiles: ssid is user input from the captive
// portal, and nmcli deletes by connection ID match ANY profile type — a
// submission named like the device's ethernet or VPN profile must never
// delete it. Only the same-named 802-11-wireless profile may go, by UUID.
func TestJoinCleanupSparesNonWifiProfiles(t *testing.T) {
	c, exec, _ := newController(joinScanAware("Home", func(argv []string) ([]byte, error) {
		if len(argv) > 2 && argv[len(argv)-2] == "connection" && argv[len(argv)-1] == "show" {
			return []byte("eth-uuid:802-3-ethernet:Home\nwifi-uuid:802-11-wireless:Home\nvpn-uuid:vpn:Home\n"), nil
		}
		if len(argv) >= 4 && argv[1] == "device" && argv[2] == "wifi" && argv[3] == "connect" {
			return []byte("Error: Connection activation failed."),
				fakeExitError{code: 4, msg: "exit status 4"}
		}
		return nil, nil
	}))

	err := c.Join(context.Background(), "Home", "wrongpass")
	require.Error(t, err)

	deletes := [][]string{}
	for _, call := range exec.recorded() {
		if len(call) > 2 && call[1] == "connection" && call[2] == "delete" {
			deletes = append(deletes, call)
		}
	}
	// Pre-delete pass and post-failure pass each delete exactly the wifi UUID.
	require.Len(t, deletes, 2)
	for _, del := range deletes {
		assert.Equal(t, []string{"nmcli", "connection", "delete", "uuid", "wifi-uuid"}, del,
			"only the same-named WIFI profile may be deleted, never ethernet/VPN")
	}
}

// TestUnescapeTerse pins the terse-mode unescaping both profile-listing
// parsers (SavedProfiles, deleteWifiProfiles) depend on — NAME is the only
// field that can carry escapes.
func TestUnescapeTerse(t *testing.T) {
	assert.Equal(t, "Cafe:5G", unescapeTerse(`Cafe\:5G`))
	assert.Equal(t, `back\slash`, unescapeTerse(`back\\slash`))
	assert.Equal(t, "plain", unescapeTerse("plain"))
}

func TestJoinSSIDNotFound(t *testing.T) {
	// The visibility scan never surfaces "Ghost": the wait loop must exhaust its
	// window, still attempt the connect, and surface nmcli's truthful error.
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		if len(argv) >= 4 && argv[3] == "connect" {
			return []byte("Error: No network with SSID 'Ghost' found."),
				fakeExitError{code: 10, msg: "exit status 10"}
		}
		return []byte("SomeOtherNet"), nil
	})
	err := c.Join(context.Background(), "Ghost", "whatever1")
	var je *JoinError
	require.ErrorAs(t, err, &je)
	assert.Equal(t, JoinErrSSIDNotFound, je.Kind)

	// Multiple rescans happened before giving up and connecting anyway.
	scans, connects := 0, 0
	for _, call := range exec.recorded() {
		joined := strings.Join(call, " ")
		if strings.Contains(joined, "device wifi list") {
			scans++
		}
		if strings.Contains(joined, "device wifi connect") {
			connects++
		}
	}
	assert.GreaterOrEqual(t, scans, 2, "wait loop must rescan more than once")
	assert.Equal(t, 1, connects, "connect must still be attempted after the wait window")
}

func TestJoinTimeout(t *testing.T) {
	c, _, _ := newController(joinScanAware("Slow", func(argv []string) ([]byte, error) {
		if len(argv) >= 4 && argv[3] == "connect" {
			return []byte("Error: Timeout expired."), fakeExitError{code: 3, msg: "exit status 3"}
		}
		return nil, nil
	}))
	err := c.Join(context.Background(), "Slow", "password1")
	var je *JoinError
	require.ErrorAs(t, err, &je)
	assert.Equal(t, JoinErrTimeout, je.Kind)
}

func TestJoinIfacePinned(t *testing.T) {
	exec := &scriptedExec{reply: joinScanAware("Home", func(argv []string) ([]byte, error) { return nil, nil })}
	clock := &fakeClock{now: time.Now()}
	c := New(exec, clock, zap.NewNop(), "wlan0")
	require.NoError(t, c.Join(context.Background(), "Home", "password1"))
	scan, connect := exec.recorded()[1], exec.recorded()[2]
	assert.Contains(t, strings.Join(scan, " "), "ifname wlan0")
	assert.Contains(t, strings.Join(connect, " "), "ifname wlan0")
}

// TestJoinWaitsForSSIDAfterAPBounce pins the post-bounce sequencing: right
// after the hotspot goes down NM's scan list is empty, so Join must keep
// rescanning until the target reappears and only then attempt the connect.
func TestJoinWaitsForSSIDAfterAPBounce(t *testing.T) {
	var scanCount int
	var mu sync.Mutex
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "device wifi list") {
			mu.Lock()
			scanCount++
			n := scanCount
			mu.Unlock()
			if n < 3 {
				return nil, nil // empty scan: radio still settling
			}
			return []byte("Home\nOther"), nil
		}
		if strings.Contains(joined, "connect") {
			return []byte("Device 'wlan0' successfully activated"), nil
		}
		return nil, nil
	})

	require.NoError(t, c.Join(context.Background(), "Home", "secret12"))

	// delete, scan, scan, scan (visible), connect — connect strictly after the
	// scan that surfaced the SSID.
	calls := exec.recorded()
	require.Len(t, calls, 5)
	assert.Contains(t, strings.Join(calls[3], " "), "device wifi list")
	assert.Contains(t, strings.Join(calls[4], " "), "device wifi connect Home")
}

// TestJoinScanRetriesToleratesErrors: scans that error while the interface
// settles ("device busy") are retried rather than aborting the join.
func TestJoinScanRetriesToleratesErrors(t *testing.T) {
	var scanCount int
	var mu sync.Mutex
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "device wifi list") {
			mu.Lock()
			scanCount++
			n := scanCount
			mu.Unlock()
			if n == 1 {
				return []byte("Error: device busy"), fakeExitError{code: 1, msg: "exit status 1"}
			}
			return []byte("Home"), nil
		}
		if strings.Contains(joined, "connect") {
			return []byte("successfully activated"), nil
		}
		return nil, nil
	})
	require.NoError(t, c.Join(context.Background(), "Home", "secret12"))
}

// --- Scan cache TTL ----------------------------------------------------------

func TestCachedScanServesWithinTTLThenRefreshes(t *testing.T) {
	var scans int
	c, _, clock := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "device wifi list") {
			scans++
			return []byte("Alpha\nBravo\n"), nil
		}
		return nil, nil
	})

	// First call populates the cache with one live scan.
	got, err := c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Alpha", "Bravo"}, got)
	assert.Equal(t, 1, scans)

	// Within TTL: served from cache, no new scan.
	clock.advance(scanCacheTTL - time.Minute)
	_, err = c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, scans)

	// Past TTL: a fresh scan runs.
	clock.advance(2 * time.Minute)
	_, err = c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 2, scans)
}

// --- scan readiness gate -----------------------------------------------------

// deviceShowOut renders the terse `nmcli device show` block shape the readiness
// gate parses: one FIELD:value line per field, grouped per device, with STATE
// carrying the raw NMDeviceState enum ahead of its localized name.
func deviceShowOut(state int, stateName string) []byte {
	return []byte("GENERAL.DEVICE:eth0\nGENERAL.TYPE:ethernet\nGENERAL.STATE:100 (connected)\n" +
		"GENERAL.DEVICE:wlan0\nGENERAL.TYPE:wifi\nGENERAL.STATE:" +
		strconv.Itoa(state) + " (" + stateName + ")\n")
}

// TestRefreshScanCacheWaitsForScannableDevice is the root-cause regression.
// Right after the setup AP is torn down the Wi-Fi device sits below
// NM_DEVICE_STATE_DISCONNECTED while wpa_supplicant re-attaches, and NM refuses
// RequestScan there — which nmcli reports as exit 0 with zero rows, not as an
// error. Scanning in that window therefore cannot fail loudly; it can only lie.
// So the scan must not be issued until the device is scannable.
func TestRefreshScanCacheWaitsForScannableDevice(t *testing.T) {
	var stateCalls int
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "device show") {
			stateCalls++
			if stateCalls <= 2 {
				return deviceShowOut(20, "unavailable"), nil
			}
			return deviceShowOut(30, "disconnected"), nil
		}
		if strings.Contains(joined, "device wifi list") {
			return []byte("Alpha\n"), nil
		}
		return nil, nil
	})

	got, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Alpha"}, got)

	// The scan ran exactly once, and only after the device left "unavailable".
	var order []string
	for _, argv := range exec.recorded() {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "device show"):
			order = append(order, "state")
		case strings.Contains(joined, "device wifi list"):
			order = append(order, "scan")
		}
	}
	assert.Equal(t, []string{"state", "state", "state", "scan"}, order,
		"no scan may be issued while the radio would reject it")
}

// TestScanReadyGivesUpAndScansAnyway: a device stuck below DISCONNECTED must
// not stall provisioning forever — the AP has to come back up either way, and
// nmcli's real answer is still better than none.
func TestScanReadyGivesUpAndScansAnyway(t *testing.T) {
	scans := 0
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "device show") {
			return deviceShowOut(20, "unavailable"), nil
		}
		if strings.Contains(joined, "device wifi list") {
			scans++
			return []byte("Alpha\n"), nil
		}
		return nil, nil
	})

	got, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Alpha"}, got)
	assert.Equal(t, 1, scans, "the scan still runs after the readiness window closes")
}

// TestScanReadyFailsOpenOnUnknownState: an nmcli output shape we cannot parse
// (different version, unexpected fields) must not gate the scan at all.
func TestScanReadyFailsOpenOnUnknownState(t *testing.T) {
	cases := map[string][]byte{
		"empty output":      nil,
		"no wifi device":    []byte("GENERAL.DEVICE:eth0\nGENERAL.TYPE:ethernet\nGENERAL.STATE:100 (connected)\n"),
		"non-numeric state": []byte("GENERAL.DEVICE:wlan0\nGENERAL.TYPE:wifi\nGENERAL.STATE:disconnected\n"),
		"unexpected shape":  []byte("wlan0 wifi disconnected\n"),
		"state before type": []byte("GENERAL.STATE:20 (unavailable)\nGENERAL.DEVICE:wlan0\nGENERAL.TYPE:wifi\n"),
	}
	for name, showOut := range cases {
		t.Run(name, func(t *testing.T) {
			scans := 0
			c, _, _ := newController(func(argv []string) ([]byte, error) {
				joined := strings.Join(argv, " ")
				if strings.Contains(joined, "device show") {
					return showOut, nil
				}
				if strings.Contains(joined, "device wifi list") {
					scans++
					return []byte("Alpha\n"), nil
				}
				return nil, nil
			})

			got, err := c.RefreshScanCache(context.Background())
			require.NoError(t, err)
			assert.Equal(t, []string{"Alpha"}, got)
			assert.Equal(t, 1, scans)
		})
	}
}

// TestScanReadyFailsOpenOnStateQueryError: nmcli itself failing is not a reason
// to withhold the scan either.
func TestScanReadyFailsOpenOnStateQueryError(t *testing.T) {
	scans := 0
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "device show") {
			return []byte("Error: unknown"), fakeExitError{code: 1, msg: "exit status 1"}
		}
		if strings.Contains(joined, "device wifi list") {
			scans++
			return []byte("Alpha\n"), nil
		}
		return nil, nil
	})

	_, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 1, scans)
}

// TestScanReadyPinsIface: with an explicit interface the state query targets it
// rather than enumerating every device.
func TestScanReadyPinsIface(t *testing.T) {
	exec := &scriptedExec{reply: func(argv []string) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "device show") {
			return deviceShowOut(30, "disconnected"), nil
		}
		return []byte("Alpha\n"), nil
	}}
	clock := &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	c := New(exec, clock, zap.NewNop(), "wlan0")

	_, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)

	var showArgs string
	for _, argv := range exec.recorded() {
		if joined := strings.Join(argv, " "); strings.Contains(joined, "device show") {
			showArgs = joined
		}
	}
	assert.Contains(t, showArgs, "device show wlan0")
}

// TestRefreshScanCacheKeepsCacheOnEmptyScan is the regression for the portal's
// "search for networks again" button blanking its own picker: the button
// bounces the AP, and the refresh that follows catches NM's BSS list still
// empty from the AP-mode flip. That empty result must not evict the entries the
// picker is showing, and must not extend their expiry either.
func TestRefreshScanCacheKeepsCacheOnEmptyScan(t *testing.T) {
	var out []byte
	c, _, clock := newController(func(argv []string) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "device wifi list") {
			return out, nil
		}
		return nil, nil
	})

	out = []byte("Alpha\nBravo\n")
	got, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)
	require.Equal(t, []string{"Alpha", "Bravo"}, got)

	// The post-bounce scan succeeds but sees nothing.
	clock.advance(time.Minute)
	out = nil
	got, err = c.RefreshScanCache(context.Background())
	require.NoError(t, err)
	assert.Empty(t, got, "the LIVE result is reported, so callers can retry")

	// ...and the picker still has its networks.
	cached, err := c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Alpha", "Bravo"}, cached)

	// The retained entries keep their ORIGINAL expiry: the empty scan bought no
	// extra life, so a stale list still ages out on schedule.
	clock.advance(scanCacheTTL - time.Minute)
	out = []byte("Charlie\n")
	cached, err = c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Charlie"}, cached)
}

// TestRefreshScanCacheEmptyWithNoCacheExpiresFast: with nothing worth keeping,
// the empty result is cached only briefly — long enough that portal page loads
// do not each fire a live scan under the AP, short enough to retry soon.
func TestRefreshScanCacheEmptyWithNoCacheExpiresFast(t *testing.T) {
	var scans int
	var out []byte
	c, _, clock := newController(func(argv []string) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "device wifi list") {
			scans++
			return out, nil
		}
		return nil, nil
	})

	got, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)
	require.Empty(t, got)
	require.Equal(t, 1, scans)

	// Within the short TTL the empty answer is served from cache.
	clock.advance(emptyScanCacheTTL / 2)
	cached, err := c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cached)
	assert.Equal(t, 1, scans, "no live scan while the AP holds the radio")

	// Past it, the next read scans again — and picks up the recovered networks.
	clock.advance(emptyScanCacheTTL)
	out = []byte("Alpha\n")
	cached, err = c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Alpha"}, cached)
	assert.Equal(t, 2, scans)
}

// TestRefreshScanCacheEmptyDoesNotReviveExpiredCache: an expired list is not
// "usable" — an empty scan past its TTL must not resurrect it.
func TestRefreshScanCacheEmptyDoesNotReviveExpiredCache(t *testing.T) {
	var out []byte
	c, _, clock := newController(func(argv []string) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "device wifi list") {
			return out, nil
		}
		return nil, nil
	})

	out = []byte("Alpha\n")
	_, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)

	clock.advance(scanCacheTTL + time.Minute)
	out = nil
	_, err = c.RefreshScanCache(context.Background())
	require.NoError(t, err)

	cached, err := c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Empty(t, cached, "stale entries stay retired")
}

// TestRefreshScanCacheErrorLeavesCacheIntact: an nmcli failure is not evidence
// about the airwaves at all, so it changes nothing.
func TestRefreshScanCacheErrorLeavesCacheIntact(t *testing.T) {
	fail := false
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		if !strings.Contains(strings.Join(argv, " "), "device wifi list") {
			return nil, nil
		}
		if fail {
			return []byte("Error: device busy"), fakeExitError{code: 1, msg: "exit status 1"}
		}
		return []byte("Alpha\n"), nil
	})

	_, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)

	fail = true
	_, err = c.RefreshScanCache(context.Background())
	require.Error(t, err)

	cached, err := c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Alpha"}, cached)
}

func TestScanForcesRescan(t *testing.T) {
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		return []byte("Alpha\n"), nil
	})
	_, err := c.Scan(context.Background(), true)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(exec.recorded()[0], " "), "--rescan yes")
}

func TestParseSSIDsDedupOrderAndCap(t *testing.T) {
	// Duplicates collapse, order is preserved, and the list caps at maxSSIDs.
	var b strings.Builder
	b.WriteString("First\nFirst\nSecond\n\n")
	for i := 0; i < 20; i++ {
		b.WriteString("net")
		b.WriteByte(byte('a' + i))
		b.WriteString("\n")
	}
	got := parseSSIDs(b.String())
	assert.Len(t, got, maxSSIDs)
	assert.Equal(t, "First", got[0])
	assert.Equal(t, "Second", got[1])
}

func TestParseSSIDsUnescapesColons(t *testing.T) {
	got := parseSSIDs(`My\:Net` + "\n")
	require.Len(t, got, 1)
	assert.Equal(t, "My:Net", got[0])
}

// --- Saved profiles ----------------------------------------------------------

func TestSavedProfilesFiltersWifiType(t *testing.T) {
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		return []byte("Home:802-11-wireless\nEth0:802-3-ethernet\nWork\\:5G:802-11-wireless\n"), nil
	})
	names, err := c.SavedProfiles(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"Home", "Work:5G"}, names)

	has, err := c.HasSavedProfile(context.Background())
	require.NoError(t, err)
	assert.True(t, has)
}

func TestHasSavedProfileFalseWhenNone(t *testing.T) {
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		return []byte("Eth0:802-3-ethernet\n"), nil
	})
	has, err := c.HasSavedProfile(context.Background())
	require.NoError(t, err)
	assert.False(t, has)
}

// TestJoinFailureCleanupSurvivesCanceledContext: when the join fails because
// the CALLER's ctx died (daemon shutdown mid-join), the broken-profile cleanup
// must still run — on a detached context — or the half-created profile biases
// the next boot to "provisioned" and defers the setup AP a full offline window.
func TestJoinFailureCleanupSurvivesCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "device wifi list") {
			return []byte("Home"), nil
		}
		if strings.Contains(joined, "connect") {
			// Shutdown arrives mid-connect: the caller's ctx dies and nmcli
			// reports the interruption.
			cancel()
			return []byte("Error: Connection activation failed."),
				fakeExitError{code: 4, msg: "exit status 4"}
		}
		return nil, nil
	})

	err := c.Join(ctx, "Home", "pw123456")
	require.Error(t, err)

	// Pre-delete listing, scan, connect, post-failure cleanup listing: the
	// cleanup pass must have run despite the canceled parent ctx, and on a
	// live context. (The listing comes back empty here, so no delete follows —
	// the listing call itself proves the cleanup pass ran.)
	calls := exec.recorded()
	require.Len(t, calls, 4)
	assert.Equal(t, []string{"nmcli", "-t", "-f", "UUID,TYPE,NAME", "connection", "show"}, calls[3])
	// The parent ctx was canceled during the connect call, so a cleanup issued
	// on it would arrive already-dead. The captured ctx state proves the
	// cleanup ran on a detached, live context instead.
	ctxErrs := exec.recordedCtxErrs()
	require.Len(t, ctxErrs, 4)
	assert.NoError(t, ctxErrs[3], "cleanup must run on a detached, live context")
}
