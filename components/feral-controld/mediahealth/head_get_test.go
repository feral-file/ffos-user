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

// A HEAD refusal drained BEFORE its GET is observed, in a play following a
// blank one: whether or not it was counted, the served GET may undo only
// what the HEAD itself contributed — never the earlier blank frame's
// failure (PR #450 review, F8).
func TestHeadGet_AServedGetNeverErasesAFrameFailure(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame(), blankFrame(), blankFrame()}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/blank-html"
	seedSoftware(t, svc.Store(), src)
	for play := int64(1); play <= 3; play++ {
		svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
		entries := []Outcome{}
		if play > 1 {
			entries = append(entries, headAt(src, play*10, "404", clock.Now()))
		}
		entries = append(entries, Outcome{Seq: play*10 + 1, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: clock.Now().UTC().Format(time.RFC3339Nano)})
		svc.ObservePlayback(items(src), 0, ready(), &Outcomes{CastSeq: 1, Entries: entries})
		if play > 1 {
			clock.Advance(2 * time.Second)
			svc.ObserveNavigation(Navigation{URL: src, Status: 200}) // the GET, served after the HEAD drained
		}
		clock.Advance(DefaultWallSettle)
		require.True(t, w.Step(context.Background()))
		clock.Advance(2 * time.Minute)
	}
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State, "blank on every play: a render negative")
	assert.Equal(t, ReasonRenderBlank, rec.Reason)
	assert.Equal(t, StageFrame, rec.Plays.LastFail.Stage)
}

// A counted HEAD the GET overrules hands the run back to the counted
// failure before it: here a blank frame, so the run is a frame run again
// and the next play's `ready` cannot end it. A second served GET reverts
// nothing more (PR #450 review, F8).
func TestHeadGet_ARevertedHeadRestoresTheFailureBeforeIt(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openTestStore(t, clock)
	src := "https://o/blank-html"
	seedSoftware(t, store, src)
	_, err := store.ApplyOutcome(wallBlank(src), true)
	require.NoError(t, err)
	clock.Advance(time.Minute)
	rec, err := store.ApplyOutcome(headAt(src, 2, "404", clock.Now()), true)
	require.NoError(t, err)
	require.Equal(t, StateBroken, rec.State, "two counted failures conclude on the HEAD")
	require.Equal(t, ReasonRenderDetect, rec.Reason)

	clock.Advance(2 * time.Second)
	_, err = store.ApplyKioskAnswer(src)
	require.NoError(t, err)
	rec, err = store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "the HEAD's verdict is withdrawn: back to L0's ok")
	assert.Equal(t, LayerL0, rec.Layer)
	assert.Equal(t, IssuePlayFailed, rec.Issue, "one failure short of a verdict")
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail)
	require.NotNil(t, rec.Plays.RunFail)
	assert.Equal(t, StageFrame, rec.Plays.RunFail.Stage, "the blank frame classifies the run again")
	assert.True(t, rec.frameRunOpen())
	assert.True(t, rec.Plays.LastFail.Superseded, "the overruled HEAD stays visible to the app")

	_, err = store.ApplyKioskAnswer(src)
	require.NoError(t, err)
	rec, err = store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "a second 2xx reverts nothing more")
}

// The play's uncounted HEAD retries do not shield its counted refusal: a
// served GET still withdraws the verdict that refusal concluded, and the
// earlier play's refusal it reinstates is never reverted in its turn (PR
// #450 review, F9).
func TestHeadGet_AServedGetUndoesTheCountedRefusalPastItsRetries(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit(failed(StageDetect, "404", 1), iframeReady) // play 1
	f.emit(failed(StageDetect, "404", 1))              // play 2: concludes
	require.Equal(t, StateBroken, f.record().State)
	f.emit(failed(StageDetect, "404", 2)) // play 2's retry: the same play, uncounted

	f.clock.Advance(2 * time.Second)
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200}) // play 2's GET was served
	rec := f.record()
	assert.Equal(t, State(""), rec.State, "the verdict's evidence was the HEAD alone: withdrawn")
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "play 1's refusal still counts")
	require.NotNil(t, rec.Plays.RunFail)
	assert.True(t, rec.Plays.RunFail.Settled)

	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	assert.Equal(t, 1, f.record().Plays.ConsecutiveFail, "the reinstated refusal is never reverted")
}

