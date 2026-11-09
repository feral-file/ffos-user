package mediahealth

import (
	"context"
	"image/color"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Step 2 (#392): a corroborated render negative concludes broken when
// mediaHealth.renderConclusive is on; suspect (advisory) otherwise.

func openConclusiveStore(t *testing.T, clock *testClock) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "mh", "store.db"), Options{Now: clock.Now, Logger: zap.NewNop(), RenderConclusive: true})
	require.NoError(t, err)
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newConclusiveWallRig(t *testing.T, clock *testClock, cap *fakeWall) (*Service, *WallCapture) {
	t.Helper()
	store := openConclusiveStore(t, clock)
	svc := New(Config{Store: store, Now: clock.Now, Online: func() bool { return true }})
	w := NewWallCapture(WallCaptureConfig{Store: store, Capturer: cap, Deliver: svc.ObserveWallFrame, Now: clock.Now})
	svc.SetWallCapture(w)
	return svc, w
}

// playOnce runs one play of src on the wall: mount, the player's iframe
// ready (plus any entries before it), the settle, the capture.
func playOnce(t *testing.T, svc *Service, w *WallCapture, clock *testClock, src string, seq int64, before ...Outcome) {
	t.Helper()
	entries := append([]Outcome{}, before...)
	entries = append(entries, Outcome{Seq: seq, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: "t"})
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
	svc.ObservePlayback(items(src), 0, ready(), &Outcomes{CastSeq: 1, Entries: entries})
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
}

func TestRenderConclusive_TwoBlankPlaysAreBroken(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame(), blankFrame()}}
	svc, w := newConclusiveWallRig(t, clock, cap)
	src := "https://o/blank"
	seedSoftware(t, svc.Store(), src)
	playOnce(t, svc, w, clock, src, 1)
	playOnce(t, svc, w, clock, src, 2)

	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, LayerPlay, rec.Layer)
	assert.Equal(t, ReasonRenderBlank, rec.Reason)
	assert.Equal(t, CategoryRenderBlank, rec.Category)
	hints, err := svc.Store().Hints([]string{src}, clock.Now())
	require.NoError(t, err)
	assert.Contains(t, hints, src, "a conclusive render verdict is skipped like any broken")
}

func TestRenderConclusive_KnownBadIsNotMedia(t *testing.T) {
	assert.Equal(t, CategoryNotMedia, CategoryOf(ReasonRenderKnownBad, 0), "a gateway error page served with a 200")
	assert.Equal(t, CategoryRenderBlank, CategoryOf(ReasonRenderBlank, 0))
}

// An L1 negative and a wall blank are independent: one of each concludes.
func TestRenderConclusive_L1AndWallCorroborate(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame()}}
	svc, w := newConclusiveWallRig(t, clock, cap)
	src := "https://o/blank"
	seedSoftware(t, svc.Store(), src)
	for i := 0; i < 2; i++ { // a blank and its same-night confirmation
		_, err := svc.Store().ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
		require.NoError(t, err)
	}
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	require.Equal(t, StateSuspect, rec.State, "a same-night confirmation alone is one observation")

	playOnce(t, svc, w, clock, src, 1)
	rec, err = svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "the wall's own blank agrees")
	assert.Equal(t, ReasonRenderBlank, rec.Reason)
}

// Two L1 conclusions on different nights corroborate; a frame that renders
// heals the render-layer broken.
func TestRenderConclusive_TwoNightsOfL1(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openConclusiveStore(t, clock)
	src := "https://o/blank"
	seedSoftware(t, store, src)
	blank := func() *Record {
		rec, err := store.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
		require.NoError(t, err)
		return rec
	}
	blank()
	assert.Equal(t, StateSuspect, blank().State, "night 1: suspect")
	clock.Advance(RecheckL1Suspect)
	rec := blank()
	assert.Equal(t, StateBroken, rec.State, "night 2 agrees")
	assert.Equal(t, LayerL1, rec.Layer)
	assert.True(t, clock.Now().Before(rec.RecheckAfter), "with the ordinary broken window (hint authority)")

	cands, err := store.L1Candidates(clock.Now().Add(RecheckL1Suspect), 5)
	require.NoError(t, err)
	assert.Contains(t, cands, src, "a render-layer broken is still rendered")
	rec, err = store.ApplyRender(src, RenderResult{Verdict: RenderOK, Variance: 0.05})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "a frame that renders heals it")
	assert.True(t, rec.L1.NegativeAt.IsZero())
}

