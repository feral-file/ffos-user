package devicectl

// The claim-time displayDefaultPlaylist send bypasses commandrouter, so it
// carries its own pre-send invalidation of the signature verdict slot
// (feral-file/ffos-user#307).

import (
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

// TestSendDisplayDefaultPlaylist_InvalidatesVerdictBeforeSend: a stale slot
// that would match the player's default playlist by reused id or URL must be
// gone by the time the send runs, and the parked schedule verdict must
// survive (this send does not touch scheduler authority).
func TestSendDisplayDefaultPlaylist_InvalidatesVerdictBeforeSend(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	active := &sigverify.Active{}
	active.Set("reused-id", "https://feed.example/p.json", sigverify.StatusValid)
	active.SetPending("scheduled", "", sigverify.StatusInvalid)

	mockCDP := mocks.NewMockCDP(ctrl)
	var attestedAtSend bool
	mockCDP.EXPECT().
		Send(cdp.METHOD_EVALUATE, gomock.Any()).
		DoAndReturn(func(string, map[string]any) (interface{}, error) {
			_, attestedAtSend = active.Lookup("reused-id", "https://feed.example/p.json")
			return map[string]any{"message": map[string]any{"ok": true}}, nil
		}).
		Times(1)

	pushed := 0
	e := &executor{logger: zap.NewNop(), cdp: mockCDP}
	e.setWithPlayerPush(func(fn func()) { pushed++; fn() })
	SetVerdictInvalidator(e, active.ClearCurrent, zap.NewNop())

	require.NoError(t, e.sendDisplayDefaultPlaylist())

	assert.Equal(t, 1, pushed, "the send runs inside the push section")
	assert.False(t, attestedAtSend, "the stale verdict must be gone before the send")
	active.Promote()
	st, ok := active.Lookup("scheduled", "")
	assert.True(t, ok, "pending survives player-owned default playback")
	assert.Equal(t, sigverify.StatusInvalid, st)
}

func TestSetVerdictInvalidator_ForeignExecutorIsLeftAlone(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	assert.NotPanics(t, func() {
		SetVerdictInvalidator(mocks.NewMockExecutor(ctrl), func() {}, zap.NewNop())
	})
}
