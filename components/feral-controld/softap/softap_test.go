package softap

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// scriptedExec fakes wrapper.Exec.
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

type fakeExitError struct{ msg string }

func (e fakeExitError) Error() string { return e.msg }

func newBackend(id string, reply func(argv []string) ([]byte, error)) (*nmBackend, *scriptedExec) {
	exec := &scriptedExec{reply: reply}
	b := NewNetworkManager(exec, zap.NewNop(), "", func() (string, error) {
		return id, nil
	}).(*nmBackend)
	return b, exec
}

// --- credentials / PSK derivation ---------------------------------------------

func TestNumericPSK(t *testing.T) {
	// Pinned expected values: the PSK must stay stable across releases for a
	// given device_id (users may have it written down), so a hash/derivation
	// change should fail loudly here, not slip through.
	cases := []struct {
		id   string
		want string
	}{
		{"a1b2c3", "28677962"},
		{"FF1-8EVTK3RE", "36169297"},
		{"a1b2c3d4e5f6", "86106003"},
		{"x", "62385986"},
	}
	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			got := numericPSK(tc.id)
			assert.Equal(t, tc.want, got)
			assert.Len(t, got, 8)
			assert.Regexp(t, "^[0-9]{8}$", got) // WPA2 minimum, digits only
		})
	}
}

// TestUpFailureErrorNeverContainsPSK: hotspot creation passes the WPA2 PSK on
// the nmcli command line, and run() folds args + captured output into its
// error, which callers log. A failed raise must redact the key from both.
func TestUpFailureErrorNeverContainsPSK(t *testing.T) {
	psk := numericPSK("a1b2c3")
	b, _ := newBackend("a1b2c3", func(argv []string) ([]byte, error) {
		if argv[1] == "connection" {
			return nil, nil // pre-create cleanup Down succeeds
		}
		// nmcli error output can echo the offending command line, PSK included.
		return []byte("Error: hotspot create failed for password " + psk),
			fakeExitError{msg: "exit status 1"}
	})

	_, err := b.Up(context.Background())
	require.Error(t, err)
	assert.NotContains(t, err.Error(), psk, "PSK must never appear in the error: %s", err)
	assert.Contains(t, err.Error(), "[redacted]")
}

func TestCredentials(t *testing.T) {
	b, _ := newBackend("  a1b2c3  ", nil) // whitespace trimmed from /etc/hostname
	info, err := b.credentials()
	require.NoError(t, err)
	assert.Equal(t, "FF1-a1b2c3", info.SSID)
	assert.Equal(t, "28677962", info.PSK) // numericPSK("a1b2c3")
}

func TestCredentialsAlreadyPrefixedHostname(t *testing.T) {
	b, _ := newBackend("FF1-8EVTK3RE", nil) // provisioned hostnames carry the prefix
	info, err := b.credentials()
	require.NoError(t, err)
	assert.Equal(t, "FF1-8EVTK3RE", info.SSID) // no FF1-FF1- doubling
	assert.Equal(t, "36169297", info.PSK)      // numericPSK of the full device_id
}

func TestCredentialsEmptyHostname(t *testing.T) {
	b, _ := newBackend("   ", nil)
	_, err := b.credentials()
	require.Error(t, err)
}

func TestCredentialsHostnameReadError(t *testing.T) {
	exec := &scriptedExec{reply: func(argv []string) ([]byte, error) { return nil, nil }}
	b := NewNetworkManager(exec, zap.NewNop(), "", func() (string, error) {
		return "", assert.AnError
	}).(*nmBackend)
	_, err := b.Up(context.Background())
	require.Error(t, err)
}

// --- Up ----------------------------------------------------------------------

func TestUp(t *testing.T) {
	b, exec := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return []byte("Hotspot active"), nil
	})
	info, err := b.Up(context.Background())
	require.NoError(t, err)
	assert.Equal(t, "FF1-a1b2c3d4e5f6", info.SSID)
	assert.Equal(t, "86106003", info.PSK) // numericPSK("a1b2c3d4e5f6")

	// Replace-not-stack: the raise pre-deletes any same-name profile so a
	// leftover from an ungraceful previous run can never become a duplicate.
	calls := exec.recorded()
	require.Len(t, calls, 2)
	assert.Equal(t, []string{"nmcli", "connection", "delete", conName}, calls[0])
	call := strings.Join(calls[1], " ")
	assert.Contains(t, call, "device wifi hotspot")
	assert.Contains(t, call, "con-name "+conName)
	assert.Contains(t, call, "ssid FF1-a1b2c3d4e5f6")
	assert.Contains(t, call, "password 86106003")
}

func TestUpWithIface(t *testing.T) {
	exec := &scriptedExec{reply: func(argv []string) ([]byte, error) { return nil, nil }}
	b := NewNetworkManager(exec, zap.NewNop(), "wlan0", func() (string, error) {
		return "a1b2c3d4e5f6", nil
	}).(*nmBackend)
	_, err := b.Up(context.Background())
	require.NoError(t, err)
	assert.Contains(t, strings.Join(exec.recorded()[1], " "), "ifname wlan0")
}

func TestUpFailure(t *testing.T) {
	b, _ := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return []byte("Error: no wifi device"), fakeExitError{msg: "exit status 1"}
	})
	_, err := b.Up(context.Background())
	require.Error(t, err)
}

// --- Down --------------------------------------------------------------------

func TestDown(t *testing.T) {
	b, exec := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return []byte("successfully deleted"), nil
	})
	require.NoError(t, b.Down(context.Background()))
	assert.Equal(t, []string{"nmcli", "connection", "delete", conName}, exec.recorded()[0])
}

func TestDownIdempotentWhenMissing(t *testing.T) {
	// nmcli reports the profile is gone; Down must treat that as success.
	b, _ := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return []byte("Error: unknown connection 'ff1-softap'."),
			fakeExitError{msg: "exit status 10"}
	})
	require.NoError(t, b.Down(context.Background()))
}

func TestDownPropagatesRealError(t *testing.T) {
	b, _ := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return []byte("Error: NetworkManager is not running"),
			fakeExitError{msg: "exit status 8"}
	})
	require.Error(t, b.Down(context.Background()))
}

// --- Status ------------------------------------------------------------------

func TestStatusActive(t *testing.T) {
	b, _ := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return []byte("preconfigured\n" + conName + "\nEth0\n"), nil
	})
	st, err := b.Status(context.Background())
	require.NoError(t, err)
	assert.True(t, st.Active)
	assert.Equal(t, "FF1-a1b2c3d4e5f6", st.SSID)
}

func TestStatusInactive(t *testing.T) {
	b, _ := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return []byte("preconfigured\nEth0\n"), nil
	})
	st, err := b.Status(context.Background())
	require.NoError(t, err)
	assert.False(t, st.Active)
	assert.Empty(t, st.SSID)
}

func TestStatusQueryError(t *testing.T) {
	b, _ := newBackend("a1b2c3d4e5f6", func(argv []string) ([]byte, error) {
		return nil, fakeExitError{msg: "exit status 8"}
	})
	_, err := b.Status(context.Background())
	require.Error(t, err)
}
