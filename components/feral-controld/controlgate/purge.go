package controlgate

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
)

// plaintextPersistedTypes are the notification types the status poller has
// always sent in plaintext with persist_record_count 1, which ff-relayer
// stores per topic and replays to every new subscriber (§9).
var plaintextPersistedTypes = []relayer.NotificationType{
	relayer.NOTIFICATION_TYPE_DEVICE_STATUS,
	relayer.NOTIFICATION_TYPE_PLAYER_STATUS,
	relayer.NOTIFICATION_TYPE_DDC_STATUS,
}

// PurgeStalePlaintext overwrites the relayer's stored plaintext history on
// an ENFORCED device: once enforced, plaintext notifications stop, but the
// relayer would keep replaying the last plaintext device/player/ddc status
// to anyone subscribing to the topic, indefinitely. It sends each persisted
// type once, in plaintext, with body {} and persist_record_count 1, which
// replaces the stored row.
//
// Run on EVERY boot in the enforced state, not once per transition: it is
// three tiny idempotent messages, and a marker file would be one more piece
// of state to carry and get wrong. It waits for the relayer connection (up to
// maxWait, polling every poll) and is best-effort: a send failure is logged,
// and the relayer may still hold older rows (a residual, §13). r must be the
// RAW relayer: the sealing wrapper would drop plaintext in this state.
// Returns how many purge messages were sent.
func PurgeStalePlaintext(ctx context.Context, auth *control.Authority, r relayer.Relayer, logger *zap.Logger,
	poll, maxWait time.Duration) int {
	if auth.State() != control.StateEnforced {
		return 0
	}
	deadline := time.Now().Add(maxWait)
	for !r.IsConnected() {
		if time.Now().After(deadline) {
			logger.Warn("Relayer never connected; stale plaintext notifications not purged")
			return 0
		}
		select {
		case <-ctx.Done():
			return 0
		case <-time.After(poll):
		}
	}
	sent := 0
	for _, t := range plaintextPersistedTypes {
		err := r.Send(ctx, relayer.Response{Type: "notification", NotificationType: string(t),
			PersistRecordCount: 1, Message: map[string]any{}})
		if err != nil {
			logger.Warn("Failed to purge stale plaintext notification", zap.String("notification_type", string(t)), zap.Error(err))
			continue
		}
		sent++
	}
	logger.Info("Purged stale plaintext notifications on the relayer (enforced)", zap.Int("types", sent))
	return sent
}
