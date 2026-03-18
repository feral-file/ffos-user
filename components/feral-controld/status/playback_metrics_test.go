package status

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRecordPlaybackAttempt(t *testing.T) {
	before := PlaybackStartTotal()
	RecordPlaybackAttempt()
	assert.Equal(t, before+1, PlaybackStartTotal())

	RecordPlaybackAttempt()
	RecordPlaybackAttempt()
	assert.Equal(t, before+3, PlaybackStartTotal())
}

func TestRecordPlaybackFailure(t *testing.T) {
	before := PlaybackStartFailures()
	RecordPlaybackFailure()
	assert.Equal(t, before+1, PlaybackStartFailures())

	RecordPlaybackFailure()
	assert.Equal(t, before+2, PlaybackStartFailures())
}
