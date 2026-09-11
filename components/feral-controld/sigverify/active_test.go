package sigverify_test

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

func TestActive_LookupMissBeforeSet(t *testing.T) {
	var a sigverify.Active

	_, ok := a.Lookup("id", "https://example.com/p.json")

	assert.False(t, ok)
}

func TestActive_MatchesByID(t *testing.T) {
	var a sigverify.Active
	a.Set("pl-1", "", sigverify.StatusInvalid)

	st, ok := a.Lookup("pl-1", "")

	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusInvalid, st)
}

func TestActive_MatchesByURLWhenIDDiffers(t *testing.T) {
	var a sigverify.Active
	a.Set("pl-1", "https://example.com/p.json", sigverify.StatusValid)

	st, ok := a.Lookup("other", "https://example.com/p.json")

	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st)
}

// TestActive_EmptyKeysNeverMatch pins the miss-over-false-hit rule: a stored
// entry with an empty URL must not match an on-screen playlist that also has
// no URL, or the player-fetched default playlist would inherit a verdict.
func TestActive_EmptyKeysNeverMatch(t *testing.T) {
	var a sigverify.Active
	a.Set("", "", sigverify.StatusValid)

	_, ok := a.Lookup("", "")

	assert.False(t, ok)
}

func TestActive_SetReplacesPreviousSlot(t *testing.T) {
	var a sigverify.Active
	a.Set("pl-1", "", sigverify.StatusValid)
	a.Set("pl-2", "", sigverify.StatusUnsigned)

	_, ok := a.Lookup("pl-1", "")
	assert.False(t, ok)
	st, ok := a.Lookup("pl-2", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusUnsigned, st)
}

// TestActive_ClearDropsCurrent: a verdict-less push (cached copy) for the
// same URL must not leave the earlier verdict standing.
func TestActive_ClearDropsCurrent(t *testing.T) {
	var a sigverify.Active
	a.Set("pl-1", "https://example.com/p.json", sigverify.StatusValid)

	a.Clear()

	_, ok := a.Lookup("pl-1", "https://example.com/p.json")
	assert.False(t, ok)
}

// TestActive_PendingIsNotLookedUpUntilPromoted: a deferred cast's verdict
// must not describe the playlist still on screen, even when both share the
// URL — only a promotion (the cutover push) moves it into current.
func TestActive_PendingIsNotLookedUpUntilPromoted(t *testing.T) {
	var a sigverify.Active
	url := "https://example.com/p.json"
	a.Set("old", url, sigverify.StatusValid)

	a.SetPending("new", url, sigverify.StatusInvalid)

	st, ok := a.Lookup("old", url)
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st, "the showing playlist keeps its own verdict")
	_, ok = a.Lookup("new", "")
	assert.False(t, ok, "pending is invisible to lookup")

	a.Promote()

	st, ok = a.Lookup("new", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusInvalid, st)
	st, ok = a.Lookup("", url)
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusInvalid, st, "after cutover the URL names the new document")
}

func TestActive_PromoteIsIdempotentAndNoopWhenNothingPending(t *testing.T) {
	var a sigverify.Active
	a.Set("cur", "", sigverify.StatusValid)

	a.Promote()
	st, ok := a.Lookup("cur", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st)

	a.SetPending("next", "", sigverify.StatusUnsigned)
	a.Promote()
	a.Promote()
	st, ok = a.Lookup("next", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusUnsigned, st)
}

// TestActive_FreshCastDropsPending: a new cast replaces the schedule, so a
// verdict parked by an earlier deferred cast must never be promoted later.
func TestActive_FreshCastDropsPending(t *testing.T) {
	var a sigverify.Active
	a.SetPending("scheduled", "", sigverify.StatusInvalid)
	a.Set("fresh", "", sigverify.StatusValid)

	a.Promote()

	_, ok := a.Lookup("scheduled", "")
	assert.False(t, ok)
	st, ok := a.Lookup("fresh", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st)
}

func TestActive_ClearPendingLeavesCurrent(t *testing.T) {
	var a sigverify.Active
	a.Set("cur", "", sigverify.StatusValid)
	a.SetPending("next", "", sigverify.StatusInvalid)

	a.ClearPending()
	a.Promote()

	st, ok := a.Lookup("cur", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st)
	_, ok = a.Lookup("next", "")
	assert.False(t, ok)
}
