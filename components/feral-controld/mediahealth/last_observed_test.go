package mediahealth

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// getMediaHealth's checkedAt is the verdict's age; lastObservedAt is when
// the device last looked at the source by any means. Office FF1,
// 2026-11-10: "When You Gotta Go" had an ok verdict from 10:12 and failed
// its play at 14:05 — the app showed "last checked 4 hours ago" beside an
// issue "started 14 minutes ago".
func TestReport_LastObservedAtIsTheLatestLook(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 10, 3, 12, 0, 0, time.UTC)}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	st := s.Store()

	played := "https://o/slow-video"
	_, err := st.ApplyProbe(played, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "video/quicktime", Sniffed: "video/quicktime"})
	require.NoError(t, err)
	verdictAt := clock.Now()

	rendered := "https://o/html"
	seedSoftware(t, st, rendered)

	clock.Advance(4 * time.Hour)
	_, err = st.ApplyOutcome(Outcome{Seq: 1, Source: played, Kind: OutcomeFail, Stage: StageWatchdog, Code: "transfer", Attempt: 1, MediaType: "video"}, true)
	require.NoError(t, err)
	failedAt := clock.Now()

	clock.Advance(time.Hour)
	_, err = st.ApplyRender(rendered, RenderResult{Verdict: RenderStalled, Err: "main document HTTP 504", MainStatus: 504})
	require.NoError(t, err)
	renderedAt := clock.Now()

	list := items(played, rendered, "https://o/never")
	rows, err := s.Report(list, nil)
	require.NoError(t, err)

	assert.Equal(t, StateOK, rows[0].State, "one failed play leaves the verdict")
	assert.Equal(t, verdictAt.Format(time.RFC3339), rows[0].CheckedAt, "the verdict's age")
	assert.Equal(t, failedAt.Format(time.RFC3339), rows[0].LastObservedAt, "the failed play was a look")
	assert.Equal(t, IssuePlayFailed, rows[0].Issue)
	assert.Equal(t, failedAt.Format(time.RFC3339), rows[0].IssueSince)

	assert.Equal(t, renderedAt.Format(time.RFC3339), rows[1].LastObservedAt, "a render with no evidence was a look too")
	assert.Equal(t, verdictAt.Format(time.RFC3339), rows[1].CheckedAt)

	assert.Empty(t, rows[2].LastObservedAt, "never observed")
	raw, err := json.Marshal(rows[2])
	require.NoError(t, err)
	assert.NotContains(t, string(raw), "lastObservedAt")
}

// lastObservedAt never reads earlier than checkedAt, and each kind of look
// counts.
func TestRecord_LastObservedAt(t *testing.T) {
	base := time.Date(2026, 11, 10, 3, 0, 0, 0, time.UTC)
	at := func(h int) *time.Time { t := base.Add(time.Duration(h) * time.Hour); return &t }
	cases := []struct {
		name string
		rec  Record
		want time.Time
	}{
		{"verdict only", Record{CheckedAt: base}, base},
		{"a transient probe attempt", Record{CheckedAt: base, LastErrorAt: at(1)}, *at(1)},
		{"a play that rendered", Record{CheckedAt: base, Plays: PlayStats{LastOKAt: at(2)}}, *at(2)},
		{"a play that failed", Record{CheckedAt: base, Plays: PlayStats{LastFailAt: at(3)}}, *at(3)},
		{"a wall frame", Record{CheckedAt: base, Wall: &WallObservation{At: *at(4)}}, *at(4)},
		{"a nightly render", Record{CheckedAt: base, L1: &L1Verdict{At: *at(5)}}, *at(5)},
		{"older looks never move it back", Record{CheckedAt: *at(6), Plays: PlayStats{LastFailAt: at(1)}}, *at(6)},
		{"never observed", Record{}, time.Time{}},
		{"observedAt wins over times a non-look set", Record{CheckedAt: base, ObservedAt: *at(1), LastErrorAt: at(3), Plays: PlayStats{LastFailAt: at(4)}}, *at(1)},
		{"observedAt never reads before checkedAt", Record{CheckedAt: *at(2), ObservedAt: *at(1)}, *at(2)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, tc.rec.lastObservedAt().Equal(tc.want), "got %v", tc.rec.lastObservedAt())
		})
	}
}

// A look that leaves the verdict alone still advances lastObservedAt, and
// it never moves back: a suspect at 09:00, a 503 probe at 10:00, a valid
// probe at 11:00 (which clears lastErrorAt and keeps the suspect) reads
// 11:00 (PR #451 review, F1).
func TestReport_ALookThatKeepsTheVerdictStillCounts(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	st := s.Store()
	src := "https://o/suspect"
	seedSoftware(t, st, src)
	for i := 0; i < 2; i++ {
		_, err := st.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.00001})
		require.NoError(t, err)
	}
	rec, err := st.Get(src)
	require.NoError(t, err)
	require.Equal(t, StateSuspect, rec.State)

	clock.Advance(time.Hour)
	_, err = st.ApplyProbe(src, transient(503, "", true))
	require.NoError(t, err)
	clock.Advance(time.Hour)
	_, err = st.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html"})
	require.NoError(t, err)

	rows, err := s.Report(items(src), nil)
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rows[0].State, "valid bytes never heal a render verdict")
	assert.Equal(t, "2026-11-10T04:00:00Z", rows[0].LastObservedAt, "the valid probe at 11:00 local was a look")
	assert.Equal(t, "2026-11-10T02:00:00Z", rows[0].CheckedAt, "the verdict's age is unchanged")
}

// The same over a render-layer broken verdict (renderConclusive on).
func TestReport_AValidProbeOverARenderBrokenIsALook(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)}
	st := openConclusiveStore(t, clock)
	src := "https://o/blank-html"
	seedSoftware(t, st, src)
	for i := 0; i < 2; i++ {
		_, err := st.ApplyOutcome(wallBlank(src), true)
		require.NoError(t, err)
		clock.Advance(time.Minute)
	}
	rec, err := st.Get(src)
	require.NoError(t, err)
	require.Equal(t, StateBroken, rec.State)
	clock.Advance(3 * time.Hour)
	rec, err = st.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html"})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.True(t, rec.lastObservedAt().Equal(clock.Now()))
}

// What is not a look: an answer served from the store, a player skip on a
// hint, a device-local failure, and an attempt with no answer during an
// outage.
func TestReport_NotALook(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)}
	st := openTestStore(t, clock)
	src := "https://o/work"
	_, err := st.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "video/mp4", Sniffed: "video/mp4"})
	require.NoError(t, err)
	verdictAt := clock.Now()

	clock.Advance(time.Hour)
	_, err = st.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, FromStore: true})
	require.NoError(t, err)
	_, err = st.ApplyOutcome(Outcome{Seq: 1, Source: src, Kind: OutcomeSkip, Stage: StageHint}, true)
	require.NoError(t, err)
	_, err = st.ApplyOutcome(Outcome{Seq: 2, Source: src, Kind: OutcomeFail, Stage: StageWebGL, Code: "context_lost", Attempt: 1}, true)
	require.NoError(t, err)
	_, err = st.ApplyProbe(src, transient(0, UnreachableNoAnswer, false)) // the device's own network is down
	require.NoError(t, err)

	rec, err := st.Get(src)
	require.NoError(t, err)
	assert.True(t, rec.lastObservedAt().Equal(verdictAt), "got %v", rec.lastObservedAt())
}
