package mediahealth

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

const (
	txA  = "EwKI3znILCW86MZiIwji_kxrFG9Rdp-2arLMpxjyojs"
	cidA = "QmPChd2hVbrJ6bfo3WBcTW4iZnpHm8TEzWkLHmLpXhF68A"
	cidB = "bafybeigdyrzt5sfp7udm7hu76uh7y26nf3efuylqabf3oclgtqy55fbzdi"
)

func TestGateways_Ref(t *testing.T) {
	g := NewGateways(nil, nil)
	cases := []struct {
		url  string
		ok   bool
		key  string
		rest string
	}{
		{"https://arweave.net/" + txA, true, "ar:" + txA, ""},
		{"https://arweave.net/" + txA + "/index.html?x=1", true, "ar:" + txA, "/index.html"},
		{"https://ar-io.dev/" + txA, true, "ar:" + txA, ""},
		{"https://ipfs.filebase.io/ipfs/" + cidA, true, "ipfs:" + cidA, ""},
		{"https://ipfs.pixura.io/ipfs/" + cidB + "/frames/1.png", true, "ipfs:" + cidB, "/frames/1.png"},
		{"https://ipfs.io/ipfs/" + cidA + "/", true, "ipfs:" + cidA, "/"},
		{"https://arweave.net/not-a-tx", false, "", ""},
		{"https://example.com/" + txA, false, "", ""},
		{"https://ipfs.io/ipns/example.com/x.png", false, "", ""},
		{"https://ipfs.io/ipfs/notacid", false, "", ""},
		{"data:,x", false, "", ""},
	}
	for _, tc := range cases {
		ref, ok := g.Ref(tc.url)
		assert.Equal(t, tc.ok, ok, tc.url)
		if tc.ok {
			assert.Equal(t, tc.key, ref.Key(), tc.url)
			assert.Equal(t, tc.rest, ref.Rest, tc.url)
		}
	}
	ref, _ := g.Ref("https://ipfs.pixura.io/ipfs/" + cidB + "/frames/1.png?v=2")
	assert.Equal(t, "https://gateway.pinata.cloud/ipfs/"+cidB+"/frames/1.png?v=2", ref.URLOn("https://gateway.pinata.cloud"))
	ref, _ = g.Ref("https://arweave.net/" + txA + "/index.html")
	assert.Equal(t, "https://ar-io.dev/"+txA+"/index.html", ref.URLOn("https://ar-io.dev"))
}

func TestGateways_ListsAndCandidates(t *testing.T) {
	g := NewGateways([]string{"https://ipfs.filebase.io/ipfs/", "http://insecure.example", "nonsense", "https://gateway.pinata.cloud", "https://ipfs.filebase.io"}, nil)
	assert.Equal(t, []string{"https://ipfs.filebase.io", "https://gateway.pinata.cloud"}, g.lists[NetworkIPFS], "https origins only, deduplicated, paths dropped")
	ref, _ := g.Ref("https://ipfs.filebase.io/ipfs/" + cidA)
	assert.Equal(t, []string{"https://gateway.pinata.cloud"}, g.candidates(ref, "ipfs.filebase.io"), "the gateway already tried is skipped")
	ref, _ = g.Ref("https://ipfs.pixura.io/ipfs/" + cidA)
	assert.Equal(t, []string{"https://ipfs.filebase.io", "https://gateway.pinata.cloud"}, g.candidates(ref, "ipfs.pixura.io"))
	empty := NewGateways([]string{"bad"}, []string{""})
	assert.Equal(t, DefaultIPFSGateways, empty.lists[NetworkIPFS], "an unusable list falls back to the default")
	assert.Equal(t, DefaultArweaveGateways, empty.lists[NetworkArweave])
}

// newGatewayProber configures an Arweave fallback (ar-io.dev) so the
// Arweave path is exercised; the shipped default is arweave.net alone.
func newGatewayProber(t *testing.T, f Fetcher, override bool) *Prober {
	t.Helper()
	p := newPassProber(t, f, &testClock{t: time.Now()})
	p.Probe.OverrideActive = override
	p.Probe.Gateways = NewGateways(nil, []string{"https://arweave.net", "https://ar-io.dev"})
	p.Online = func() bool { return true }
	return p
}