// A wall frame ends its play: the next mount's content-type check and GET
// failing 404 count as that play's own failure and conclude on the fetch
// reason (PR #450 review, F10).
func TestHeadGet_TheNextPlaysFetchFailureCountsAfterAFrame(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame(), blankFrame(), blankFrame(), blankFrame()}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/gone-html"
	seedSoftware(t, svc.Store(), src)
	for play := int64(1); play <= 4; play++ {
		svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
		entries := []Outcome{}
		if play > 1 {
			svc.ObserveNavigation(Navigation{URL: src, Status: 404}) // the GET, observed at once
			entries = append(entries, headAt(src, play*10, "404", clock.Now()))
		}
		entries = append(entries, Outcome{Seq: play*10 + 1, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: clock.Now().UTC().Format(time.RFC3339Nano)})
		svc.ObservePlayback(items(src), 0, ready(), &Outcomes{CastSeq: 1, Entries: entries})
		clock.Advance(DefaultWallSettle)
		require.True(t, w.Step(context.Background()))
		clock.Advance(2 * time.Minute)
	}
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "a 404 every play is a fetch verdict, not an advisory blank")
	assert.Equal(t, LayerPlay, rec.Layer)
	assert.Equal(t, ReasonRenderDetect, rec.Reason)
	assert.Equal(t, 404, rec.HTTPStatus)
}

// The GET overruling the play's counted HEAD frees that play's count: the
// play's real failures after it count once, whichever order the GET and
// the drain came in (PR #450 review, F13).
func TestHeadGet_AWithdrawnPlayCountsItsRealFailure(t *testing.T) {
	f := newPlayFeed(t, true)
	for play := 0; play < 2; play++ {
		f.emit(failed(StageDetect, "404", 1))
		f.clock.Advance(2 * time.Second)
		f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200}) // the GET, served after the drain
		f.emit(failed(StageWatchdog, "fixed", 1), failed(StageWatchdog, "fixed", 2), failed(StageWatchdog, "fixed", 3), failed(StageExhausted, "3", 3))
	}
	rec := f.record()
	assert.Equal(t, 2, rec.Plays.ConsecutiveFail, "one real failure per play")
	assert.Equal(t, StateBroken, rec.State, "two plays that never rendered")
	assert.Equal(t, LayerPlay, rec.Layer)
	assert.NotEqual(t, ReasonRenderDetect, rec.Reason, "the HEAD was overruled: a render failure")
}

// A wall frame does not end its mount: the iframe reloading after it and
// getting a 404 is the same play, so one mount never corroborates itself
// (PR #450 review, F14).
func TestHeadGet_ASameMountReloadAfterAFrameIsTheSamePlay(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame()}}
	svc, w := newWallRig(t, clock, cap)
	src := "https://o/reloading-html"
	seedSoftware(t, svc.Store(), src)
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
	svc.ObservePlayback(items(src), 0, ready(), readyOutcomes(1, src))
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background())) // blank: one counted failure
	clock.Advance(5 * time.Second)
	svc.ObserveNavigation(Navigation{URL: src, Status: 404}) // the iframe reloads, same mount
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "one mount, one failure")
	assert.NotEqual(t, StateBroken, rec.State)
}

