package mediahealth

import (
	"context"
	"image"
	"image/color"
	"image/draw"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakeWall is a WallCapturer returning one queued frame per capture; onCapture
// runs inside the capture (e.g. the wall moving on).
type fakeWall struct {
	frames    []*WallFrame
	calls     int
	onCapture func()
}

func (f *fakeWall) CaptureWall(context.Context) (*WallFrame, error) {
	f.calls++
	if f.onCapture != nil {
		f.onCapture()
	}
	if len(f.frames) == 0 {
		return nil, ErrFrameUnavailable
	}
	fr := f.frames[0]
	f.frames = f.frames[1:]
	return fr, nil
}

func ready() *int   { v := 2; return &v }
func pending() *int { v := 0; return &v }

// newWallRig is a Service over a real store with a wall-capture runner whose
// frames go through the Service's play run, as in main.
func newWallRig(t *testing.T, clock *testClock, cap *fakeWall) (*Service, *WallCapture) {
	t.Helper()
	store := openTestStore(t, clock)
	svc := New(Config{Store: store, Now: clock.Now, Online: func() bool { return true }})
	w := NewWallCapture(WallCaptureConfig{Store: store, Capturer: cap, Deliver: svc.ObserveWallFrame, Now: clock.Now})
	svc.SetWallCapture(w)
	return svc, w
}

func artFrame() *WallFrame   { return &WallFrame{Image: checkerboard(), Clipped: true} }
func blankFrame() *WallFrame { return &WallFrame{Image: uniform(color.Black), Clipped: true} }

// readyOutcomes is the player's ring for one play of src: an iframe `ready`.
func readyOutcomes(seq int64, src string) *Outcomes {
	return &Outcomes{CastSeq: 1, Entries: []Outcome{{Seq: seq, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: "t"}}}
}

func TestWallCapture_OncePerMountAfterTheSettle(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{artFrame(), artFrame()}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/html"
	seedSoftware(t, svc.Store(), src)

	w.Observe([]string{src}, 0, ready())
	assert.False(t, w.Step(context.Background()), "not before the settle")
	clock.Advance(DefaultWallSettle)
	assert.True(t, w.Step(context.Background()))
	assert.False(t, w.Step(context.Background()), "once per mount")
	assert.Equal(t, 1, cap.calls)

	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State)
	assert.Equal(t, LayerPlay, rec.Layer, "a rendered frame on the wall is the play's evidence")
	require.NotNil(t, rec.Wall)
	assert.Equal(t, RenderOK, rec.Wall.Verdict)
	assert.Equal(t, 0, rec.Plays.OK, "a frame is not a play of its own")

	// A new mount of the same source inside WallRecheckOK: left alone.
	w.Observe([]string{src}, 0, pending())
	w.Observe([]string{src}, 0, ready())
	clock.Advance(DefaultWallSettle)
	assert.False(t, w.Step(context.Background()), "an ok frame needs no corroboration for a day")
	clock.Advance(WallRecheckOK)
	w.Observe([]string{src}, 0, pending())
	w.Observe([]string{src}, 0, ready())
	clock.Advance(DefaultWallSettle)
	assert.True(t, w.Step(context.Background()), "and is looked at again after it")
}

func TestWallCapture_OnlyHTMLWorksWhileAwake(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{artFrame()}}
	svc, w := newWallRig(t, clock, cap)
	img := "https://o/a.png"
	_, err := svc.Store().ApplyProbe(img, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "image/png", Sniffed: "image/png"})
	require.NoError(t, err)
	w.Observe([]string{img}, 0, ready())
	clock.Advance(DefaultWallSettle)
	assert.False(t, w.Step(context.Background()), "an image proves itself through its own ready")

	html := "https://o/html"
	seedSoftware(t, svc.Store(), html)
	asleep := true
	w.cfg.Awake = func() bool { return !asleep }
	w.Observe([]string{html}, 0, ready())
	clock.Advance(DefaultWallSettle)
	assert.False(t, w.Step(context.Background()), "never while the panel sleeps: a dark panel reads blank")
	asleep = false
	assert.True(t, w.Step(context.Background()), "the mount is still pending once it wakes")
	assert.Equal(t, 1, cap.calls)
}

