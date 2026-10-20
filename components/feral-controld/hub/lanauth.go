package hub

import (
	"crypto/subtle"
	"net/http"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
)

// TopicHeader carries the caller's proof that it already holds this device's
// relayer topic. Phase 0 of feral-file#3551 (LAN containment) uses the topic as
// the LAN credential for the few surfaces that hand a LAN peer more than the
// topic itself already grants: the topic is a bearer credential on ff-relayer,
// so anyone holding it can already drive the device remotely, and requiring it
// here grants a LAN caller nothing a topic holder lacks. A phone that paired
// holds it (from the claim QR or, pre-claim, from /api/v2/status); ff1-cli
// holds it in its keyring.
const TopicHeader = "X-FF1-Topic"

// topicRequiredBody is the 401 body for a refused topic-gated request. The code
// is the stable contract clients branch on ("re-pair this client"); keep it
// identical across every gated surface.
const topicRequiredBody = `{"error":{"code":"topic_required"}}` + "\n"

// topicGatedCommands are the /api/cast commands a LAN caller may run on a
// CLAIMED device only by presenting the current topic in TopicHeader:
//
//   - sshAccess opens a shell as feralfile, who has passwordless sudo — strictly
//     more than the topic grants, so it must never be cheaper than the topic.
//   - connect overwrites the persisted claim record with no proof (#3551 F5).
//     HAZARD: ff-app sends connect LAN-first before every playlist cast
//     (canvas_client_service_v2 connectToDevice), does not send TopicHeader,
//     and does not fall back to the relayer on a LAN HTTP error. Gating
//     connect therefore fails LAN casts from an app without the header on a
//     claimed device; ship it only together with an app that sends the
//     header, or drop connect from this set.
//
// Unclaimed devices leave both open: the first claim must keep working, and an
// unclaimed device serves its topic on /api/v2/status anyway. Relayer-originated
// commands never pass through the hub (the mediator calls the command router
// directly), and the relayer already proved the topic, so they are unaffected.
//
// Deliberately NOT gated here, documented residuals until #3551 phase 1:
// factoryReset, startWifiSetup, and the keyboard/mouse/gesture input commands
// stay open to any LAN caller.
var topicGatedCommands = map[commands.Type]bool{
	commands.CMD_SSH_ACCESS: true,
	commands.CMD_CONNECT:    true,
}

// topicGatedRoutes are whole hub routes (keyed on the middleware route label)
// that need the same proof on a claimed device. screenshot is here because the
// screen can carry the topic: any LAN caller can put the claim QR up
// (showPairingQRCode stays open) and a screenshot would then hand it the topic
// that /api/v2/status now withholds. The physical QR stays the pairing channel
// — a person in the room reads it; a LAN peer must not.
var topicGatedRoutes = map[string]bool{
	"screenshot": true,
}

// topicDenial names why a topic-gated request was refused, for the log line
// only. The wire answer is the same 401 for every reason, so a caller cannot
// probe which one applied.
type topicDenial string

const (
	topicDenialNone     topicDenial = ""
	topicDenialMissing  topicDenial = "header_missing"
	topicDenialMismatch topicDenial = "header_mismatch"
	topicDenialNoTopic  topicDenial = "device_has_no_topic"
)

// topicDenialFor applies the phase-0 policy to one request: an unclaimed device
// admits it; a claimed one admits it only when TopicHeader equals the current
// topic. A claimed device that holds no topic yet (e.g. the relayer has not
// assigned one since a restart cleared it) refuses — fail closed, because there
// is nothing a caller could prove. claim must be ONE snapshot, so claimed-ness
// and the topic come from the same write.
func topicDenialFor(claim state.ClaimInfo, r *http.Request) topicDenial {
	if !claim.Claimed {
		return topicDenialNone
	}
	if claim.TopicID == "" {
		return topicDenialNoTopic
	}
	got := r.Header.Get(TopicHeader)
	if got == "" {
		return topicDenialMissing
	}
	// Constant-time: the topic is a bearer credential, and a byte-by-byte
	// early exit would let a LAN peer recover it one byte at a time. (Its
	// length is not secret; ConstantTimeCompare returning early on a length
	// mismatch leaks only that.)
	if subtle.ConstantTimeCompare([]byte(got), []byte(claim.TopicID)) != 1 {
		return topicDenialMismatch
	}
	return topicDenialNone
}

// claimSnapshotFor reports the claim snapshot the topic gate decides from.
// The hub's claimSnapshot seam is nil in production (the global state manager
// is the source of truth) and set by tests.
func (h *hub) claimSnapshotFor() state.ClaimInfo {
	if h.claimSnapshot != nil {
		return h.claimSnapshot()
	}
	return state.ClaimSnapshot()
}

// refuseWithoutTopic applies topicDenialFor and, when it refuses, writes the
// 401 and logs why. It reports whether the request was refused; the caller
// must return without running the guarded action when it was. subject names
// the gated route or command for the log.
func (h *hub) refuseWithoutTopic(w http.ResponseWriter, r *http.Request, subject string) bool {
	denial := topicDenialFor(h.claimSnapshotFor(), r)
	if denial == topicDenialNone {
		return false
	}
	// Never log the presented header value: a near-miss topic is still
	// credential material.
	h.logger.Warn("Hub request refused: topic required on a claimed device",
		zap.String("subject", subject),
		zap.String("reason", string(denial)),
		zap.String("remote_addr", r.RemoteAddr),
	)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(topicRequiredBody))
	return true
}
