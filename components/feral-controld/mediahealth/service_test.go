package mediahealth

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestService(t *testing.T, f Fetcher, clock *testClock, manifest string) *Service {
	t.Helper()
	p := newPassProber(t, f, clock)
	path := filepath.Join(t.TempDir(), "ffos-player-contract.json")
	if manifest != "" {
		require.NoError(t, os.WriteFile(path, []byte(manifest), 0o600))
	}
	return New(Config{Store: p.Store, Prober: p, Contract: &PlayerContract{Path: path}, Now: clock.Now, Online: func() bool { return true }})
}

const manifestWithContract = `{"contracts":{"mediaHealth":{"version":1}}}`

func items(sources ...string) []dp1playlist.PlaylistItem {
	out := make([]dp1playlist.PlaylistItem, len(sources))
	for i, s := range sources {
		out[i] = dp1playlist.PlaylistItem{ID: s, Title: "t" + s, Source: s}
	}
	return out
}

func TestService_OutcomesDrainIntoTheStore(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()

	list := items("https://o/a", "https://o/b")
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image"},
		{Seq: 2, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"},
	}})
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image"},
		{Seq: 2, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"},
		{Seq: 3, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image"},
	}})
	health := s.ItemHealths(list, nil)
	require.Len(t, health, 2)
	assert.Equal(t, StateBroken, health[0].State)
	assert.Equal(t, CategoryRenderFailed, health[0].Category)
	assert.Equal(t, StateOK, health[1].State)

	hints := s.Hints([]string{"https://o/a", "https://o/b"})
	assert.Len(t, hints, 1)
	assert.Contains(t, hints, "https://o/a")
	assert.True(t, s.PlayerSupportsHints())

	rows, err := s.Report(list, nil)
	require.NoError(t, err)
	assert.Equal(t, 2, rows[0].Plays.Fail)
	assert.Equal(t, "thttps://o/a", rows[0].Title)
}

func TestService_InferredFailureWithoutTheContract(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, "")
	defer func() { _ = s.Close() }()
	assert.False(t, s.PlayerSupportsHints())

	list := items("https://o/a", "https://o/b")
	failed, ready := 3, 2
	s.ObservePlayback(list, 0, &failed, nil)
	s.ObservePlayback(list, 0, &failed, nil)
	s.ObservePlayback(list, 0, &failed, nil)
	rec, err := s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.Fail, "one failure per mount, not per poll")
	assert.Equal(t, StageInferred, rec.Plays.LastFail.Stage)

	s.ObservePlayback(list, 0, &ready, nil)
	s.ObservePlayback(list, 0, &failed, nil)
	rec, err = s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, 2, rec.Plays.Fail, "a ready in between is a new mount")
	assert.Equal(t, StateBroken, rec.State)
	assert.Nil(t, s.Hints([]string{"https://o/b"}))
}

func TestService_OfflineFailuresAreNotCounted(t *testing.T) {
	clock := &testClock{t: time.Now()}
	p := newPassProber(t, newTableFetcher(), clock)
	online := false
	s := New(Config{Store: p.Store, Prober: p, Contract: &PlayerContract{Path: filepath.Join(t.TempDir(), "none")}, Now: clock.Now, Online: func() bool { return online }})
	defer func() { _ = s.Close() }()
	list := items("https://o/a")
	failed := 3
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad},
		{Seq: 2, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad},
	}})
	s.ObservePlayback(list, 0, &failed, nil)
	rec, err := s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Empty(t, rec.State, "two failures and an inferred one, all offline: no verdict")
	assert.Equal(t, 0, rec.Plays.Fail)

	online = true
	// Two DISTINCT plays (another source between them) once online.
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 2, Entries: []Outcome{
		{Seq: 3, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1},
		{Seq: 4, Source: "https://o/other", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"},
		{Seq: 5, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1},
	}})
	rec, err = s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "the same two failures online conclude")

	unknown := New(Config{Store: p.Store, Prober: p, Contract: &PlayerContract{}, Now: clock.Now})
	unknown.ObservePlayback(items("https://o/b"), 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "https://o/b", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1},
		{Seq: 2, Source: "https://o/x", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"},
		{Seq: 3, Source: "https://o/b", Kind: OutcomeFail, Stage: StageLoad, Attempt: 1},
	}})
	rec, err = p.Store.Get("https://o/b")
	require.NoError(t, err)
	assert.Empty(t, rec.State, "no connectivity verdict wired reads as offline")
}

