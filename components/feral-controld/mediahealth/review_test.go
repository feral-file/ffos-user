package mediahealth

// Regressions from the PR #438 review (feralfile-bot, round 1): each test
// is one finding, named for it.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// F1: the player numbers entries for the page lifetime, so a cast must not
// reset the cursor; a sequence rollback (page restart) must.
func TestDrainer_CursorSurvivesCastsAndResetsOnRollback(t *testing.T) {
	var d Drainer
	ring := []Outcome{{Seq: 1, Source: "a", Kind: OutcomeFail, Stage: StageLoad}}
	fresh, _, _ := d.Drain(&Outcomes{CastSeq: 1, Entries: ring})
	assert.Len(t, fresh, 1)
	fresh, _, _ = d.Drain(&Outcomes{CastSeq: 2, Entries: ring})
	assert.Empty(t, fresh, "a cast with the same ring re-applies nothing")
	fresh, _, _ = d.Drain(&Outcomes{CastSeq: 2, Entries: append(ring, Outcome{Seq: 2, Source: "b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"})})
	assert.Equal(t, []string{"b"}, sourcesOf(fresh))

	// Page restart with the same castSeq: numbering restarts below the cursor.
	fresh, gap, _ := d.Drain(&Outcomes{CastSeq: 2, Entries: []Outcome{{Seq: 1, Source: "c", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"}}})
	assert.Equal(t, []string{"c"}, sourcesOf(fresh), "a rollback means everything present is new")
	assert.Equal(t, int64(0), gap)

	d.Reset()
	fresh, _, _ = d.Drain(&Outcomes{CastSeq: 3, Entries: []Outcome{{Seq: 1, Source: "c", Kind: OutcomeOK, Stage: StageReady, MediaType: "image", At: "new-page"}}})
	assert.Equal(t, []string{"c"}, sourcesOf(fresh), "an explicit reset (generation bump) followed by a new page's ring forgets the cursor")
}

func TestService_SameRingAcrossCastsDoesNotCorroborate(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	list := items("https://o/a")
	ring := []Outcome{{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1}}
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: ring})
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 2, Entries: ring})
	rec, err := s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Fail)
	assert.Empty(t, rec.State)
}

// F3: several stages of ONE attempt are one failure; a new play counts again.
func TestService_OnePlayCountsOnce(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	list := items("https://o/a", "https://o/b")
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageDetect, Code: "503", Attempt: 1},
		{Seq: 2, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image", Attempt: 1},
		{Seq: 3, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image", Attempt: 2},
		{Seq: 4, Source: "https://o/a", Kind: OutcomeFail, Stage: StageExhausted, Attempt: 3},
	}})
	rec, err := s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Fail, "detect, load, retry and exhaustion of one mount are one failure")
	assert.Empty(t, rec.State)
	assert.Equal(t, StageExhausted, rec.Plays.LastFail.Stage, "the terminal stage is still the diagnostic")

	// Another item plays, then a comes round again: a distinct play.
	s.ObservePlayback(list, 1, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 5, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"},
		{Seq: 6, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image", Attempt: 1},
	}})
	rec, err = s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Plays.Fail)
	assert.Equal(t, StateBroken, rec.State, "the second distinct play corroborates")

	// One-item playlist: exhaustion ends the play, the next mount counts.
	one := items("https://o/c")
	s.ObservePlayback(one, 0, nil, &Outcomes{CastSeq: 2, Entries: []Outcome{
		{Seq: 7, Source: "https://o/c", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1},
		{Seq: 8, Source: "https://o/c", Kind: OutcomeFail, Stage: StageExhausted, Attempt: 3},
		{Seq: 9, Source: "https://o/c", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1},
	}})
	rec, err = s.Store().Get("https://o/c")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Plays.Fail)
	assert.Equal(t, StateBroken, rec.State)
}

