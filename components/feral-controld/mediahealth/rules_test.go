package mediahealth

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// playFeed drives the Service the way the status poller does: each call
// appends entries to the player's ring (fresh seq, timestamp now) and
// drains it.
type playFeed struct {
	t     *testing.T
	s     *Service
	clock *testClock
	src   string
	seq   int64
	ring  []Outcome
}

func newPlayFeed(t *testing.T, online bool) *playFeed {
	t.Helper()
	clock := &testClock{t: time.Now().Add(-48 * time.Hour)}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	s.online = func() bool { return online }
	t.Cleanup(func() { _ = s.Close() })
	return &playFeed{t: t, s: s, clock: clock, src: "https://dead.example/work"}
}

func (f *playFeed) emit(entries ...Outcome) {
	for _, e := range entries {
		f.seq++
		f.clock.Advance(time.Second)
		e.Seq = f.seq
		if e.Source == "" {
			e.Source = f.src
		}
		e.At = f.clock.t.UTC().Format(time.RFC3339Nano)
		f.ring = append(f.ring, e)
	}
	f.s.ObservePlayback(items(f.src, "https://o/other"), 0, nil, &Outcomes{CastSeq: 1, Entries: f.ring})
}

func (f *playFeed) record() *Record {
	rec, err := f.s.Store().Get(f.src)
	require.NoError(f.t, err)
	return rec
}

func failed(stage, code string, attempt int) Outcome {
	return Outcome{Kind: OutcomeFail, Stage: stage, Code: code, Attempt: attempt, MediaType: "iframe"}
}

