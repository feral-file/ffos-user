package wifictl

import (
	"context"
	"fmt"
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
func (c scriptedCmd) Pid() int                        { return 0 }

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
	err := c.Join(context.Background(), "Home", "supersecret", false)
	require.NoError(t, err)

	// Pre-delete listing (no matches, so no delete), visibility scan, connect.
	calls := exec.recorded()
	require.Len(t, calls, 3)
	assert.Equal(t, []string{"nmcli", "-t", "-f", "UUID,TYPE,NAME", "connection", "show"}, calls[0])
	assert.Contains(t, strings.Join(calls[1], " "), "device wifi list --rescan yes")
	assert.Contains(t, strings.Join(calls[2], " "), "device wifi connect Home password supersecret")
}

// profileListReply reports whether argv is the UUID,TYPE,NAME profile listing
// (as opposed to the per-profile -g field reads, which also end in
// "connection show uuid <uuid>").
func profileListReply(argv []string) bool {
	return strings.HasSuffix(strings.Join(argv, " "), "-f UUID,TYPE,NAME connection show")
}

// fieldRead answers a per-profile `-g <field> connection show uuid <uuid>`
// read from the given per-uuid values, reporting whether it matched.
func fieldRead(argv []string, field string, byUUID map[string]string) ([]byte, bool) {
	if len(argv) == 7 && argv[1] == "-g" && argv[2] == field &&
		argv[3] == "connection" && argv[4] == "show" && argv[5] == "uuid" {
		return []byte(byUUID[argv[6]] + "\n"), true
	}
	return nil, false
}

func TestJoinAuthFailureCleansUpProfile(t *testing.T) {
	// The first profile listing (pre-delete) is empty; after the failed connect
	// the half-created wifi profile shows up — targeting the join SSID with a
	// PSK — and must be deleted BY UUID.
	listings := 0
	c, exec, _ := newController(joinScanAware("Home", func(argv []string) ([]byte, error) {
		if profileListReply(argv) {
			listings++
			if listings == 1 {
				return nil, nil
			}
			return []byte("half-created-uuid:802-11-wireless:Home\n"), nil
		}
		if out, ok := fieldRead(argv, "802-11-wireless.ssid",
			map[string]string{"half-created-uuid": "Home"}); ok {
			return out, nil
		}
		if out, ok := fieldRead(argv, "802-11-wireless-security.key-mgmt",
			map[string]string{"half-created-uuid": "wpa-psk"}); ok {
			return out, nil
		}
		if len(argv) >= 4 && argv[1] == "device" && argv[2] == "wifi" && argv[3] == "connect" {
			return []byte("Error: Connection activation failed: (7) Secrets were required."),
				fakeExitError{code: 4, msg: "exit status 4"}
		}
		return nil, nil
	}))

	err := c.Join(context.Background(), "Home", "wrongpass", false)
	require.Error(t, err)

	var je *JoinError
	require.ErrorAs(t, err, &je)
	assert.Equal(t, JoinErrAuth, je.Kind)

	// Pre-delete listing (empty), visibility scan, connect (fails), cleanup
	// listing + the profile's two field reads, then the UUID-scoped delete of
	// the half-created broken profile.
	calls := exec.recorded()
	require.Len(t, calls, 7)
	assert.Equal(t, []string{"nmcli", "connection", "delete", "uuid", "half-created-uuid"}, calls[6])
}