// F2: a deadline mid-body is the probe's clock, not truncation.
func TestProbeOne_BodyReadDeadlineIsTransient(t *testing.T) {
	resp := ok(200, "image/png", []byte("\x89PNG\r\n\x1a\n"), 1024)
	resp.resp.ReadErr = context.DeadlineExceeded
	f := &scriptedFetcher{answers: []scriptedAnswer{resp}}
	res := newProbe(f).ProbeOne(context.Background(), "https://origin.example/a.png")
	assert.Equal(t, VerdictInconclusive, res.Verdict)
	assert.Equal(t, ReasonTransport, res.Reason)

	// The same short read with the connection genuinely closed early IS
	// truncation evidence.
	resp = ok(200, "image/png", []byte("\x89PNG\r\n\x1a\n"), 1024)
	resp.resp.ReadErr = errors.New("unexpected EOF")
	f = &scriptedFetcher{answers: []scriptedAnswer{resp}}
	res = newProbe(f).ProbeOne(context.Background(), "https://origin.example/a.png")
	assert.Equal(t, VerdictDead, res.Verdict)
	assert.Equal(t, ReasonTruncated, res.Reason)
}

// F5: a source the offline cache can replay is never hinted.
func TestService_HintsExcludeReplayableSources(t *testing.T) {
	clock := &testClock{t: time.Now()}
	p := newPassProber(t, newTableFetcher(), clock)
	path := filepath.Join(t.TempDir(), "m.json")
	require.NoError(t, writeManifest(path))
	s := New(Config{Store: p.Store, Prober: p, Contract: &PlayerContract{Path: path}, Now: clock.Now,
		Online:     func() bool { return true },
		Replayable: func(src string) bool { return src == "https://o/cached" }})
	defer func() { _ = s.Close() }()
	for _, src := range []string{"https://o/cached", "https://o/dead"} {
		_, err := p.Store.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
		require.NoError(t, err)
	}
	hints := s.HintArgument(items("https://o/cached", "https://o/dead")).(map[string]Hint)
	assert.Len(t, hints, 1)
	assert.Contains(t, hints, "https://o/dead")
	assert.NotContains(t, hints, "https://o/cached", "replay would have served it")
}

// F6: a transient answer after an expired verdict holds the next attempt
// without reviving the verdict's authority to skip.
func TestStore_TransientRetryHoldDoesNotExtendAnExpiredVerdict(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	const src = "https://o/a"
	_, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	clock.Advance(RecheckBroken + time.Minute)
	rec, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictInconclusive, Status: 429, Reason: ReasonHTTPStatus, Detail: "HTTP 429"})
	require.NoError(t, err)
	assert.False(t, rec.Conclusive(clock.t), "the expired verdict is not revived")
	assert.False(t, rec.Due(clock.t), "but the origin is left alone")
	assert.True(t, rec.Due(clock.t.Add(RetryTransient)))
	due, err := s.Due(clock.t, 10, nil)
	require.NoError(t, err)
	assert.Empty(t, due, "the index follows the retry hold")
	hints, err := s.Hints([]string{src}, clock.t)
	require.NoError(t, err)
	assert.Empty(t, hints)

	// The cast-time pass respects the hold too: no request, inconclusive.
	f := newTableFetcher()
	f.answers[src] = ok(404, "", nil, -1)
	prober := &Prober{Probe: &Probe{Fetcher: f}, Store: s, Now: clock.Now}
	res := prober.Preflight(context.Background(), []string{src})
	assert.Equal(t, VerdictInconclusive, res[0].Verdict)
	assert.True(t, res[0].FromStore)
	assert.Equal(t, 0, f.count(src))
}

// F7: the current item is rechecked when due (a one-item playlist).
func TestLookahead_RechecksTheCurrentItemWhenDue(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/only"] = ok(503, "", nil, -1)
	l := newTestLookahead(t, f, clock)
	l.Observe([]string{"https://o/only"}, 0)
	assert.True(t, l.step(context.Background(), nil), "a never-seen current item is probed")
	assert.Equal(t, 1, f.count("https://o/only"))
	assert.False(t, l.step(context.Background(), nil), "held for the transient retry")
	clock.Advance(RetryTransient + time.Second)
	f.answers["https://o/only"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	assert.True(t, l.step(context.Background(), nil), "rechecked once due")
	rec, err := l.Store.Get("https://o/only")
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State)
}

