package netlog

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Per-rung timeouts. Deliberately short: the ladder runs when the network is
// already sick, must finish inside the runNetworkDiagnostics reply budget
// (the hub's 30 s write deadline), and every rung that would hang is evidence
// in itself. Worst case, all rungs timing out sums to roughly 23 s.
const (
	nmcliProbeTimeout  = 3 * time.Second
	pingProbeTimeout   = 4 * time.Second // ping -W 2 plus exec slack
	dnsProbeTimeout    = 3 * time.Second
	tcpProbeTimeout    = 3 * time.Second
	portalProbeTimeout = 5 * time.Second

	// nmSnapshotMaxBytes bounds the raw nmcli capture stored per ladder run
	// so one record cannot dominate the 8 MB ring.
	nmSnapshotMaxBytes = 2048

	// publicDNSAddr is the resolver for the "is DNS itself broken or just the
	// venue's resolver" split. Cloudflare rather than Google — the monitord
	// probe's suspected blind spot is exactly Google-blocking networks.
	publicDNSAddr = "1.1.1.1:53"
)

// LinkProber is the consumer-owned seam onto status.LinkChecker.LinkTelemetry
// (the same seam netmetrics uses).
type LinkProber interface {
	LinkTelemetry(ctx context.Context) (wired, wifi bool, err error)
}

// prober is the production Prober. The exec/dial/http/lookup seams exist so
// every rung is unit-testable without a network; production wiring fills them
// with the real implementations via NewProber.
type prober struct {
	link LinkProber
	exec wrapper.Exec

	dial      func(ctx context.Context, network, addr string) (net.Conn, error)
	httpDo    func(req *http.Request) (*http.Response, error)
	lookupSys func(ctx context.Context, host string) ([]string, error)
	lookupPub func(ctx context.Context, host string) ([]string, error)
}

// NewProber builds the production prober over the shared link checker and
// exec wrapper.
func NewProber(link LinkProber, exec wrapper.Exec) Prober {
	d := &net.Dialer{}
	pubResolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "udp", publicDNSAddr)
		},
	}
	// The portal client must not follow redirects (a redirect IS the portal
	// verdict) and must not reuse connections (each run probes the network's
	// current state, not a cached socket's).
	httpClient := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		Transport:     &http.Transport{DisableKeepAlives: true},
	}
	return &prober{
		link:   link,
		exec:   exec,
		dial:   d.DialContext,
		httpDo: httpClient.Do,
		lookupSys: func(ctx context.Context, host string) ([]string, error) {
			return net.DefaultResolver.LookupHost(ctx, host)
		},
		lookupPub: func(ctx context.Context, host string) ([]string, error) { return pubResolver.LookupHost(ctx, host) },
	}
}

func (p *prober) Link(ctx context.Context) Step {
	wired, wifi, err := p.link.LinkTelemetry(ctx)
	if err != nil {
		return Step{Status: StatusError, Detail: err.Error()}
	}
	if !wired && !wifi {
		return Step{Status: StatusFail, Detail: "no activated uplink"}
	}
	return Step{Status: StatusOK, Detail: linkDetail(wired, wifi)}
}

func linkDetail(wired, wifi bool) string {
	switch {
	case wired && wifi:
		return "ethernet+wifi"
	case wired:
		return "ethernet"
	default:
		return "wifi"
	}
}

// NMSnapshot captures device state + reason codes at run time (plan branch
// D1). Best-effort: an error yields "", never fails the run.
func (p *prober) NMSnapshot(ctx context.Context) string {
	probeCtx, cancel := context.WithTimeout(ctx, nmcliProbeTimeout)
	defer cancel()
	// GENERAL.REASON is NM's last state-change reason enum for the device —
	// the closest nmcli gets to the D-Bus StateChanged reason codes without a
	// signal subscription (branch D2, deferred).
	cmd := p.exec.CommandContext(probeCtx, "nmcli", "-t", "-f",
		"GENERAL.DEVICE,GENERAL.TYPE,GENERAL.STATE,GENERAL.REASON,GENERAL.CONNECTION", "device", "show")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	if len(out) > nmSnapshotMaxBytes {
		out = out[:nmSnapshotMaxBytes]
	}
	return string(out)
}

