package mediahealth

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

type testClock struct{ t time.Time }

func (c *testClock) Now() time.Time          { return c.t }
func (c *testClock) Advance(d time.Duration) { c.t = c.t.Add(d) }

func openTestStore(t *testing.T, clock *testClock) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "media-health", "store.db"), Options{
		Now:    clock.Now,
		Build:  Build{Controld: "test"},
		Logger: zap.NewNop(),
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestStore_ApplyProbe_AliveDeadTransient(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	const src = "https://origin.example/a.png"

	rec, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "image/png", Sniffed: "image/png", WireURL: src})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State)
	assert.Equal(t, LayerL0, rec.Layer)
	assert.Equal(t, "origin.example", rec.Host)
	assert.Equal(t, clock.t.Add(RecheckOK), rec.RecheckAfter)
	assert.False(t, rec.Conclusive(clock.t))
	assert.Equal(t, "test", rec.Build.Controld)

	clock.Advance(time.Hour)
	rec, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictInconclusive, Status: 503, Reason: ReasonHTTPStatus, Detail: "HTTP 503"})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "a transient attempt never changes the verdict")
	assert.Equal(t, "HTTP 503", rec.LastError)
	assert.Equal(t, clock.t.Add(-time.Hour).Add(RecheckOK), rec.RecheckAfter, "an existing verdict keeps its own recheck")

	clock.Advance(time.Hour)
	rec, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus, Detail: "HTTP 404"})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, CategorySourceMissing, rec.Category)
	assert.Equal(t, 1, rec.ConsecutiveBroken)
	assert.Equal(t, clock.t.Add(RecheckBroken), rec.RecheckAfter)
	assert.True(t, rec.Conclusive(clock.t))
	assert.False(t, rec.Conclusive(clock.t.Add(RecheckBroken)), "past the window the verdict no longer skips the work")

	// Guard refusals and inline items write nothing.
	before, _ := s.Get(src)
	rec, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictInconclusive, Err: &FetchError{Kind: FetchErrGuard, Err: errors.New("x")}})
	require.NoError(t, err)
	assert.Equal(t, before.LastError, rec.LastError)
}

func TestStore_ApplyProbe_BrokenBackoff(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	const src = "https://origin.example/a"
	var rec *Record
	for i := 0; i < BrokenBackoffAfter; i++ {
		var err error
		rec, err = s.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 410, Reason: ReasonHTTPStatus})
		require.NoError(t, err)
		clock.Advance(RecheckBroken)
	}
	assert.Equal(t, BrokenBackoffAfter, rec.ConsecutiveBroken)
	assert.Equal(t, RecheckBrokenBackoff, rec.RecheckAfter.Sub(rec.CheckedAt))

	rec, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "a fixed source heals")
	assert.Equal(t, 0, rec.ConsecutiveBroken)
}

func TestStore_TransientOnlyRecordSchedulesRetryButHasNoVerdict(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	rec, err := s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictInconclusive, Reason: ReasonTransport, Err: errors.New("timeout")})
	require.NoError(t, err)
	assert.Empty(t, rec.State)
	assert.Equal(t, clock.t.Add(RetryTransient), rec.RecheckAfter)
	health, err := s.ItemHealths([]string{"https://o/a", "https://o/never"})
	require.NoError(t, err)
	assert.Equal(t, StateUnknown, health[0].State)
	assert.Equal(t, StateUnknown, health[1].State)
	hints, err := s.Hints([]string{"https://o/a"}, clock.t)
	require.NoError(t, err)
	assert.Empty(t, hints)
}

