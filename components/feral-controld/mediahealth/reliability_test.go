package mediahealth

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// An iframe/object/PDF ready proves the mount, not the work (Chromium
// reports ready for its own error page). It counts the play but writes no
// verdict, heals nothing and leaves the record due for L0.
func TestStore_OpaqueReadyIsNotEvidence(t *testing.T) {
	cases := []struct {
		mediaType string
		evidence  bool
	}{
		{"image", true}, {"video", true}, {"audio", true}, {"model", true},
		{"iframe", false}, {"object", false}, {"pdf", false}, {"", false},
	}
	for _, tc := range cases {
		t.Run("type="+tc.mediaType, func(t *testing.T) {
			clock := &testClock{t: time.Now()}
			s := openTestStore(t, clock)
			rec, err := s.ApplyOutcome(Outcome{Seq: 1, Source: "https://o/a", Kind: OutcomeOK, Stage: StageReady, MediaType: tc.mediaType}, true)
			require.NoError(t, err)
			assert.Equal(t, 1, rec.Plays.OK, "the play is always counted")
			if tc.evidence {
				assert.Equal(t, StateOK, rec.State)
				assert.False(t, rec.Due(clock.t), "a real success holds the record for its ok window")
				return
			}
			assert.Equal(t, State(""), rec.State, "no verdict from an opaque ready")
			assert.True(t, rec.Due(clock.t), "the record stays due, so L0 looks")
		})
	}
}

func TestStore_OpaqueReadyNeverHealsABrokenVerdict(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	_, err := s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	clock.Advance(time.Minute)
	rec, err := s.ApplyOutcome(Outcome{Seq: 1, Source: "https://o/a", Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: clock.t.Format(time.RFC3339Nano)}, true)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "an error page in an iframe is not a heal")
	assert.True(t, rec.Conclusive(clock.t), "the hint keeps its authority")
}

func dnsUnresolved(healthy bool) ProbeResult {
	return ProbeResult{Verdict: VerdictInconclusive, Reason: ReasonTransport, Err: errors.New("lookup: server misbehaving"), DNSUnresolved: true, ResolverHealthy: healthy}
}

// A name that fails to resolve on every counted attempt for a day, while
// other names resolve, is broken (reason dns); it heals on the first
// answer.
func TestStore_DNSStreakConcludesAfterADay(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	src := "https://gone.example/a"
	rec, err := s.ApplyProbe(src, dnsUnresolved(true))
	require.NoError(t, err)
	assert.Equal(t, State(""), rec.State, "one SERVFAIL is weather")
	assert.Equal(t, 1, rec.DNSFailures)
	start := clock.t

	clock.Advance(12 * time.Hour)
	rec, err = s.ApplyProbe(src, dnsUnresolved(true))
	require.NoError(t, err)
	assert.Equal(t, State(""), rec.State, "inside the day: still weather")

	clock.Advance(13 * time.Hour)
	rec, err = s.ApplyProbe(src, dnsUnresolved(false))
	require.NoError(t, err)
	assert.Equal(t, 2, rec.DNSFailures, "a failure while nothing resolved does not count")
	assert.Equal(t, State(""), rec.State)
	require.NotNil(t, rec.DNSFailSince)
	assert.Equal(t, start.UTC(), rec.DNSFailSince.UTC(), "nor does it end the streak")

	clock.Advance(time.Hour)
	rec, err = s.ApplyProbe(src, dnsUnresolved(true))
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "a day of counted failures concludes")
	assert.Equal(t, ReasonDNS, rec.Reason)
	assert.Equal(t, CategorySourceMissing, rec.Category)
	assert.Equal(t, LayerL0, rec.Layer)
	assert.True(t, rec.Conclusive(clock.t))
	assert.True(t, rec.RetryAfter.IsZero(), "a verdict carries no transient hold")

	hints, err := s.Hints([]string{src}, clock.t)
	require.NoError(t, err)
	assert.Contains(t, hints, src, "the concluded name is hinted")

	clock.Advance(RecheckBroken + time.Second)
	rec, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "the zone came back: healed")
	assert.Nil(t, rec.DNSFailSince)
	assert.Zero(t, rec.DNSFailures)
}

