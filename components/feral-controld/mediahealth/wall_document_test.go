package mediahealth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wall's frame over a document the kiosk itself failed to fetch is a
// picture of the error page, never the work (office FF1, 2026-11-10: a
// frame of nftstorage.link's 504 page healed "Soundgarden Live 1992 #18"
// to ok and reset its failure run).

// errorPagePlayOf runs one play of src: mount, the player's load watchdog,
// the kiosk's own GET of the document answering docStatus (0 = no
// navigation observed), a retry served 2xx when served is set, the iframe's
// ready, the settle, the capture.
func errorPagePlayOf(t *testing.T, svc *Service, w *WallCapture, clock *testClock, src string, seq *int64, docStatus int, served bool) {
	t.Helper()
	stamp := func() string { return clock.Now().UTC().Format(time.RFC3339Nano) }
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1}) // the mount begins after this poll
	clock.Advance(15 * time.Second)
	*seq++
	watchdog := Outcome{Seq: *seq, Source: src, Kind: OutcomeFail, Stage: StageWatchdog, Code: "fixed", Attempt: 1, MediaType: "iframe", At: stamp()}
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1, Entries: []Outcome{watchdog}})
	clock.Advance(10 * time.Second)
	if docStatus != 0 {
		svc.ObserveNavigation(Navigation{URL: src, Status: docStatus}) // the retry's GET
		svc.ObserveNavigation(Navigation{URL: src, Status: docStatus})
	}
	if served {
		clock.Advance(3 * time.Second)
		svc.ObserveNavigation(Navigation{URL: src, Status: 200})
	}
	clock.Advance(4 * time.Second)
	*seq++
	readyEntry := Outcome{Seq: *seq, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", Attempt: 2, At: stamp()}
	svc.ObservePlayback(items(src), 0, ready(), &Outcomes{CastSeq: 1, Entries: []Outcome{watchdog, readyEntry}})
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
}

func TestWallDocument_AFrameOfTheKiosksErrorPageIsNotEvidence(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{{Image: errorPage(), Clipped: true}, {Image: errorPage(), Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://nftstorage.link/ipfs/QmTaJHhgsWFABS8tKpB5fdCaDP8oUaQ12qgMPyv6fzw2iG"
	seedSoftware(t, svc.Store(), src)
	var seq int64

	errorPagePlayOf(t, svc, w, clock, src, &seq, 504, false)
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "L0's ok stands")
	assert.Equal(t, LayerL0, rec.Layer, "not healed to play by a frame of the 504 page")
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "the play's failure stands")
	assert.Equal(t, IssuePlayFailed, rec.Issue)
	require.NotNil(t, rec.Wall)
	assert.Equal(t, RenderStalled, rec.Wall.Verdict, "recorded as no evidence, like an L1 render over a failed document")
	assert.Equal(t, 504, rec.Wall.MainStatus)

	clock.Advance(time.Minute)
	errorPagePlayOf(t, svc, w, clock, src, &seq, 504, false)
	rec, err = svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "two plays that never rendered")
	assert.Equal(t, LayerPlay, rec.Layer)
	hints, err := svc.Store().Hints([]string{src}, clock.Now())
	require.NoError(t, err)
	assert.Contains(t, hints, src)
}

