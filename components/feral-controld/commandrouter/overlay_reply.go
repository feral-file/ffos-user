package commandrouter

import (
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// replyKeyOverlay mirrors status.PlayerStatus.Overlay's wire key (issue
// #381) on the raw checkStatus reply this package forwards untyped.
const replyKeyOverlay = "overlay"

// annotateOverlayReply fills a direct checkStatus reply's overlay key the
// same way status.pollPlayerStatus's annotateOverlay fills
// PlayerStatus.Overlay for the poller's pushed player_status notifications:
// controld-owned, so any player-supplied value is dropped first and the key
// is set only from the wired overlay source — the same combined
// mint-pairing/setupui read main.go builds for status.Poller.SetOverlaySource,
// so this can never disagree with it. overlaySource may be nil (wiring not
// yet reached this handler), in which case the key is only ever dropped,
// never set.
//
// Mutates result in place (this process's own decode of the player's reply,
// never shared). The drop is unconditional on BOTH the raw map and its
// "message" sub-map when the reply carries the {messageID, message:{...}}
// envelope — mirroring playerresponse.SanitizeShowingKey's two-location
// strip exactly (applied just above this call for the same checkStatus
// reply), not just the single resolved write target below: a reply
// (spoofed or not) could carry the key at either location, or both, and
// the controld-owned contract is that neither survives. The write-back, by
// contrast, only ever needs one target — whichever map actually carries the
// player's status fields.
func annotateOverlayReply(result interface{}, overlaySource func() *status.Overlay) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	delete(m, replyKeyOverlay)
	target := m
	if msg, ok := m["message"].(map[string]interface{}); ok {
		delete(msg, replyKeyOverlay)
		target = msg
	}
	if overlaySource == nil {
		return
	}
	overlay := overlaySource()
	if overlay == nil {
		return
	}
	target[replyKeyOverlay] = overlay
}
