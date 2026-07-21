package wifictl

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// scriptedExec fakes wrapper.Exec: every nmcli invocation is recorded and
// answered by reply.
type scriptedExec struct {
	mu    sync.Mutex
	calls [][]string
	reply func(argv []string) ([]byte, error)
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

func (e *scriptedExec) CommandContext(_ context.Context, name string, arg ...string) wrapper.ExecCmd {
	argv := append([]string{name}, arg...)
	e.mu.Lock()
	e.calls = append(e.calls, argv)
	e.mu.Unlock()
	out, err := e.reply(argv)
	return scriptedCmd{out: out, err: err}
}

func (e *scriptedExec) recorded() [][]string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
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
		return []byte("Device 'wlan0' successfully activated"), nil
	}))
	err := c.Join(context.Background(), "Home", "supersecret")
	require.NoError(t, err)

	// Pre-delete, visibility scan, connect: three calls in that order.
	calls := exec.recorded()
	require.Len(t, calls, 3)
	assert.Equal(t, []string{"nmcli", "connection", "delete", "Home"}, calls[0])
	assert.Contains(t, strings.Join(calls[1], " "), "device wifi list --rescan yes")
	assert.Contains(t, strings.Join(calls[2], " "), "device wifi connect Home password supersecret")
}

func TestJoinAuthFailureCleansUpProfile(t *testing.T) {
	c, exec, _ := newController(joinScanAware("Home", func(argv []string) ([]byte, error) {
		// The connect attempt fails with an auth exit code; deletes succeed.
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

	// Pre-delete, visibility scan, connect (fails), post-failure cleanup delete
	// = 4 calls, the last one removing the half-created broken profile.
	calls := exec.recorded()
	require.Len(t, calls, 4)
	assert.Equal(t, []string{"nmcli", "connection", "delete", "Home"}, calls[3])
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
