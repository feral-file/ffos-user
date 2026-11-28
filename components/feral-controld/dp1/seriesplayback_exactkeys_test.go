package dp1

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// The raw seriesPlayback read must see the same items and members the typed
// decode does: a case variant is unknown to DP-1.
func TestReadItemSeriesPlaybackModes_MatchesMemberNamesExactly(t *testing.T) {
	d := &dp1{json: wrapper.NewJSON()}

	modes := d.readItemSeriesPlaybackModes(json.RawMessage(
		`{"items":[{"SeriesPlayback":"full"},{"seriesPlayback":"first","ſeriesPlayback":"full"}],`+
			`"itemſ":[{"seriesPlayback":"full"},{"seriesPlayback":"full"}]}`), 2)

	assert.Nil(t, modes[0])
	first, ok := ParseSeriesPlaybackMode("first")
	assert.True(t, ok)
	if assert.NotNil(t, modes[1]) {
		assert.Equal(t, first, *modes[1])
	}
}
