package mediahealth

import (
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The kiosk observer sees the iframe's real status; the player sees only a
// `network` failure on its content-type check and a `ready` on the error
// page. Folded together under the play-run rules, the second such play
// concludes — which the player's entries alone never could.
func TestNavigation_ObservedErrorPageConcludesOnTheSecondPlay(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit() // the poll that installs the wall
	play := func() {
		// Realistic order: the frame's response is observed at mount, the
		// player's entries for that play are drained on a later poll.
		f.s.ObserveNavigation(Navigation{URL: f.src + "?display_mode=crop", Status: 404})
		f.emit(Outcome{Kind: OutcomeFail, Stage: StageDetect, Code: CodeNetwork, Attempt: 1}, iframeReady)
	}
	play()
	rec := f.record()
	assert.Equal(t, 1, rec.Plays.Fail, "the observed 404 counts the play")
	assert.Zero(t, rec.Plays.OK, "the error page's ready is not a success")
	assert.NotEqual(t, StateBroken, rec.State)

	play()
	rec = f.record()
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, LayerPlay, rec.Layer)
	assert.Equal(t, CategorySourceMissing, rec.Category)
	assert.Equal(t, 404, rec.HTTPStatus)
}

func TestNavigation_StatusTable(t *testing.T) {
	cases := []struct {
		status   int
		category Category
	}{
		{403, CategorySourceBlocked}, {429, CategorySourceBlocked}, {410, CategorySourceMissing}, {502, CategoryRenderFailed},
	}
	for _, tc := range cases {
		t.Run("status="+strconv.Itoa(tc.status), func(t *testing.T) {
			f := newPlayFeed(t, true)
			f.emit()
			for range 2 {
				f.s.ObserveNavigation(Navigation{URL: f.src, Status: tc.status})
				f.emit(iframeReady)
			}
			rec := f.record()
			assert.Equal(t, StateBroken, rec.State)
			assert.Equal(t, tc.category, rec.Category)
		})
	}
}

func TestNavigation_NetworkErrorIsALoadFailure(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit()
	for range 2 {
		f.s.ObserveNavigation(Navigation{URL: f.src, Error: "net::ERR_NAME_NOT_RESOLVED"})
		f.emit(failed(StageWatchdog, "fixed", 1), failed(StageExhausted, "3", 3))
	}
	rec := f.record()
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, CategoryRenderFailed, rec.Category)
	assert.Equal(t, 2, rec.Plays.Fail, "one count per play, whatever the player adds after")
}

func TestNavigation_IgnoredObservations(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit()
	f.s.ObserveNavigation(Navigation{URL: f.src, Error: "net::ERR_ABORTED", Canceled: true})
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 200})
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 302})
	f.s.ObserveNavigation(Navigation{URL: "https://elsewhere.example/not-on-the-wall", Status: 404})
	f.s.ObserveNavigation(Navigation{URL: "http://127.0.0.1:8080/", Status: 404})
	f.s.ObserveNavigation(Navigation{URL: f.src})
	rec := f.record()
	// A success is a look at the source (PR #451, F4): it records that and
	// nothing else. The cancel, the redirect, the strangers and the empty
	// observation record nothing at all.
	require.NotNil(t, rec, "the success is a look")
	assert.Equal(t, State(""), rec.State, "never a verdict")
	assert.False(t, rec.ObservedAt.IsZero())
	assert.Zero(t, rec.Plays.Fail+rec.Plays.OK+rec.Plays.ConsecutiveFail, "no play counted")
	assert.Nil(t, rec.Plays.LastFail)
	assert.Empty(t, rec.Issue)
}

func TestNavigation_RetriesWithinOnePlayCountOnce(t *testing.T) {
	f := newPlayFeed(t, true)
	f.emit()
	for range 3 {
		f.s.ObserveNavigation(Navigation{URL: f.src, Status: 403})
	}
	f.emit(failed(StageExhausted, "3", 3))
	rec := f.record()
	assert.Equal(t, 1, rec.Plays.Fail)
	assert.NotEqual(t, StateBroken, rec.State)
}

func TestNavigation_OfflineNeverCounts(t *testing.T) {
	f := newPlayFeed(t, false)
	f.emit()
	for range 3 {
		f.s.ObserveNavigation(Navigation{URL: f.src, Status: 404})
		f.emit(iframeReady)
	}
	rec := f.record()
	assert.Zero(t, rec.Plays.Fail)
	assert.NotEqual(t, StateBroken, rec.State)
}

func TestNavigation_NoWallNoMatch(t *testing.T) {
	f := newPlayFeed(t, true)
	f.s.ObservePlayback(nil, 0, nil, nil)
	f.s.ObserveNavigation(Navigation{URL: f.src, Status: 404})
	rec := f.record()
	assert.Nil(t, rec)
}

func TestNavigationKey(t *testing.T) {
	cases := []struct {
		a, b  string
		equal bool
	}{
		{"https://o/w", "https://o/w?display_mode=crop", true},
		{"https://o/w?fxhash=oo1&fxiteration=22", "https://o/w?fxhash=oo1&fxiteration=22&display_mode=fit", true},
		{"https://o/w?b=2&a=1", "https://o/w?a=1&b=2", true},
		{"https://o/w#0x41", "https://o/w?display_mode=crop", true},
		{"https://o/w", "https://o/w?v=2", false},
		{"https://o/w", "https://o/other", false},
		{"data:,inline", "data:,inline", false},
	}
	for _, tc := range cases {
		got := navigationKey(tc.a) != "" && navigationKey(tc.a) == navigationKey(tc.b)
		assert.Equal(t, tc.equal, got, "%s vs %s", tc.a, tc.b)
	}
}

// Of two fragment variants of one URL, the one at or after the player's
// index is the one being mounted.
func TestNavigation_FragmentVariantsResolveFromTheIndex(t *testing.T) {
	f := newPlayFeed(t, true)
	a, b := "https://o/w#one", "https://o/w#two"
	f.s.ObservePlayback(items(a, b), 1, nil, &Outcomes{CastSeq: 1})
	f.s.ObserveNavigation(Navigation{URL: "https://o/w?display_mode=crop", Status: 404})
	recB, err := f.s.Store().Get(b)
	require.NoError(t, err)
	recA, err := f.s.Store().Get(a)
	require.NoError(t, err)
	assert.NotNil(t, recB, "the variant at the index")
	assert.Nil(t, recA)
}