// F8: on-wall rows at the head of the due index must not shadow off-wall
// rows behind them.
func TestLookahead_SweepSkipsOnWallRowsInsideTheQuery(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	f := newTableFetcher()
	l := newTestLookahead(t, f, clock)
	// The whole wall is inside the window the walk owns (current + 9), so
	// every on-wall row is excluded from the sweep's query; on-wall rows
	// BEYOND the window are not (the #392 follow-up lets the sweep refresh
	// them, by due time, like any other row).
	l.Window = 9
	wall := make([]string, 0, 10)
	for i := 0; i < 10; i++ {
		src := "https://o/wall" + string(rune('a'+i))
		wall = append(wall, src)
		_, err := l.Store.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
		require.NoError(t, err)
		clock.Advance(time.Second)
	}
	const off = "https://o/offwall"
	f.answers[off] = ok(http.StatusOK, "image/png", minimalPNG(1, 1), -1)
	_, err := l.Store.ApplyProbe(off, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	// Everything is due; the wall items sit ahead of the off-wall row in the index.
	clock.Advance(RecheckBroken + time.Minute)
	for _, src := range wall {
		f.answers[src] = ok(http.StatusNotFound, "", nil, -1)
	}
	l.Observe(wall, 0)
	// Drain the lookahead window (9 upcoming items + the current one), then
	// the sweep must reach the off-wall record.
	for i := 0; i < 12 && f.count(off) == 0; i++ {
		require.True(t, l.step(context.Background(), nil))
	}
	assert.Equal(t, 1, f.count(off), "the off-wall row is reached despite ten due window rows ahead of it")
}

func writeManifest(path string) error {
	return os.WriteFile(path, []byte(manifestWithContract), 0o600)
}

// Regressions from the PR #438 review (feralfile-bot, round 2).

// F1 (round 2): a generation bump that is a CDP reconnect to the SAME page
// must not re-apply the ring the page still holds; one that is a new page
// must. The drainer tells them apart by the identities it consumed.
func TestDrainer_ReconnectToSamePageReappliesNothing(t *testing.T) {
	var d Drainer
	ring := []Outcome{
		{Seq: 1, Source: "a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t1"},
		{Seq: 2, Source: "a", Kind: OutcomeFail, Stage: StageWatchdog, Attempt: 1, At: "t2"},
	}
	fresh, _, restarted := d.Drain(&Outcomes{CastSeq: 1, Entries: ring})
	assert.Len(t, fresh, 2)
	assert.False(t, restarted)

	d.Reset() // generation bump: a transport reconnect
	fresh, gap, restarted := d.Drain(&Outcomes{CastSeq: 1, Entries: ring})
	assert.Empty(t, fresh, "the surviving page's ring was consumed already")
	assert.False(t, restarted, "a ring holding remembered entries is the same page")
	assert.Equal(t, int64(0), gap)

	more := append(ring, Outcome{Seq: 3, Source: "b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image", At: "t3"})
	fresh, gap, restarted = d.Drain(&Outcomes{CastSeq: 1, Entries: more})
	assert.Equal(t, []string{"b"}, sourcesOf(fresh), "the cursor moved past the remembered entries")
	assert.Equal(t, int64(0), gap, "no gap is reported across the reconnect")
	assert.False(t, restarted)

	d.Reset() // generation bump: a real reload, numbering restarts
	fresh, _, restarted = d.Drain(&Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t9"},
	}})
	assert.Equal(t, []string{"a"}, sourcesOf(fresh), "a ring holding nothing remembered is a new page")
	assert.True(t, restarted)

	d.Reset()
	fresh, _, restarted = d.Drain(&Outcomes{CastSeq: 1})
	assert.Empty(t, fresh)
	assert.False(t, restarted, "an empty ring cannot be classified yet")
	fresh, _, restarted = d.Drain(&Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t9"},
	}})
	assert.Empty(t, fresh, "the reset stays pending until a ring shows up; this one is the surviving page")
	assert.False(t, restarted)
}

func TestDrainer_SeenMemoryIsBounded(t *testing.T) {
	var d Drainer
	for seq := int64(1); seq <= 3*seenCap; seq++ {
		d.Drain(&Outcomes{CastSeq: 1, Entries: []Outcome{{Seq: seq, Source: "a", At: "t"}}})
	}
	assert.Len(t, d.seen, seenCap)
	assert.Len(t, d.order, seenCap)
}

// F1 (round 2), end to end: a reconnect between two polls of one failing
// play must not turn that play into two.
func TestService_ReconnectDoesNotCorroborate(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	list := items("https://o/a")
	ring := []Outcome{{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t1"}}
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: ring})
	s.ResetOutcomeCursor()
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: ring})
	rec, err := s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Fail, "the same failed play seen again after a reconnect counts once")
	assert.Empty(t, rec.State)

	// The retry of the same play after the reconnect is still the same run.
	ring = append(ring, Outcome{Seq: 2, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 2, At: "t2"})
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: ring})
	rec, err = s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Fail, "attempt 2 of the run is not a distinct play")

	// A real page restart is a distinct play: the second failure corroborates.
	s.ResetOutcomeCursor()
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t9"},
	}})
	rec, err = s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Plays.Fail)
	assert.Equal(t, StateBroken, rec.State, "a failure on a new page is a second distinct play")
}