// TestJoinCleanupSparesNonWifiProfiles: ssid is user input from the captive
// portal, and nmcli deletes by connection ID match ANY profile type — a
// submission named like the device's ethernet or VPN profile must never
// delete it. Only the 802-11-wireless profile targeting that SSID may go, by
// UUID (non-Wi-Fi rows are filtered before any per-profile field read runs).
func TestJoinCleanupSparesNonWifiProfiles(t *testing.T) {
	c, exec, _ := newController(joinScanAware("Home", func(argv []string) ([]byte, error) {
		if profileListReply(argv) {
			return []byte("eth-uuid:802-3-ethernet:Home\nwifi-uuid:802-11-wireless:Home\nvpn-uuid:vpn:Home\n"), nil
		}
		if out, ok := fieldRead(argv, "802-11-wireless.ssid",
			map[string]string{"wifi-uuid": "Home"}); ok {
			return out, nil
		}
		if out, ok := fieldRead(argv, "802-11-wireless-security.key-mgmt",
			map[string]string{"wifi-uuid": "wpa-psk"}); ok {
			return out, nil
		}
		if len(argv) >= 4 && argv[1] == "device" && argv[2] == "wifi" && argv[3] == "connect" {
			return []byte("Error: Connection activation failed."),
				fakeExitError{code: 4, msg: "exit status 4"}
		}
		return nil, nil
	}))

	err := c.Join(context.Background(), "Home", "wrongpass", false)
	require.Error(t, err)

	deletes := [][]string{}
	fieldReads := [][]string{}
	for _, call := range exec.recorded() {
		if len(call) > 2 && call[1] == "connection" && call[2] == "delete" {
			deletes = append(deletes, call)
		}
		if len(call) == 7 && call[1] == "-g" {
			fieldReads = append(fieldReads, call)
		}
	}
	// Pre-delete pass and post-failure pass each delete exactly the wifi UUID.
	require.Len(t, deletes, 2)
	for _, del := range deletes {
		assert.Equal(t, []string{"nmcli", "connection", "delete", "uuid", "wifi-uuid"}, del,
			"only the WIFI profile targeting the SSID may be deleted, never ethernet/VPN")
	}
	for _, fr := range fieldReads {
		assert.Equal(t, "wifi-uuid", fr[6],
			"per-profile field reads must never target non-Wi-Fi profiles")
	}
}

// TestDeleteWifiProfilesScope pins the D9 fix and its two safety rails: the
// delete matches the profile's TARGET SSID (a stale PSK profile not named
// after its SSID is exactly the profile NM reuses over fresh credentials),
// never touches non-PSK security (a portal PSK join must not destroy an
// 802.1X profile for the same SSID), and keeps any profile it cannot read
// (unconfirmed is not deletable).
func TestDeleteWifiProfilesScope(t *testing.T) {
	ssids := map[string]string{
		"renamed-uuid": "Home", // name != SSID: the D9 case — must be deleted
		"eap-uuid":     "Home",
		"other-uuid":   "Elsewhere",
		"open-uuid":    "Home",
	}
	keyMgmts := map[string]string{
		"renamed-uuid": "wpa-psk",
		"eap-uuid":     "wpa-eap",
		"other-uuid":   "wpa-psk",
		"open-uuid":    "", // no security section: open network, deletable
	}
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		if profileListReply(argv) {
			return []byte("renamed-uuid:802-11-wireless:Old Router\n" +
				"eap-uuid:802-11-wireless:Home\n" +
				"other-uuid:802-11-wireless:Home\n" +
				"open-uuid:802-11-wireless:HomeOpen\n" +
				"broken-uuid:802-11-wireless:Broken\n"), nil
		}
		if len(argv) == 7 && argv[1] == "-g" && argv[6] == "broken-uuid" {
			return nil, fakeExitError{code: 10, msg: "exit status 10"}
		}
		if out, ok := fieldRead(argv, "802-11-wireless.ssid", ssids); ok {
			return out, nil
		}
		if out, ok := fieldRead(argv, "802-11-wireless-security.key-mgmt", keyMgmts); ok {
			return out, nil
		}
		return nil, nil
	})

	c.deleteWifiProfiles(context.Background(), "Home")

	var deleted []string
	for _, call := range exec.recorded() {
		if len(call) == 5 && call[1] == "connection" && call[2] == "delete" {
			deleted = append(deleted, call[4])
		}
	}
	assert.ElementsMatch(t, []string{"renamed-uuid", "open-uuid"}, deleted,
		"delete PSK/open profiles targeting the SSID; spare 802.1X, other SSIDs, and unreadable profiles")
}