func TestStore_ApplyOutcome_Corroboration(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	const src = "https://origin.example/v.mp4"

	rec, err := s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeFail, Stage: StageDecode, Code: "decode", MediaType: "video"})
	require.NoError(t, err)
	assert.Empty(t, rec.State, "one failure is recorded, not concluded")
	assert.Equal(t, 1, rec.Plays.Fail)
	assert.Equal(t, "decode", rec.Plays.LastFail.Code)

	rec, err = s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeFail, Stage: StageDecode, Code: "decode"})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "a second failure in a distinct play concludes")
	assert.Equal(t, LayerPlay, rec.Layer)
	assert.Equal(t, CategoryRenderFailed, rec.Category)
	assert.Equal(t, ReasonRenderDecode, rec.Reason)
	assert.True(t, rec.Conclusive(clock.t))

	rec, err = s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "video"})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "a play success heals")
	assert.Equal(t, 0, rec.Plays.ConsecutiveFail)
	assert.Equal(t, 1, rec.Plays.OK)
	assert.Equal(t, "video", rec.MediaType)
}

func TestStore_ApplyOutcome_SingleFailCorroboratedByL0(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	const src = "https://origin.example/a"
	_, err := s.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	rec, err := s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeFail, Stage: StageExhausted, Attempt: 3})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, LayerPlay, rec.Layer, "one play failure plus a standing L0 verdict is corroborated")
	assert.Equal(t, ReasonRenderExhausted, rec.Reason)
}

func TestStore_ApplyOutcome_DetectStatusCategories(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	for _, tc := range []struct {
		code string
		want Category
	}{{"404", CategorySourceMissing}, {"403", CategorySourceBlocked}, {"network", CategoryRenderFailed}} {
		src := "https://origin.example/" + tc.code
		_, _ = s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: tc.code})
		rec, err := s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: tc.code})
		require.NoError(t, err)
		assert.Equal(t, tc.want, rec.Category, tc.code)
	}
}

func TestStore_ApplyOutcome_DeviceLocalAndSkipNeverConclude(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	const src = "https://origin.example/a"
	for i := 0; i < 5; i++ {
		rec, err := s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeFail, Stage: StageWebGL})
		require.NoError(t, err)
		assert.Empty(t, rec.State)
		assert.Equal(t, 0, rec.Plays.Fail, "webgl is recorded on lastFail only")
		assert.Equal(t, StageWebGL, rec.Plays.LastFail.Stage)
	}
	rec, err := s.ApplyOutcome(Outcome{Source: src, Kind: OutcomeSkip, Stage: StageHint})
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Skipped)
	assert.Empty(t, rec.State)
}

func TestStore_HintsAndItemHealths(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	_, _ = s.ApplyProbe("https://o/ok", ProbeResult{Verdict: VerdictAlive, Status: 200})
	_, _ = s.ApplyProbe("https://o/dead", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	_, _ = s.ApplyProbe("https://o/old", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})

	later := clock.t.Add(RecheckBroken + time.Minute)
	hints, err := s.Hints([]string{"https://o/ok", "https://o/dead", "https://o/old", "https://o/none"}, clock.t)
	require.NoError(t, err)
	assert.Len(t, hints, 2)
	assert.Equal(t, Hint{State: StateBroken, Category: CategorySourceMissing, Reason: ReasonHTTPStatus}, hints["https://o/dead"])

	hints, err = s.Hints([]string{"https://o/dead"}, later)
	require.NoError(t, err)
	assert.Empty(t, hints, "a broken verdict past its recheck window stops hinting")

	health, err := s.ItemHealths([]string{"https://o/ok", "https://o/old", "https://o/none"})
	require.NoError(t, err)
	assert.Equal(t, ItemHealth{State: StateOK}, health[0])
	assert.Equal(t, ItemHealth{State: StateBroken, Category: CategorySourceMissing, Reason: ReasonHTTPStatus, Layer: LayerL0}, health[1], "status still shows the last observation")
	assert.Equal(t, ItemHealth{State: StateUnknown}, health[2])

	rows, err := s.Report([]ReportInput{{ID: "a", Title: "A", Source: "https://o/dead"}, {ID: "b", Source: "https://o/none"}})
	require.NoError(t, err)
	assert.Equal(t, "a", rows[0].ID)
	assert.Equal(t, StateBroken, rows[0].State)
	assert.NotEmpty(t, rows[0].CheckedAt)
	assert.Equal(t, 1, rows[1].Index)
	assert.Equal(t, StateUnknown, rows[1].State)
}