func TestStore_DNSStreakNeedsEnoughAttemptsAndEndsOnAnyAnswer(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	src := "https://flaky.example/a"
	_, err := s.ApplyProbe(src, dnsUnresolved(true))
	require.NoError(t, err)
	clock.Advance(30 * time.Hour)
	rec, err := s.ApplyProbe(src, dnsUnresolved(true))
	require.NoError(t, err)
	assert.Equal(t, State(""), rec.State, "two attempts are not enough, however far apart")

	// A timeout that got past DNS (the name resolved) ends the streak.
	rec, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictInconclusive, Reason: ReasonTransport, Err: errors.New("i/o timeout")})
	require.NoError(t, err)
	assert.Nil(t, rec.DNSFailSince)
	assert.Zero(t, rec.DNSFailures)
}

func TestProber_ResolverHealthEvidence(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://up.example/a"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://gone.example/a"] = scriptedAnswer{err: &FetchError{Kind: FetchErrDNSUnresolved, Err: errors.New("server misbehaving")}}
	p := newPassProber(t, f, clock)

	res := p.ProbeAndRecord(context.Background(), "https://gone.example/a")
	assert.True(t, res.DNSUnresolved)
	assert.False(t, res.ResolverHealthy, "offline and nothing resolved yet: does not count")
	rec, err := p.Store.Get("https://gone.example/a")
	require.NoError(t, err)
	assert.Zero(t, rec.DNSFailures)

	p.ProbeAndRecord(context.Background(), "https://up.example/a")
	clock.Advance(2 * time.Hour)
	res = p.ProbeAndRecord(context.Background(), "https://gone.example/a")
	assert.False(t, res.ResolverHealthy, "another name resolved, but too long ago")

	p.ProbeAndRecord(context.Background(), "https://up.example/a")
	res = p.ProbeAndRecord(context.Background(), "https://gone.example/a")
	assert.True(t, res.ResolverHealthy, "another name resolved within the hour")

	clock.Advance(2 * time.Hour)
	p.Online = func() bool { return true }
	res = p.ProbeAndRecord(context.Background(), "https://gone.example/a")
	assert.True(t, res.ResolverHealthy, "the online verdict (itself a DNS-dependent check) is evidence too")
}

// F19 (PR #438 review): a recorded transient answer follows its record's
// retry time — a 429 with Retry-After: 30 is due again after 30 s, not
// after the lookahead's one-hour in-memory hold. The hold stays for
// results that write no record.
func TestLookahead_RecordedTransientFollowsItsRetryAfter(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	l := newTestLookahead(t, f, clock) // Window: 3
	l.Prober.Hosts = &HostLimiter{Now: clock.Now}
	src := "https://limited.example/a"
	f.answers[src] = ok(429, "", nil, -1, "Retry-After", "30")
	l.Observe([]string{"https://o/cur", src}, 0)
	f.answers["https://o/cur"] = ok(200, "image/png", minimalPNG(1, 1), -1)

	require.True(t, l.step(context.Background(), nil))
	require.Equal(t, 1, f.count(src))
	require.True(t, l.step(context.Background(), nil), "then the current item")
	assert.False(t, l.step(context.Background(), nil), "inside the origin's 30 s nothing is due")

	f.answers[src] = ok(200, "image/png", minimalPNG(1, 1), -1)
	clock.Advance(31 * time.Second)
	require.True(t, l.step(context.Background(), nil), "past Retry-After the source is due again")
	assert.Equal(t, 2, f.count(src))
	rec, err := l.Store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "the recovered source gets its fresh verdict")
}

func TestLookahead_UnrecordedRefusalIsStillHeld(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	l := newTestLookahead(t, f, clock)
	src := "https://refused.example/a"
	f.answers[src] = fail(FetchErrGuard)
	l.Observe([]string{"https://o/cur", src}, 0)
	f.answers["https://o/cur"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	require.True(t, l.step(context.Background(), nil))
	require.True(t, l.step(context.Background(), nil))
	clock.Advance(31 * time.Second)
	assert.False(t, l.step(context.Background(), nil), "a refusal writes no record, so the hour hold keeps it from looping")
	assert.Equal(t, 1, f.count(src))
	clock.Advance(probeHold)
	assert.True(t, l.step(context.Background(), nil))
}
