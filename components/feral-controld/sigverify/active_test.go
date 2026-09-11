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

// TestActive_DifferentIDsSameURL_Miss pins that the URL never overrides a
// known id: a document republished at the same URL is a different document,
// and its status poll must not receive the earlier one's verdict.
func TestActive_DifferentIDsSameURL_Miss(t *testing.T) {
	var a sigverify.Active
	a.Set("pl-1", "https://example.com/p.json", sigverify.StatusValid)

	_, ok := a.Lookup("pl-2", "https://example.com/p.json")

	assert.False(t, ok)
}

// TestActive_URLMatchesWhenEitherSideLacksID: the URL is the fallback
// identity only when an id is missing on one side (URL casts whose reply
// echoes no playlist.id, or a slot set without one).
func TestActive_URLMatchesWhenEitherSideLacksID(t *testing.T) {
	var a sigverify.Active
	a.Set("pl-1", "https://example.com/p.json", sigverify.StatusValid)
	st, ok := a.Lookup("", "https://example.com/p.json")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st)

	var b sigverify.Active
	b.Set("", "https://example.com/p.json", sigverify.StatusInvalid)
	st, ok = b.Lookup("pl-9", "https://example.com/p.json")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusInvalid, st)
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

// Soft-refresh reconciliation: the player may keep the old item on screen
// after accepting a refresh:true push, so the slot must stay honest across
// both phases (feral-file/ffos-user#339 review).

func TestActive_ReconcileSoft_DifferingIDsLeaveCurrent(t *testing.T) {
	var a sigverify.Active
	url := "https://example.com/p.json"
	a.Set("old", url, sigverify.StatusValid)

	a.ReconcileSoft("new", url, sigverify.StatusInvalid)

	st, ok := a.Lookup("old", url)
	assert.True(t, ok, "while the old document still shows its verdict stands")
	assert.Equal(t, sigverify.StatusValid, st)
	_, ok = a.Lookup("new", url)
	assert.False(t, ok, "once the new document shows, nothing attests for it")
}

func TestActive_ReconcileSoft_SameStatusRefreshesIdentity(t *testing.T) {
	var a sigverify.Active
	url := "https://example.com/p.json"
	a.Set("", url, sigverify.StatusValid)

	a.ReconcileSoft("", url, sigverify.StatusValid)

	st, ok := a.Lookup("", url)
	assert.True(t, ok, "whichever document shows, valid is true of it")
	assert.Equal(t, sigverify.StatusValid, st)
}

func TestActive_ReconcileSoft_ChangedStatusURLOnlyClears(t *testing.T) {
	var a sigverify.Active
	url := "https://example.com/p.json"
	a.Set("", url, sigverify.StatusValid)

	a.ReconcileSoft("", url, sigverify.StatusUnsigned)

	_, ok := a.Lookup("", url)
	assert.False(t, ok, "two indistinguishable documents with different verdicts: attest nothing")
}

func TestActive_ReconcileSoft_NoCurrentStaysUnattested(t *testing.T) {
	var a sigverify.Active

	a.ReconcileSoft("id", "https://example.com/p.json", sigverify.StatusValid)

	_, ok := a.Lookup("id", "https://example.com/p.json")
	assert.False(t, ok)
}

func TestActive_ReconcileSoft_LeavesPending(t *testing.T) {
	var a sigverify.Active
	a.Set("cur", "", sigverify.StatusValid)
	a.SetPending("next", "", sigverify.StatusInvalid)

	a.ReconcileSoft("cur", "", sigverify.StatusValid)
	a.Promote()

	st, ok := a.Lookup("next", "")
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusInvalid, st)
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

// TestActive_UnverifiedPendingClearsOnPromote: a scheduled document without
// a verdict (cached copy) must not inherit the previous document's verdict
// when its cohort reaches the screen — promotion clears current. Until the
// cutover the showing document keeps its own.
func TestActive_UnverifiedPendingClearsOnPromote(t *testing.T) {
	var a sigverify.Active
	url := "https://example.com/p.json"
	a.Set("cur", url, sigverify.StatusValid)
	a.SetPendingUnverified()

	st, ok := a.Lookup("cur", url)
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusValid, st, "still showing until the cutover")

	a.Promote()

	_, ok = a.Lookup("cur", url)
	assert.False(t, ok, "the unverified document is on screen now; nothing may vouch for it")
}

// TestActive_RestagedPendingReplacesEarlierPending: a future-only refresh
// that replaces the schedule restages pending, so the cutover promotes the
// document the schedule actually holds, not the one first deferred.
func TestActive_RestagedPendingReplacesEarlierPending(t *testing.T) {
	var a sigverify.Active
	url := "https://example.com/p.json"
	a.SetPending("v1", url, sigverify.StatusValid)
	a.SetPending("v2", url, sigverify.StatusUnsigned)

	a.Promote()

	st, ok := a.Lookup("", url)
	assert.True(t, ok)
	assert.Equal(t, sigverify.StatusUnsigned, st)
	_, ok = a.Lookup("v1", "")
	assert.False(t, ok)
}
