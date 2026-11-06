package mediahealth

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func transient(status int, unreachable Unreachable, healthy bool) ProbeResult {
	return ProbeResult{Verdict: VerdictInconclusive, Status: status, Reason: ReasonTransport, Err: errors.New("transient"), Unreachable: unreachable, NetworkHealthy: healthy}
}

// Every row of the issue table, set by a probe.
func TestIssue_ProbeTable(t *testing.T) {
	cases := []struct {
		name string
		res  ProbeResult
		want Issue
	}{
		{"no answer", transient(0, UnreachableNoAnswer, true), IssueNoAnswer},
		{"dns", transient(0, UnreachableDNS, true), IssueDNS},
		{"server error", transient(502, UnreachableServer, true), IssueServerError},
		{"429", transient(429, "", false), IssueRateLimited},
		{"503 with Retry-After", ProbeResult{Verdict: VerdictInconclusive, Status: 503, Reason: ReasonHTTPStatus, RetryAfter: time.Minute}, IssueRateLimited},
		{"401", transient(401, "", false), IssueAccessDenied},
		{"403", transient(403, "", false), IssueAccessDenied},
		{"407", transient(407, "", false), IssueAccessDenied},
		{"406 is about the request", transient(406, "", false), ""},
		{"no answer during an outage", transient(0, UnreachableNoAnswer, false), ""},
		{"dns during an outage", transient(0, UnreachableDNS, false), ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{t: time.Now()}
			s := openTestStore(t, clock)
			rec, err := s.ApplyProbe("https://o/a", tc.res)
			require.NoError(t, err)
			assert.Equal(t, tc.want, rec.Issue)
			if tc.want != "" {
				require.NotNil(t, rec.IssueSince)
				assert.WithinDuration(t, clock.t, *rec.IssueSince, 0)
			}
		})
	}
}

func TestIssue_SinceSurvivesAChangeOfKindAndResetsOnClear(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	start := clock.t
	_, err := s.ApplyProbe("https://o/a", transient(0, UnreachableNoAnswer, true))
	require.NoError(t, err)
	clock.Advance(time.Hour)
	rec, err := s.ApplyProbe("https://o/a", transient(502, UnreachableServer, true))
	require.NoError(t, err)
	assert.Equal(t, IssueServerError, rec.Issue, "the latest attempt names the issue")
	assert.WithinDuration(t, start, *rec.IssueSince, 0, "but the trouble began at the first")

	clock.Advance(time.Hour)
	rec, err = s.ApplyProbe("https://o/a", transient(0, UnreachableNoAnswer, false))
	require.NoError(t, err)
	assert.Equal(t, IssueServerError, rec.Issue, "an attempt that says nothing (an outage) leaves the issue standing")

	clock.Advance(time.Hour)
	rec, err = s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	assert.Empty(t, rec.Issue, "an answer clears it")
	assert.Nil(t, rec.IssueSince)

	rec, err = s.ApplyProbe("https://o/a", transient(429, "", false))
	require.NoError(t, err)
	assert.WithinDuration(t, clock.t, *rec.IssueSince, 0, "a new run of trouble starts fresh")
}

func TestIssue_AVerdictReplacesTheIssue(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := openTestStore(t, clock)
	_, err := s.ApplyProbe("https://o/a", transient(0, UnreachableNoAnswer, true))
	require.NoError(t, err)
	rec, err := s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.Empty(t, rec.Issue, "a broken item shows its category, not an issue")

	// The unreachable streak concluding also clears the issue.
	s2 := openTestStore(t, clock)
	for i := 0; i < 3; i++ {
		rec, err = s2.ApplyProbe("https://o/b", transient(0, UnreachableNoAnswer, true))
		require.NoError(t, err)
		clock.Advance(13 * time.Hour)
	}
	assert.Equal(t, StateBroken, rec.State)
	assert.Empty(t, rec.Issue)
}

func TestIssue_PlayFailedThenVerdictOrHeal(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(infuraPlay()...)
	rec := f.record()
	assert.Equal(t, IssuePlayFailed, rec.Issue, "one counted failed play")
	f.emit(infuraPlay()...)
	rec = f.record()
	assert.Equal(t, StateBroken, rec.State)
	assert.Empty(t, rec.Issue, "the verdict replaces it")

	g := newPlayFeed(t, true)
	g.emit(infuraPlay()...)
	g.emit(Outcome{Kind: OutcomeOK, Stage: StageReady, MediaType: "image", Attempt: 1})
	assert.Empty(t, g.record().Issue, "a real success clears it")

	h := newPlayFeed(t, true)
	h.emit(infuraPlay()...)
	h.emit(iframeReady)
	assert.Equal(t, IssuePlayFailed, h.record().Issue, "an iframe ready proves nothing: the issue stands")
}

