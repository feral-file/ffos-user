package netlog

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// res builds a LadderResult for classifier rows. Portal defaults to skipped.
func res(mut func(*LadderResult)) *LadderResult {
	r := &LadderResult{PortalVerdict: PortalSkipped}
	mut(r)
	return r
}

func ok() Step    { return Step{Status: StatusOK} }
func fail() Step  { return Step{Status: StatusFail} }
func errS() Step  { return Step{Status: StatusError} }
func skipS() Step { return Step{Status: StatusSkip} }

// healthyBase fills every rung ok — rows then break exactly the rungs their
// scenario claims, so each row documents one failure geometry.
func healthyBase(r *LadderResult) {
	r.Link, r.Lease, r.Gateway = ok(), ok(), ok()
	r.DNSConfigured, r.DNSPublic = ok(), ok()
	r.TCPBackend, r.TCPNeutral, r.Portal = ok(), ok(), ok()
	r.PortalVerdict = PortalClear
}

// TestClassify pins the full taxonomy (plan §5: every class gets a row, and
// ambiguous combinations pin their verdict explicitly; unknown/error inputs
// must never produce a confident class).
func TestClassify(t *testing.T) {
	tests := []struct {
		name string
		r    *LadderResult
		want Class
	}{
		{"healthy on-demand run", res(healthyBase), ClassOnline},
		{"link confirmed absent", res(func(r *LadderResult) { r.Link = fail() }), ClassLinkDown},
		{"link probe broke", res(func(r *LadderResult) { r.Link = errS() }), ClassUnknownLink},
		{
			"portal interception is definitive even with TCP ok",
			res(func(r *LadderResult) { healthyBase(r); r.PortalVerdict = PortalIntercepted; r.TCPBackend = ok() }),
			ClassCaptivePortal,
		},
		{
			"backend down, WAN fine",
			res(func(r *LadderResult) { healthyBase(r); r.TCPBackend = fail(); r.PortalVerdict = PortalClear }),
			ClassBackendOnlyDown,
		},
		{
			// The backend dial resolves through the CONFIGURED resolver, so a
			// broken venue resolver masquerades as a backend outage. Pinned:
			// the dns evidence reclassifies it.
			"venue resolver broken masquerading as backend outage",
			res(func(r *LadderResult) {
				healthyBase(r)
				r.TCPBackend, r.DNSConfigured = fail(), fail()
				r.PortalVerdict = PortalClear
			}),
			ClassDNSBroken,
		},
		{
			"dns evidence incomplete keeps backend-only verdict",
			res(func(r *LadderResult) {
				healthyBase(r)
				r.TCPBackend, r.DNSConfigured, r.DNSPublic = fail(), fail(), fail()
				r.PortalVerdict = PortalClear
			}),
			ClassBackendOnlyDown,
		},
		{
			"backend probe broke with WAN fine",
			res(func(r *LadderResult) { healthyBase(r); r.TCPBackend = errS(); r.PortalVerdict = PortalClear }),
			ClassUnknownProbe,
		},
		{
			"no lease",
			res(func(r *LadderResult) {
				healthyBase(r)
				r.Lease, r.TCPNeutral = fail(), fail()
				r.PortalVerdict = PortalUnreachable
			}),
			ClassNoLease,
		},
		{
			"lease probe broke",
			res(func(r *LadderResult) {
				healthyBase(r)
				r.Lease, r.TCPNeutral = errS(), fail()
				r.PortalVerdict = PortalUnreachable
			}),
			ClassUnknownProbe,
		},
		{
			"gateway dead",
			res(func(r *LadderResult) {
				healthyBase(r)
				r.Gateway, r.TCPNeutral = fail(), fail()
				r.PortalVerdict = PortalUnreachable
			}),
			ClassGatewayDead,
		},
		{
			// No gateway in the lease at all: gateway rung is skipped —
			// "wan-down" must not be claimed without gateway evidence.
			"gateway unknown blocks a wan-down claim",
			res(func(r *LadderResult) {
				healthyBase(r)
				r.Gateway, r.TCPNeutral = skipS(), fail()
				r.PortalVerdict = PortalUnreachable
			}),
			ClassUnknownProbe,
		},
		{
			"wan down with healthy local net",
			res(func(r *LadderResult) {
				healthyBase(r)
				r.TCPNeutral = fail()
				r.TCPBackend = fail()
				r.PortalVerdict = PortalUnreachable
			}),
			ClassWANDown,
		},
		{
			"neutral probe broke",
			res(func(r *LadderResult) { healthyBase(r); r.TCPNeutral = errS() }),
			ClassUnknownProbe,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, Classify(tt.r))
		})
	}
}