// F9: fragment variants of one wire URL are two sources with two records;
// the pass shares the request, never the stored verdict.
func TestPreflight_FragmentVariantsKeepTheirOwnVerdicts(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	p := newPassProber(t, f, clock)
	_, err := p.Store.ApplyProbe("https://o/a#one", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	_, err = p.Store.ApplyOutcome(Outcome{Source: "https://o/a#two", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"}, true)
	require.NoError(t, err)

	results := p.Preflight(context.Background(), []string{"https://o/a#one", "https://o/a#two"})
	assert.Equal(t, VerdictDead, results[0].Verdict)
	assert.True(t, results[0].FromStore)
	assert.Equal(t, VerdictAlive, results[1].Verdict, "the sibling keeps its own play-ready verdict")
	assert.True(t, results[1].FromStore)
	assert.Equal(t, 0, f.count("https://o/a#one")+f.count("https://o/a#two"), "both answered from the store")

	// Two unknown variants: one request (the fake keys by the URL the pass
	// hands it; net/http drops the fragment on the wire), two records.
	f.answers["https://o/b#one"] = ok(404, "", nil, -1)
	results = p.Preflight(context.Background(), []string{"https://o/b#one", "https://o/b#two"})
	assert.Equal(t, VerdictDead, results[0].Verdict)
	assert.Equal(t, VerdictDead, results[1].Verdict)
	assert.Equal(t, 1, f.count("https://o/b#one"), "wire-identical sources still share one request")
	assert.Equal(t, 0, f.count("https://o/b#two"))
	for _, src := range []string{"https://o/b#one", "https://o/b#two"} {
		rec, err := p.Store.Get(src)
		require.NoError(t, err)
		require.NotNil(t, rec, src)
		assert.Equal(t, StateBroken, rec.State, src)
	}

	// A stored verdict for one variant does not spare an unknown sibling
	// its own request.
	f.answers["https://o/a#three"] = ok(503, "", nil, -1)
	results = p.Preflight(context.Background(), []string{"https://o/a#one", "https://o/a#three"})
	assert.True(t, results[0].FromStore)
	assert.Equal(t, VerdictInconclusive, results[1].Verdict)
	assert.Equal(t, 1, f.count("https://o/a#three"), "the unknown variant is probed on its own")
	rec, err := p.Store.Get("https://o/a#three")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, "HTTP 503", rec.LastError, "the transient lands on the variant's own record")
}

// F10: a source the guard refuses writes no record and so stays due; the
// worker must step past it rather than probe it every tick.
func TestLookahead_StepsPastRefusedSources(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/cur"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://o/refused"] = scriptedAnswer{err: &FetchError{Kind: FetchErrTooLong, Err: errors.New("too long")}}
	f.answers["https://o/next"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	l := newTestLookahead(t, f, clock)
	l.Observe([]string{"https://o/cur", "https://o/refused", "https://o/next"}, 0)

	assert.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/refused"))
	assert.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/next"), "the item behind the refused one is reached")
	assert.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/cur"), "and so is the current item's recheck")
	assert.False(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/refused"), "the refusal was probed exactly once")
	rec, err := l.Store.Get("https://o/refused")
	require.NoError(t, err)
	assert.Nil(t, rec, "a refusal is never a verdict, nor a record")

	clock.Advance(probeHold + time.Second)
	assert.True(t, l.step(context.Background(), nil), "past the hold the refused source gets another look")
	assert.Equal(t, 2, f.count("https://o/refused"))

	// The sweep holds too: a due off-wall record that the guard now refuses
	// does not eat the sweep budget every tick.
	l.Observe(nil, 0)
	_, err = l.Store.ApplyProbe("https://o/swept", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	f.answers["https://o/swept"] = scriptedAnswer{err: &FetchError{Kind: FetchErrGuard, Err: errors.New("reserved")}}
	clock.Advance(RecheckBroken + time.Second)
	assert.True(t, l.step(context.Background(), nil))
	assert.Equal(t, 1, f.count("https://o/swept"))
	assert.False(t, l.step(context.Background(), nil), "held until probeHold passes")
	assert.Equal(t, 1, f.count("https://o/swept"))
}

// Regressions from the PR #438 review (feralfile-bot, round 3).

// F12: a playlist repeating one source thousands of times is one request,
// one record and one write — not one db.Batch wait per copy.
func TestPreflight_RepeatedSourceIsOneWrite(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/a"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p := newPassProber(t, f, clock)
	sources := make([]string, 2048)
	for i := range sources {
		sources[i] = "https://o/a"
	}
	start := time.Now()
	results := p.Preflight(context.Background(), sources)
	assert.Less(t, time.Since(start), 3*time.Second, "the fan-out must not serialize one batch wait per copy")
	assert.Equal(t, 1, f.count("https://o/a"))
	for i := range results {
		require.Equal(t, VerdictAlive, results[i].Verdict, "item %d", i)
	}
	rec, err := p.Store.Get("https://o/a")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, StateOK, rec.State)

	// Distinct fragment variants of one URL: one request, one write, a
	// record each.
	variants := make([]string, 512)
	for i := range variants {
		variants[i] = fmt.Sprintf("https://o/b#%d", i)
	}
	f.answers["https://o/b#0"] = ok(404, "", nil, -1)
	start = time.Now()
	p.Preflight(context.Background(), variants)
	assert.Less(t, time.Since(start), 3*time.Second)
	assert.Equal(t, 1, f.count("https://o/b#0"))
	for _, src := range []string{"https://o/b#1", "https://o/b#511"} {
		rec, err := p.Store.Get(src)
		require.NoError(t, err)
		require.NotNil(t, rec, src)
		assert.Equal(t, StateBroken, rec.State, src)
	}
}

func TestStore_ApplyProbesLastWriteWinsAndSkipsNonWrites(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openTestStore(t, clock)
	err := store.ApplyProbes([]ProbeWrite{
		{Source: "https://o/a", Result: ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus}},
		{Source: "https://o/a", Result: ProbeResult{Verdict: VerdictAlive, Status: 200}},
		{Source: "https://o/guard", Result: ProbeResult{Verdict: VerdictInconclusive}},
		{Source: "data:,x", Result: ProbeResult{Verdict: VerdictInline}},
		{Source: "https://o/stored", Result: ProbeResult{Verdict: VerdictDead, FromStore: true}},
	})
	require.NoError(t, err)
	rec, err := store.Get("https://o/a")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, StateOK, rec.State, "the last write for a source wins")
	for _, src := range []string{"https://o/guard", "data:,x", "https://o/stored"} {
		rec, err := store.Get(src)
		require.NoError(t, err)
		assert.Nil(t, rec, src)
	}
	require.NoError(t, store.ApplyProbes(nil))
}

// F13: a provably malformed data: URI is conclusive and gets a record and
// a hint; a well-formed inline source stays unrecorded; the sweep
// re-settles the record without network.
func TestPreflight_RecordsMalformedDataURIs(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/alive"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	l := newTestLookahead(t, f, clock)
	p := l.Prober
	results := p.Preflight(context.Background(), []string{"data:image/png;base64", "https://o/alive", "data:,fine"})
	assert.Equal(t, VerdictDead, results[0].Verdict)
	assert.Equal(t, VerdictAlive, results[1].Verdict)
	assert.Equal(t, VerdictInline, results[2].Verdict)

	rec, err := p.Store.Get("data:image/png;base64")
	require.NoError(t, err)
	require.NotNil(t, rec, "the malformed URI is recorded")
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, CategoryUnsupported, rec.Category)
	assert.Equal(t, ReasonDataURIInvalid, rec.Reason)
	rec, err = p.Store.Get("data:,fine")
	require.NoError(t, err)
	assert.Nil(t, rec, "a well-formed inline source is not recorded")

	hints, err := p.Store.Hints([]string{"data:image/png;base64", "https://o/alive", "data:,fine"}, clock.Now())
	require.NoError(t, err)
	assert.Contains(t, hints, "data:image/png;base64")
	assert.Len(t, hints, 1)

	// Off the wall and due, the sweep re-settles it: no request, the
	// broken count advances, and it never shadows the rows behind it.
	l.Observe(nil, 0)
	clock.Advance(RecheckBroken + time.Second)
	assert.True(t, l.step(context.Background(), nil))
	rec, err = p.Store.Get("data:image/png;base64")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, 2, rec.ConsecutiveBroken)
	assert.Equal(t, 1, f.count("https://o/alive"), "no network for a data: recheck")
}