func TestStore_DueAndRecheckIndex(t *testing.T) {
	clock := &testClock{t: time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC)}
	s := openTestStore(t, clock)
	_, _ = s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	_, _ = s.ApplyProbe("https://o/b", ProbeResult{Verdict: VerdictAlive, Status: 200})

	due, err := s.Due(clock.t.Add(RecheckBroken), 10)
	require.NoError(t, err)
	assert.Equal(t, []string{"https://o/a"}, due)

	// Re-probing a moves its index entry; the old one must not linger.
	clock.Advance(RecheckBroken)
	_, _ = s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	due, err = s.Due(clock.t, 10)
	require.NoError(t, err)
	assert.Empty(t, due)
	due, err = s.Due(clock.t.Add(RecheckOK), 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"https://o/a", "https://o/b"}, due)

	n, err := s.Count()
	require.NoError(t, err)
	assert.Equal(t, 2, n)
}

func TestStore_DiskGuardRefusesWritesBelowReserve(t *testing.T) {
	clock := &testClock{t: time.Now()}
	free := int64(10 << 30)
	s, err := Open(filepath.Join(t.TempDir(), "mh", "store.db"), Options{
		Now:          clock.Now,
		MinFreeBytes: 5 << 30,
		FreeBytes:    func(string) (int64, error) { return free, nil },
	})
	require.NoError(t, err)
	defer func() { _ = s.Close() }()

	_, err = s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
	free = 1 << 30
	_, err = s.ApplyProbe("https://o/b", ProbeResult{Verdict: VerdictAlive, Status: 200})
	assert.ErrorIs(t, err, ErrLowDisk)
	rec, err := s.Get("https://o/b")
	require.NoError(t, err)
	assert.Nil(t, rec, "a refused write leaves no record")
	free = 10 << 30
	_, err = s.ApplyProbe("https://o/b", ProbeResult{Verdict: VerdictAlive, Status: 200})
	require.NoError(t, err)
}

func TestStore_SurvivesReopenAndRefusesNewerRecords(t *testing.T) {
	clock := &testClock{t: time.Now()}
	path := filepath.Join(t.TempDir(), "mh", "store.db")
	s, err := Open(path, Options{Now: clock.Now})
	require.NoError(t, err)
	_, err = s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	require.NoError(t, s.Close())

	s, err = Open(path, Options{Now: clock.Now})
	require.NoError(t, err)
	defer func() { _ = s.Close() }()
	rec, err := s.Get("https://o/a")
	require.NoError(t, err)
	require.NotNil(t, rec)
	assert.Equal(t, StateBroken, rec.State)

	// A record from a newer binary is neither read nor rewritten (put always
	// stamps recordVersion, so the newer version is forced in directly).
	require.NoError(t, s.db.Update(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketItems).Get([]byte(Key("https://o/a")))
		patched := []byte(`{"v":99` + string(raw[len(`{"v":1`):]))
		return tx.Bucket(bucketItems).Put([]byte(Key("https://o/a")), patched)
	}))
	_, err = s.Get("https://o/a")
	assert.ErrorIs(t, err, ErrNewerRecord)
	_, err = s.ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictAlive, Status: 200})
	assert.ErrorIs(t, err, ErrNewerRecord)
	health, err := s.ItemHealths([]string{"https://o/a"})
	require.NoError(t, err)
	assert.Equal(t, StateUnknown, health[0].State, "an unreadable record reads as unknown for the surfaces")
}

func TestDestroy_RemovesTheStoreDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mh", "store.db")
	s, err := Open(path, Options{})
	require.NoError(t, err)
	require.NoError(t, s.Close())
	require.NoError(t, Destroy(path))
	_, err = Open(path, Options{})
	require.NoError(t, err, "reopening after destroy starts an empty store")
}

func TestKey_IsSHA256Hex(t *testing.T) {
	assert.Len(t, Key("https://o/a"), 64)
	assert.NotEqual(t, Key("https://o/a"), Key("https://o/a#x"))
}
