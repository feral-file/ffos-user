package netlog

import (
	"context"
	"sync"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Class is the outage taxonomy (docs/wan-outage-observability.md stage 1).
// Every value the classifier can emit; unknown-* means the evidence did not
// support a confident verdict — the classifier must NEVER guess.
type Class string

const (
	ClassOnline          Class = "online" // on-demand run while healthy, or the probe-bias contradiction
	ClassLinkDown        Class = "link-down"
	ClassNoLease         Class = "no-lease"
	ClassGatewayDead     Class = "gateway-dead"
	ClassDNSBroken       Class = "dns-broken"
	ClassCaptivePortal   Class = "captive-portal"
	ClassWANDown         Class = "wan-down"
	ClassBackendOnlyDown Class = "backend-only-down"
	ClassUnknownLink     Class = "unknown-link"  // could not even establish link state
	ClassUnknownProbe    Class = "unknown-probe" // a rung needed for the verdict errored
)

// StepStatus is one probe rung's outcome. The ok/fail/error distinction is
// load-bearing for classification: fail is measured evidence ("the dial was
// refused"), error is absent evidence ("the probe itself broke"), and only
// evidence may produce a confident class.
type StepStatus string

const (
	StatusOK    StepStatus = "ok"
	StatusFail  StepStatus = "fail"
	StatusError StepStatus = "error"
	StatusSkip  StepStatus = "skip" // rung not run (short-circuited or no input)
)

// Step is one rung's recorded outcome.
type Step struct {
	Status StepStatus `json:"status"`
	Detail string     `json:"detail,omitempty"`
	Millis int64      `json:"ms,omitempty"`
}

// PortalVerdict is the captive-portal rung's tri-state.
type PortalVerdict string

const (
	PortalClear       PortalVerdict = "clear"       // got the expected 204
	PortalIntercepted PortalVerdict = "intercepted" // got a redirect/other body: something is answering for the internet
	PortalUnreachable PortalVerdict = "unreachable" // no HTTP answer at all
	PortalSkipped     PortalVerdict = "skipped"
)

// LadderResult is one full diagnosis run, recorded to the ring and returned
// by runNetworkDiagnostics. Field order mirrors rung order.
type LadderResult struct {
	Trigger string `json:"trigger"` // "failure-edge" | "on-demand"
	Class   Class  `json:"class"`

	Link          Step          `json:"link"`
	Lease         Step          `json:"lease"`
	Gateway       Step          `json:"gateway"`
	DNSConfigured Step          `json:"dns_configured"`
	DNSPublic     Step          `json:"dns_public"`
	TCPBackend    Step          `json:"tcp_backend"`
	TCPNeutral    Step          `json:"tcp_neutral"`
	Portal        Step          `json:"portal"`
	PortalVerdict PortalVerdict `json:"portal_verdict"`

	// NMSnapshot is the raw (truncated) nmcli device state/reason capture at
	// run time — plan branch D1's poll-time reason-code capture.
	NMSnapshot string `json:"nm_snapshot,omitempty"`
	Millis     int64  `json:"ms"`
}

// LeaseInfo is what the lease rung extracts for the rungs below it.
type LeaseInfo struct {
	Gateway string   `json:"gateway,omitempty"`
	DNS     []string `json:"dns,omitempty"`
}

// Prober is the seam the ladder runs on; the production implementation lives
// in prober.go, tests substitute a table-driven fake. Every method must honor
// ctx and be safe for CONCURRENT calls: the ladder runs the independent lower
// rungs in parallel (see Run's timeout-budget note). The production prober is
// stateless per call, so this holds by construction; fakes must synchronize.
type Prober interface {
	// Link reports whether any uplink (ethernet or wifi, own AP included) is
	// ACTIVATED. StatusError means the survey itself failed.
	Link(ctx context.Context) Step
	// NMSnapshot best-effort captures nmcli device state+reason for the record.
	NMSnapshot(ctx context.Context) string
	// Lease reports whether an ACTIVATED device holds an IPv4 address, and the
	// gateway/DNS it declares.
	Lease(ctx context.Context) (LeaseInfo, Step)
	// GatewayPing reports ICMP reachability of the lease's gateway.
	GatewayPing(ctx context.Context, gateway string) Step
	// ResolveConfigured resolves host through the system resolver;
	// ResolvePublic through a hardcoded public one. Fail = did not resolve.
	ResolveConfigured(ctx context.Context, host string) Step
	ResolvePublic(ctx context.Context, host string) Step
	// DialTCP reports a TCP connect to addr (used for backend and neutral).
	DialTCP(ctx context.Context, addr string) Step
	// PortalCheck fetches the 204 probe URL without following redirects.
	PortalCheck(ctx context.Context, url string) (PortalVerdict, Step)
}

// Ladder target defaults. Constants rather than config: they encode the
// DIAGNOSTIC geometry (a neutral IP that needs no DNS and is not Google, a
// portal probe that answers 204), not per-site tuning.
const (
	// neutralTCPAddr is a TLS-serving anycast IP reachable without DNS and
	// independent of both Google (the monitord probe's suspected blind spot)
	// and the Feral File backend. 1.1.1.1 serves 443 globally.
	neutralTCPAddr = "1.1.1.1:443"
	// portalProbeURL must be plain HTTP (a portal can only intercept what it
	// can read) and reliably 204. The gstatic endpoint is the de-facto
	// standard; on a Google-blocking network it reads "unreachable", which
	// the classifier treats as non-evidence, never as a portal.
	portalProbeURL = "http://connectivitycheck.gstatic.com/generate_204"
)

// Ladder runs the rung sequence over a Prober and classifies the outcome.
// Concurrent Run callers share ONE in-flight execution (singleflight):
// overlapping diagnosis runs would double network load exactly when the
// network is sick, and — decisively — an on-demand command arriving during
// an automatic failure-edge run would otherwise wait out the whole run
// behind a mutex only to burn its own reply budget re-probing the same
// network for a near-identical answer.
type Ladder struct {
	sf     singleflight.Group
	prober Prober
	clock  wrapper.Clock
	logger *zap.Logger

	// backendAddr/probeHost derive from the relayer endpoint at wiring time:
	// the backend the device actually needs reachable.
	backendAddr string
	probeHost   string
	neutralAddr string
	portalURL   string
}

// NewLadder wires a diagnosis ladder. backendHost is the relayer endpoint's
// hostname (dialed as host:443 and used as the DNS probe name).
func NewLadder(prober Prober, backendHost string, clock wrapper.Clock, logger *zap.Logger) *Ladder {
	return &Ladder{
		prober:      prober,
		clock:       clock,
		logger:      logger,
		backendAddr: backendHost + ":443",
		probeHost:   backendHost,
		neutralAddr: neutralTCPAddr,
		portalURL:   portalProbeURL,
	}
}

// Run executes one full ladder pass, or joins the pass already in flight
// (the joiner gets that run's result — its Trigger field says which caller
// started it). Probe discipline: the caller (recorder) enforces the rate
// limit for automatic triggers.
//
// Timeout budget: the serial prefix (link 3 s + NM snapshot 3 s + lease 3 s)
// plus the CONCURRENT lower rungs — gateway is the slowest at 7 s worst
// (4 s ping + 3 s ARP fallback), against DNS/TCP at 3 s and portal at 5 s —
// bounds a full pass at ~16 s even with every rung timing out. That fits the
// executor's 25 s backstop and the hub's 30 s write deadline with margin;
// the rungs were originally serial and worst-cased at ~33 s, which silently
// truncated on-demand diagnosis to unknown-* on exactly the sick networks it
// targets. Rungs never short-circuit on failure — the classifier needs the
// full evidence set (e.g. "gateway dead but neutral TCP ok" is a
// contradiction worth recording verbatim) — except that the lower rungs are
// skipped when there is provably no link to run them over.
func (l *Ladder) Run(ctx context.Context, trigger string) *LadderResult {
	v, _, _ := l.sf.Do("run", func() (interface{}, error) {
		return l.runPass(ctx, trigger), nil
	})
	return v.(*LadderResult)
}

// runPass is one real execution (see Run for the sharing and budget rules).
func (l *Ladder) runPass(ctx context.Context, trigger string) *LadderResult {
	started := l.clock.Now()
	res := &LadderResult{Trigger: trigger, PortalVerdict: PortalSkipped}

	res.Link = l.timed(ctx, func(c context.Context) Step { return l.prober.Link(c) })
	res.NMSnapshot = l.prober.NMSnapshot(ctx)

	// With no link (or unknowable link) the lower rungs can only time out
	// one after another against a dead interface: skip them. The classifier
	// answers from the link rung alone in both cases.
	if res.Link.Status == StatusOK {
		var lease LeaseInfo
		res.Lease = l.timed(ctx, func(c context.Context) Step {
			var s Step
			lease, s = l.prober.Lease(c)
			return s
		})

		// The remaining rungs are independent measurements: run them
		// concurrently so the pass is bounded by the slowest rung, not the
		// sum (the budget math above depends on this). Each goroutine writes
		// a distinct res field; wg.Wait orders those writes before Classify
		// reads them.
		var wg sync.WaitGroup
		parallel := func(target *Step, fn func(context.Context) Step) {
			wg.Add(1)
			go func() {
				defer wg.Done()
				*target = l.timed(ctx, fn)
			}()
		}
		if gw := lease.Gateway; gw != "" {
			parallel(&res.Gateway, func(c context.Context) Step { return l.prober.GatewayPing(c, gw) })
		} else {
			res.Gateway = Step{Status: StatusSkip, Detail: "no gateway in lease"}
		}
		parallel(&res.DNSConfigured, func(c context.Context) Step { return l.prober.ResolveConfigured(c, l.probeHost) })
		parallel(&res.DNSPublic, func(c context.Context) Step { return l.prober.ResolvePublic(c, l.probeHost) })
		parallel(&res.TCPBackend, func(c context.Context) Step { return l.prober.DialTCP(c, l.backendAddr) })
		parallel(&res.TCPNeutral, func(c context.Context) Step { return l.prober.DialTCP(c, l.neutralAddr) })
		parallel(&res.Portal, func(c context.Context) Step {
			var s Step
			res.PortalVerdict, s = l.prober.PortalCheck(c, l.portalURL)
			return s
		})
		wg.Wait()
	} else {
		skip := Step{Status: StatusSkip, Detail: "no link"}
		res.Lease, res.Gateway, res.DNSConfigured, res.DNSPublic = skip, skip, skip, skip
		res.TCPBackend, res.TCPNeutral, res.Portal = skip, skip, skip
	}

	res.Class = Classify(res)
	res.Millis = l.clock.Now().Sub(started).Milliseconds()
	return res
}

// stepDetailMaxBytes bounds one rung's Detail string. The detail can carry
// network-controlled content (resolver answers, error strings from the venue
// network under investigation), and an adversarial or broken network must
// not get to set the ring's retention by inflating records — the same rule
// as nmSnapshotMaxBytes.
const stepDetailMaxBytes = 256

// timed wraps one rung with duration accounting and the detail bound (here,
// so every rung — including future ones — inherits it).
func (l *Ladder) timed(ctx context.Context, fn func(context.Context) Step) Step {
	start := l.clock.Now()
	s := fn(ctx)
	s.Millis = l.clock.Now().Sub(start).Milliseconds()
	if len(s.Detail) > stepDetailMaxBytes {
		s.Detail = s.Detail[:stepDetailMaxBytes]
	}
	return s
}

// Classify turns one run's evidence into a Class. Pure — the whole taxonomy
// is table-tested. Rules, in order of decisiveness:
//
//  1. No link evidence beats everything: fail → link-down, error → unknown-link.
//  2. An intercepted portal probe is definitive (something local answered FOR
//     the internet) regardless of what the TCP rungs saw — portals often let
//     TCP SYNs through while hijacking payloads.
//  3. Neutral TCP ok means the WAN works: backend ok → online; backend fail →
//     dns-broken when the evidence shows exactly "configured resolver broken,
//     public resolver fine" (the backend dial resolves through the configured
//     resolver, so that failure mode masquerades as a backend outage),
//     otherwise backend-only-down.
//  4. Neutral TCP fail walks down the stack for the first measured local
//     failure: no-lease, then gateway-dead, then wan-down when the local net
//     is measured healthy. Any error (not fail) among those rungs makes the
//     verdict unknown-probe: "the WAN is down" must not be claimed off a
//     broken measurement.
func Classify(r *LadderResult) Class {
	switch r.Link.Status {
	case StatusFail:
		return ClassLinkDown
	case StatusError, StatusSkip:
		return ClassUnknownLink
	case StatusOK:
	}

	if r.PortalVerdict == PortalIntercepted {
		return ClassCaptivePortal
	}

	switch r.TCPNeutral.Status {
	case StatusOK:
		switch r.TCPBackend.Status {
		case StatusOK:
			return ClassOnline
		case StatusFail:
			if r.DNSConfigured.Status == StatusFail && r.DNSPublic.Status == StatusOK {
				return ClassDNSBroken
			}
			return ClassBackendOnlyDown
		default:
			return ClassUnknownProbe
		}
	case StatusFail:
		switch r.Lease.Status {
		case StatusFail:
			return ClassNoLease
		case StatusOK:
		default:
			return ClassUnknownProbe
		}
		switch r.Gateway.Status {
		case StatusFail:
			return ClassGatewayDead
		case StatusOK:
			return ClassWANDown
		default:
			return ClassUnknownProbe
		}
	default:
		return ClassUnknownProbe
	}
}