var iframeReady = Outcome{Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", Attempt: 1}

// infuraPlay is one play of a dead iframe host on the office FF1: the
// content-type check fails on the network, three 45 s load timeouts, then
// the retry budget is spent.
func infuraPlay() []Outcome {
	return []Outcome{
		{Kind: OutcomeFail, Stage: StageDetect, Code: CodeNetwork, Attempt: 1},
		failed(StageWatchdog, "fixed", 1), failed(StageWatchdog, "fixed", 2), failed(StageWatchdog, "fixed", 3),
		failed(StageExhausted, "3", 3),
	}
}

// Fix 1: an uncountable entry must not open the play run.
func TestRules_UncountableEntryNoLongerSwallowsThePlay(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(infuraPlay()...)
	rec := f.record()
	assert.Equal(t, 1, rec.Plays.Fail, "the first load timeout counts the play")
	assert.NotEqual(t, StateBroken, rec.State, "one failed play is recorded, not concluded")

	f.emit(infuraPlay()...)
	rec = f.record()
	assert.Equal(t, 2, rec.Plays.Fail)
	assert.Equal(t, StateBroken, rec.State, "the second failed play concludes")
	assert.Equal(t, LayerPlay, rec.Layer)
	assert.Equal(t, CategoryRenderFailed, rec.Category)
}

func TestRules_NetworkCheckThenWorkingIframeNeverCounts(t *testing.T) {
	f := newPlayFeed(t, true)
	for range 6 {
		f.emit(Outcome{Kind: OutcomeFail, Stage: StageDetect, Code: CodeNetwork, Attempt: 1}, iframeReady)
	}
	rec := f.record()
	assert.Zero(t, rec.Plays.Fail, "a CORS-blocked check before a working iframe is never evidence")
	assert.NotEqual(t, StateBroken, rec.State)
}

func TestRules_GPULossDoesNotSplitAPlay(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(failed(StageWatchdog, "fixed", 1), Outcome{Kind: OutcomeFail, Stage: StageWebGL, Code: "context_lost"}, failed(StageWatchdog, "fixed", 2))
	assert.Equal(t, 1, f.record().Plays.Fail, "one play, one count")
}

func TestRules_OfflineNeverCounts(t *testing.T) {
	f := newPlayFeed(t, false)
	f.emit(infuraPlay()...)
	f.emit(infuraPlay()...)
	rec := f.record()
	assert.Zero(t, rec.Plays.Fail)
	assert.NotEqual(t, StateBroken, rec.State)
}

// Fix 2: an iframe ready after a definite refusal is the error page.
func TestRules_ErrorPageReadyDoesNotEraseTheRefusal(t *testing.T) {
	f := newPlayFeed(t, true)
	refusedPlay := []Outcome{{Kind: OutcomeFail, Stage: StageDetect, Code: "429", Attempt: 1}, iframeReady}
	f.emit(refusedPlay...)
	rec := f.record()
	assert.Equal(t, 1, rec.Plays.Fail)
	assert.Zero(t, rec.Plays.OK, "the error page is not a successful play")
	f.emit(refusedPlay...)
	rec = f.record()
	assert.Equal(t, StateBroken, rec.State, "two refused plays conclude")
	assert.Equal(t, CategorySourceBlocked, rec.Category, "the kiosk itself was refused")
	assert.Equal(t, 429, rec.HTTPStatus)
}

func TestRules_RefusalStatusesThatAreAboutTheCheckStillHeal(t *testing.T) {
	for _, code := range []string{"405", "408", "416", "500", "503"} {
		t.Run(code, func(t *testing.T) {
			f := newPlayFeed(t, true)
			for range 4 {
				f.emit(Outcome{Kind: OutcomeFail, Stage: StageDetect, Code: code, Attempt: 1}, iframeReady)
			}
			assert.NotEqual(t, StateBroken, f.record().State, "the fallback GET may serve the work: the iframe ready keeps resetting the run")
		})
	}
}

func TestRules_RefusalThenRealRendererReadyHeals(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(Outcome{Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1}, Outcome{Kind: OutcomeOK, Stage: StageReady, MediaType: "image", Attempt: 1})
	rec := f.record()
	assert.Equal(t, StateOK, rec.State, "a decoded image is the work, whatever HEAD said")
}

// Fix 3: a probe cut short by the CALLER's context records nothing.
func TestRules_PassCeilingIsNotTheSourcesFailure(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.block = make(chan struct{})
	f.answers["https://slow.example/a"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p := newPassProber(t, f, clock)
	p.PhaseCeiling = 50 * time.Millisecond
	res := p.Preflight(context.Background(), []string{"https://slow.example/a"})
	close(f.block)
	assert.Equal(t, VerdictInconclusive, res[0].Verdict)
	rec, err := p.Store.Get("https://slow.example/a")
	require.NoError(t, err)
	assert.Nil(t, rec, "no record, no one-hour hold: the lookahead's walk will reach it")

	l := &Lookahead{Prober: p, Store: p.Store, Now: clock.Now}
	f.block = nil
	l.Observe([]string{"https://o/cur", "https://o/next", "https://slow.example/a"}, 0)
	f.answers["https://o/cur"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	f.answers["https://o/next"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	for range 3 {
		l.step(context.Background(), nil)
	}
	assert.Equal(t, 2, f.count("https://slow.example/a"), "probed again right away by the walk")
}

// Fix 4: the unreachable streak.
func unreachableResult(kind Unreachable, status int) ProbeResult {
	return ProbeResult{Verdict: VerdictInconclusive, Status: status, Reason: ReasonTransport, Err: errors.New("no answer"), Unreachable: kind, NetworkHealthy: true}
}

func TestRules_UnreachableStreakConcludes(t *testing.T) {
	cases := []struct {
		name       string
		kind       Unreachable
		status     int
		wantReason Reason
	}{
		{"no answer (ipfs.infura.io)", UnreachableNoAnswer, 0, ReasonUnreachable},
		{"server error", UnreachableServer, 502, ReasonUnreachable},
		{"dns", UnreachableDNS, 0, ReasonDNS},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{t: time.Now()}
			s := openTestStore(t, clock)
			src := "https://dead.example/a"
			for i := 0; i < 2; i++ {
				rec, err := s.ApplyProbe(src, unreachableResult(tc.kind, tc.status))
				require.NoError(t, err)
				assert.NotEqual(t, StateBroken, rec.State)
				clock.Advance(13 * time.Hour)
			}
			rec, err := s.ApplyProbe(src, unreachableResult(tc.kind, tc.status))
			require.NoError(t, err)
			assert.Equal(t, StateBroken, rec.State, "3 counted attempts over 24 h")
			assert.Equal(t, tc.wantReason, rec.Reason)
			assert.Equal(t, CategorySourceMissing, rec.Category)
			assert.Equal(t, tc.status, rec.HTTPStatus)
		})
	}
}

func TestRules_AnyAnswerEndsTheUnreachableStreak(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	src := "https://slow.example/a"
	_, err := s.ApplyProbe(src, unreachableResult(UnreachableNoAnswer, 0))
	require.NoError(t, err)
	clock.Advance(13 * time.Hour)
	_, err = s.ApplyProbe(src, unreachableResult(UnreachableNoAnswer, 0))
	require.NoError(t, err)
	clock.Advance(time.Hour)
	// A slow origin that answered once — even a 429 — is alive.
	rec, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictInconclusive, Status: 429, Reason: ReasonHTTPStatus, Err: errors.New("HTTP 429")})
	require.NoError(t, err)
	assert.Zero(t, rec.UnreachableFailures)
	clock.Advance(13 * time.Hour)
	rec, err = s.ApplyProbe(src, unreachableResult(UnreachableNoAnswer, 0))
	require.NoError(t, err)
	assert.NotEqual(t, StateBroken, rec.State, "the streak restarted")
}