// TestJoinHiddenSkipsVisibilityWaitAndPassesHiddenYes pins the hidden-network
// join contract (D3): a hidden SSID never appears in scan output, so the
// post-bounce visibility wait must be skipped (it could only burn its whole
// window) and nmcli must be told `hidden yes` so NM probes for the network
// directly instead of consulting broadcast scan results.
func TestJoinHiddenSkipsVisibilityWaitAndPassesHiddenYes(t *testing.T) {
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		if profileListReply(argv) {
			return nil, nil
		}
		if strings.Contains(strings.Join(argv, " "), "device wifi list") {
			t.Fatal("hidden join must not run the visibility scan")
		}
		return []byte("successfully activated"), nil
	})
	require.NoError(t, c.Join(context.Background(), "GhostNet", "secret12", true))

	connect := exec.recorded()[len(exec.recorded())-1]
	joined := strings.Join(connect, " ")
	assert.Contains(t, joined, "device wifi connect GhostNet")
	assert.Contains(t, joined, "hidden yes")
}

// TestJoinNonHiddenOmitsHiddenFlag: the flag must appear ONLY when requested —
// `hidden yes` on a broadcast network changes NM's probing behavior for no
// reason.
func TestJoinNonHiddenOmitsHiddenFlag(t *testing.T) {
	c, exec, _ := newController(joinScanAware("Home", func(argv []string) ([]byte, error) {
		if profileListReply(argv) {
			return nil, nil
		}
		return []byte("successfully activated"), nil
	}))
	require.NoError(t, c.Join(context.Background(), "Home", "secret12", false))
	connect := exec.recorded()[len(exec.recorded())-1]
	assert.NotContains(t, strings.Join(connect, " "), "hidden",
		"non-hidden joins must not pass the hidden flag")
}

// TestJoinOpenNetworkOmitsPassword pins the open-network fix: an empty PSK
// must omit the password argument entirely — `password ""` makes NM build a
// WPA-PSK security block around the empty key, and the open network then
// reads as "wrong password" forever.
func TestJoinOpenNetworkOmitsPassword(t *testing.T) {
	c, exec, _ := newController(joinScanAware("CafeOpen", func(argv []string) ([]byte, error) {
		if profileListReply(argv) {
			return nil, nil
		}
		return []byte("successfully activated"), nil
	}))
	require.NoError(t, c.Join(context.Background(), "CafeOpen", "", false))

	connect := exec.recorded()[len(exec.recorded())-1]
	joined := strings.Join(connect, " ")
	assert.Contains(t, joined, "device wifi connect CafeOpen")
	assert.NotContains(t, joined, "password",
		"an open-network join must not send a password argument at all")
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
	err := c.Join(context.Background(), "Ghost", "whatever1", false)
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
	err := c.Join(context.Background(), "Slow", "password1", false)
	var je *JoinError
	require.ErrorAs(t, err, &je)
	assert.Equal(t, JoinErrTimeout, je.Kind)
}

func TestJoinIfacePinned(t *testing.T) {
	exec := &scriptedExec{reply: joinScanAware("Home", func(argv []string) ([]byte, error) { return nil, nil })}
	clock := &fakeClock{now: time.Now()}
	c := New(exec, clock, zap.NewNop(), "wlan0")
	require.NoError(t, c.Join(context.Background(), "Home", "password1", false))
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

	require.NoError(t, c.Join(context.Background(), "Home", "secret12", false))

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
	require.NoError(t, c.Join(context.Background(), "Home", "secret12", false))
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

func TestRefreshScanCachePreservesCandidatesBeyondDisplayCap(t *testing.T) {
	var ssids []string
	// One past the portal's nine-slot display cap (portal.maxDisplayedSSIDs —
	// the only live cap; wifictl itself no longer caps anything).
	for i := 0; i < 10; i++ {
		ssids = append(ssids, fmt.Sprintf("Net%02d", i))
	}
	scanOutput := []byte(strings.Join(ssids, "\n") + "\n")
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "device wifi list") {
			return scanOutput, nil
		}
		return nil, nil
	})

	live, err := c.RefreshScanCache(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ssids, live)

	cached, err := c.CachedScan(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ssids, cached,
		"the portal cache must remain uncapped until the setup SSID is removed")
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
	_, err := c.scan(context.Background(), true)
	require.NoError(t, err)
	assert.Contains(t, strings.Join(exec.recorded()[0], " "), "--rescan yes")
}