// Lease reads the ACTIVATED devices' IPv4 lease state from one nmcli pass:
// an address present = lease ok; the first gateway found feeds the gateway
// rung; DNS servers are recorded for the run's evidence.
func (p *prober) Lease(ctx context.Context) (LeaseInfo, Step) {
	probeCtx, cancel := context.WithTimeout(ctx, nmcliProbeTimeout)
	defer cancel()
	cmd := p.exec.CommandContext(probeCtx, "nmcli", "-t", "-f",
		"GENERAL.DEVICE,GENERAL.TYPE,GENERAL.STATE,IP4.ADDRESS,IP4.GATEWAY,IP4.DNS", "device", "show")
	out, err := cmd.Output()
	if err != nil {
		return LeaseInfo{}, Step{Status: StatusError, Detail: err.Error()}
	}
	info, hasAddr, surveyed := parseLease(string(out))
	if !surveyed {
		return LeaseInfo{}, Step{Status: StatusError, Detail: "no activated ethernet/wifi block in nmcli output"}
	}
	if !hasAddr {
		return info, Step{Status: StatusFail, Detail: "no IPv4 address on any activated device"}
	}
	return info, Step{Status: StatusOK, Detail: fmt.Sprintf("gw=%s dns=%s", info.Gateway, strings.Join(info.DNS, ","))}
}

// parseLease walks terse `nmcli device show` blocks (same block-delimiter
// contract as status.LinkChecker.linkProbe: GENERAL.DEVICE opens each block
// because -f lists it first). Only ACTIVATED ethernet/wifi blocks count.
// surveyed=false means no such block existed — non-evidence, not a verdict.
func parseLease(output string) (info LeaseInfo, hasAddr, surveyed bool) {
	type block struct {
		typ     string
		state   int
		addrs   []string
		gateway string
		dns     []string
	}
	var cur block
	flush := func() {
		if cur.typ != "ethernet" && cur.typ != "wifi" {
			return
		}
		if cur.state != 100 { // NM_DEVICE_STATE_ACTIVATED
			return
		}
		surveyed = true
		if len(cur.addrs) > 0 {
			hasAddr = true
		}
		if info.Gateway == "" && cur.gateway != "" {
			info.Gateway = cur.gateway
		}
		info.DNS = append(info.DNS, cur.dns...)
	}
	for _, line := range strings.Split(output, "\n") {
		field, value, ok := strings.Cut(strings.TrimSpace(line), ":")
		if !ok {
			continue
		}
		// IP4.ADDRESS/IP4.DNS render indexed ("IP4.ADDRESS[1]"); strip the index.
		if i := strings.IndexByte(field, '['); i >= 0 {
			field = field[:i]
		}
		switch field {
		case "GENERAL.DEVICE":
			flush()
			cur = block{}
		case "GENERAL.TYPE":
			cur.typ = value
		case "GENERAL.STATE":
			// "100 (connected)" — locale-stable numeric enum first (same
			// parsing rule as status.LinkChecker.linkProbe). Unparsable
			// stays 0 (not ACTIVATED), the safe reading.
			num, _, _ := strings.Cut(value, " ")
			if n, convErr := strconv.Atoi(num); convErr == nil {
				cur.state = n
			}
		case "IP4.ADDRESS":
			if value != "" {
				cur.addrs = append(cur.addrs, value)
			}
		case "IP4.GATEWAY":
			if value != "" && value != "--" {
				cur.gateway = value
			}
		case "IP4.DNS":
			if value != "" {
				cur.dns = append(cur.dns, value)
			}
		}
	}
	flush()
	return info, hasAddr, surveyed
}

