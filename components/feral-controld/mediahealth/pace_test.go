package mediahealth

import (
	"context"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The cast pass probes its first Items sources and leaves the rest to the
// lookahead — unless everything seen so far is dead, in which case it
// keeps going so an all-dead cast is still refused.
func TestPreflight_FirstItemsThenExtendsOnlyWhileAllDead(t *testing.T) {
	wall := func(n int) []string {
		out := make([]string, n)
		for i := range out {
			out[i] = fmt.Sprintf("https://o/%d", i)
		}
		return out
	}
	t.Run("a live opening stops at the first batch", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		f := newTableFetcher()
		sources := wall(10)
		for _, u := range sources {
			f.answers[u] = ok(200, "image/png", minimalPNG(1, 1), -1)
		}
		p := newPassProber(t, f, clock)
		p.Items = 3
		results := p.Preflight(context.Background(), sources)
		for i := 0; i < 3; i++ {
			assert.Equal(t, VerdictAlive, results[i].Verdict, "item %d", i)
			assert.Equal(t, 1, f.count(sources[i]))
		}
		for i := 3; i < 10; i++ {
			assert.Equal(t, VerdictInconclusive, results[i].Verdict, "item %d", i)
			assert.ErrorIs(t, results[i].Err, errBeyondPass)
			assert.Equal(t, 0, f.count(sources[i]), "beyond the pass nothing is dialed")
			rec, err := p.Store.Get(sources[i])
			require.NoError(t, err)
			assert.Nil(t, rec, "beyond the pass nothing is recorded: the source stays unknown")
		}
	})
	t.Run("a dead opening extends until something is not dead", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		f := newTableFetcher()
		sources := wall(10)
		for i, u := range sources {
			if i == 4 {
				f.answers[u] = ok(200, "image/png", minimalPNG(1, 1), -1)
				continue
			}
			f.answers[u] = ok(404, "", nil, -1)
		}
		p := newPassProber(t, f, clock)
		p.Items = 3
		results := p.Preflight(context.Background(), sources)
		for i := 0; i < 6; i++ {
			assert.Equal(t, 1, f.count(sources[i]), "two batches run: item %d", i)
		}
		assert.Equal(t, VerdictAlive, results[4].Verdict)
		for i := 6; i < 10; i++ {
			assert.Equal(t, 0, f.count(sources[i]), "the live item in batch two ends the extension: item %d", i)
			assert.ErrorIs(t, results[i].Err, errBeyondPass)
		}
	})
	t.Run("all dead runs to the end and is rejectable", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		f := newTableFetcher()
		sources := wall(10)
		for _, u := range sources {
			f.answers[u] = ok(404, "", nil, -1)
		}
		p := newPassProber(t, f, clock)
		p.Items = 3
		results := p.Preflight(context.Background(), sources)
		for i := 0; i < 10; i++ {
			assert.Equal(t, VerdictDead, results[i].Verdict, "item %d", i)
			assert.Equal(t, 1, f.count(sources[i]))
		}
	})
	t.Run("past the budget the cast cannot be refused, so nothing extends", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		f := newTableFetcher()
		sources := wall(10)
		for _, u := range sources {
			f.answers[u] = ok(404, "", nil, -1)
		}
		p := newPassProber(t, f, clock)
		p.Items = 3
		p.MaxAttempts = 8
		results := p.Preflight(context.Background(), sources)
		for i := 0; i < 3; i++ {
			assert.Equal(t, VerdictDead, results[i].Verdict, "item %d", i)
		}
		for i := 3; i < 8; i++ {
			assert.ErrorIs(t, results[i].Err, errBeyondPass, "item %d: the budget-exhausted tail admits the cast whatever these say", i)
			assert.Equal(t, 0, f.count(sources[i]))
		}
		assert.ErrorIs(t, results[8].Err, errBudgetExhausted)
	})
	t.Run("a stored alive record stops the extension before it starts", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		f := newTableFetcher()
		sources := wall(6)
		for _, u := range sources {
			f.answers[u] = ok(404, "", nil, -1)
		}
		p := newPassProber(t, f, clock)
		p.Items = 2
		_, err := p.Store.ApplyProbe(sources[5], ProbeResult{Verdict: VerdictAlive, Status: 200})
		require.NoError(t, err)
		results := p.Preflight(context.Background(), sources)
		assert.True(t, results[5].FromStore)
		assert.Equal(t, VerdictAlive, results[5].Verdict)
		assert.Equal(t, 1, f.count(sources[0]))
		assert.Equal(t, 1, f.count(sources[1]))
		assert.Equal(t, 0, f.count(sources[2]), "the cast cannot be all dead; the pass stops after its first batch")
		assert.ErrorIs(t, results[2].Err, errBeyondPass)
	})
	t.Run("an inline item stops the extension too", func(t *testing.T) {
		clock := &testClock{t: time.Now()}
		f := newTableFetcher()
		sources := append(wall(4), "data:,inline")
		for _, u := range sources[:4] {
			f.answers[u] = ok(404, "", nil, -1)
		}
		p := newPassProber(t, f, clock)
		p.Items = 2
		results := p.Preflight(context.Background(), sources)
		assert.Equal(t, 0, f.count(sources[2]))
		assert.ErrorIs(t, results[2].Err, errBeyondPass)
	})
}