// served is a small valid answer a fallback gateway returns.
func mp4(status int) scriptedAnswer {
	return ok(status, "image/png", minimalPNG(1, 1), -1)
}

// The office FF1 fixture, both networks: the source's own gateway answers
// 404, the next one serves it. With the kiosk override active the work is
// ok and loads through that gateway.
func TestGateways_FallbackServesAndIsRecorded(t *testing.T) {
	cases := []struct{ src, alt string }{
		{"https://arweave.net/" + txA, "https://ar-io.dev/" + txA},
		{"https://ipfs.filebase.io/ipfs/" + cidA, "https://gateway.pinata.cloud/ipfs/" + cidA},
	}
	for _, tc := range cases {
		t.Run(tc.src, func(t *testing.T) {
			f := newTableFetcher()
			f.answers[tc.src] = ok(404, "text/html", []byte("404 - Page cannot be found"), -1)
			f.answers[tc.alt] = mp4(200)
			p := newGatewayProber(t, f, true)

			res := p.ProbeAndRecord(context.Background(), tc.src)
			assert.Equal(t, VerdictAlive, res.Verdict)
			rec, err := p.Store.Get(tc.src)
			require.NoError(t, err)
			assert.Equal(t, StateOK, rec.State)
			assert.NotEmpty(t, rec.ServedBy)
			assert.Empty(t, rec.Issue)

			target, ok := p.Store.GatewayOverride(tc.src, "")
			require.True(t, ok, "the kiosk loads it through the gateway that served it")
			assert.Equal(t, tc.alt, target)
			target, ok = p.Store.GatewayOverride(tc.src+"/assets/app.js?v=1", "")
			require.True(t, ok, "and the work's own files follow")
			assert.Equal(t, tc.alt+"/assets/app.js?v=1", target)
			_, ok = p.Store.GatewayOverride(tc.alt, "")
			assert.False(t, ok, "a request already on the recorded gateway is left alone")

			// It heals: the source's own gateway serves again.
			f.answers[tc.src] = mp4(200)
			p.ProbeAndRecord(context.Background(), tc.src)
			rec, err = p.Store.Get(tc.src)
			require.NoError(t, err)
			assert.Empty(t, rec.ServedBy)
			_, ok = p.Store.GatewayOverride(tc.src, "")
			assert.False(t, ok, "the kiosk goes back to the source's own gateway")
		})
	}
}

// Without the kiosk interceptor the wall would still load the failing
// gateway: the verdict must not claim otherwise.
func TestGateways_NoOverrideMeansGatewayFailure(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "text/html", nil, -1)
	f.answers["https://ar-io.dev/"+txA] = mp4(200)
	p := newGatewayProber(t, f, false)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictInconclusive, res.Verdict)
	assert.True(t, res.GatewayFailed)
	rec, err := p.Store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, IssueServerError, rec.Issue)
	assert.Empty(t, rec.ServedBy)
	_, ok := p.Store.GatewayOverride(src, "")
	assert.False(t, ok)
}

func TestGateways_EveryGatewayMissingIsBroken(t *testing.T) {
	f := newTableFetcher()
	src := "https://ipfs.pixura.io/ipfs/" + cidA
	f.answers[src] = ok(404, "", nil, -1)
	f.answers["https://ipfs.filebase.io/ipfs/"+cidA] = ok(404, "", nil, -1)
	f.answers["https://gateway.pinata.cloud/ipfs/"+cidA] = ok(410, "", nil, -1)
	p := newGatewayProber(t, f, true)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictDead, res.Verdict, "confirmed by every configured gateway")
	assert.Contains(t, res.Detail, "also answered HTTP 410")
}

func TestGateways_ContentRuleOnTheFallbackStands(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "", nil, -1)
	f.answers["https://ar-io.dev/"+txA] = ok(200, "video/mp4", []byte("<html><body>not a video</body></html>"), -1)
	p := newGatewayProber(t, f, true)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictDead, res.Verdict, "a content verdict on what the other gateway served is about the content")
	assert.Equal(t, ReasonTypeMismatch, res.Reason)
}