func TestParseSSIDsDedupAndOrder(t *testing.T) {
	// Duplicates collapse, order is preserved, and nothing is truncated:
	// display capping is the portal's concern (portal.maxDisplayedSSIDs), and
	// a cap at this layer would fabricate absence for the evidence callers.
	var b strings.Builder
	b.WriteString("First\nFirst\nSecond\n\n")
	for i := 0; i < 20; i++ {
		b.WriteString("net")
		b.WriteByte(byte('a' + i))
		b.WriteString("\n")
	}
	got := parseSSIDs(b.String())
	assert.Len(t, got, 22)
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

	err := c.Join(ctx, "Home", "pw123456", false)
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

// --- SavedWifiSSIDs / ScanAllSSIDs (boot relocation evidence, M-0b) -----------

// TestSavedWifiSSIDsReadsProfileFieldsNotNames: the SSID must come from the
// profile's 802-11-wireless.ssid resolved by UUID (an ops-created profile may
// be named anything, and name resolution can match a same-named profile of a
// different type), and a hidden profile must flip anyHidden — scan absence is
// not evidence for a network that never appears in scans.
func TestSavedWifiSSIDsReadsProfileFieldsNotNames(t *testing.T) {
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "-f UUID,TYPE,NAME connection show"):
			return []byte("uuid-office:802-11-wireless:office-frame\nuuid-eth:802-3-ethernet:office-frame\nuuid-home:802-11-wireless:HomeNet\n"), nil
		case strings.Contains(joined, "802-11-wireless.ssid connection show uuid uuid-office"):
			return []byte("Gallery5G\n"), nil
		case strings.Contains(joined, "802-11-wireless.hidden connection show uuid uuid-office"):
			return []byte("yes\n"), nil
		case strings.Contains(joined, "802-11-wireless.ssid connection show uuid uuid-home"):
			return []byte("HomeNet\n"), nil
		case strings.Contains(joined, "802-11-wireless.hidden connection show uuid uuid-home"):
			return []byte("no\n"), nil
		}
		return nil, fakeExitError{code: 1, msg: "unexpected: " + joined}
	})

	ssids, anyHidden, err := c.SavedWifiSSIDs(context.Background())
	if err != nil {
		t.Fatalf("SavedWifiSSIDs: %v", err)
	}
	if len(ssids) != 2 || ssids[0] != "Gallery5G" || ssids[1] != "HomeNet" {
		t.Fatalf("ssids = %v, want [Gallery5G HomeNet] (profile fields, not names)", ssids)
	}
	if !anyHidden {
		t.Fatal("anyHidden = false, want true (office-frame targets a hidden network)")
	}
	// The same-named ethernet profile must never be queried: name-based
	// resolution would have hit it and read back an empty ssid.
	for _, call := range exec.recorded() {
		if strings.Contains(strings.Join(call, " "), "uuid-eth") {
			t.Fatalf("non-wifi profile was queried: %v", call)
		}
	}
}

// TestSavedWifiSSIDsEmptySSIDIsError: a wifi profile whose ssid reads back
// empty (exit 0) must be an error, not a silent skip — a list silently
// missing a profile is the false "none in range" the relocation check must
// never act on.
func TestSavedWifiSSIDsEmptySSIDIsError(t *testing.T) {
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "-f UUID,TYPE,NAME connection show"):
			return []byte("uuid-weird:802-11-wireless:imported-profile\n"), nil
		case strings.Contains(joined, "802-11-wireless.ssid connection show uuid uuid-weird"):
			return []byte("\n"), nil
		}
		return nil, fakeExitError{code: 1, msg: "unexpected: " + joined}
	})

	if _, _, err := c.SavedWifiSSIDs(context.Background()); err == nil {
		t.Fatal("expected an error for an empty ssid read; got nil")
	}
}

// TestSavedWifiSSIDsFailsClosedOnProfileReadError: a partial list silently
// missing a profile is exactly the false "none in range" the relocation check
// must never act on, so any per-profile read failure is an error, not a skip.
func TestSavedWifiSSIDsFailsClosedOnProfileReadError(t *testing.T) {
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		if strings.Contains(joined, "-f UUID,TYPE,NAME connection show") {
			return []byte("uuid-home:802-11-wireless:HomeNet\n"), nil
		}
		return nil, fakeExitError{code: 10, msg: "nmcli: unknown connection"}
	})

	if _, _, err := c.SavedWifiSSIDs(context.Background()); err == nil {
		t.Fatal("expected an error when a profile read fails; got nil")
	}
}

