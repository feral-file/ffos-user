package netlog

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// scriptedExec routes CommandContext by binary name to canned outputs.
// Hand-rolled (not the central mocks package) for the same import-cycle
// reason as netmetrics' fake: mocks -> relayer -> netlog.
type scriptedExec struct {
	byBinary map[string]execScript
}

type execScript struct {
	output string
	err    error
}

func (s scriptedExec) CommandContext(_ context.Context, name string, _ ...string) wrapper.ExecCmd {
	sc, ok := s.byBinary[name]
	if !ok {
		sc = execScript{err: errors.New("unscripted binary: " + name)}
	}
	return scriptedCmd{sc}
}

type scriptedCmd struct{ sc execScript }

func (c scriptedCmd) String() string { return "scripted" }
func (c scriptedCmd) Run() error     { return c.sc.err }
func (c scriptedCmd) Start() error   { return c.sc.err }
func (c scriptedCmd) Wait() error    { return c.sc.err }
func (c scriptedCmd) Output() ([]byte, error) {
	if c.sc.err != nil {
		return nil, c.sc.err
	}
	return []byte(c.sc.output), nil
}
func (c scriptedCmd) CombinedOutput() ([]byte, error) { return c.Output() }
func (c scriptedCmd) Pid() int                        { return 0 }

func leaseBlock(dev, typ, state string, extra ...string) string {
	b := "GENERAL.DEVICE:" + dev + "\nGENERAL.TYPE:" + typ + "\nGENERAL.STATE:" + state + "\n"
	for _, e := range extra {
		b += e + "\n"
	}
	return b
}

func TestParseLease(t *testing.T) {
	tests := []struct {
		name         string
		output       string
		wantGateway  string
		wantHasAddr  bool
		wantSurveyed bool
	}{
		{
			name: "activated wifi with lease",
			output: leaseBlock("wlan0", "wifi", "100 (connected)",
				"IP4.ADDRESS[1]:192.168.1.23/24", "IP4.GATEWAY:192.168.1.1",
				"IP4.DNS[1]:192.168.1.1", "IP4.DNS[2]:9.9.9.9") +
				leaseBlock("lo", "loopback", "100 (connected (externally))"),
			wantGateway:  "192.168.1.1",
			wantHasAddr:  true,
			wantSurveyed: true,
		},
		{
			name:         "activated but no address",
			output:       leaseBlock("eth0", "ethernet", "100 (connected)", "IP4.GATEWAY:--"),
			wantSurveyed: true,
		},
		{
			// nmcli renders requested-but-unset fields as "--" on every
			// line, not by omission: an unguarded IP4.ADDRESS:-- must not
			// read as "has a lease" (that would make no-lease unreachable
			// on real DHCP-failure hardware).
			name: "placeholder values on all IP4 fields",
			output: leaseBlock("wlan0", "wifi", "100 (connected)",
				"IP4.ADDRESS[1]:--", "IP4.GATEWAY:--", "IP4.DNS[1]:--"),
			wantSurveyed: true,
		},
		{
			// The mixed state (real uplink up while the setup AP is still
			// raised): the AP's shared-mode address and gateway are
			// SELF-evidence and must not read as a lease — otherwise
			// no-lease is unreachable and the gateway rung pings the
			// device itself.
			name: "own setup AP block is excluded from lease evidence",
			output: leaseBlock("wlan0", "wifi", "100 (connected)",
				"GENERAL.CONNECTION:ff1-softap",
				"IP4.ADDRESS[1]:10.42.0.1/24", "IP4.GATEWAY:10.42.0.1") +
				leaseBlock("eth0", "ethernet", "100 (connected)",
					"GENERAL.CONNECTION:Wired connection 1"),
			wantSurveyed: true,
		},
		{
			// The DHCP-failure presentation this survey exists for: NM parks
			// the device at IP_CONFIG (70) while DHCP times out. It must
			// count as an eligible interface WITHOUT a lease — an
			// ACTIVATED-only gate here made no-lease unreachable and the
			// ladder misread venue DHCP failures as link-down.
			name: "dhcp-stuck device surveys as eligible without lease",
			output: leaseBlock("eth0", "ethernet", "70 (connecting (getting IP configuration))",
				"IP4.ADDRESS[1]:--", "IP4.GATEWAY:--"),
			wantSurveyed: true,
		},
		{
			// FAILED (120) numbers above ACTIVATED: a naive >= threshold
			// would count it. A failed device is not an eligible interface.
			name:   "failed device does not survey",
			output: leaseBlock("wlan0", "wifi", "120 (connection failed)"),
		},
		{
			name:   "nothing activated",
			output: leaseBlock("wlan0", "wifi", "30 (disconnected)"),
		},
		{
			name:   "empty output",
			output: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, hasAddr, surveyed := parseLease(tt.output, "ff1-softap")
			assert.Equal(t, tt.wantSurveyed, surveyed, "surveyed")
			assert.Equal(t, tt.wantHasAddr, hasAddr, "hasAddr")
			assert.Equal(t, tt.wantGateway, info.Gateway, "gateway")
		})
	}
}

