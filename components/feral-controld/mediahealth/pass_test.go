package mediahealth

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// tableFetcher answers by URL, counting requests per URL and in flight, so
// the pass's dedup, budget and pool can be asserted.
type tableFetcher struct {
	mu       sync.Mutex
	answers  map[string]scriptedAnswer
	calls    map[string]int
	inflight int32
	peak     int32
	block    chan struct{}
}

func newTableFetcher() *tableFetcher {
	return &tableFetcher{answers: map[string]scriptedAnswer{}, calls: map[string]int{}}
}

func (f *tableFetcher) Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	cur := atomic.AddInt32(&f.inflight, 1)
	for {
		p := atomic.LoadInt32(&f.peak)
		if cur <= p || atomic.CompareAndSwapInt32(&f.peak, p, cur) {
			break
		}
	}
	defer atomic.AddInt32(&f.inflight, -1)
	f.mu.Lock()
	f.calls[req.URL]++
	a, ok := f.answers[req.URL]
	f.mu.Unlock()
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, &FetchError{Kind: FetchErrTimeout, Err: ctx.Err()}
		}
	}
	if !ok {
		return nil, &FetchError{Kind: FetchErrTransport, Err: fmt.Errorf("no answer for %s", req.URL)}
	}
	return a.resp, a.err
}

func (f *tableFetcher) count(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[url]
}

func newPassProber(t *testing.T, f Fetcher, clock *testClock) *Prober {
	t.Helper()
	store := openTestStore(t, clock)
	return &Prober{
		Probe: &Probe{Fetcher: f, Validator: NewContentValidator(DefaultProbeMaxBytes, nil)},
		Store: store,
		Now:   clock.Now,
	}
}

func TestPreflight_ProbesEverythingAndRecords(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/alive"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://o/dead"] = ok(404, "", nil, -1)
	f.answers["https://o/flaky"] = ok(503, "", nil, -1)
	p := newPassProber(t, f, clock)

	results := p.Preflight(context.Background(), []string{
		"https://o/alive", "https://o/dead", "https://o/flaky", "data:image/png;base64,iVBORw0KGgo=", "https://o/dead#dup",
	})
	require.Len(t, results, 5)
	assert.Equal(t, VerdictAlive, results[0].Verdict)
	assert.Equal(t, VerdictDead, results[1].Verdict)
	assert.Equal(t, VerdictInconclusive, results[2].Verdict)
	assert.Equal(t, VerdictInline, results[3].Verdict)
	assert.Equal(t, VerdictDead, results[4].Verdict, "a fragment-only duplicate shares the verdict")
	assert.Equal(t, 1, f.count("https://o/dead"), "wire-identical sources are probed once")
	assert.Equal(t, 1, f.count("https://o/alive"), "no early exit: every source is probed")

	rec, err := p.Store.Get("https://o/dead")
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	rec, err = p.Store.Get("https://o/flaky")
	require.NoError(t, err)
	assert.Empty(t, rec.State)
	assert.Equal(t, "HTTP 503", rec.LastError)
}

func TestPreflight_ConclusiveStoreAnswersWithoutARequest(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/dead"] = ok(404, "", nil, -1)
	f.answers["https://o/alive"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p := newPassProber(t, f, clock)

	first := p.Preflight(context.Background(), []string{"https://o/dead", "https://o/alive"})
	assert.False(t, first[0].FromStore)
	second := p.Preflight(context.Background(), []string{"https://o/dead", "https://o/alive"})
	assert.True(t, second[0].FromStore)
	assert.Equal(t, VerdictDead, second[0].Verdict)
	assert.Equal(t, 404, second[0].Status)
	assert.True(t, second[1].FromStore)
	assert.Equal(t, VerdictAlive, second[1].Verdict)
	assert.Equal(t, 1, f.count("https://o/dead"), "a conclusive record costs no request")

	clock.Advance(RecheckBroken + time.Second)
	third := p.Preflight(context.Background(), []string{"https://o/dead"})
	assert.False(t, third[0].FromStore, "past the recheck window the source is probed again")
	assert.Equal(t, 2, f.count("https://o/dead"))
}

func TestPreflight_BudgetAndDataURIsOutsideIt(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	sources := make([]string, 0, 6)
	for i := 0; i < 3; i++ {
		u := fmt.Sprintf("https://o/%d", i)
		f.answers[u] = ok(404, "", nil, -1)
		sources = append(sources, u)
	}
	sources = append(sources, "data:,inline", "data:nocomma", "https://o/0?v=2")
	p := newPassProber(t, f, clock)
	p.MaxAttempts = 2

	results := p.Preflight(context.Background(), sources)
	assert.Equal(t, VerdictDead, results[0].Verdict)
	assert.Equal(t, VerdictDead, results[1].Verdict)
	assert.Equal(t, VerdictInconclusive, results[2].Verdict, "past the budget: never probed, never dead")
	assert.ErrorIs(t, results[2].Err, errBudgetExhausted)
	assert.Equal(t, VerdictInline, results[3].Verdict, "data: URIs are settled outside the budget")
	assert.Equal(t, VerdictDead, results[4].Verdict)
	assert.Equal(t, VerdictInconclusive, results[5].Verdict, "a wire-distinct variant past the budget is inconclusive")
	assert.Equal(t, 0, f.count("https://o/2"))
}

func TestPreflight_PhaseCeilingLeavesStragglersInconclusive(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.block = make(chan struct{})
	for i := 0; i < 4; i++ {
		f.answers[fmt.Sprintf("https://o/%d", i)] = ok(200, "image/png", minimalPNG(1, 1), -1)
	}
	p := newPassProber(t, f, clock)
	p.PhaseCeiling = 50 * time.Millisecond
	p.Concurrency = 2

	start := time.Now()
	results := p.Preflight(context.Background(), []string{"https://o/0", "https://o/1", "https://o/2", "https://o/3"})
	assert.Less(t, time.Since(start), 2*time.Second)
	for i, r := range results {
		assert.Equal(t, VerdictInconclusive, r.Verdict, "item %d", i)
		assert.Error(t, r.Err)
	}
	assert.LessOrEqual(t, atomic.LoadInt32(&f.peak), int32(2), "the worker pool bounds concurrency")
	close(f.block)
}

func TestPreflight_EmptyAndNilStore(t *testing.T) {
	p := &Prober{Probe: &Probe{Fetcher: newTableFetcher()}}
	assert.Empty(t, p.Preflight(context.Background(), nil))
	f := newTableFetcher()
	f.answers["https://o/a"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p = &Prober{Probe: &Probe{Fetcher: f}}
	res := p.Preflight(context.Background(), []string{"https://o/a"})
	assert.Equal(t, VerdictAlive, res[0].Verdict, "no store: the pass still answers admission")
}

func TestProbeAndRecord(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/a"] = ok(http.StatusGone, "", nil, -1)
	p := newPassProber(t, f, clock)
	res := p.ProbeAndRecord(context.Background(), "https://o/a")
	assert.Equal(t, VerdictDead, res.Verdict)
	rec, err := p.Store.Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
}
