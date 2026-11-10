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
