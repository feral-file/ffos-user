package mediahealth

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestLookahead(t *testing.T, f Fetcher, clock *testClock) *Lookahead {
	t.Helper()
	p := newPassProber(t, f, clock)
	return &Lookahead{Prober: p, Store: p.Store, Now: clock.Now}
}

func TestLookahead_ProbesUpcomingItemsInPlayOrder(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	for _, u := range []string{"https://o/0", "https://o/1", "https://o/2", "https://o/3", "https://o/4"} {
		f.answers[u] = ok(200, "image/png", minimalPNG(1, 1), -1)
	}
	l := newTestLookahead(t, f, clock)
	l.Observe([]string{"https://o/0", "https://o/1", "https://o/2", "https://o/3", "https://o/4"}, 3)

	// One walk from index 3: the items after it in play order, wrapping,
	// then the current item last — one request per step.
	for _, want := range []string{"https://o/4", "https://o/0", "https://o/1", "https://o/2", "https://o/3"} {
		assert.True(t, l.step(context.Background(), nil), want)
		assert.Equal(t, 1, f.count(want), want)
	}
	assert.False(t, l.step(context.Background(), nil), "the wall is covered; nothing is due in the background")
	for _, u := range []string{"https://o/0", "https://o/1", "https://o/2", "https://o/3", "https://o/4"} {
		assert.Equal(t, 1, f.count(u), "each item once: %s", u)
	}
}

func TestLookahead_SkipsRecordsThatAreNotDue(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/0"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://o/1"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	l := newTestLookahead(t, f, clock)
	l.Observe([]string{"https://o/0", "https://o/1"}, 0)
	assert.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/1"))
	assert.True(t, l.step(context.Background(), nil), "then the current item")
	assert.Equal(t, 1, f.count("https://o/0"))
	assert.False(t, l.step(context.Background(), nil), "an ok record inside its window is not re-probed")
	clock.Advance(RecheckOK + time.Second)
	assert.True(t, l.step(context.Background(), nil), "past its recheck it is due again")
	assert.Equal(t, 2, f.count("https://o/1"))
}

func TestLookahead_BackgroundSweepIsTokenBucketed(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	l := newTestLookahead(t, f, clock)
	l.SweepPerHour = 2
	// Three due broken records, none on the wall.
	for _, u := range []string{"https://o/a", "https://o/b", "https://o/c"} {
		f.answers[u] = ok(404, "", nil, -1)
		_, err := l.Store.ApplyProbe(u, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
		require.NoError(t, err)
	}
	clock.Advance(RecheckBroken + time.Second)
	l.Observe(nil, 0)

	assert.True(t, l.step(context.Background(), nil))
	assert.True(t, l.step(context.Background(), nil))
	assert.False(t, l.step(context.Background(), nil), "two per hour: the third waits")
	clock.Advance(31 * time.Minute)
	assert.True(t, l.step(context.Background(), nil), "tokens refill with time")
}

func TestLookahead_AdmitGateAndIgnoredInputs(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/0"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://o/1"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	l := newTestLookahead(t, f, clock)
	admit := false
	l.Admit = func() bool { return admit }
	l.Observe([]string{"https://o/0", "https://o/1", "data:,x"}, 0)
	assert.False(t, l.step(context.Background(), nil), "refused by the resource gate")
	admit = true
	assert.True(t, l.step(context.Background(), nil))
	assert.True(t, l.step(context.Background(), nil), "the current item")
	assert.False(t, l.step(context.Background(), nil), "data: items are never probed ahead")

	// No size cap: a wall holds whatever the playlist resolved to.
	big := make([]string, 5000)
	for i := range big {
		big[i] = "https://big.example/" + strconv.Itoa(i)
	}
	l.Observe(big, 0)
	l.mu.Lock()
	assert.Len(t, l.sources, 5000, "a large snapshot is retained")
	l.mu.Unlock()
}

func TestLookahead_RunStopsOnContext(t *testing.T) {
	clock := &testClock{t: time.Now()}
	l := newTestLookahead(t, newTableFetcher(), clock)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	l.Observe([]string{"https://o/0"}, 0)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}
