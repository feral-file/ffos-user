package netlog

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// scriptedProber returns canned steps and records which rungs ran. The call
// log is mutex-guarded: the ladder runs the lower rungs concurrently.
type scriptedProber struct {
	mu      sync.Mutex
	link    Step
	lease   Step
	leaseGw string
	calls   []string
}

func (s *scriptedProber) record(call string) {
	s.mu.Lock()
	s.calls = append(s.calls, call)
	s.mu.Unlock()
}

func (s *scriptedProber) recorded() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calls...)
}

func (s *scriptedProber) Link(context.Context) Step { s.record("link"); return s.link }
func (s *scriptedProber) NMSnapshot(context.Context) string {
	s.record("nm")
	return "GENERAL.STATE:100 (connected)"
}
func (s *scriptedProber) Lease(context.Context) (LeaseInfo, Step) {
	s.record("lease")
	return LeaseInfo{Gateway: s.leaseGw}, s.lease
}
func (s *scriptedProber) GatewayPing(_ context.Context, gw string) Step {
	s.record("gw:" + gw)
	return Step{Status: StatusOK}
}
func (s *scriptedProber) ResolveConfigured(context.Context, string) Step {
	s.record("dns-conf")
	return Step{Status: StatusOK}
}
func (s *scriptedProber) ResolvePublic(context.Context, string) Step {
	s.record("dns-pub")
	return Step{Status: StatusOK}
}
func (s *scriptedProber) DialTCP(_ context.Context, addr string) Step {
	s.record("tcp:" + addr)
	return Step{Status: StatusOK}
}
func (s *scriptedProber) PortalCheck(context.Context, string) (PortalVerdict, Step) {
	s.record("portal")
	return PortalClear, Step{Status: StatusOK}
}

// TestLadderRunSkipsLowerRungsWithoutLink: probing DNS/TCP over a dead
// interface only stacks timeouts — the run must answer from the link rung.
func TestLadderRunSkipsLowerRungsWithoutLink(t *testing.T) {
	p := &scriptedProber{link: Step{Status: StatusFail}}
	l := NewLadder(p, "relayer.example.com", wrapper.NewClock(), zap.NewNop())

	got := l.Run(context.Background(), "failure-edge")

	assert.Equal(t, ClassLinkDown, got.Class)
	assert.Equal(t, []string{"link", "nm"}, p.recorded(), "lower rungs must be skipped")
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
	assert.Contains(t, p.recorded(), "gw:192.168.1.1")
	assert.Contains(t, p.recorded(), "tcp:relayer.example.com:443")
	assert.Contains(t, p.recorded(), "tcp:"+neutralTCPAddr)
	assert.Contains(t, p.recorded(), "portal")
}

// slowProber makes every lower rung sleep, to expose the run's time shape.
type slowProber struct {
	scriptedProber
	rungDelay time.Duration
}

func (s *slowProber) GatewayPing(ctx context.Context, gw string) Step {
	time.Sleep(s.rungDelay)
	return s.scriptedProber.GatewayPing(ctx, gw)
}
func (s *slowProber) ResolveConfigured(ctx context.Context, h string) Step {
	time.Sleep(s.rungDelay)
	return s.scriptedProber.ResolveConfigured(ctx, h)
}
func (s *slowProber) ResolvePublic(ctx context.Context, h string) Step {
	time.Sleep(s.rungDelay)
	return s.scriptedProber.ResolvePublic(ctx, h)
}
func (s *slowProber) DialTCP(ctx context.Context, a string) Step {
	time.Sleep(s.rungDelay)
	return s.scriptedProber.DialTCP(ctx, a)
}
func (s *slowProber) PortalCheck(ctx context.Context, u string) (PortalVerdict, Step) {
	time.Sleep(s.rungDelay)
	return s.scriptedProber.PortalCheck(ctx, u)
}

// TestLadderLowerRungsRunConcurrently pins the timeout-budget geometry: the
// six lower rungs must be bounded by the SLOWEST rung, not the sum — the
// serial shape worst-cased at ~33 s, which silently truncated on-demand
// diagnosis past the executor's 25 s backstop (the bug this test guards).
func TestLadderLowerRungsRunConcurrently(t *testing.T) {
	p := &slowProber{
		scriptedProber: scriptedProber{link: Step{Status: StatusOK}, lease: Step{Status: StatusOK}, leaseGw: "192.168.1.1"},
		rungDelay:      150 * time.Millisecond,
	}
	l := NewLadder(p, "relayer.example.com", wrapper.NewClock(), zap.NewNop())

	start := time.Now()
	got := l.Run(context.Background(), "on-demand")
	elapsed := time.Since(start)

	assert.Equal(t, ClassOnline, got.Class)
	// Six rungs x 150ms serial would be >=900ms; concurrent is ~150ms. The
	// 600ms line keeps the assertion robust on a loaded CI box while still
	// failing decisively if the rungs regress to serial.
	assert.Less(t, elapsed, 600*time.Millisecond,
		"lower rungs must run concurrently (elapsed %v)", elapsed)
}

// blockingLinkProber parks Run inside the link rung until released, counting
// real executions.
type blockingLinkProber struct {
	scriptedProber
	started chan struct{}
	release chan struct{}
	runs    atomic.Int32
}

func (b *blockingLinkProber) Link(ctx context.Context) Step {
	if b.runs.Add(1) == 1 {
		close(b.started)
	}
	<-b.release
	return Step{Status: StatusFail}
}

// TestLadderSharesConcurrentRuns: a second caller arriving while a pass is in
// flight must JOIN it (one execution, same result) rather than queueing a
// duplicate probe burst — the on-demand-during-automatic-run collision.
func TestLadderSharesConcurrentRuns(t *testing.T) {
	p := &blockingLinkProber{started: make(chan struct{}), release: make(chan struct{})}
	l := NewLadder(p, "relayer.example.com", wrapper.NewClock(), zap.NewNop())

	results := make(chan *LadderResult, 2)
	go func() { results <- l.Run(context.Background(), "failure-edge") }()
	<-p.started
	go func() { results <- l.Run(context.Background(), "on-demand") }()
	// Give the joiner a beat to reach the singleflight, then release the run.
	time.Sleep(20 * time.Millisecond)
	close(p.release)

	a, b := <-results, <-results
	assert.Same(t, a, b, "concurrent callers must share one run")
	assert.EqualValues(t, 1, p.runs.Load(), "exactly one real execution")
	assert.Equal(t, "failure-edge", a.Trigger, "the run keeps its starter's trigger")
}

// TestLadderRunSkipsGatewayWithoutLeaseGateway: a lease with no gateway must
// mark the rung skip (feeding the unknown-not-wan-down pin above).
func TestLadderRunSkipsGatewayWithoutLeaseGateway(t *testing.T) {
	p := &scriptedProber{link: Step{Status: StatusOK}, lease: Step{Status: StatusOK}}
	l := NewLadder(p, "relayer.example.com", wrapper.NewClock(), zap.NewNop())

	got := l.Run(context.Background(), "on-demand")

	assert.Equal(t, StatusSkip, got.Gateway.Status)
	assert.NotContains(t, p.recorded(), "gw:")
}