func TestService_PreflightAndReset(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := newTableFetcher()
	f.answers["https://o/a"] = ok(404, "", nil, -1)
	s := newTestService(t, f, clock, manifestWithContract)
	res := s.Preflight(context.Background(), []string{"https://o/a"})
	assert.Equal(t, VerdictDead, res[0].Verdict)
	path := s.Store().Path()
	require.NoError(t, s.Reset())
	_, err := os.Stat(filepath.Dir(path))
	assert.True(t, os.IsNotExist(err), "factory reset removes the store directory")
}

func TestPlayerContract_ReReadsOnChange(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	c := &PlayerContract{Path: path}
	assert.False(t, c.Supported(), "no manifest: capability absent")
	require.NoError(t, os.WriteFile(path, []byte(manifestWithContract), 0o600))
	c.checked = time.Time{}
	assert.True(t, c.Supported())
	require.NoError(t, os.WriteFile(path, []byte(`{"contracts":{"mediaHealth":{"version":2}}}`), 0o600))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))
	c.checked = time.Time{}
	assert.False(t, c.Supported(), "another version is capability absent")
	require.NoError(t, os.WriteFile(path, []byte(`not json`), 0o600))
	c.checked = time.Time{}
	c.mtime = time.Time{}
	assert.False(t, c.Supported(), "an unreadable manifest never latches true")
	assert.False(t, (*PlayerContract)(nil).Supported())
}

// An onchfs work the device has not read from chain yet fails in the
// player for want of bytes. Those failures, and the ready of its error
// page, must leave no record: nothing could heal a verdict on a
// device-held source (feral-file/ffos-user#427).
func TestService_OnchfsOutcomesAreNotRecorded(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()

	src := "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid/?fxhash=0x06ec"
	list := items(src, "https://o/b")
	play := func(seq int64) []Outcome {
		return []Outcome{
			{Seq: seq, Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: CodeNetwork},
			{Seq: seq + 1, Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "unreachable", Attempt: 1},
			{Seq: seq + 2, Source: src, Kind: OutcomeFail, Stage: StageExhausted, Attempt: 3},
			{Seq: seq + 3, Source: "https://o/b", Kind: OutcomeOK, Stage: StageReady, MediaType: "image"},
		}
	}
	entries := append(play(1), play(5)...)
	entries = append(entries, Outcome{Seq: 9, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe"})
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 1, Entries: entries})

	rec, err := s.Store().Get(src)
	require.NoError(t, err)
	assert.Nil(t, rec, "two failed plays and a ready of an onchfs work write nothing")
	assert.Empty(t, s.Hints([]string{src}))
}

// An onchfs entry between two failures of another source ends that
// source's play run like any other source's entry: the second failure is
// a new play.
func TestService_OnchfsEntryEndsAnotherSourcesRun(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()

	onchfs := "https://cy46rsmjzxosyd46sunre2rbwglegcmfjqfi4cu3cgj2mvogoeva.onchfs.invalid/"
	s.ObservePlayback(items("https://o/a", onchfs), 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image", Attempt: 1},
		{Seq: 2, Source: onchfs, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe"},
		{Seq: 3, Source: "https://o/a", Kind: OutcomeFail, Stage: StageLoad, Code: "image", Attempt: 1},
	}})
	rec, err := s.Store().Get("https://o/a")
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
}