func TestRules_ProbeClassifiesUnreachable(t *testing.T) {
	cases := []struct {
		name string
		ans  scriptedAnswer
		want Unreachable
	}{
		{"timeout before a response", fail(FetchErrTimeout), UnreachableNoAnswer},
		{"refused or reset", fail(FetchErrTransport), UnreachableNoAnswer},
		{"servfail", fail(FetchErrDNSUnresolved), UnreachableDNS},
		{"502", ok(502, "", nil, -1), UnreachableServer},
		{"503 with Retry-After is a pause, not a failure", ok(503, "", nil, -1, "Retry-After", "60"), ""},
		{"429 is an answer", ok(http.StatusTooManyRequests, "", nil, -1), ""},
		{"403 is an answer", ok(403, "", nil, -1), ""},
		{"guard refusal is policy", fail(FetchErrGuard), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newTableFetcher()
			f.answers["https://o/a"] = tc.ans
			res := newProbe(f).ProbeOne(context.Background(), "https://o/a")
			assert.Equal(t, tc.want, res.Unreachable)
		})
	}
}

func TestRules_LegacyDNSStreakIsMigrated(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	since := clock.t.Add(-20 * time.Hour)
	raw := []byte(`{"v":1,"source":"https://gone.example/a","dnsFailSince":"` + since.UTC().Format(time.RFC3339Nano) + `","dnsFailures":16,"checkedAt":"0001-01-01T00:00:00Z","recheckAfter":"0001-01-01T00:00:00Z","plays":{"ok":0,"fail":0},"build":{}}`)
	rec, err := s.decode(raw)
	require.NoError(t, err)
	require.NotNil(t, rec.UnreachableSince)
	assert.Equal(t, since.UTC(), rec.UnreachableSince.UTC())
	assert.Equal(t, 16, rec.UnreachableFailures)
	assert.Nil(t, rec.LegacyDNSFailSince, "never written again")
}

// Fix 5: the play layer backs off like L0.
func TestRules_PlayVerdictBacksOffAfterAWeekOfReconclusions(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(infuraPlay()...)
	f.emit(infuraPlay()...)
	rec := f.record()
	require.Equal(t, StateBroken, rec.State)
	assert.Equal(t, 1, rec.ConsecutiveBroken)
	assert.WithinDuration(t, rec.CheckedAt.Add(RecheckBroken), rec.RecheckAfter, time.Second)

	// Inside the window a replay (repeat-one) does not advance the backoff.
	f.emit(infuraPlay()...)
	assert.Equal(t, 1, f.record().ConsecutiveBroken)

	for day := 2; day <= BrokenBackoffAfter; day++ {
		f.clock.Advance(RecheckBroken + time.Minute)
		f.emit(infuraPlay()...)
		rec = f.record()
		assert.Equal(t, day, rec.ConsecutiveBroken, "day %d", day)
	}
	assert.WithinDuration(t, rec.CheckedAt.Add(RecheckBrokenBackoff), rec.RecheckAfter, time.Second, "a week of failures: retried weekly")
}

