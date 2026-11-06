package commandrouter

import (
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
)

// replyKeyDeviceSettings/replyKeySeriesPlayback mirror
// status.PlayerDeviceSettings.SeriesPlayback's wire location
// (deviceSettings.seriesPlayback) on the raw checkStatus reply this package
// forwards untyped. Unlike replyKeyOverlay (a flat top-level key),
// deviceSettings is itself a sub-map here.
const (
	replyKeyDeviceSettings = "deviceSettings"
	replyKeySeriesPlayback = "seriesPlayback"
)

// annotateSeriesPlaybackReply fills a direct checkStatus reply's
// deviceSettings.seriesPlayback key the same way status.pollPlayerStatus's
// annotateSeriesPlayback fills PlayerDeviceSettings.SeriesPlayback for the
// poller's pushed player_status notifications: controld-owned, so any
// player-supplied value is dropped first and the key is set only from
// source — the same device-level seriesplayback.Store read
// currentSeriesPlaybackMode uses, so this can never disagree with it.
// source may be nil (wiring not yet reached this handler), in which case
// the key is only ever dropped, never set.
//
// Mutates result in place (this process's own decode of the player's
// reply, never shared). The drop is unconditional on BOTH the raw map's
// deviceSettings sub-map and its "message" sub-map's own deviceSettings
// when the reply carries the {messageID, message:{...}} envelope —
// mirroring annotateOverlayReply's two-location strip exactly (applied
// alongside it, for the same checkStatus reply), not just the single
// resolved write target below: a reply (spoofed or not) could carry the
// key at either location, or both, and the controld-owned contract is that
// neither survives.
func annotateSeriesPlaybackReply(result interface{}, source func() *dp1.SeriesPlaybackMode) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	dropSeriesPlaybackKey(m)
	target := m
	if msg, ok := m["message"].(map[string]interface{}); ok {
		dropSeriesPlaybackKey(msg)
		target = msg
	}
	if source == nil {
		return
	}
	mode := source()
	if mode == nil {
		return
	}
	settings, ok := target[replyKeyDeviceSettings].(map[string]interface{})
	if !ok {
		// deviceSettings may be entirely absent from this round's reply (every
		// one of its OTHER sub-fields was also unset) — a device override must
		// not be dropped just because of that, so the sub-map is created here
		// rather than skipped, mirroring status.annotateSeriesPlayback's own
		// nil-PlayerDeviceSettings allocation.
		settings = map[string]interface{}{}
		target[replyKeyDeviceSettings] = settings
	}
	settings[replyKeySeriesPlayback] = string(*mode)
}

// dropSeriesPlaybackKey removes the seriesPlayback key from m's
// deviceSettings sub-map, if either is present — the "drop" half of the
// drop-then-set contract, applied before source is consulted so a
// spoofed/player-supplied value is never visible even when source is nil or
// reports no override.
func dropSeriesPlaybackKey(m map[string]interface{}) {
	settings, ok := m[replyKeyDeviceSettings].(map[string]interface{})
	if !ok {
		return
	}
	delete(settings, replyKeySeriesPlayback)
}
