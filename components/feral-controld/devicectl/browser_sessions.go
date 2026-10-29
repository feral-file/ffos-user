package devicectl

import (
	"context"

	"go.uber.org/zap"
)

// BrowserSessionCleanup is the mint-pairing surface factory reset needs: wait
// for session creations already sent to the relayer to finish, then revoke
// what the relayer holds for the outgoing topic. Owned here, by the consumer,
// so devicectl never has to know about mint pairing itself.
//
// It lives outside executor.go on purpose: the Executor mock is generated from
// that file, and a mock carrying this type would import devicectl, which
// devicectl's own tests already import back.
type BrowserSessionCleanup interface {
	// CloseActivePairing ends a pairing session in progress and waits for its
	// worker to exit. A worker left polling would keep answering for a claim
	// the reset has already invalidated.
	CloseActivePairing(ctx context.Context) (closed bool, err error)
	WaitForInFlightCreates(ctx context.Context) (inFlight int, err error)
	RevokeTopicSessions(ctx context.Context, topicID string) (revoked int, err error)
}

// SetBrowserSessionCleanup injects the seam factory reset uses to end the
// outgoing topic's browser sessions before it clears the claim. Optional: an
// executor without it resets exactly as it did before owner-kept sessions
// existed. Wired once at composition time, before commands are served — the
// same type-asserted seam shape as SetBootRecoverySession.
func SetBrowserSessionCleanup(exec Executor, cleanup BrowserSessionCleanup, logger *zap.Logger) {
	setter, ok := exec.(interface {
		setBrowserSessionCleanup(BrowserSessionCleanup)
	})
	if !ok {
		logger.Warn("Executor does not support browser session cleanup wiring")
		return
	}
	setter.setBrowserSessionCleanup(cleanup)
}

func (e *executor) setBrowserSessionCleanup(cleanup BrowserSessionCleanup) {
	e.browserSessions = cleanup
}