// F22 (PR #438 review): a failure must never vouch for its own network.
// Evidence is read before the current answer is added, never comes from
// the failing host, and a 5xx is not end-to-end evidence at all.
func TestRules_UnreachableNeedsIndependentNetworkEvidence(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://bad.example/a"] = ok(502, "", nil, -1)
	f.answers["https://proxy-b.example/a"] = ok(502, "", nil, -1)
	f.answers["https://good.example/a"] = ok(200, "image/png", minimalPNG(1, 1), -1)
	p := newPassProber(t, f, clock)

	for i := 0; i < 3; i++ {
		res := p.ProbeAndRecord(context.Background(), "https://bad.example/a")
		assert.Equal(t, UnreachableServer, res.Unreachable)
		assert.False(t, res.NetworkHealthy, "attempt %d: a 502 cannot vouch for itself", i)
		clock.Advance(13 * time.Hour)
	}
	rec, err := p.Store.Get("https://bad.example/a")
	require.NoError(t, err)
	assert.NotEqual(t, StateBroken, rec.State, "26 h of self-corroborated 502s never conclude")

	res := p.ProbeAndRecord(context.Background(), "https://proxy-b.example/a")
	assert.False(t, res.NetworkHealthy, "another host's 502 is not evidence either: an intermediary answers 502 for every host")

	p.ProbeAndRecord(context.Background(), "https://good.example/a")
	res = p.ProbeAndRecord(context.Background(), "https://bad.example/a")
	assert.True(t, res.NetworkHealthy, "an end-to-end answer from a different host is")

	f.answers["https://good.example/b"] = ok(504, "", nil, -1)
	res = p.ProbeAndRecord(context.Background(), "https://good.example/b")
	assert.False(t, res.NetworkHealthy, "the failing host's own earlier answer does not count for it")
}

// slowFetcher answers after delay, or gives up when the context ends.
type slowFetcher struct {
	delay time.Duration
	ans   scriptedAnswer
}

func (f slowFetcher) Fetch(ctx context.Context, _ FetchRequest) (*FetchResponse, error) {
	select {
	case <-time.After(f.delay):
		return f.ans.resp, f.ans.err
	case <-ctx.Done():
		return nil, &FetchError{Kind: FetchErrTimeout, Err: ctx.Err()}
	}
}

// Background probes get BackgroundItemTimeout; the cast pass keeps the
// Probe's short ItemTimeout. A gateway that answers its 404 slowly is
// only a timeout at cast, but a real answer in the background.
func TestRules_BackgroundProbesWaitLongerThanTheCast(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := slowFetcher{delay: 200 * time.Millisecond, ans: ok(404, "text/html", []byte("404 - Page cannot be found"), -1)}
	p := newPassProber(t, f, clock)
	p.Probe.ItemTimeout = 50 * time.Millisecond
	p.BackgroundItemTimeout = 2 * time.Second

	cast := p.Preflight(context.Background(), []string{"https://arweave.example/tx"})
	assert.Equal(t, VerdictInconclusive, cast[0].Verdict, "at cast the slow answer is a timeout")
	rec, err := p.Store.Get("https://arweave.example/tx")
	require.NoError(t, err)
	assert.Nil(t, rec, "the cast's short deadline is not the source's failure: nothing recorded, no hold, no issue")

	bg := p.ProbeAndRecord(context.Background(), "https://arweave.example/tx")
	assert.Equal(t, 404, bg.Status, "in the background the gateway's real answer arrives")
	assert.Equal(t, VerdictDead, bg.Verdict)
}

func TestRules_BackgroundTimeoutDefaults(t *testing.T) {
	assert.Equal(t, 30*time.Second, DefaultBackgroundItemTimeout)
	assert.Equal(t, 5*time.Second, DefaultItemTimeout, "the cast keeps its short deadline")
}
