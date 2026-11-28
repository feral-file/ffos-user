package commandrouter

import (
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
	"github.com/feral-file/ffos-user/components/feral-controld/playerresponse"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// Native status is constructed from typed host state, not forwarded from a
// document. Use the same presented-vs-selected mapping as periodic status.
// Trusted annotations enter only through controld's own sources. The legacy
// unfenced signature slot cannot attest a native occurrence and is not read.
func (h *handler) nativeStatusReply(state playerlink.Snapshot) (interface{}, error) {
	reply, err := status.NativePlayerStatus(state)
	if err != nil {
		return controlFailure("invalid_response", "Native player state is invalid")
	}
	if h.overlaySource != nil {
		reply.Overlay = h.overlaySource()
	}
	if mode := h.currentSeriesPlaybackMode(); mode != nil {
		reply.DeviceSettings.SeriesPlayback = mode
	}
	if key := reply.DeviceSettings.ShowingKey; key != nil && !playerresponse.IsCanonicalShowingKey(*key) {
		reply.DeviceSettings.ShowingKey = nil
	}
	// These internal observation fields are absent from native snapshots today.
	// Keep the egress boundary explicit if the mapper gains observations later.
	reply.Outcomes = nil
	reply.SkippedIndices = nil
	return map[string]any{"message": reply}, nil
}
