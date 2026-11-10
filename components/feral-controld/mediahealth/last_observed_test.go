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
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.True(t, tc.rec.lastObservedAt().Equal(tc.want), "got %v", tc.rec.lastObservedAt())
		})
	}
}