func TestGateways_StopsAtTheFirstThatServes(t *testing.T) {
	f := newTableFetcher()
	src := "https://ipfs.pixura.io/ipfs/" + cidA
	f.answers[src] = ok(404, "", nil, -1)
	f.answers["https://ipfs.filebase.io/ipfs/"+cidA] = mp4(200)
	f.answers["https://gateway.pinata.cloud/ipfs/"+cidA] = mp4(200)
	p := newGatewayProber(t, f, true)
	p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, 1, f.count("https://ipfs.filebase.io/ipfs/"+cidA))
	assert.Zero(t, f.count("https://gateway.pinata.cloud/ipfs/"+cidA), "one proof is enough")
}

func TestGateways_UnconfirmedFeedsTheStreakAndTheCastRecordsNothing(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "", nil, -1)
	f.answers["https://ar-io.dev/"+txA] = ok(502, "", nil, -1)
	p := newGatewayProber(t, f, true)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictInconclusive, res.Verdict, "a 404 nobody confirmed is not a verdict")
	assert.Equal(t, UnreachableServer, res.Unreachable)

	g := newTableFetcher()
	g.answers[src] = ok(404, "", nil, -1)
	g.answers["https://ar-io.dev/"+txA] = fail(FetchErrTimeout)
	q := newGatewayProber(t, g, true)
	cast := q.Preflight(context.Background(), []string{src})
	assert.Equal(t, VerdictInconclusive, cast[0].Verdict)
	rec, err := q.Store.Get(src)
	require.NoError(t, err)
	assert.Nil(t, rec, "the cast's deadline ran out during the confirmation: nothing recorded")
}

func TestGateways_OtherHostsKeepTheirSingleRequest(t *testing.T) {
	f := newTableFetcher()
	src := "https://raw.example/missing.png"
	f.answers[src] = ok(404, "", nil, -1)
	p := newGatewayProber(t, f, true)
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictDead, res.Verdict)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.calls, 1)
}

// The index survives a restart: the kiosk keeps loading through the
// fallback gateway after controld restarts.
func TestGateways_OverrideIndexSurvivesReopen(t *testing.T) {
	clock := &testClock{t: time.Now()}
	path := filepath.Join(t.TempDir(), "mh", "store.db")
	s, err := Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	src := "https://arweave.net/" + txA
	_, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 206, ServedBy: "https://ar-io.dev"})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	s, err = Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	target, ok := s.GatewayOverride(src, "")
	require.True(t, ok)
	assert.Equal(t, "https://ar-io.dev/"+txA, target)

	_, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	_, ok = s.GatewayOverride(src, "")
	assert.False(t, ok, "confirmed missing clears the override")
}

// A request loaded through the fallback gateway reports that gateway's
// URL to the observer: it still belongs to the wall source.
func TestGateways_ObserverMatchesAnOverriddenRequest(t *testing.T) {
	f := newPlayFeed(t, true)
	f.src = "https://arweave.net/" + txA
	f.emit()
	f.s.ObserveNavigation(Navigation{URL: "https://ar-io.dev/" + txA + "?display_mode=crop", Status: 404})
	rec := f.record()
	require.NotNil(t, rec)
	assert.Equal(t, 1, rec.Plays.Fail)
}

func TestGateways_ReportShowsTheServingHost(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	src := "https://arweave.net/" + txA
	_, err := s.Store().ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 206, ServedBy: "https://ar-io.dev"})
	require.NoError(t, err)
	rows, err := s.Report(items(src), nil)
	require.NoError(t, err)
	assert.Equal(t, "ar-io.dev", rows[0].ServedBy, "a host, never a URL")
}