func TestLeaseStepMapping(t *testing.T) {
	mk := func(out string, err error) Prober {
		return &prober{exec: scriptedExec{map[string]execScript{"nmcli": {out, err}}}}
	}

	_, step := mk(leaseBlock("wlan0", "wifi", "100 (connected)", "IP4.ADDRESS[1]:10.0.0.2/24"), nil).Lease(context.Background())
	assert.Equal(t, StatusOK, step.Status)

	_, step = mk(leaseBlock("wlan0", "wifi", "100 (connected)"), nil).Lease(context.Background())
	assert.Equal(t, StatusFail, step.Status)

	_, step = mk("", errors.New("nmcli exploded")).Lease(context.Background())
	assert.Equal(t, StatusError, step.Status, "probe breakage is error, not fail")

	_, step = mk(leaseBlock("lo", "loopback", "100 (connected)"), nil).Lease(context.Background())
	assert.Equal(t, StatusError, step.Status, "unsurveyed output is non-evidence")
}

func TestArpAlive(t *testing.T) {
	assert.True(t, arpAlive("192.168.1.1 dev wlan0 lladdr aa:bb:cc:dd:ee:ff REACHABLE"))
	assert.True(t, arpAlive("192.168.1.1 dev wlan0 lladdr aa:bb:cc:dd:ee:ff STALE"))
	assert.False(t, arpAlive("192.168.1.1 dev wlan0 FAILED"))
	assert.False(t, arpAlive("192.168.1.1 dev wlan0 lladdr aa:bb:cc:dd:ee:ff FAILED"))
	assert.False(t, arpAlive(""))
}

// TestGatewayPing pins the ICMP→ARP fallback: a ping-dropping gateway with a
// live ARP entry must read alive, or every ICMP-filtering venue would
// misclassify real WAN outages as gateway-dead.
func TestGatewayPing(t *testing.T) {
	tests := []struct {
		name string
		ping execScript
		arp  execScript
		want StepStatus
	}{
		{"icmp answers", execScript{output: "1 received"}, execScript{}, StatusOK},
		{"icmp dropped, arp resolved", execScript{err: errors.New("exit 1")},
			execScript{output: "192.168.1.1 dev wlan0 lladdr aa:bb:cc:dd:ee:ff REACHABLE"}, StatusOK},
		{"icmp dropped, arp unresolved", execScript{err: errors.New("exit 1")},
			execScript{output: "192.168.1.1 dev wlan0 FAILED"}, StatusFail},
		// An unavailable ARP fallback is a broken measurement, not negative
		// evidence: it must read error (→ unknown-probe), never fail
		// (→ gateway-dead), or a device with broken diagnostic tooling would
		// report confident gateway failures.
		{"icmp dropped, arp read broke", execScript{err: errors.New("exit 1")},
			execScript{err: errors.New("no ip binary")}, StatusError},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &prober{exec: scriptedExec{map[string]execScript{"ping": tt.ping, "ip": tt.arp}}}
			got := p.GatewayPing(context.Background(), "192.168.1.1")
			assert.Equal(t, tt.want, got.Status)
		})
	}
}