func TestWallCapture_DropsAFrameIfTheWallMoved(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame()}}
	svc, w := newWallRig(t, clock, cap)
	a, b := "https://o/a", "https://o/b"
	seedSoftware(t, svc.Store(), a)
	seedSoftware(t, svc.Store(), b)
	cap.onCapture = func() { w.Observe([]string{a, b}, 1, ready()) }
	w.Observe([]string{a, b}, 0, ready())
	clock.Advance(DefaultWallSettle)
	assert.False(t, w.Step(context.Background()))
	for _, src := range []string{a, b} {
		rec, err := svc.Store().Get(src)
		require.NoError(t, err)
		assert.Nil(t, rec.Wall, "a frame taken across a transition is filed under neither work: %s", src)
	}
}

func TestWallCapture_MostlyMaskedIsNotEvidence(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := blankFrame()
	f.Masks = []image.Rectangle{image.Rect(0, 0, 64, 48)} // 75% under the player's UI
	cap := &fakeWall{frames: []*WallFrame{f}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/html"
	seedSoftware(t, svc.Store(), src)
	w.Observe([]string{src}, 0, ready())
	clock.Advance(DefaultWallSettle)
	assert.False(t, w.Step(context.Background()))
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Nil(t, rec.Wall)
}

// Two plays whose wall frame read blank conclude suspect (advisory). The
// iframe `ready` of the second play must not end the frame run, or a blank
// work could never conclude.
func TestWallCapture_TwoBlankPlaysAreSuspect(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame(), blankFrame()}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/blank"
	seedSoftware(t, svc.Store(), src)
	sources := []string{src}

	for play := int64(1); play <= 2; play++ {
		svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
		svc.ObservePlayback(items(src), 0, ready(), readyOutcomes(play, src))
		clock.Advance(DefaultWallSettle)
		require.True(t, w.Step(context.Background()), "play %d", play)
		rec, err := svc.Store().Get(src)
		require.NoError(t, err)
		if play == 1 {
			assert.Equal(t, StateOK, rec.State, "one blank play is recorded, not concluded")
			assert.Equal(t, IssuePlayFailed, rec.Issue)
		} else {
			assert.Equal(t, StateSuspect, rec.State, "two blank plays: suspect")
			assert.Equal(t, CategoryRenderBlank, rec.Category)
			assert.Equal(t, ReasonRenderBlank, rec.Reason)
			assert.Equal(t, LayerPlay, rec.Layer)
		}
		_ = sources
	}
	hints, err := svc.Store().Hints([]string{src}, clock.Now())
	require.NoError(t, err)
	assert.Empty(t, hints, "suspect is never a skip")
}

// A rendered frame during a play whose content-type check said 404 is the
// gateway's error page: the HEAD wins and the record does not change.
func TestWallCapture_ARenderedFrameNeverOverrulesA404(t *testing.T) {
	clock := &testClock{t: time.Now()}
	page := image.NewRGBA(image.Rect(0, 0, 64, 64))
	draw.Draw(page, page.Bounds(), checkerboard(), image.Point{}, draw.Src)
	cap := &fakeWall{frames: []*WallFrame{{Image: page, Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/gone"
	seedSoftware(t, svc.Store(), src)
	svc.ObservePlayback(items(src), 0, ready(), &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 1, Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1, At: "t"},
		{Seq: 2, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: "t"},
	}})
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, LayerL0, rec.Layer, "the frame did not write ok over the 404")
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "and the fetch run stands")
	assert.Equal(t, IssuePlayFailed, rec.Issue)
	require.NotNil(t, rec.Wall, "what the wall showed is still recorded")
	assert.Equal(t, RenderOK, rec.Wall.Verdict)
}
