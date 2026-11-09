package mintpairing

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	minter "github.com/feral-file/ff-art-computer-handoff/clients/ephemeral-token-minter/go"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Display grants (docs/ff1-control-v1.md §5.7): a Play 0.5 site sends its
// display-grant keys with its mint request and, once the owner approves, is
// issued a display-only grant instead of a relayer session. It then casts
// as sealed envelopes over the v2 relay. The approval UX, the fences and the
// abandon rules are the session path's; only what is minted differs.

// DisplayGrantIssuer is the control authority's display-grant surface.
// *control.Authority implements it.
type DisplayGrantIssuer interface {
	IssueDisplayGrant(approver string, req control.DisplayGrantRequest) (control.DisplayGrant, error)
	RevokeDisplayGrant(ctl string) error
	Status() control.Status
}

// Rejection reasons of a 0.5 request (also the controller outcome's reason).
const (
	// reasonTooManySites: the display-grant cap; the owner removes a site.
	reasonTooManySites = "too_many_sites"
	// reasonDisplayGrantUnavailable: malformed keys, no v2 endpoint, no
	// authority, or a write the device could not confirm. Play 0.5 shows it
	// as "this FF1 needs an update".
	reasonDisplayGrantUnavailable = "display_grant_unavailable"
)

// SetDisplayGrants wires the issuer and the v2 endpoint. See Service.
func (s *service) SetDisplayGrants(issuer DisplayGrantIssuer, relayV2Endpoint string) {
	if s == nil {
		return
	}
	s.displayGrants, s.relayV2Endpoint = issuer, relayV2Endpoint
}

// isDisplayRequest reports a 0.5 request: display-grant keys were sent, well
// formed or not, in either copy the device holds. Such a request is never
// answered with a session.
func isDisplayRequest(channel brokerChannel, request minter.MintRequest) bool {
	announced, held := announcedDisplayGrant(channel, request)
	return len(request.DisplayGrant) > 0 || (held && len(announced) > 0)
}

// announcedDisplayGrant is the displayGrant member of the request the site
// announced at create (play#19, read from the channel when the device joined
// one), and whether the device holds that copy NEXT to the encrypted one.
// The announced request itself (Seq 0) has no second copy.
func announcedDisplayGrant(channel brokerChannel, request minter.MintRequest) (json.RawMessage, bool) {
	j, ok := channel.(interface {
		JoinedMintRequest() (minter.MintRequest, bool)
	})
	if !ok || request.Seq == 0 {
		return nil, false
	}
	joined, ok := j.JoinedMintRequest()
	if !ok {
		return nil, false
	}
	return joined.DisplayGrant, true
}

// displayGrantKeysFor reads the site's keys from the copy that will be used,
// and refuses when the device holds both copies and they disagree.
func displayGrantKeysFor(channel brokerChannel, request minter.MintRequest) (control.DisplayGrantRequest, error) {
	announced, held := announcedDisplayGrant(channel, request)
	return displayGrantKeys(announced, held, request.DisplayGrant)
}

// displayGrantKeys is the one validation hook for both entry points. With
// both copies held, one carrying keys and the other not is as much a
// mismatch as different keys: a site never does that, and falling back to a
// session for it would hand a 0.5 site something it cannot use.
func displayGrantKeys(announced json.RawMessage, held bool, encrypted json.RawMessage) (control.DisplayGrantRequest, error) {
	if held && (len(announced) > 0) != (len(encrypted) > 0) {
		return control.DisplayGrantRequest{}, errors.New("display grant keys in only one of the announced and encrypted requests")
	}
	keys, err := minter.ParseDisplayGrantKeys(encrypted)
	if err != nil {
		return control.DisplayGrantRequest{}, err
	}
	if held {
		other, err := minter.ParseDisplayGrantKeys(announced)
		if err != nil {
			return control.DisplayGrantRequest{}, fmt.Errorf("announced request: %w", err)
		}
		if other.SigJWK != keys.SigJWK || other.EncJWK != keys.EncJWK {
			return control.DisplayGrantRequest{}, errors.New("announced and encrypted display grant keys differ")
		}
	}
	sig, err := control.PublicJWKFromECDSA(keys.Sig)
	if err != nil {
		return control.DisplayGrantRequest{}, err
	}
	enc, err := control.PublicJWKFromECDH(keys.Enc)
	if err != nil {
		return control.DisplayGrantRequest{}, err
	}
	return control.DisplayGrantRequest{Sig: sig, Enc: enc}, nil
}

// displayGrantExpiry maps the owner's decision to the grant's expiry: a
// timed grant lives as long as the session would have, a kept one never
// expires, and the fallback (owner kept, requester never declared the
// capability) gets the longest timed life.
func displayGrantExpiry(lifetime sessionLifetime, request minter.MintRequest, now time.Time) int64 {
	switch lifetime {
	case lifetimePersistent:
		return 0
	case lifetimeTimedFallbackRequester:
		return now.Add(maxSessionTTLSeconds * time.Second).Unix()
	default:
		return now.Add(time.Duration(effectiveSessionTTLSeconds(request.RequestedExpiresInSeconds)) * time.Second).Unix()
	}
}

