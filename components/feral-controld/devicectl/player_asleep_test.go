package devicectl

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/sleepschedule"
)

// PlayerAsleep (the media-health L1 gate, ffos-user#392) is true only when
// the schedule wants the panel asleep AND the player confirmed it; every
// other tracker state reads awake, the side that never renders beside the
// wall.
func TestPlayerAsleep_RequiresDesiredAndConfirmed(t *testing.T) {
	e := &executor{}
	assert.False(t, e.PlayerAsleep(), "bare executor: awake")

	e.sleepScheduleDesiredCache = sleepschedule.StateSleeping
	assert.False(t, e.PlayerAsleep(), "desired sleeping but the player never confirmed")

	e.sleepPlayerState = playerSleeping
	assert.True(t, e.PlayerAsleep())

	e.sleepPlayerState = playerUnknownFailed
	assert.False(t, e.PlayerAsleep(), "a failed apply is not confirmation")
	e.sleepPlayerState = playerFreshDocument
	assert.False(t, e.PlayerAsleep(), "a fresh document reloaded awake")

	e.sleepPlayerState = playerSleeping
	e.sleepScheduleDesiredCache = sleepschedule.StateAwake
	assert.False(t, e.PlayerAsleep(), "the schedule wants it awake: a wake is being driven")

	assert.True(t, PlayerAsleep(func() Executor {
		e.sleepScheduleDesiredCache = sleepschedule.StateSleeping
		return e
	}()))
	assert.False(t, PlayerAsleep(nil), "an executor without the tracker reads awake")
}
