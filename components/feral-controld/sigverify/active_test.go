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