// Off by default: the same evidence stays suspect.
func TestRenderConclusive_OffKeepsSuspect(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openTestStore(t, clock)
	src := "https://o/blank"
	seedSoftware(t, store, src)
	for i := 0; i < 2; i++ {
		_, err := store.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
		require.NoError(t, err)
	}
	clock.Advance(RecheckL1Suspect)
	rec, err := store.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State)
}

// Both layers failing report the fetch reason: an L1 blank over an open
// content-type 404 run concludes source_missing, not render_blank.
func TestRenderConclusive_BothLayersReportTheFetchReason(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openConclusiveStore(t, clock)
	src := "https://o/gone"
	seedSoftware(t, store, src)
	_, err := store.ApplyOutcome(Outcome{Seq: 1, Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "404", At: "t"}, true)
	require.NoError(t, err)
	for i := 0; i < 2; i++ {
		_, err = store.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
		require.NoError(t, err)
	}
	rec, err := store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, ReasonRenderDetect, rec.Reason)
	assert.Equal(t, CategorySourceMissing, rec.Category)
	assert.Equal(t, 404, rec.HTTPStatus)
}

// One play is one failure: a HEAD 404, its error page's ready, and a
// failing frame of that error page count once.
func TestRenderConclusive_OnePlayCountsOnce(t *testing.T) {
	clock := &testClock{t: time.Now()}
	page := &WallFrame{Image: uniform(color.White), Clipped: true}
	cap := &fakeWall{frames: []*WallFrame{page}}
	svc, w := newConclusiveWallRig(t, clock, cap)
	src := "https://o/gone"
	seedSoftware(t, svc.Store(), src)
	playOnce(t, svc, w, clock, src, 2, Outcome{Seq: 1, Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1, At: "t"})
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "the frame of the error page is the same play")
	assert.NotEqual(t, StateBroken, rec.State)
	require.NotNil(t, rec.Wall)
	assert.Equal(t, RenderBlank, rec.Wall.Verdict, "what the wall showed is recorded")
}

// A render verdict keeps the gateway route: the work is retried through the
// gateway that serves its bytes.
func TestRenderConclusive_RenderBrokenKeepsItsRoute(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame(), blankFrame()}}
	svc, w := newConclusiveWallRig(t, clock, cap)
	src := "https://ipfs.pixura.io/ipfs/" + cidA + "/index.html"
	_, err := svc.Store().ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html", ServedBy: "https://ipfs.filebase.io"})
	require.NoError(t, err)
	playOnce(t, svc, w, clock, src, 1)
	playOnce(t, svc, w, clock, src, 2)
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	require.Equal(t, StateBroken, rec.State)
	target, ok := svc.Store().GatewayOverride(src, "")
	require.True(t, ok, "the fallback route survives a render verdict")
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/"+cidA+"/index.html", target)
}

// Two L1 negatives corroborate only when they are a night apart: two
// known-bad renders an hour apart are one observation.
func TestRenderConclusive_L1NegativesMustBeANightApart(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openConclusiveStore(t, clock)
	src := "https://o/error-page"
	seedSoftware(t, store, src)
	knownBad := func() *Record {
		rec, err := store.ApplyRender(src, RenderResult{Verdict: RenderKnownBad, Label: "kubo-500"})
		require.NoError(t, err)
		return rec
	}
	assert.Equal(t, StateSuspect, knownBad().State)
	clock.Advance(time.Hour)
	assert.Equal(t, StateSuspect, knownBad().State, "an hour later is the same night")
	clock.Advance(l1NightGap)
	rec := knownBad()
	assert.Equal(t, StateBroken, rec.State, "a night later agrees")
	assert.Equal(t, CategoryNotMedia, rec.Category)
}