func TestPreflight_PerHostInFlightCap(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.block = make(chan struct{})
	sources := make([]string, 6)
	for i := range sources {
		sources[i] = fmt.Sprintf("https://one.example/%d", i)
		f.answers[sources[i]] = ok(200, "image/png", minimalPNG(1, 1), -1)
	}
	p := newPassProber(t, f, clock)
	p.Concurrency = 4
	p.Hosts = &HostLimiter{MaxInFlight: 2, Now: clock.Now}
	// Let the first requests pile up against the cap, then release.
	time.AfterFunc(100*time.Millisecond, func() { close(f.block) })
	results := p.Preflight(context.Background(), sources)
	for i, r := range results {
		assert.Equal(t, VerdictAlive, r.Verdict, "item %d", i)
	}
	assert.LessOrEqual(t, atomic.LoadInt32(&f.peak), int32(2), "one host never sees more than its cap, whatever the pool size")
}

func TestPreflight_CooledHostIsSkippedWithoutARecord(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://limited.example/a"] = ok(429, "", nil, -1, "Retry-After", "30")
	f.answers["https://limited.example/b"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://other.example/c"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p := newPassProber(t, f, clock)
	p.Concurrency = 1
	p.Hosts = &HostLimiter{Now: clock.Now}

	results := p.Preflight(context.Background(), []string{"https://limited.example/a", "https://limited.example/b", "https://other.example/c"})
	assert.Equal(t, VerdictInconclusive, results[0].Verdict)
	assert.Equal(t, 30*time.Second, results[0].RetryAfter)
	rec, err := p.Store.Get("https://limited.example/a")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.WithinDuration(t, clock.t.Add(30*time.Second), rec.RetryAfter, 0, "the record's hold is the origin's Retry-After, not the hour")

	assert.Equal(t, VerdictInconclusive, results[1].Verdict, "a sibling on the cooled host is not dialed")
	assert.Equal(t, 0, f.count("https://limited.example/b"))
	var cooled *hostCooldownError
	assert.ErrorAs(t, results[1].Err, &cooled)
	rec, err = p.Store.Get("https://limited.example/b")
	require.NoError(t, err)
	assert.Nil(t, rec, "a skipped sibling writes no record: unknown, not held")
	assert.Equal(t, VerdictAlive, results[2].Verdict, "another host is unaffected")

	clock.Advance(31 * time.Second)
	again := p.Preflight(context.Background(), []string{"https://limited.example/b"})
	assert.Equal(t, VerdictAlive, again[0].Verdict, "after the cooldown the host is dialed again")
	assert.Equal(t, 1, f.count("https://limited.example/b"))
}

func TestPreflight_CooldownWithoutRetryAfterUsesTheDefault(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://limited.example/a"] = ok(503, "", nil, -1)
	f.answers["https://limited.example/b"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p := newPassProber(t, f, clock)
	p.Concurrency = 1
	p.Hosts = &HostLimiter{Now: clock.Now}

	results := p.Preflight(context.Background(), []string{"https://limited.example/a", "https://limited.example/b"})
	assert.Zero(t, results[0].RetryAfter)
	rec, err := p.Store.Get("https://limited.example/a")
	require.NoError(t, err)
	assert.WithinDuration(t, clock.t.Add(RetryTransient), rec.RetryAfter, 0, "no Retry-After: the record keeps the hour")
	until, cooled := p.Hosts.Cooled("limited.example")
	require.True(t, cooled)
	assert.Equal(t, clock.t.Add(DefaultHostCooldown), until, "the host is cooled for the default minute")
	assert.Equal(t, 0, f.count("https://limited.example/b"))

	// A 403 is transient for the record but says nothing about the host.
	f.answers["https://walled.example/a"] = ok(403, "", nil, -1)
	f.answers["https://walled.example/b"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p.Preflight(context.Background(), []string{"https://walled.example/a", "https://walled.example/b"})
	assert.Equal(t, 1, f.count("https://walled.example/b"), "an auth wall cools nothing")
}

func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		header string
		want   time.Duration
		ok     bool
	}{
		{"", 0, false},
		{"30", 30 * time.Second, true},
		{" 30 ", 30 * time.Second, true},
		{"0", 0, true},
		{"-1", 0, false},
		{"soon", 0, false},
		{now.Add(90 * time.Second).Format(http.TimeFormat), 90 * time.Second, true},
		{now.Add(-time.Minute).Format(http.TimeFormat), 0, true},
	}
	for _, tc := range cases {
		got, ok := parseRetryAfter(tc.header, now)
		assert.Equal(t, tc.ok, ok, tc.header)
		assert.Equal(t, tc.want, got, tc.header)
	}
}

func TestStore_TransientHoldIsBoundedByRetryAfter(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	transient := func(d time.Duration) ProbeResult {
		return ProbeResult{Verdict: VerdictInconclusive, Status: 429, Reason: ReasonHTTPStatus, RetryAfter: d}
	}
	rec, err := s.ApplyProbe("https://o/a", transient(45*time.Second))
	require.NoError(t, err)
	assert.Equal(t, clock.t.Add(45*time.Second), rec.RetryAfter)
	rec, err = s.ApplyProbe("https://o/b", transient(6*time.Hour))
	require.NoError(t, err)
	assert.Equal(t, clock.t.Add(RetryTransient), rec.RetryAfter, "an origin's day-long pause never silences its own check past the hour")
	rec, err = s.ApplyProbe("https://o/c", transient(0))
	require.NoError(t, err)
	assert.Equal(t, clock.t.Add(RetryTransient), rec.RetryAfter)
}

func TestHostLimiter_AcquireWaitsForRelease(t *testing.T) {
	h := &HostLimiter{MaxInFlight: 1}
	require.NoError(t, h.Acquire(context.Background(), "a.example"))
	require.NoError(t, h.Acquire(context.Background(), "b.example"), "another host has its own slots")
	require.NoError(t, h.Acquire(context.Background(), ""), "no host, no limit")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	assert.ErrorIs(t, h.Acquire(ctx, "a.example"), context.DeadlineExceeded, "a full host blocks until the context gives up")

	got := make(chan error, 1)
	go func() { got <- h.Acquire(context.Background(), "a.example") }()
	select {
	case err := <-got:
		t.Fatalf("acquired a full host: %v", err)
	case <-time.After(30 * time.Millisecond):
	}
	h.Release("a.example")
	select {
	case err := <-got:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("a release did not wake the waiter")
	}
}

func TestHostLimiter_CooldownBoundsAndKeepsTheLater(t *testing.T) {
	clock := &testClock{t: time.Now()}
	h := &HostLimiter{Now: clock.Now}
	h.Cooldown("a.example", 0)
	until, ok := h.Cooled("a.example")
	require.True(t, ok)
	assert.Equal(t, clock.t.Add(DefaultHostCooldown), until)
	h.Cooldown("a.example", 10*time.Second)
	until, _ = h.Cooled("a.example")
	assert.Equal(t, clock.t.Add(DefaultHostCooldown), until, "a shorter later pause does not cut the standing one")
	h.Cooldown("a.example", 48*time.Hour)
	until, _ = h.Cooled("a.example")
	assert.Equal(t, clock.t.Add(MaxHostCooldown), until, "capped")
	_, ok = h.Cooled("")
	assert.False(t, ok)
	clock.Advance(MaxHostCooldown)
	_, ok = h.Cooled("a.example")
	assert.False(t, ok, "expired entries are dropped")
}

// The whole wall is walked in play order, so the items the cast pass left
// get their first verdict before playback reaches them; a cooled host is
// stepped over, not held.
func TestLookahead_WalksTheWholeWallForUnprobedSources(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	l := newTestLookahead(t, f, clock)
	l.Prober.Hosts = &HostLimiter{Now: clock.Now}
	wall := make([]string, 10)
	for i := range wall {
		wall[i] = fmt.Sprintf("https://o/%d", i)
		f.answers[wall[i]] = ok(200, "image/png", minimalPNG(1, 1), -1)
	}
	wall[5] = "https://cool.example/5"
	f.answers[wall[5]] = ok(200, "image/png", minimalPNG(1, 1), -1)
	// The pass covered 0..3 (the current item and the window).
	for _, src := range wall[:4] {
		_, err := l.Store.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200})
		require.NoError(t, err)
	}
	l.Prober.Hosts.Cooldown("cool.example", time.Minute)
	l.Observe(wall, 0)

	for _, want := range []string{wall[4], wall[6], wall[7], wall[8], wall[9]} {
		require.True(t, l.step(context.Background(), nil), want)
		assert.Equal(t, 1, f.count(want), want)
	}
	assert.Equal(t, 0, f.count(wall[5]), "a source on a cooled host is stepped over, not held")
	assert.False(t, l.step(context.Background(), nil), "nothing unprobed is left while the host is cool")
	clock.Advance(61 * time.Second)
	require.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count(wall[5]), "once the cooldown ends the skipped source is probed")
	assert.False(t, l.step(context.Background(), nil))
}

func TestLookahead_WalkSkipsACooledHostWithoutHolding(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	l := newTestLookahead(t, f, clock)
	l.Prober.Hosts = &HostLimiter{Now: clock.Now}
	f.answers["https://cool.example/1"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://o/2"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://o/0"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	l.Prober.Hosts.Cooldown("cool.example", time.Minute)
	l.Observe([]string{"https://o/0", "https://cool.example/1", "https://o/2"}, 0)
	require.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/2"), "the next item on a cool host is skipped for the one behind it")
	require.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/0"))
	assert.False(t, l.step(context.Background(), nil))
	clock.Advance(61 * time.Second)
	require.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://cool.example/1"))
}