// displayLifetimeName is deliveredOutcomeLifetime for a grant: the grant is
// exactly what was decided, so the decided lifetime is the delivered one.
func displayLifetimeName(lifetime sessionLifetime) string {
	switch lifetime {
	case lifetimePersistent:
		return "persistent"
	case lifetimeTimedFallbackRequester:
		return "timed_fallback_requester"
	default:
		return "timed"
	}
}

// completeDisplayGrant is completeDecisionFor's accepted path for a 0.5
// request: issue the grant, then deliver it, with the session path's fences
// before and after each step. The grant is revoked wherever the session
// would have been: a claim or pairing that moved before delivery, or a
// delivery the broker provably refused.
func (s *service) completeDisplayGrant(ctx context.Context, active *activePairing, channel brokerChannel, request minter.MintRequest, guard topicGuard, approvalRequestID string, decision approvalDecisionRequest) (bool, error) {
	reject := func(reason string, retryable bool, cause error) (bool, error) {
		s.logger.Warn("Refusing a display grant", zap.String("reason", reason), zap.Error(cause),
			zap.String("channelID", request.ChannelID), zap.String("origin", request.Origin))
		if err := s.sendTerminalRejectionAndOutcomeWithReason(channel, request, approvalRequestID, reason, retryable, "failed", reason); err != nil {
			return false, fmt.Errorf("send display grant rejection: %w", err)
		}
		return true, fmt.Errorf("display grant refused: %s", reason)
	}
	// Checked again here although displayGrantPrecheck ran before the owner
	// was asked: a test or a bare completeDecision may skip the precheck.
	grantReq, err := s.displayGrantPrecheck(channel, request)
	if err != nil {
		return reject(reasonDisplayGrantUnavailable, false, err)
	}
	profile := s.displayGrants.Status().Profile
	if profile == nil {
		return reject(reasonDisplayGrantUnavailable, false, errors.New("control state is locked"))
	}
	if !guard.sameAs(currentTopicGuard()) {
		return s.rejectTopicChanged(channel, request, guard.topicID, approvalRequestID)
	}
	if active != nil && !s.isActive(active) {
		return s.rejectSuperseded(channel, request, approvalRequestID)
	}

	lifetime := s.sessionLifetimeFor(decision, request)
	grantReq.Origin = request.Origin
	// The broker attested the origin only when the site created the channel
	// and the device joined it; on a device-initiated channel the origin is
	// the site's own claim.
	grantReq.OriginAttested = active != nil && active.joined
	grantReq.ExpiresAt = displayGrantExpiry(lifetime, request, time.Now())
	grant, err := s.displayGrants.IssueDisplayGrant(decision.Approver, grantReq)
	switch {
	case errors.Is(err, control.ErrTooManyDisplayGrants):
		return reject(reasonTooManySites, false, err)
	case errors.Is(err, control.ErrDurability):
		// Committed but unconfirmed: never hand out a grant the device may
		// forget on a power cut. Retryable: the disk, not the request, is
		// the problem.
		s.revokeAbandonedDisplayGrant(grant.Ctl)
		return reject(reasonDisplayGrantUnavailable, true, err)
	case err != nil:
		return reject(reasonDisplayGrantUnavailable, false, err)
	}
	if !guard.sameAs(currentTopicGuard()) {
		terminalSent, topicErr := s.rejectTopicChanged(channel, request, guard.topicID, approvalRequestID)
		s.revokeAbandonedDisplayGrant(grant.Ctl)
		return terminalSent, topicErr
	}
	if active != nil && !s.beginDelivery(active) {
		terminalSent, err := s.rejectSuperseded(channel, request, approvalRequestID)
		s.revokeAbandonedDisplayGrant(grant.Ctl)
		return terminalSent, err
	}

	result, err := s.displayGrantResult(grant, *profile)
	if err != nil {
		s.revokeAbandonedDisplayGrant(grant.Ctl)
		return reject(reasonDisplayGrantUnavailable, false, err)
	}
	successCtx, cancelSuccess := context.WithTimeout(context.Background(), wrapper.HTTPClientTimeout)
	_, err = channel.SendMintDisplayGrant(successCtx, request, result)
	cancelSuccess()
	if err != nil {
		outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), terminalOperationTimeout)
		s.sendApprovalOutcome(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, "failed", "")
		cancelOutcome()
		if classifyDeliveryFailure(err) == deliveryNeverSent {
			s.revokeAbandonedDisplayGrant(grant.Ctl)
		} else {
			s.logger.Warn("Left a possibly delivered display grant in place after a failed browser delivery",
				zap.Error(err), zap.String("ctl", grant.Ctl))
		}
		return false, fmt.Errorf("send display grant: %w", err)
	}
	if after := currentTopicGuard(); !after.sameAs(guard) {
		s.logger.Warn("Relayer topic changed while delivering a display grant; revoking it",
			zap.String("mintedForTopicFP", helper.TopicFingerprint(guard.topicID)),
			zap.String("currentTopicFP", helper.TopicFingerprint(after.topicID)),
			zap.String("ctl", grant.Ctl))
		s.revokeAbandonedDisplayGrant(grant.Ctl)
	}

	outcomeCtx, cancelOutcome := context.WithTimeout(context.Background(), terminalOperationTimeout)
	s.sendApprovalOutcomeFull(outcomeCtx, approvalRequestID, request.ChannelID, request.MessageID, "completed", displayLifetimeName(lifetime), "", grant.Ctl)
	cancelOutcome()
	return true, nil
}

