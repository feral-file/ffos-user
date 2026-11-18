package refresher

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

func TestNativeRefreshDoesNotUseOutgoingPlaylistWithIncomingFence(t *testing.T) {
	fence := &playerlink.Preconditions{Instance: "host", Generation: 2}
	s := &status.PlayerStatus{Runtime: &status.RuntimeStatus{Instance: "host", Generation: 2,
		Selected: &status.OccurrenceStatus{ID: "B", Generation: 2}, Presented: &status.OccurrenceStatus{ID: "A", Generation: 1}}}
	require.False(t, nativeRefreshMatches(s, fence), "status source A cannot be refreshed under B's command fence")
	s.Runtime.Presented = &status.OccurrenceStatus{ID: "B", Generation: 2}
	require.True(t, nativeRefreshMatches(s, fence))
	s.Runtime.Instance = "replacement-host"
	require.False(t, nativeRefreshMatches(s, fence))
	s.Runtime = nil
	require.False(t, nativeRefreshMatches(s, fence))
}