// TestSavedWifiSSIDsPreservesWhitespaceSSID (review P1): leading/trailing
// spaces are valid SSID bytes and the scan side preserves them, so the
// profile read may strip ONLY nmcli's trailing newline. A TrimSpace here made
// a saved " Cafe " compare unequal to its own scan sighting — an in-range
// network reading as "absent" is the false relocation evidence the
// fail-closed contract exists to prevent.
func TestSavedWifiSSIDsPreservesWhitespaceSSID(t *testing.T) {
	c, _, _ := newController(func(argv []string) ([]byte, error) {
		joined := strings.Join(argv, " ")
		switch {
		case strings.Contains(joined, "-f UUID,TYPE,NAME connection show"):
			return []byte("uuid-cafe:802-11-wireless:cafe\n"), nil
		case strings.Contains(joined, "802-11-wireless.ssid connection show uuid uuid-cafe"):
			return []byte(" Cafe \n"), nil
		case strings.Contains(joined, "802-11-wireless.hidden connection show uuid uuid-cafe"):
			return []byte("no\n"), nil
		}
		return nil, fakeExitError{code: 1, msg: "unexpected: " + joined}
	})

	ssids, _, err := c.SavedWifiSSIDs(context.Background())
	if err != nil {
		t.Fatalf("SavedWifiSSIDs: %v", err)
	}
	if len(ssids) != 1 || ssids[0] != " Cafe " {
		t.Fatalf("ssids = %q, want [\" Cafe \"] (SSID whitespace preserved, only the newline terminator stripped)", ssids)
	}
}

// TestScanAllSSIDsPinsConfiguredInterface (review P1): every other
// radio-touching command pins ifname when the controller is configured with
// one; the relocation-evidence scan must too, or on multi-radio hardware it
// can report a different radio's air and fabricate "saved SSID absent".
func TestScanAllSSIDsPinsConfiguredInterface(t *testing.T) {
	exec := &scriptedExec{reply: func(argv []string) ([]byte, error) {
		return []byte("Net\n"), nil
	}}
	clock := &fakeClock{now: time.Date(2026, 7, 20, 12, 0, 0, 0, time.UTC)}
	c := New(exec, clock, zap.NewNop(), "wlan1")

	if _, err := c.ScanAllSSIDs(context.Background()); err != nil {
		t.Fatalf("ScanAllSSIDs: %v", err)
	}
	last := exec.recorded()[len(exec.recorded())-1]
	joined := strings.Join(last, " ")
	if !strings.Contains(joined, "device wifi list") {
		t.Fatalf("last call is not the scan; argv = %v", last)
	}
	if !strings.Contains(joined, "ifname wlan1") {
		t.Fatalf("ScanAllSSIDs must pin the configured interface; argv = %v", last)
	}
}

// TestScanAllSSIDsIsUncapped: ScanAllSSIDs must return the FULL list — its
// callers treat scan presence as relocation evidence, and a saved network
// truncated out by any cap (the portal's display cap is nine) would read as
// absent and fire a false relocation.
func TestScanAllSSIDsIsUncapped(t *testing.T) {
	const total = 12 // comfortably past the portal's nine-slot display cap
	var lines []string
	for i := 0; i < total; i++ {
		lines = append(lines, fmt.Sprintf("Net%02d", i))
	}
	out := []byte(strings.Join(lines, "\n") + "\n")
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		return out, nil
	})

	all, err := c.ScanAllSSIDs(context.Background())
	if err != nil {
		t.Fatalf("ScanAllSSIDs: %v", err)
	}
	if len(all) != total {
		t.Fatalf("ScanAllSSIDs returned %d SSIDs, want %d (uncapped)", len(all), total)
	}
	// And it must force a fresh scan: stale results are not relocation evidence.
	last := exec.recorded()[len(exec.recorded())-1]
	joined := strings.Join(last, " ")
	if !strings.Contains(joined, "--rescan yes") {
		t.Fatalf("ScanAllSSIDs must force a rescan; argv = %v", last)
	}
}

// --- ActivationProfiles / ActivateProfile (recheck blink, §4.2) ---------------

