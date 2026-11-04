package mediahealth

// Regressions from the PR #438 review (feralfile-bot, round 1): each test
// is one finding, named for it.

import (
	"context"
	"errors"
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
	fresh, _ := d.Drain(&Outcomes{CastSeq: 1, Entries: ring})
	assert.Len(t, fresh, 1)
	fresh, _ = d.Drain(&Outcomes{CastSeq: 2, Entries: ring})
	assert.Empty(t, fresh, "a cast with the same ring re-applies nothing")
	fresh, _ = d.Drain(&Outcomes{CastSeq: 2, Entries: append(ring, Outcome{Seq: 2, Source: "b", Kind: OutcomeOK, Stage: StageReady})})
	assert.Equal(t, []string{"b"}, sourcesOf(fresh))

	// Page restart with the same castSeq: numbering restarts below the cursor.
	fresh, gap := d.Drain(&Outcomes{CastSeq: 2, Entries: []Outcome{{Seq: 1, Source: "c", Kind: OutcomeOK, Stage: StageReady}}})
	assert.Equal(t, []string{"c"}, sourcesOf(fresh), "a rollback means everything present is new")
	assert.Equal(t, int64(0), gap)

	d.Reset()
	fresh, _ = d.Drain(&Outcomes{CastSeq: 3, Entries: []Outcome{{Seq: 1, Source: "c", Kind: OutcomeOK, Stage: StageReady}}})
	assert.Equal(t, []string{"c"}, sourcesOf(fresh), "an explicit reset (generation bump) forgets the cursor")
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
		{Seq: 5, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady},
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
	l.Window = 1
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
	// Drain the lookahead window (1 upcoming item + the current one), then
	// the sweep must reach the off-wall record.
	for i := 0; i < 3 && f.count(off) == 0; i++ {
		require.True(t, l.step(context.Background(), nil))
	}
	assert.Equal(t, 1, f.count(off), "the off-wall row is reached despite ten due on-wall rows ahead of it")
}

func writeManifest(path string) error {
	return os.WriteFile(path, []byte(manifestWithContract), 0o600)
}