func TestDialTCPMapping(t *testing.T) {
	okDial := func(context.Context, string, string) (net.Conn, error) {
		c, s := net.Pipe()
		go func() { _ = s.Close() }()
		return c, nil
	}
	p := &prober{dial: okDial}
	assert.Equal(t, StatusOK, p.DialTCP(context.Background(), "1.1.1.1:443").Status)

	p = &prober{dial: func(context.Context, string, string) (net.Conn, error) {
		return nil, errors.New("connection refused")
	}}
	assert.Equal(t, StatusFail, p.DialTCP(context.Background(), "1.1.1.1:443").Status)

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	assert.Equal(t, StatusError, p.DialTCP(canceled, "1.1.1.1:443").Status,
		"a dead parent ctx is a broken measurement, not network evidence")
}

func TestPortalCheck(t *testing.T) {
	respond := func(code int) func(*http.Request) (*http.Response, error) {
		return func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: code, Body: http.NoBody}, nil
		}
	}

	p := &prober{httpDo: respond(http.StatusNoContent)}
	v, step := p.PortalCheck(context.Background(), portalProbeURL)
	assert.Equal(t, PortalClear, v)
	assert.Equal(t, StatusOK, step.Status)

	p = &prober{httpDo: respond(http.StatusFound)}
	v, _ = p.PortalCheck(context.Background(), portalProbeURL)
	assert.Equal(t, PortalIntercepted, v, "a redirect on the 204 endpoint is the portal signature")

	p = &prober{httpDo: respond(http.StatusOK)}
	v, _ = p.PortalCheck(context.Background(), portalProbeURL)
	assert.Equal(t, PortalIntercepted, v, "any non-204 body is something answering for the internet")

	p = &prober{httpDo: func(*http.Request) (*http.Response, error) { return nil, errors.New("no route to host") }}
	v, step = p.PortalCheck(context.Background(), portalProbeURL)
	assert.Equal(t, PortalUnreachable, v)
	assert.Equal(t, StatusFail, step.Status)
}

// TestLinkStepMapping pins the exclusion-aware verdict: the rung asks about
// EXTERNAL uplinks (the device's own setup AP passed as the exclusion), so a
// raised AP with no station link reads link-down, not "wifi up".
func TestLinkStepMapping(t *testing.T) {
	p := &prober{link: fakeLink{t: t, link: true}, softapProfile: "ff1-softap"}
	got := p.Link(context.Background())
	require.Equal(t, StatusOK, got.Status)
	assert.Equal(t, "wifi", got.Detail)

	p = &prober{link: fakeLink{t: t, link: true, wired: true}, softapProfile: "ff1-softap"}
	assert.Equal(t, "ethernet", p.Link(context.Background()).Detail)

	// Own-AP-only radio: the exclusion-aware probe reports no link.
	p = &prober{link: fakeLink{t: t}, softapProfile: "ff1-softap"}
	step := p.Link(context.Background())
	assert.Equal(t, StatusFail, step.Status)
	assert.Contains(t, step.Detail, "own setup AP excluded")

	p = &prober{link: fakeLink{t: t, err: errors.New("nmcli wedged")}, softapProfile: "ff1-softap"}
	assert.Equal(t, StatusError, p.Link(context.Background()).Status)
}

type fakeLink struct {
	t           *testing.T
	link, wired bool
	err         error
}

func (f fakeLink) DiagnosticLinkDetail(_ context.Context, excludeProfile string) (bool, bool, error) {
	if f.t != nil {
		require.Equal(f.t, "ff1-softap", excludeProfile, "the rung must exclude the setup AP profile")
	}
	return f.link, f.wired, f.err
}

func TestNMSnapshotTruncates(t *testing.T) {
	long := strings.Repeat("GENERAL.STATE:100 (connected)\n", 200)
	p := &prober{exec: scriptedExec{map[string]execScript{"nmcli": {output: long}}}}
	snap := p.NMSnapshot(context.Background())
	assert.LessOrEqual(t, len(snap), nmSnapshotMaxBytes)
	assert.NotEmpty(t, snap)
}