// GatewayPing probes the gateway with ICMP, falling back to the kernel's ARP
// table when ICMP is dropped: many gateways drop ping, and misreading
// "gateway filters ICMP during a WAN outage" as gateway-dead would poison the
// wan-down class. ARP proof of life (a resolved lladdr in a live state)
// upgrades an ICMP fail to ok.
func (p *prober) GatewayPing(ctx context.Context, gateway string) Step {
	probeCtx, cancel := context.WithTimeout(ctx, pingProbeTimeout)
	defer cancel()
	cmd := p.exec.CommandContext(probeCtx, "ping", "-c", "1", "-W", "2", gateway)
	if _, err := cmd.Output(); err == nil {
		return Step{Status: StatusOK, Detail: "icmp"}
	} else if probeCtx.Err() != nil {
		return Step{Status: StatusError, Detail: probeCtx.Err().Error()}
	}

	neighCtx, cancel2 := context.WithTimeout(ctx, nmcliProbeTimeout)
	defer cancel2()
	out, err := p.exec.CommandContext(neighCtx, "ip", "neigh", "show", "to", gateway).Output()
	if err != nil {
		return Step{Status: StatusFail, Detail: "icmp failed; arp unknown"}
	}
	if arpAlive(string(out)) {
		return Step{Status: StatusOK, Detail: "icmp filtered; arp resolved"}
	}
	return Step{Status: StatusFail, Detail: "icmp failed; arp unresolved"}
}

// arpAlive reports whether an `ip neigh` line shows a resolved, non-FAILED
// neighbor entry (has a lladdr and not in FAILED/INCOMPLETE state).
func arpAlive(out string) bool {
	for _, line := range strings.Split(out, "\n") {
		if !strings.Contains(line, "lladdr") {
			continue
		}
		if strings.Contains(line, "FAILED") || strings.Contains(line, "INCOMPLETE") {
			continue
		}
		return true
	}
	return false
}

func (p *prober) ResolveConfigured(ctx context.Context, host string) Step {
	return resolveStep(ctx, p.lookupSys, host)
}

func (p *prober) ResolvePublic(ctx context.Context, host string) Step {
	return resolveStep(ctx, p.lookupPub, host)
}

func resolveStep(ctx context.Context, lookup func(context.Context, string) ([]string, error), host string) Step {
	probeCtx, cancel := context.WithTimeout(ctx, dnsProbeTimeout)
	defer cancel()
	addrs, err := lookup(probeCtx, host)
	if err != nil {
		if ctx.Err() != nil {
			return Step{Status: StatusError, Detail: ctx.Err().Error()}
		}
		return Step{Status: StatusFail, Detail: err.Error()}
	}
	return Step{Status: StatusOK, Detail: strings.Join(addrs, ",")}
}

func (p *prober) DialTCP(ctx context.Context, addr string) Step {
	probeCtx, cancel := context.WithTimeout(ctx, tcpProbeTimeout)
	defer cancel()
	conn, err := p.dial(probeCtx, "tcp", addr)
	if err != nil {
		// The parent ctx dying is a broken measurement (error); the probe's
		// own timeout expiring is the measurement (fail: nothing answered).
		if ctx.Err() != nil {
			return Step{Status: StatusError, Detail: ctx.Err().Error()}
		}
		return Step{Status: StatusFail, Detail: err.Error()}
	}
	_ = conn.Close()
	return Step{Status: StatusOK}
}

func (p *prober) PortalCheck(ctx context.Context, url string) (PortalVerdict, Step) {
	probeCtx, cancel := context.WithTimeout(ctx, portalProbeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, url, nil)
	if err != nil {
		return PortalUnreachable, Step{Status: StatusError, Detail: err.Error()}
	}
	resp, err := p.httpDo(req)
	if err != nil {
		if ctx.Err() != nil {
			return PortalUnreachable, Step{Status: StatusError, Detail: ctx.Err().Error()}
		}
		return PortalUnreachable, Step{Status: StatusFail, Detail: err.Error()}
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNoContent {
		return PortalClear, Step{Status: StatusOK, Detail: "204"}
	}
	// Anything else answering on a plain-HTTP 204 endpoint is something local
	// speaking FOR the internet — the captive-portal signature.
	return PortalIntercepted, Step{Status: StatusOK, Detail: fmt.Sprintf("http %d", resp.StatusCode)}
}