// TestActivationProfilesReadsFieldsFailClosed pins the blink helper's
// contract: per-profile fields are read by UUID (type-filtered before any
// field read), a blank timestamp is a legitimate 0 (never-activated profile),
// and ANY per-profile read failure — or an unparseable non-blank timestamp —
// fails the whole listing, because the caller ACTIVATES off this list and the
// §4.2 fail-bias is "a listing error aborts the blink, never a blind
// activation".
func TestActivationProfilesReadsFieldsFailClosed(t *testing.T) {
	t.Run("happy path with MRU fields", func(t *testing.T) {
		ssids := map[string]string{"u1": "Home", "u2": "Studio"}
		hiddens := map[string]string{"u1": "no", "u2": "yes"}
		stamps := map[string]string{"u1": "1722400000", "u2": ""}
		c, _, _ := newController(func(argv []string) ([]byte, error) {
			if profileListReply(argv) {
				return []byte("u1:802-11-wireless:Home\nu2:802-11-wireless:StudioProfile\n" +
					"e1:802-3-ethernet:Wired\n"), nil
			}
			if out, ok := fieldRead(argv, "802-11-wireless.ssid", ssids); ok {
				return out, nil
			}
			if out, ok := fieldRead(argv, "802-11-wireless.hidden", hiddens); ok {
				return out, nil
			}
			if out, ok := fieldRead(argv, "connection.timestamp", stamps); ok {
				return out, nil
			}
			return nil, nil
		})
		got, err := c.ActivationProfiles(context.Background())
		require.NoError(t, err)
		require.Len(t, got, 2, "non-Wi-Fi profiles are filtered before any field read")
		assert.Equal(t, ActivationProfile{UUID: "u1", SSID: "Home", Hidden: false, LastUsed: 1722400000}, got[0])
		assert.Equal(t, ActivationProfile{UUID: "u2", SSID: "Studio", Hidden: true, LastUsed: 0}, got[1],
			"a blank timestamp is a never-activated 0, not an error")
	})

	t.Run("per-profile read failure fails the listing", func(t *testing.T) {
		c, _, _ := newController(func(argv []string) ([]byte, error) {
			if profileListReply(argv) {
				return []byte("u1:802-11-wireless:Home\n"), nil
			}
			return nil, fakeExitError{code: 10, msg: "exit status 10"}
		})
		_, err := c.ActivationProfiles(context.Background())
		assert.Error(t, err, "an unreadable profile must fail closed — never a partial list")
	})

	t.Run("unparseable timestamp fails the listing", func(t *testing.T) {
		c, _, _ := newController(func(argv []string) ([]byte, error) {
			if profileListReply(argv) {
				return []byte("u1:802-11-wireless:Home\n"), nil
			}
			if out, ok := fieldRead(argv, "802-11-wireless.ssid", map[string]string{"u1": "Home"}); ok {
				return out, nil
			}
			if out, ok := fieldRead(argv, "802-11-wireless.hidden", map[string]string{"u1": "no"}); ok {
				return out, nil
			}
			if out, ok := fieldRead(argv, "connection.timestamp", map[string]string{"u1": "garbage"}); ok {
				return out, nil
			}
			return nil, nil
		})
		_, err := c.ActivationProfiles(context.Background())
		assert.Error(t, err)
	})
}

// TestActivateProfile pins the explicit-activation argv and its error
// wrapping (the blink logs and moves to the next candidate on failure).
func TestActivateProfile(t *testing.T) {
	c, exec, _ := newController(func(argv []string) ([]byte, error) {
		if len(argv) == 5 && argv[1] == "connection" && argv[2] == "up" && argv[3] == "uuid" {
			if argv[4] == "bad" {
				return []byte("Error: network could not be found"), fakeExitError{code: 4, msg: "exit status 4"}
			}
			return []byte("Connection successfully activated"), nil
		}
		return nil, nil
	})
	require.NoError(t, c.ActivateProfile(context.Background(), "good"))
	err := c.ActivateProfile(context.Background(), "bad")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network could not be found",
		"the nmcli output must surface for the blink's candidate log")
	assert.Equal(t, []string{"nmcli", "connection", "up", "uuid", "good"}, exec.recorded()[0])
}

// --- SetDeviceAutoconnect (rescan-bounce suppression seam) --------------------