// Regressions from the PR #438 review (feralfile-bot, round 4).

// F15: a ring entry is applied once for the life of the store, so a
// controld restart under a surviving player page re-applies nothing.
func TestStore_ApplyOutcomeIsIdempotentPerRingEntry(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openTestStore(t, clock)
	entry := Outcome{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t1"}
	for i := 0; i < 3; i++ {
		_, err := store.ApplyOutcome(entry, true)
		require.NoError(t, err)
	}
	rec, err := store.Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Fail, "the same entry counts once")
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail)

	okEntry := Outcome{Seq: 2, Source: "https://o/a", Kind: OutcomeOK, Stage: StageReady, MediaType: "image", At: "t2"}
	_, err = store.ApplyOutcome(okEntry, true)
	require.NoError(t, err)
	_, err = store.ApplyOutcome(okEntry, true)
	require.NoError(t, err)
	rec, err = store.Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.OK)

	// The inferred fallback carries no seq: it is bounded by the service's
	// once-per-mount rule, not by identity, so two mounts count twice.
	inferred := Outcome{Source: "https://o/b", Kind: OutcomeFail, Stage: StageInferred}
	_, err = store.ApplyOutcome(inferred, true)
	require.NoError(t, err)
	_, err = store.ApplyOutcome(inferred, true)
	require.NoError(t, err)
	rec, err = store.Get("https://o/b")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Plays.Fail)

	// The consumed ring is bounded; the oldest identities fall out.
	for seq := int64(10); seq < int64(10+consumedCap+8); seq++ {
		_, err := store.ApplyOutcome(Outcome{Seq: seq, Source: "https://o/c", Kind: OutcomeSkip, Stage: StageHint, At: "t"}, true)
		require.NoError(t, err)
	}
	_, err = store.ApplyOutcome(entry, true)
	require.NoError(t, err)
	rec, err = store.Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Plays.Fail, "an identity older than the ring is forgotten (one possible double count, never a lost observation)")
}