// The latest document answer is what the frame shows: a retry served 2xx
// after the 504 makes the frame the work's, and it heals.
func TestWallDocument_AServedRetryMakesTheFrameEvidenceAgain(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{{Image: checkerboard(), Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/flaky-gateway"
	seedSoftware(t, svc.Store(), src)
	var seq int64
	errorPagePlayOf(t, svc, w, clock, src, &seq, 504, true)
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, LayerPlay, rec.Layer, "the frame is of the served document: play evidence")
	assert.Zero(t, rec.Plays.ConsecutiveFail)
	assert.Equal(t, RenderOK, rec.Wall.Verdict)
}

// A failed document answer belongs to its own mount: the next mount of the
// same source (repeat-one) with no failed navigation of its own trusts its
// frame.
func TestWallDocument_AFailureBeforeTheMountDoesNotTaintIt(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{{Image: errorPage(), Clipped: true}, {Image: checkerboard(), Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/repeat-one"
	seedSoftware(t, svc.Store(), src)
	var seq int64
	errorPagePlayOf(t, svc, w, clock, src, &seq, 504, false)
	clock.Advance(time.Minute)
	errorPagePlayOf(t, svc, w, clock, src, &seq, 0, false)
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, RenderOK, rec.Wall.Verdict, "this mount's frame is judged on its own")
	assert.Equal(t, LayerPlay, rec.Layer)
}

// The kiosk's own failed answer is the strongest proof a frame is an error
// page: three works showing the same page over a 504 teach its fingerprint.
func TestWallDocument_TheKiosksErrorPageIsLearned(t *testing.T) {
	clock := &testClock{t: time.Now()}
	frames := []*WallFrame{}
	for i := 0; i < 3; i++ {
		frames = append(frames, &WallFrame{Image: errorPage(), Clipped: true})
	}
	svc, w := newWallRig(t, clock, &fakeWall{frames: frames})
	var seq int64
	for _, src := range []string{"https://gw.example/ipfs/a", "https://gw.example/ipfs/b", "https://gw.example/ipfs/c"} {
		seedSoftware(t, svc.Store(), src)
		errorPagePlayOf(t, svc, w, clock, src, &seq, 504, false)
	}
	learned := svc.Store().LearnedFingerprints()
	require.Len(t, learned, 1)
	assert.Equal(t, learnedLabel, learned[0].Label)
}

// The kiosk's document navigations happen while the player still reports
// pending, often more than one poll before ready: a 504 seen before the
// LAST pending poll is still this mount's (PR #450 review, F18).
func TestWallDocument_AFailureBeforeTheLastPendingPollIsThisMounts(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{{Image: errorPage(), Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/slow-to-ready"
	seedSoftware(t, svc.Store(), src)
	svc.ObservePlayback(items(src, "https://o/other"), 1, ready(), readyOutcomes(1, "https://o/other")) // the previous item, on screen
	clock.Advance(5 * time.Second)
	svc.ObservePlayback(items(src, "https://o/other"), 0, pending(), &Outcomes{CastSeq: 1})
	clock.Advance(time.Second)
	svc.ObserveNavigation(Navigation{URL: src, Status: 504}) // the GET, early in the mount
	for i := 0; i < 2; i++ {                                 // two more pending polls before ready
		clock.Advance(5 * time.Second)
		svc.ObservePlayback(items(src, "https://o/other"), 0, pending(), &Outcomes{CastSeq: 1})
	}
	clock.Advance(5 * time.Second)
	svc.ObservePlayback(items(src, "https://o/other"), 0, ready(), &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: 2, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: clock.Now().UTC().Format(time.RFC3339Nano)},
	}})
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, RenderStalled, rec.Wall.Verdict, "the frame is the 504 page")
	assert.Equal(t, 504, rec.Wall.MainStatus)
	assert.Equal(t, LayerL0, rec.Layer, "not healed")
}

// A record with no L0 type (the guard refused, or L0 was transient) whose
// first play shows an error page is still an HTML work: the error page's
// ready records the player's media type — nothing else in that play
// carries it when the content-type check's 5xx went straight to the
// iframe — so the capture judges the frame and its hash can be learned.
func TestWallDocument_AnUntypedRecordIsJudgedOnItsErrorPagePlay(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{{Image: errorPage(), Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/untyped"
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1}) // no record yet
	clock.Advance(2 * time.Second)
	svc.ObserveNavigation(Navigation{URL: src, Status: 504}) // the kiosk's GET: the record's first entry
	clock.Advance(2 * time.Second)
	svc.ObservePlayback(items(src), 0, ready(), readyOutcomes(1, src)) // the iframe's ready on the error page
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()), "captured: the error page's ready typed the record")
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, "iframe", rec.MediaType)
	require.NotNil(t, rec.Wall)
	assert.Equal(t, RenderStalled, rec.Wall.Verdict)
	assert.Equal(t, 504, rec.Wall.MainStatus)
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail)
	assert.Zero(t, rec.Plays.OK, "the error page's ready is not a success")
}

// A cast's first item loads before the next poll reports the new wall: its
// document navigation is matched against the playlist last pushed (Hints),
// so a 504 there is observed and its error-page frame is no evidence.
func TestWallDocument_TheFirstItemOfACastIsObserved(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{{Image: errorPage(), Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/first-of-cast"
	seedSoftware(t, svc.Store(), src)
	svc.Hints([]string{src, "https://o/second"})                                    // the push
	svc.ObserveNavigation(Navigation{URL: src + "?display_mode=crop", Status: 504}) // the kiosk loads it at once
	clock.Advance(3 * time.Second)
	svc.ObservePlayback(items(src, "https://o/second"), 0, ready(), readyOutcomes(1, src)) // the first poll sees it on screen
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "the GET 504 was observed and counted")
	assert.Equal(t, RenderStalled, rec.Wall.Verdict)
	assert.Equal(t, 504, rec.Wall.MainStatus)
	assert.Equal(t, LayerL0, rec.Layer, "not healed")
}

// On a fresh daemon there is no poll before the first pending one: a 504
// observed before any poll is still this mount's, however many pending
// polls follow (PR #450 review, F18 second round).
func TestWallDocument_AFailureBeforeTheFirstPollIsThisMounts(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{{Image: errorPage(), Clipped: true}}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/first-after-start"
	seedSoftware(t, svc.Store(), src)
	svc.Hints([]string{src})
	svc.ObserveNavigation(Navigation{URL: src, Status: 504}) // before any poll
	for i := 0; i < 2; i++ {
		clock.Advance(5 * time.Second)
		svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
	}
	clock.Advance(5 * time.Second)
	svc.ObservePlayback(items(src), 0, ready(), readyOutcomes(1, src))
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, RenderStalled, rec.Wall.Verdict)
	assert.Equal(t, 504, rec.Wall.MainStatus)
	assert.Equal(t, LayerL0, rec.Layer, "not healed")
}