func TestIssue_OfflinePlaysWriteNoIssue(t *testing.T) {
	f := newPlayFeed(t, false)
	f.emit(infuraPlay()...)
	assert.Empty(t, f.record().Issue)
}

// The kiosk's own evidence wins over the probe's.
func TestIssue_KioskServedDocumentClearsProbeSideIssues(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit()
	for _, res := range []ProbeResult{transient(403, "", false), transient(0, UnreachableNoAnswer, true)} {
		_, err := f.s.Store().ApplyProbe(f.src, res)
		require.NoError(t, err)
		f.s.ObserveNavigation(Navigation{URL: f.src + "?display_mode=crop", Status: 200})
		rec := f.record()
		assert.Empty(t, rec.Issue, "the origin served the kiosk")
		assert.Nil(t, rec.UnreachableSince, "and the unreachable streak ends")
		assert.NotEqual(t, StateOK, rec.State, "a 200 is not a success: the work may still fail to render")
	}

	f.emit(infuraPlay()...)
	require.Equal(t, IssuePlayFailed, f.record().Issue)
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	assert.Equal(t, IssuePlayFailed, f.record().Issue, "a play failure is about rendering: a 200 does not clear it")
}

// The cast's 5 s deadline is not a real attempt.
func TestIssue_CastTimeoutWritesNothing(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://slow.example/a"] = fail(FetchErrTimeout)
	f.answers["https://refused.example/a"] = fail(FetchErrTransport)
	p := newPassProber(t, f, clock)
	p.Online = func() bool { return true }
	p.Preflight(context.Background(), []string{"https://slow.example/a", "https://refused.example/a"})
	rec, err := p.Store.Get("https://slow.example/a")
	require.NoError(t, err)
	assert.Nil(t, rec, "no hold, no streak, no issue")
	rec, err = p.Store.Get("https://refused.example/a")
	require.NoError(t, err)
	require.NotNil(t, rec, "a refused connection is a real answer at any deadline")
	assert.Equal(t, IssueNoAnswer, rec.Issue)

	res := p.ProbeAndRecord(context.Background(), "https://slow.example/a")
	assert.True(t, res.Recorded, "a background timeout is a real attempt")
	rec, err = p.Store.Get("https://slow.example/a")
	require.NoError(t, err)
	assert.Equal(t, IssueNoAnswer, rec.Issue)
}

func TestIssue_Surfaces(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 6, 14, 0, 0, 0, time.UTC)}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	st := s.Store()
	_, err := st.ApplyProbe("https://o/flaky", transient(0, UnreachableNoAnswer, true))
	require.NoError(t, err)
	_, err = st.ApplyProbe("https://o/throttled", ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	_, err = st.ApplyProbe("https://o/throttled", transient(429, "", false))
	require.NoError(t, err)
	_, err = st.ApplyProbe("https://o/dead", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	list := items("https://o/flaky", "https://o/throttled", "https://o/dead", "https://o/never")
	for i := range list {
		// Real ids and titles: the shared helper reuses the source for both.
		list[i].ID, list[i].Title = "id-"+strconv.Itoa(i), "work "+strconv.Itoa(i)
	}

	health := s.ItemHealths(list, nil)
	assert.Equal(t, ItemHealth{State: StateUnknown, Issue: IssueNoAnswer}, health[0])
	assert.Equal(t, ItemHealth{State: StateOK, Issue: IssueRateLimited}, health[1], "plays fine, host throttles the checks")
	assert.Empty(t, health[2].Issue, "broken shows its category instead")
	assert.Equal(t, ItemHealth{State: StateUnknown}, health[3])
	raw, err := json.Marshal(health[3])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "issue", "never-checked items carry no issue key")

	rows, err := s.Report(list, nil)
	require.NoError(t, err)
	assert.Equal(t, IssueNoAnswer, rows[0].Issue)
	assert.Equal(t, "2026-11-06T14:00:00Z", rows[0].IssueSince)
	assert.Equal(t, "2026-11-06T15:00:00Z", rows[0].NextCheckAt, "the one-hour transient hold")
	assert.Equal(t, "2026-11-07T14:00:00Z", rows[2].NextCheckAt, "the broken recheck")
	assert.Empty(t, rows[2].IssueSince)
	assert.Empty(t, rows[3].NextCheckAt, "nothing scheduled for a never-checked item")
	for _, row := range rows {
		raw, err := json.Marshal(row)
		require.NoError(t, err)
		assert.NotContains(t, string(raw), "https://", "no source URL on the surface")
	}
}

func TestIssue_FailedPlaysOnTheReport(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(infuraPlay()...)
	rows, err := f.s.Report(items(f.src), nil)
	require.NoError(t, err)
	assert.Equal(t, 1, rows[0].FailedPlays)
	assert.Equal(t, IssuePlayFailed, rows[0].Issue)
}