// scriptedProber returns canned steps and records which rungs ran.
type scriptedProber struct {
	link    Step
	lease   Step
	leaseGw string
	calls   []string
}

func (s *scriptedProber) Link(context.Context) Step { s.calls = append(s.calls, "link"); return s.link }
func (s *scriptedProber) NMSnapshot(context.Context) string {
	s.calls = append(s.calls, "nm")
	return "GENERAL.STATE:100 (connected)"
}
func (s *scriptedProber) Lease(context.Context) (LeaseInfo, Step) {
	s.calls = append(s.calls, "lease")
	return LeaseInfo{Gateway: s.leaseGw}, s.lease
}
func (s *scriptedProber) GatewayPing(_ context.Context, gw string) Step {
	s.calls = append(s.calls, "gw:"+gw)
	return Step{Status: StatusOK}
}
func (s *scriptedProber) ResolveConfigured(context.Context, string) Step {
	s.calls = append(s.calls, "dns-conf")
	return Step{Status: StatusOK}
}
func (s *scriptedProber) ResolvePublic(context.Context, string) Step {
	s.calls = append(s.calls, "dns-pub")
	return Step{Status: StatusOK}
}
func (s *scriptedProber) DialTCP(_ context.Context, addr string) Step {
	s.calls = append(s.calls, "tcp:"+addr)
	return Step{Status: StatusOK}
}
func (s *scriptedProber) PortalCheck(context.Context, string) (PortalVerdict, Step) {
	s.calls = append(s.calls, "portal")
	return PortalClear, Step{Status: StatusOK}
}

// TestLadderRunSkipsLowerRungsWithoutLink: probing DNS/TCP over a dead
// interface only stacks timeouts — the run must answer from the link rung.
func TestLadderRunSkipsLowerRungsWithoutLink(t *testing.T) {
	p := &scriptedProber{link: Step{Status: StatusFail}}
	l := NewLadder(p, "relayer.example.com", wrapper.NewClock(), zap.NewNop())

	got := l.Run(context.Background(), "failure-edge")

	assert.Equal(t, ClassLinkDown, got.Class)
	assert.Equal(t, []string{"link", "nm"}, p.calls, "lower rungs must be skipped")
	assert.Equal(t, StatusSkip, got.TCPNeutral.Status)
	assert.Equal(t, PortalSkipped, got.PortalVerdict)
	assert.NotEmpty(t, got.NMSnapshot)
}

func TestLadderRunFullPass(t *testing.T) {
	p := &scriptedProber{link: Step{Status: StatusOK}, lease: Step{Status: StatusOK}, leaseGw: "192.168.1.1"}
	l := NewLadder(p, "relayer.example.com", wrapper.NewClock(), zap.NewNop())

	got := l.Run(context.Background(), "on-demand")

	assert.Equal(t, ClassOnline, got.Class)
	assert.Equal(t, "on-demand", got.Trigger)
	assert.Contains(t, p.calls, "gw:192.168.1.1")
	assert.Contains(t, p.calls, "tcp:relayer.example.com:443")
	assert.Contains(t, p.calls, "tcp:"+neutralTCPAddr)
	assert.Contains(t, p.calls, "portal")
}

// TestLadderRunSkipsGatewayWithoutLeaseGateway: a lease with no gateway must
// mark the rung skip (feeding the unknown-not-wan-down pin above).
func TestLadderRunSkipsGatewayWithoutLeaseGateway(t *testing.T) {
	p := &scriptedProber{link: Step{Status: StatusOK}, lease: Step{Status: StatusOK}}
	l := NewLadder(p, "relayer.example.com", wrapper.NewClock(), zap.NewNop())

	got := l.Run(context.Background(), "on-demand")

	assert.Equal(t, StatusSkip, got.Gateway.Status)
	assert.NotContains(t, p.calls, "gw:")
}
