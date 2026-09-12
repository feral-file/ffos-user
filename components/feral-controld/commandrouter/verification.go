package commandrouter

import (
	"errors"
	"fmt"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/sigverify"
)

// Reply keys for the signature verdict on a displayPlaylist acceptance
// (feral-file/ffos-user#307). Additive, inside the same map that carries
// ok — see docs/controld-inbound-controller-messages.md. Stable strings:
// controllers read them.
const (
	replyKeySignatureStatus = "signatureStatus"
	replyKeySigners         = "signers"
	replyKeyLegacySignature = "legacySignature"
)

// annotateCastReply merges the verdict into the reply map the caller is
// about to return: into "message" when the reply has the player's
// {messageID, message:{ok...}} envelope (or the deferred acceptance built in
// the same shape), else into the top level — mirroring how
// playerresponse.OK locates ok. Anything that is not a map is returned
// untouched; the verdict is reporting, never a reason to fail a reply.
//
// Mutates in place (the map is this process's own decode of the player's
// reply, not shared) and returns it for call-site readability.
func annotateCastReply(result any, v *sigverify.Verdict) any {
	m, ok := result.(map[string]any)
	if !ok || v == nil {
		return result
	}
	target := m
	if msg, ok := m["message"].(map[string]any); ok {
		target = msg
	}
	target[replyKeySignatureStatus] = string(v.Status)
	if len(v.Signers) > 0 {
		signers := make([]any, 0, len(v.Signers))
		for _, s := range v.Signers {
			entry := map[string]any{
				"alg":  s.Alg,
				"kid":  s.Kid,
				"role": s.Role,
				"ok":   s.OK,
			}
			if s.Reason != "" {
				entry["reason"] = s.Reason
			}
			signers = append(signers, entry)
		}
		target[replyKeySigners] = signers
	}
	if v.LegacyPresent {
		target[replyKeyLegacySignature] = true
	}
	return m
}

// logSignatureVerdict writes the one structured line per cast that makes the
// verdict greppable on a device: status, each signer's identity and outcome,
// and the playlist identity. Warn for a false claim (invalid), Info
// otherwise — an unsigned app cast is the ordinary case today and must not
// page. Kids are DIDs bounded by sigverify (MaxKidLen), safe to log; the
// playlist URL is the caster's own input and already logged by the cast
// path. The playlist id is caster-controlled and unbounded on the open hub
// (a 4 MiB inline cast may carry a 4 MiB id), so it is cut to the daemon's
// standard log-field cap before it reaches the journal.
func (h *handler) logSignatureVerdict(v *sigverify.Verdict, playlistID, playlistURL string) {
	source := "inline"
	if playlistURL != "" {
		source = "url"
	}
	fields := []zap.Field{
		zap.String("signature_status", string(v.Status)),
		zap.ByteString("playlist_id", helper.TruncateBytes([]byte(playlistID), logger.MAX_FIELD_LENGTH)),
		zap.String("source", source),
		zap.Bool("legacy_signature", v.LegacyPresent),
	}
	if v.Reason != "" {
		fields = append(fields, zap.String("reason", v.Reason))
	}
	if len(v.Signers) > 0 {
		fields = append(fields, zap.Any("signers", v.Signers))
	}
	if v.Status == sigverify.StatusInvalid {
		h.logger.Warn("displayPlaylist: playlist signature verification failed", fields...)
		return
	}
	h.logger.Info("displayPlaylist: playlist signature verdict", fields...)
}

// strictRejection returns the SigInvalidError strict mode raises for v, or
// nil when v proves the document valid. A nil verdict is a rejection too: it
// means the document could not be verified (today: the offline cached copy,
// whose stored body is a hydrated re-marshal), and strict does not guess.
// The reason is Verdict.PublicReason, the closed vocabulary — never
// Verdict.Reason, which carries the document's own role string and belongs
// in the log and the owner's cast reply only.
// Restoring the offline fallback under strict needs the download-time
// verdict persisted beside the cached record — the follow-up noted on
// loadCachedPlaylistForURL.
func strictRejection(v *sigverify.Verdict) *SigInvalidError {
	if v == nil {
		return &SigInvalidError{Reason: "cached copy carries no verdict"}
	}
	if v.Status == sigverify.StatusValid {
		return nil
	}
	return &SigInvalidError{Status: v.Status, Reason: v.PublicReason()}
}

// StrictPushGate builds the scheduler's push gate (playlistschedule's
// SetPushGate) from the owner's mode reader: a scheduler-owned cutover is
// judged AT PUSH TIME through the same Mode.Allows predicate as a cast, so
// a schedule accepted under notify cannot carry a non-valid cohort onto the
// screen after the owner switches to strict. The scheduler's cached document
// keeps the verdict attached at cast time (cloned by pointer, never
// persisted: a restart-restored source is refetched, and so re-verified,
// before it can push again), so the gate reads the same verdict the cast
// reply reported. The error text uses the public vocabulary only; it goes to
// the scheduler's log, not to a caller.
// SchedulerPushGate is the gate the displayAt scheduler consults before every
// cutover (timer, wake, reconnect, retry) and every mode-relaxation re-drive.
// It is a hard block while a factory reset is staged, THEN the strict-mode
// decision. The reset block does not depend on the mode: a staged reset owns
// the screen (the reset narration) and is about to reboot, and it clears the
// mode record — so the strict gate alone would read the restored default
// (notify) and wave an in-flight cutover through, overwriting the narration.
// A blocked cutover holds the current active set and arms no retry; if the
// reset rolls back, devicectl's latch release re-drives from the record then
// on disk. resetStaged nil means "not wired" (never blocks).
func SchedulerPushGate(resetStaged func() bool, mode func() sigverify.Mode) func(playlist *dp1.Playlist) error {
	strict := StrictPushGate(mode)
	return func(playlist *dp1.Playlist) error {
		if resetStaged != nil && resetStaged() {
			return errors.New("factory reset staged")
		}
		return strict(playlist)
	}
}

func StrictPushGate(mode func() sigverify.Mode) func(playlist *dp1.Playlist) error {
	return func(playlist *dp1.Playlist) error {
		var verdict *sigverify.Verdict
		if playlist != nil {
			verdict = playlist.Verification
		}
		if mode == nil || mode().Allows(verdict) {
			return nil
		}
		reason := "document carries no verdict"
		if verdict != nil {
			reason = verdict.PublicReason()
		}
		return fmt.Errorf("strict signature verification: %s", reason)
	}
}