// displayGrantPrecheck is everything that can refuse a 0.5 request before
// the owner is asked: no authority or v2 endpoint on this device, or keys
// that do not parse or disagree between the two copies. It returns the
// parsed keys for issuance.
func (s *service) displayGrantPrecheck(channel brokerChannel, request minter.MintRequest) (control.DisplayGrantRequest, error) {
	if s.displayGrants == nil || s.relayV2Endpoint == "" {
		return control.DisplayGrantRequest{}, errors.New("no control authority or no v2 endpoint")
	}
	return displayGrantKeysFor(channel, request)
}

// rejectDisplayUnavailable answers a 0.5 request the precheck refused. The
// owner was never asked, so there is no approval outcome to send.
func (s *service) rejectDisplayUnavailable(channel brokerChannel, request minter.MintRequest, cause error) bool {
	s.logger.Warn("Refusing a display grant before approval", zap.Error(cause),
		zap.String("channelID", request.ChannelID), zap.String("origin", request.Origin))
	ctx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	_, err := channel.SendMintRejection(ctx, request, minter.MintRejection{Reason: reasonDisplayGrantUnavailable})
	if err != nil {
		s.logger.Warn("Failed to send display grant rejection", zap.Error(err), zap.String("channelID", request.ChannelID))
	}
	return err == nil
}

// displayGrantsWired reports whether this device can answer 0.5 requests with
// a display grant (control authority and a v2 endpoint). Such a device may
// pair without a legacy topic. Set once at wiring time, before serving.
func (s *service) displayGrantsWired() bool {
	return s.displayGrants != nil && s.relayV2Endpoint != ""
}

// rejectSessionWithoutTopic answers a 0.4 (session) request on a device that
// has no legacy topic. The owner was never asked, so there is no approval
// outcome to send.
func (s *service) rejectSessionWithoutTopic(channel brokerChannel, request minter.MintRequest) bool {
	s.logger.Warn("Refusing a browser session request: no legacy relayer topic",
		zap.String("channelID", request.ChannelID), zap.String("origin", request.Origin))
	ctx, cancel := context.WithTimeout(context.Background(), terminalOperationTimeout)
	defer cancel()
	_, err := channel.SendMintRejection(ctx, request, minter.MintRejection{Reason: "topic_not_ready", Retryable: true})
	if err != nil {
		s.logger.Warn("Failed to send session rejection", zap.Error(err), zap.String("channelID", request.ChannelID))
	}
	return err == nil
}

// displayGrantResult is the mint_succeeded displayGrant member (§5.7).
func (s *service) displayGrantResult(grant control.DisplayGrant, profile control.Profile) (minter.DisplayGrantResult, error) {
	sig, err := json.Marshal(profile.Sig)
	if err != nil {
		return minter.DisplayGrantResult{}, err
	}
	enc, err := json.Marshal(profile.Enc)
	if err != nil {
		return minter.DisplayGrantResult{}, err
	}
	result := minter.DisplayGrantResult{
		Version:         1,
		Ctl:             grant.Ctl,
		Profile:         minter.DisplayGrantProfile{Version: 1, Dev: profile.Dev, Gen: profile.Gen, Sig: sig, Enc: enc},
		RelayV2Endpoint: s.relayV2Endpoint,
	}
	if grant.ExpiresAt != 0 {
		result.ExpiresAt = time.Unix(grant.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	return result, nil
}

// revokeAbandonedDisplayGrant is revokeAbandonedSession for a grant the site
// never received (or may not have): best effort, logged.
func (s *service) revokeAbandonedDisplayGrant(ctl string) {
	if s.displayGrants == nil || ctl == "" {
		return
	}
	if err := s.displayGrants.RevokeDisplayGrant(ctl); err != nil {
		s.logger.Warn("Failed to revoke an abandoned display grant", zap.Error(err), zap.String("ctl", ctl))
	}
}
