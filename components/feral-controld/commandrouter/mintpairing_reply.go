package commandrouter

import (
	"github.com/feral-file/ffos-user/components/feral-controld/mintpairing"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
)

// replyKeyMintPairing mirrors status.PlayerStatus.MintPairing's wire key
// (issue #381) on the raw checkStatus reply this package forwards untyped.
const replyKeyMintPairing = "mintPairing"

// annotateMintPairingOverlayReply fills a direct checkStatus reply's
// mintPairing key the same way status.pollPlayerStatus's
// annotateMintPairingOverlay fills PlayerStatus.MintPairing for the poller's
// pushed player_status notifications: controld-owned, so any player-supplied
// value is dropped first and the key is set only from mintPairing's own
// OverlayStatus() — the same overlay.Controller.Current() read
// DisplayActive() already uses, and status.BuildMintPairingOverlay, so this
// can never disagree with either. mintPairing may be nil (feature not wired,
// mirroring the handler's existing nil-guard pattern for this dependency),
// in which case the key is only ever dropped, never set.
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
func annotateMintPairingOverlayReply(result interface{}, mintPairing mintpairing.Service) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	delete(m, replyKeyMintPairing)
	target := m
	if msg, ok := m["message"].(map[string]interface{}); ok {
		delete(msg, replyKeyMintPairing)
		target = msg
	}
	if mintPairing == nil {
		return
	}
	s := mintPairing.OverlayStatus()
	overlay := status.BuildMintPairingOverlay(s.Showing, s.State, s.ChannelID, s.PairingCode, s.ExpiresAt)
	if overlay == nil {
		return
	}
	target[replyKeyMintPairing] = overlay
}

// replyKeyClaimQRShowing mirrors status.PlayerStatus.ClaimQRShowing's wire
// key on the raw checkStatus reply this package forwards untyped.
const replyKeyClaimQRShowing = "claimQrShowing"

// annotateClaimQRShowingReply fills a direct checkStatus reply's
// claimQrShowing key the same way status.pollPlayerStatus's
// annotateClaimQRShowing fills PlayerStatus.ClaimQRShowing for the poller's
// pushed player_status notifications — sibling of
// annotateMintPairingOverlayReply above, same controld-owned,
// drop-then-set-from-the-seam contract, same two-location strip (bare and
// enveloped) for the same spoofing reason. claimQRShowing may be nil
// (feature not wired), in which case the key is only ever dropped.
func annotateClaimQRShowingReply(result interface{}, claimQRShowing func() bool) {
	m, ok := result.(map[string]interface{})
	if !ok {
		return
	}
	delete(m, replyKeyClaimQRShowing)
	target := m
	if msg, ok := m["message"].(map[string]interface{}); ok {
		delete(msg, replyKeyClaimQRShowing)
		target = msg
	}
	if claimQRShowing == nil {
		return
	}
	if claimQRShowing() {
		target[replyKeyClaimQRShowing] = true
	}
}