// autoconnectReply answers the two argv shapes the seam issues: the device
// listing used to resolve an unpinned iface, and the flip itself.
func autoconnectReply(devices string) func(argv []string) ([]byte, error) {
	return func(argv []string) ([]byte, error) {
		if strings.Contains(strings.Join(argv, " "), "-f DEVICE,TYPE device status") {
			return []byte(devices), nil
		}
		// The flip itself: nmcli prints nothing on success.
		return nil, nil
	}
}

// TestSetDeviceAutoconnectArgv pins the argv of both directions and the
// unpinned-iface resolution: production wires an empty iface, so the wifi row
// of `device status` is what the flip must land on — flipping the ethernet
// device would silently do nothing for the race the suppression exists to
// remove.
func TestSetDeviceAutoconnectArgv(t *testing.T) {
	c, exec, _ := newController(autoconnectReply("eno1:ethernet\nwlp2s0:wifi\nlo:loopback\n"))

	require.NoError(t, c.SetDeviceAutoconnect(context.Background(), false))
	require.NoError(t, c.SetDeviceAutoconnect(context.Background(), true))

	calls := exec.recorded()
	require.Len(t, calls, 4, "each flip resolves the device then sets it")
	assert.Equal(t, []string{"nmcli", "device", "set", "wlp2s0", "autoconnect", "off"}, calls[1])
	assert.Equal(t, []string{"nmcli", "device", "set", "wlp2s0", "autoconnect", "on"}, calls[3])
}

// TestSetDeviceAutoconnectUsesPinnedIface: a pinned iface skips the listing
// entirely (one fewer nmcli round-trip inside the bounce's critical window).
func TestSetDeviceAutoconnectUsesPinnedIface(t *testing.T) {
	exec := &scriptedExec{reply: autoconnectReply("")}
	c := New(exec, &fakeClock{now: time.Date(2026, 8, 5, 12, 0, 0, 0, time.UTC)}, zap.NewNop(), "wlan9")

	require.NoError(t, c.SetDeviceAutoconnect(context.Background(), false))

	calls := exec.recorded()
	require.Len(t, calls, 1)
	assert.Equal(t, []string{"nmcli", "device", "set", "wlan9", "autoconnect", "off"}, calls[0])
}

// TestSetDeviceAutoconnectFailures: every failure mode must surface as an
// error, because the caller's fail-open branch keys on it — a silent success
// on an unflipped device would make it believe the race was suppressed.
func TestSetDeviceAutoconnectFailures(t *testing.T) {
	t.Run("no wifi device", func(t *testing.T) {
		c, _, _ := newController(autoconnectReply("eno1:ethernet\n"))
		err := c.SetDeviceAutoconnect(context.Background(), false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "no wifi device")
	})

	t.Run("listing fails", func(t *testing.T) {
		c, _, _ := newController(func(argv []string) ([]byte, error) {
			return nil, fakeExitError{code: 1, msg: "exit status 1"}
		})
		require.Error(t, c.SetDeviceAutoconnect(context.Background(), false))
	})

	t.Run("flip fails", func(t *testing.T) {
		c, _, _ := newController(func(argv []string) ([]byte, error) {
			joined := strings.Join(argv, " ")
			if strings.Contains(joined, "-f DEVICE,TYPE device status") {
				return []byte("wlp2s0:wifi\n"), nil
			}
			return []byte("Error: Device 'wlp2s0' not found."), fakeExitError{code: 10, msg: "exit status 10"}
		})
		err := c.SetDeviceAutoconnect(context.Background(), false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not found", "nmcli's own message must reach the caller's log")
	})
}

// TestSetDeviceAutoconnectHonorsCtx: the WifiController contract requires every
// method to respect ctx. The seam owns no waits of its own, so honoring it
// means handing the caller's ctx to the exec seam — which is what actually
// kills a wedged nmcli inside the bounce.
func TestSetDeviceAutoconnectHonorsCtx(t *testing.T) {
	c, exec, _ := newController(autoconnectReply("wlp2s0:wifi\n"))
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_ = c.SetDeviceAutoconnect(ctx, false)

	ctxErrs := exec.recordedCtxErrs()
	require.Len(t, ctxErrs, 2, "the assertion below is vacuous if nothing ran")
	for i, err := range ctxErrs {
		assert.Error(t, err, "call %d must carry the caller's canceled ctx", i)
	}
}