func TestService_RestartDoesNotCorroborate(t *testing.T) {
	clock := &testClock{t: time.Now()}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "store.db")
	manifest := filepath.Join(dir, "ffos-player-contract.json")
	require.NoError(t, os.WriteFile(manifest, []byte(manifestWithContract), 0o600))
	open := func() *Service {
		store, err := Open(dbPath, Options{Now: clock.Now})
		require.NoError(t, err)
		return New(Config{Store: store, Contract: &PlayerContract{Path: manifest}, Now: clock.Now, Online: func() bool { return true }})
	}
	list := items("https://o/a")
	ring := []Outcome{{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t1"}}

	s := open()
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: ring})
	require.NoError(t, s.Close())

	// controld restarts; Chromium keeps its page and its ring.
	s = open()
	defer func() { _ = s.Close() }()
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: ring})
	rec, err := s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Fail, "the play consumed before the restart is not a second play")
	assert.Empty(t, rec.State)
	assert.Nil(t, s.HintArgument(list), "no hint was manufactured")

	// A genuinely new play after the restart still corroborates.
	ring = append(ring,
		Outcome{Seq: 2, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image", At: "t2"},
		Outcome{Seq: 3, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: "t3"})
	s.ObservePlayback(items("https://o/a", "https://o/b"), 0, nil, &Outcomes{CastSeq: 1, Entries: ring})
	rec, err = s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Plays.Fail)
	assert.Equal(t, StateBroken, rec.State)
}

// Regressions from the PR #438 review (feralfile-bot, round 5).