// F25 (PR #438 review): two sources naming one CID through different
// gateways keep independent overrides — one recovering leaves the other
// on its fallback.
func TestGateways_OverrideIsScopedToTheSourcesGateway(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	a := "https://ipfs.pixura.io/ipfs/" + cidA
	b := "https://ipfs.example.org/ipfs/" + cidA
	for _, src := range []string{a, b} {
		_, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, ServedBy: "https://ipfs.filebase.io"})
		require.NoError(t, err)
	}
	// a's own gateway serves again.
	_, err := s.ApplyProbe(a, ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)

	_, ok := s.GatewayOverride(a, "")
	assert.False(t, ok, "a is back on its own gateway")
	target, ok := s.GatewayOverride(b, "")
	require.True(t, ok, "b still needs its fallback")
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/"+cidA, target)
	target, ok = s.GatewayOverride(b+"/frames/2.png", "")
	require.True(t, ok, "and b's files follow it")
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/"+cidA+"/frames/2.png", target)

}

// F25, round 2: two files under one CID on the SAME gateway are probed
// independently; the route stays while either still needs it.
func TestGateways_RouteStaysWhileAnySourceNeedsIt(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	missing := "https://ipfs.pixura.io/ipfs/" + cidA + "/missing.html"
	present := "https://ipfs.pixura.io/ipfs/" + cidA + "/present.html"
	for _, src := range []string{missing, present} {
		_, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, ServedBy: "https://ipfs.filebase.io"})
		require.NoError(t, err)
	}
	_, err := s.ApplyProbe(present, ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	target, ok := s.GatewayOverride(missing, "")
	require.True(t, ok, "missing.html still needs the fallback")
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/"+cidA+"/missing.html", target)
	_, ok = s.GatewayOverride("https://ipfs.pixura.io/ipfs/"+cidA+"/assets/app.js", "")
	assert.True(t, ok, "the work's own files under the CID still follow the route")

	_, err = s.ApplyProbe(missing, ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	_, ok = s.GatewayOverride(missing, "")
	assert.False(t, ok, "the route clears once no source on it needs it")
}

// The per-source index survives a restart too, including the last-source rule.
func TestGateways_RouteSourcesSurviveReopen(t *testing.T) {
	clock := &testClock{t: time.Now()}
	path := filepath.Join(t.TempDir(), "mh", "store.db")
	st, err := Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	a := "https://ipfs.pixura.io/ipfs/" + cidA + "/a.html"
	b := "https://ipfs.pixura.io/ipfs/" + cidA + "/b.html"
	for _, src := range []string{a, b} {
		_, err = st.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, ServedBy: "https://ipfs.filebase.io"})
		require.NoError(t, err)
	}
	require.NoError(t, st.Close())
	st, err = Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	_, err = st.ApplyProbe(a, ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	_, ok := st.GatewayOverride(b, "")
	assert.True(t, ok, "b's entry was reloaded and still holds the route")
}

// An index written in any older layout is rebuilt from the records at
// Open, so a deploy never leaves a record saying "served by" with nothing
// for the kiosk to look up (office FF1, 2026-11-06).
func TestGateways_IndexIsRebuiltFromRecordsAcrossLayouts(t *testing.T) {
	clock := &testClock{t: time.Now()}
	path := filepath.Join(t.TempDir(), "mh", "store.db")
	st, err := Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	src := "https://arweave.net/" + txA
	_, err = st.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, ServedBy: "https://ar-io.dev"})
	require.NoError(t, err)
	// Simulate an older build's index: a route-only key, and no layout stamp.
	require.NoError(t, st.db.Update(func(tx *bolt.Tx) error {
		ref, _ := st.gateways.Ref(src)
		require.NoError(t, tx.DeleteBucket(bucketServedBy))
		b, err := tx.CreateBucket(bucketServedBy)
		require.NoError(t, err)
		require.NoError(t, b.Put([]byte(ref.RouteKey()), []byte("https://ar-io.dev")))
		return tx.Bucket(bucketMeta).Delete(metaServedByIndexKey)
	}))
	require.NoError(t, st.Close())

	st, err = Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	target, ok := st.GatewayOverride(src, "")
	require.True(t, ok, "the override was rebuilt from the record")
	assert.Equal(t, "https://ar-io.dev/"+txA, target)
}

// shapeFetcher answers a URL differently for the probe's ranged request
// and for a whole-file (unranged) one, and keeps every request's shape.
type shapeFetcher struct {
	*tableFetcher
	whole map[string]scriptedAnswer
	mu    sync.Mutex
	reqs  []FetchRequest
}