// Withdrawing a verdict the HEAD alone concluded restores the fallback
// route that conclusion dropped: the GET was just served through it (PR
// #450 review, F15).
func TestHeadGet_AWithdrawnVerdictRestoresItsRoute(t *testing.T) {
	f := newPlayFeed(t, true)
	f.src = "https://ipfs.pixura.io/ipfs/" + cidA
	_, err := f.s.Store().ApplyProbe(f.src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html", ServedBy: "https://ipfs.filebase.io"})
	require.NoError(t, err)
	_, ok := f.s.Store().GatewayOverride(f.src, "")
	require.True(t, ok)

	f.emit(failed(StageDetect, "404", 1), iframeReady) // play 1
	f.emit(failed(StageDetect, "404", 1))              // play 2: concludes, the route is dropped
	rec := f.record()
	require.Equal(t, StateBroken, rec.State)
	require.Empty(t, rec.ServedBy)

	f.clock.Advance(2 * time.Second)
	f.s.ObserveNavigation(Navigation{URL: "https://ipfs.filebase.io/ipfs/" + cidA, Status: 200}) // served through the fallback
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	rec = f.record()
	assert.Equal(t, StateOK, rec.State, "withdrawn: back to L0's ok through the fallback")
	assert.Equal(t, "https://ipfs.filebase.io", rec.ServedBy, "the route comes back")
	target, ok := f.s.Store().GatewayOverride(f.src, "")
	require.True(t, ok, "and the kiosk loads through it again")
	assert.Equal(t, "https://ipfs.filebase.io/ipfs/"+cidA, target)
}

// A refusal its own play's GET confirmed (the kiosk got a 404 too) is
// settled: a later 2xx of the same document reverts nothing, whichever
// order the HEAD and the GET arrived in (PR #450 review, F16).
func TestHeadGet_AGetThatFailedTooSettlesTheRefusal(t *testing.T) {
	for _, headFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "HEAD drained first", false: "GET observed first"}[headFirst], func(t *testing.T) {
			f := newPlayFeed(t, true)
			for play := 0; play < 2; play++ {
				if headFirst {
					f.emit(failed(StageDetect, "404", 1))
					f.s.ObserveNavigation(Navigation{URL: f.src, Status: 404})
				} else {
					f.s.ObserveNavigation(Navigation{URL: f.src, Status: 404})
					f.emit(failed(StageDetect, "404", 1))
				}
				f.emit(iframeReady)
			}
			rec := f.record()
			require.Equal(t, StateBroken, rec.State)
			require.Equal(t, 2, rec.Plays.ConsecutiveFail, "one count per play")

			f.clock.Advance(2 * time.Second)
			f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200}) // a reload served
			rec = f.record()
			assert.Equal(t, StateBroken, rec.State, "the real fetch failed: nothing to revert")
			assert.Equal(t, 2, rec.Plays.ConsecutiveFail)
		})
	}
}

// Withdrawing a HEAD's verdict restores the verdict it replaced — a
// render-layer broken with its window and backoff — not unknown (PR #450
// review, F17).
func TestHeadGet_AWithdrawnVerdictRestoresTheRenderVerdict(t *testing.T) {
	clock := &testClock{t: time.Now()}
	cap := &fakeWall{frames: []*WallFrame{blankFrame(), blankFrame()}}
	svc, w := newConclusiveWallRig(t, clock, cap)
	src := "https://o/blank"
	seedSoftware(t, svc.Store(), src)
	playOnce(t, svc, w, clock, src, 1)
	clock.Advance(time.Minute)
	playOnce(t, svc, w, clock, src, 2)
	broken, err := svc.Store().Get(src)
	require.NoError(t, err)
	require.Equal(t, ReasonRenderBlank, broken.Reason)

	// Repeat-one: the next mount's HEAD 404s and concludes over the blank
	// verdict, then its GET is served a second later.
	clock.Advance(time.Minute)
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
	svc.ObservePlayback(items(src), 0, nil, &Outcomes{CastSeq: 1, Entries: []Outcome{headAt(src, 30, "404", clock.Now())}})
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	require.Equal(t, ReasonRenderDetect, rec.Reason, "the HEAD's verdict replaced the blank one")
	clock.Advance(time.Second)
	svc.ObserveNavigation(Navigation{URL: src, Status: 200})
	rec, err = svc.Store().Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "the blank verdict is back")
	assert.Equal(t, ReasonRenderBlank, rec.Reason)
	assert.Equal(t, CategoryRenderBlank, rec.Category)
	assert.Equal(t, broken.RecheckAfter, rec.RecheckAfter, "with its window")
	assert.Equal(t, broken.ConsecutiveBroken, rec.ConsecutiveBroken, "and its backoff")
	assert.Empty(t, rec.Issue)
	hints, err := svc.Store().Hints([]string{src}, clock.Now())
	require.NoError(t, err)
	assert.Contains(t, hints, src, "still skipped")
}