// F17: an outcome is dated by the player's timestamp; one older than the
// record's current verdict counts in the history but never supersedes.
func TestStore_StaleOutcomesNeverSupersedeANewerVerdict(t *testing.T) {
	base := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	clock := &testClock{t: base}
	store := openTestStore(t, clock)

	// Healthy L0 at 12:00; two distinct failed plays from 11:00 and 11:01
	// drained at 12:05 (a restart handed back the page's old ring).
	_, err := store.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	clock.Advance(5 * time.Minute)
	for i, at := range []string{"2026-11-04T11:00:00.000Z", "2026-11-04T11:01:00.000Z"} {
		_, err := store.ApplyOutcome(Outcome{Seq: int64(i + 1), Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: at}, true)
		require.NoError(t, err)
	}
	rec, err := store.Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "old failures do not overturn the newer L0 verdict")
	assert.Equal(t, 2, rec.Plays.Fail, "but they are history")
	assert.Equal(t, 0, rec.Plays.ConsecutiveFail, "and supply no corroboration")
	assert.Equal(t, base, rec.CheckedAt)
	assert.Equal(t, time.Date(2026, 11, 4, 11, 1, 0, 0, time.UTC), *rec.Plays.LastFailAt)
	hints, err := store.Hints([]string{"https://o/a"}, clock.Now())
	require.NoError(t, err)
	assert.Empty(t, hints)

	// The reverse: a 404 at 12:05, then an old `ready` from 11:30.
	_, err = store.ApplyProbe("https://o/b", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	_, err = store.ApplyOutcome(Outcome{Seq: 3, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image", At: "2026-11-04T11:30:00Z"}, true)
	require.NoError(t, err)
	rec, err = store.Get("https://o/b")
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "an old success does not heal a newer broken verdict")
	assert.Equal(t, 1, rec.Plays.OK)
	assert.Equal(t, time.Date(2026, 11, 4, 11, 30, 0, 0, time.UTC), *rec.Plays.LastOKAt)

	// A success dated after the verdict heals it, dated by the player.
	_, err = store.ApplyOutcome(Outcome{Seq: 4, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image", At: "2026-11-04T12:05:00Z"}, true)
	require.NoError(t, err)
	rec, err = store.Get("https://o/b")
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State)
	assert.Equal(t, time.Date(2026, 11, 4, 12, 5, 0, 0, time.UTC), rec.CheckedAt)

	// Fresh failures (after the verdict) still corroborate, dated by the player.
	clock.Advance(time.Minute)
	for i, at := range []string{"2026-11-04T12:05:10Z", "2026-11-04T12:05:20Z"} {
		_, err := store.ApplyOutcome(Outcome{Seq: int64(10 + i), Source: "https://o/b", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1, At: at}, true)
		require.NoError(t, err)
	}
	rec, err = store.Get("https://o/b")
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, time.Date(2026, 11, 4, 12, 5, 20, 0, time.UTC), rec.CheckedAt)
	assert.Equal(t, rec.CheckedAt.Add(RecheckBroken), rec.RecheckAfter)
}

func TestOutcomeTime(t *testing.T) {
	now := time.Date(2026, 11, 4, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, now, outcomeTime(Outcome{}, now), "no timestamp: the drain time")
	assert.Equal(t, now, outcomeTime(Outcome{At: "t1"}, now), "unparseable: the drain time")
	assert.Equal(t, now, outcomeTime(Outcome{At: "2026-11-04T13:00:00Z"}, now), "a player clock ahead of the device is clamped")
	assert.Equal(t, time.Date(2026, 11, 4, 11, 59, 59, 500000000, time.UTC), outcomeTime(Outcome{At: "2026-11-04T11:59:59.500Z"}, now))
}

// #392 follow-up: `state: broken` is the last observation, `skipped` is
// what the player reports it is resolving past, and `unskipped` says why a
// broken item is not skipped — one case each.
func TestService_ItemHealths_SkippedAndUnskipped(t *testing.T) {
	clock := &testClock{t: time.Now()}
	replayable := map[string]bool{}
	store := openTestStore(t, clock)
	// The contract check caches its answer for a while, so the no-contract
	// and with-contract cases get a service each over the same store.
	noContract := New(Config{Store: store, Contract: &PlayerContract{Path: filepath.Join(t.TempDir(), "none")}, Now: clock.Now, Online: func() bool { return true },
		Replayable: func(src string) bool { return replayable[src] }})
	manifest := filepath.Join(t.TempDir(), "ffos-player-contract.json")
	require.NoError(t, os.WriteFile(manifest, []byte(manifestWithContract), 0o600))
	svc := New(Config{Store: store, Contract: &PlayerContract{Path: manifest}, Now: clock.Now, Online: func() bool { return true },
		Replayable: func(src string) bool { return replayable[src] }})
	defer func() { _ = svc.Close() }()
	list := items("https://o/broken", "https://o/replay", "https://o/expired", "https://o/ok")
	for _, src := range []string{"https://o/broken", "https://o/replay", "https://o/expired"} {
		_, err := store.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
		require.NoError(t, err)
	}
	_, err := store.ApplyProbe("https://o/ok", ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)

	// Case 3: no contract declared — nothing is hinted, every broken row
	// says so, and nothing is skipped whatever the state.
	h := noContract.ItemHealths(list, nil)
	require.Len(t, h, 4)
	for i := 0; i < 3; i++ {
		assert.Equal(t, StateBroken, h[i].State)
		assert.False(t, h[i].Skipped)
		assert.Equal(t, UnskippedPlayer, h[i].Unskipped, i)
	}
	assert.False(t, h[3].Skipped)
	assert.Empty(t, h[3].Unskipped, "an ok row never carries a reason")

	// With the contract: the player reports what it skips.
	replayable["https://o/replay"] = true
	// Expire the third verdict only: it was written at the same time as the
	// others, so re-write it with a recheck in the past.
	rec, err := store.Get("https://o/expired")
	require.NoError(t, err)
	require.True(t, rec.Conclusive(clock.Now()))
	clock.Advance(RecheckBroken + time.Second)
	for _, src := range []string{"https://o/broken", "https://o/replay"} {
		_, err := store.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus}) // fresh again
		require.NoError(t, err)
	}
	h = svc.ItemHealths(list, []int{0})
	assert.True(t, h[0].Skipped, "the player says it skips index 0")
	assert.Empty(t, h[0].Unskipped)
	assert.False(t, h[1].Skipped)
	assert.Equal(t, UnskippedReplayable, h[1].Unskipped, "case 2: the cache can serve it, so the hint left it out")
	assert.False(t, h[2].Skipped)
	assert.Equal(t, StateBroken, h[2].State, "still the last observation")
	assert.Equal(t, UnskippedExpired, h[2].Unskipped, "case 1: past its window, not conclusive, not hinted")

	// Case 4, both ways: a conclusive broken item the player is not yet
	// skipping is pending; an ok item the player still skips is skipped.
	h = svc.ItemHealths(list, []int{3})
	assert.False(t, h[0].Skipped)
	assert.Equal(t, UnskippedPending, h[0].Unskipped, "hinted now, the push has not reached the player")
	assert.True(t, h[3].Skipped, "healed to ok, but the player keeps the old hint until the next push")
	assert.Equal(t, StateOK, h[3].State)

	// Out-of-range or negative indices are ignored.
	h = svc.ItemHealths(list, []int{-1, 9})
	for _, row := range h {
		assert.False(t, row.Skipped)
	}

	// getMediaHealth carries the same two fields.
	rows, err := svc.Report(list, []int{0})
	require.NoError(t, err)
	assert.True(t, rows[0].Skipped)
	assert.Equal(t, UnskippedReplayable, rows[1].Unskipped)
	assert.Equal(t, UnskippedExpired, rows[2].Unskipped)
}