func (f *shapeFetcher) Fetch(ctx context.Context, req FetchRequest) (*FetchResponse, error) {
	f.mu.Lock()
	f.reqs = append(f.reqs, req)
	a, isWhole := f.whole[req.URL]
	f.mu.Unlock()
	if req.RangeBytes == 0 && isWhole {
		f.tableFetcher.mu.Lock()
		f.calls[req.URL]++
		f.tableFetcher.mu.Unlock()
		return a.resp, a.err
	}
	return f.tableFetcher.Fetch(ctx, req)
}

// A fallback gateway is confirmed with the request the kiosk makes — no
// Range — because ar-io.dev served a 32 KiB range of a 195 MB video and
// answered the whole-file load with 402 Payment Required; "served" on the
// ranged read put every item on the wall through a gateway that would not
// play it (office FF1, 2026-11-06).
func TestGateways_ConfirmationIsTheKiosksRequestShape(t *testing.T) {
	src := "https://ipfs.pixura.io/ipfs/" + cidA
	alt := "https://ipfs.filebase.io/ipfs/" + cidA
	alt2 := "https://gateway.pinata.cloud/ipfs/" + cidA
	f := &shapeFetcher{tableFetcher: newTableFetcher(), whole: map[string]scriptedAnswer{}}
	f.answers[src] = ok(404, "", nil, -1)
	f.answers[alt] = ok(206, "video/mp4", minimalPNG(1, 1), -1, "Content-Range", "bytes 0-32767/195254037")
	f.answers[alt2] = ok(206, "video/mp4", minimalPNG(1, 1), -1, "Content-Range", "bytes 0-32767/195254037")
	f.whole[alt] = ok(402, "application/json", []byte(`{"x402Version":1}`), -1)
	f.whole[alt2] = ok(402, "application/json", []byte(`{"x402Version":1}`), -1)
	p := newGatewayProber(t, f, true)

	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictInconclusive, res.Verdict, "a gateway that will not serve the whole file did not confirm anything")
	assert.Equal(t, UnreachableServer, res.Unreachable)
	assert.Empty(t, res.ServedBy)
	assert.False(t, res.GatewayFailed)
	_, routed := p.Store.GatewayOverride(src, "")
	assert.False(t, routed, "the kiosk is never routed through it")
	rec, err := p.Store.Get(src)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, IssueServerError, rec.Issue)
	assert.NotEqual(t, StateBroken, rec.State)

	f.mu.Lock()
	reqs := append([]FetchRequest(nil), f.reqs...)
	f.mu.Unlock()
	for _, r := range reqs {
		if r.URL == src {
			assert.Equal(t, DefaultProbeMaxBytes, r.RangeBytes, "the probe itself stays ranged")
			continue
		}
		assert.Zero(t, r.RangeBytes, "the confirmation is unranged: %s", r.URL)
		assert.Equal(t, DefaultProbeMaxBytes, r.MaxBodyBytes, "and reads only the window")
	}

	// The same gateway serving the whole file (the fetcher reads a full
	// window of it) is a confirmation.
	window := append(minimalPNG(1, 1), make([]byte, DefaultProbeMaxBytes)...)[:DefaultProbeMaxBytes]
	f.whole[alt] = ok(200, "image/png", window, 195254037)
	res = p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictAlive, res.Verdict)
	assert.Equal(t, "https://ipfs.filebase.io", res.ServedBy)
}

// The shipped Arweave list is arweave.net alone: a 404 there has no other
// gateway to confirm it and stays the gateway's problem — never broken
// on one gateway's word (7 of 60 healthy works read 404 from arweave.net
// in the office fixture).
func TestGateways_NoOtherGatewayLeavesThe404Unconfirmed(t *testing.T) {
	f := newTableFetcher()
	src := "https://arweave.net/" + txA
	f.answers[src] = ok(404, "text/html", nil, -1)
	p := newPassProber(t, f, &testClock{t: time.Now()})
	p.Probe.OverrideActive = true
	p.Online = func() bool { return true }
	res := p.ProbeAndRecord(context.Background(), src)
	assert.Equal(t, VerdictInconclusive, res.Verdict)
	assert.Equal(t, UnreachableServer, res.Unreachable)
	assert.Contains(t, res.Detail, "no other gateway configured")
	rec, err := p.Store.Get(src)
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, IssueServerError, rec.Issue)
	assert.NotEqual(t, StateBroken, rec.State)
	f.mu.Lock()
	defer f.mu.Unlock()
	assert.Len(t, f.calls, 1, "no candidate, no extra request")
}

