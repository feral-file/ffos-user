package offlinecache

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

// notifySendTimeout bounds how long relayer.Send may block for one
// offline_cache_status push. OnItemStateChanged runs on Service's worker
// goroutine (see service.go's notify), so an unbounded block here would
// stall the entire download queue behind a slow/backpressured relayer
// connection.
const notifySendTimeout = 5 * time.Second

// Notifier implements ProgressObserver by dual-sending offline_cache_status
// over the relayer (remote) and hub WS (local) paths, mirroring
// status.go's sendNotification envelope shape exactly so mobile clients can
// reuse the same "type"/"notification_type"/"message" parsing. Unlike
// status.go's poll-driven dedup-by-hash scheme, no dedup is needed here:
// Service.notify only calls OnItemStateChanged on a genuine state
// transition, so every call is already a distinct event worth sending.
type Notifier struct {
	relayer relayer.Relayer
	ws      ws.WS
	logger  *zap.Logger
}

// NewNotifier constructs a Notifier. Either dependency may be nil (relayer
// disabled, hub disabled) — each send path is skipped independently.
func NewNotifier(r relayer.Relayer, w ws.WS, logger *zap.Logger) *Notifier {
	return &Notifier{relayer: r, ws: w, logger: logger}
}

func (n *Notifier) OnItemStateChanged(status ItemStatus) {
	data := map[string]interface{}{
		"type":                 "notification",
		"notification_type":    string(relayer.NOTIFICATION_TYPE_OFFLINE_CACHE_STATUS),
		"message":              status,
		"persist_record_count": 1,
	}

	if n.relayer != nil && n.relayer.IsConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySendTimeout)
		if err := n.relayer.Send(ctx, data); err != nil {
			n.logger.Warn("offline cache: failed to send offline_cache_status via relayer",
				zap.String("item_id", status.ItemID), zap.String("state", string(status.State)), zap.Error(err))
		}
		cancel()
	}

	if n.ws != nil {
		if err := n.ws.SendAll(data); err != nil {
			n.logger.Warn("offline cache: failed to send offline_cache_status via websocket",
				zap.String("item_id", status.ItemID), zap.String("state", string(status.State)), zap.Error(err))
		}
	}
}
