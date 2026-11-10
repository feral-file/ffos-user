package mediahealth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The kiosk's own GET of a document outranks the player's content-type
// check (HEAD) for the same play, whichever reaches controld first: the
// observer's events arrive at once, the player's ring every poll.

// headAt is the player's HEAD refusal, stamped at t.
func headAt(src string, seq int64, code string, t time.Time) Outcome {
	return Outcome{Seq: seq, Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: code, Attempt: 1, MediaType: "iframe", At: t.UTC().Format(time.RFC3339Nano)}
}

func TestHeadGet_ServedFirstOverrulesALaterDrainedHead(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit()
	head := f.clock.Now()
	f.clock.Advance(500 * time.Millisecond)
	f.s.ObserveNavigation(Navigation{URL: f.src + "?display_mode=crop", Status: 200}) // the GET, served
	f.clock.Advance(4 * time.Second)                                                  // the next poll drains the HEAD
	f.s.ObservePlayback(items(f.src, "https://o/other"), 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{headAt(f.src, 1, "404", head)}})
	rec := f.record()
	assert.Zero(t, rec.Plays.Fail, "the HEAD's 404 is not the document's")
	assert.Zero(t, rec.Plays.ConsecutiveFail)
	assert.Empty(t, rec.Issue)
}

func TestHeadGet_ServedAfterACountedHeadRevertsIt(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(failed(StageDetect, "404", 1))
	rec := f.record()
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail)
	assert.Equal(t, IssuePlayFailed, rec.Issue)

	f.clock.Advance(2 * time.Second)
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	rec = f.record()
	assert.Zero(t, rec.Plays.ConsecutiveFail, "the GET was served: the HEAD's count is reverted")
	assert.Zero(t, rec.Plays.Fail)
	assert.Empty(t, rec.Issue)
	assert.True(t, rec.Plays.LastFail.Superseded)

	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	assert.Zero(t, f.record().Plays.Fail, "a second 2xx in the play reverts nothing more")
}

func TestHeadGet_AVerdictConcludedByTheHeadIsWithdrawn(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(failed(StageDetect, "404", 1), iframeReady) // play 1
	f.emit(failed(StageDetect, "404", 1))              // play 2: concludes
	rec := f.record()
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, ReasonRenderDetect, rec.Reason)

	f.clock.Advance(2 * time.Second)
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200}) // play 2's GET was served
	rec = f.record()
	assert.Equal(t, State(""), rec.State, "its evidence was the HEAD alone: withdrawn")
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "play 1's refusal still counts")
	assert.Equal(t, IssuePlayFailed, rec.Issue, "one failure short of a verdict, reported as such")

	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200}) // the frame reloads in the same play
	assert.Equal(t, 1, f.record().Plays.ConsecutiveFail, "a refusal is reverted once: play 1's stays")
}

func TestHeadGet_AnObservedGet404IsNeverReverted(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit()
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 404}) // the GET itself failed
	f.clock.Advance(2 * time.Second)
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	assert.Equal(t, 1, f.record().Plays.ConsecutiveFail, "only the HEAD is outranked, never the GET")
}

func TestHeadGet_OutsideThePlayWindowNothingIsReverted(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(failed(StageDetect, "404", 1))
	f.clock.Advance(headGetWindow + time.Second)
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	assert.Equal(t, 1, f.record().Plays.ConsecutiveFail, "a GET minutes later is another play")
}

// A HEAD refusal the kiosk's GET already overruled is history only: it
// must not become the run's last failure, or the play's `ready` resets a
// blank-frame run and a repeatedly blank work never concludes (PR #450
// review, F5).
func TestHeadGet_AnOverruledHeadKeepsTheFrameRun(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame(), blankFrame(), blankFrame()}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/blank-html"
	seedSoftware(t, svc.Store(), src)
	for play := int64(1); play <= 3; play++ {
		svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
		head := clock.Now()
		clock.Advance(500 * time.Millisecond)
		svc.ObserveNavigation(Navigation{URL: src, Status: 200}) // the GET, served
		clock.Advance(4 * time.Second)                           // the next poll drains the HEAD
		svc.ObservePlayback(items(src), 0, ready(), &Outcomes{CastSeq: 1, Entries: []Outcome{
			headAt(src, play*10, "404", head),
			{Seq: play*10 + 1, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: clock.Now().UTC().Format(time.RFC3339Nano)},
		}})
		clock.Advance(DefaultWallSettle)
		require.True(t, w.Step(context.Background()))
		clock.Advance(2 * time.Minute)
	}
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Zero(t, rec.Plays.Fail-rec.Plays.ConsecutiveFail, "every counted failure is a frame of this run")
	assert.Equal(t, StateSuspect, rec.State, "blank on every play: a render negative")
	assert.Equal(t, ReasonRenderBlank, rec.Reason)
	require.NotNil(t, rec.Plays.LastFail)
	assert.Equal(t, StageFrame, rec.Plays.LastFail.Stage, "the overruled HEAD never became the last failure")
}