// #392 follow-up: a due record on the wall beyond the lookahead window is
// swept, so an expired verdict there is refreshed instead of waiting for
// playback to reach it.
func TestLookahead_SweepsDueOnWallRecordsBeyondTheWindow(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	l := newTestLookahead(t, f, clock) // Window: 3
	wall := make([]string, 12)
	for i := range wall {
		wall[i] = fmt.Sprintf("https://o/%d", i)
		f.answers[wall[i]] = ok(200, "image/png", minimalPNG(1, 1), -1)
		_, err := l.Store.ApplyProbe(wall[i], ProbeResult{Verdict: VerdictAlive, Status: 200})
		require.NoError(t, err)
	}
	// Position 9 is broken and its verdict expires; nothing in the window
	// (0..3) is due.
	_, err := l.Store.ApplyProbe(wall[9], ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	clock.Advance(RecheckBroken + time.Second)
	for _, src := range wall[:4] {
		_, err := l.Store.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200})
		require.NoError(t, err)
	}
	l.Observe(wall, 0)
	// The window items 4..8 and 10, 11 are due too (ok, 7 d old) but the
	// sweep answers the earliest due first; what matters is that the
	// on-wall record at 9 is reachable at all.
	seen := map[string]bool{}
	for i := 0; i < 12 && !seen[wall[9]]; i++ {
		if !l.step(context.Background(), nil) {
			break
		}
		for _, src := range wall {
			if f.count(src) > 0 {
				seen[src] = true
			}
		}
	}
	assert.True(t, seen[wall[9]], "a due on-wall record beyond the window is swept")
}