// A broken conclusion from ANY layer drops the route: a play-layer verdict
// over a work "served by" a fallback left the kiosk loading the gateway the
// verdict had just proven does not play it.
func TestGateways_BrokenPlayVerdictDropsTheRoute(t *testing.T) {
	f := newPlayFeed(t, true)
	f.src = "https://ipfs.pixura.io/ipfs/" + cidA
	_, err := f.s.Store().ApplyProbe(f.src, ProbeResult{Verdict: VerdictAlive, Status: 200, WireURL: f.src, ServedBy: "https://ipfs.filebase.io"})
	require.NoError(t, err)
	_, routed := f.s.Store().GatewayOverride(f.src, "")
	require.True(t, routed)

	f.emit(infuraPlay()...)
	_, routed = f.s.Store().GatewayOverride(f.src, "")
	assert.True(t, routed, "one failed play is recorded only")
	f.emit(infuraPlay()...)
	rec := f.record()
	require.Equal(t, StateBroken, rec.State)
	assert.Equal(t, LayerPlay, rec.Layer)
	assert.Empty(t, rec.ServedBy)
	_, routed = f.s.Store().GatewayOverride(f.src, "")
	assert.False(t, routed, "the kiosk goes back to the source's own gateway")
}

// F25, round 3: two files under one CID can need DIFFERENT fallbacks (the
// first gateway serves a.html but not b.html). Each source is loaded
// through the gateway that served IT, and a work's own files follow the
// document that embeds them (the Referer), never the route's other source.
func TestGateways_EachSourceLoadsThroughTheGatewayThatServedIt(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	a := "https://ipfs.pixura.io/ipfs/" + cidA + "/a.html"
	b := "https://ipfs.pixura.io/ipfs/" + cidA + "/b.html"
	_, err := s.ApplyProbe(a, ProbeResult{Verdict: VerdictAlive, Status: 200, ServedBy: "https://ipfs.filebase.io"})
	require.NoError(t, err)
	_, err = s.ApplyProbe(b, ProbeResult{Verdict: VerdictAlive, Status: 200, ServedBy: "https://gateway.pinata.cloud"})
	require.NoError(t, err)

	target, ok := s.GatewayOverride(a, "")
	require.True(t, ok)
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/"+cidA+"/a.html", target)
	target, ok = s.GatewayOverride(b, "")
	require.True(t, ok)
	assert.Equal(t, "https://gateway.pinata.cloud/ipfs/"+cidA+"/b.html", target, "b goes to the gateway that served b")
	target, ok = s.GatewayOverride(b+"?display_mode=crop", "")
	require.True(t, ok)
	assert.Equal(t, "https://gateway.pinata.cloud/ipfs/"+cidA+"/b.html?display_mode=crop", target, "the player's own parameters do not hide the source")

	asset := "https://ipfs.pixura.io/ipfs/" + cidA + "/assets/app.js"
	target, ok = s.GatewayOverride(asset, b)
	require.True(t, ok)
	assert.Equal(t, "https://gateway.pinata.cloud/ipfs/"+cidA+"/assets/app.js", target, "b's own file follows b")
	target, ok = s.GatewayOverride(asset, a+"?display_mode=crop")
	require.True(t, ok)
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/"+cidA+"/assets/app.js", target, "a's own file follows a")
	target, ok = s.GatewayOverride(asset, "")
	require.True(t, ok)
	assert.Equal(t, "https://gateway.pinata.cloud/ipfs/"+cidA+"/assets/app.js", target, "nothing ties it to a source: the stable best-effort choice")
}
