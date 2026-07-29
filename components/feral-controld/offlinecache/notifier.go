package offlinecache

import (
	"context"
	"sync"
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

// notifyQueueCapacity bounds Notifier's background WS-send queue (see
// wsQueue's doc). Sized well above what a single playlist's captures
// could plausibly enqueue in a burst (each item transitions through at
// most a handful of states — queued/downloading/ready/failed) so the
// common case never drops a notification; it exists only to cap memory
// if the background worker somehow falls permanently behind (e.g. every
// hub client wedged for the whole ws.go sendWriteWait-bounded duration,
// repeatedly, across many items).
const notifyQueueCapacity = 256

// Notifier implements ProgressObserver by dual-sending offline_cache_status
// over the relayer (remote) and hub WS (local) paths, mirroring
// status.go's sendNotification envelope shape exactly so mobile clients can
// reuse the same "type"/"notification_type"/"message" parsing. Unlike
// status.go's poll-driven dedup-by-hash scheme, no dedup is needed here:
// Service.notify only calls OnItemStateChanged on a genuine state
// transition, so every call is already a distinct event worth sending.
//
// The hub WS send runs on its own background worker (runWSWorker) rather
// than inline on OnItemStateChanged's caller: that caller is Service's
// single capture-worker goroutine (see service.go's notify), and
// ws.WS.SendAll's per-connection writes are each individually bounded
// (ws.go's sendWriteWait) but the LOOP over however many hub clients are
// connected has no aggregate bound — a page with several slow/wedged
// clients could otherwise block the capture worker for
// (client count) * sendWriteWait, stalling the entire download queue
// behind what is meant to be a best-effort notification side-channel.
// Queueing decouples that: OnItemStateChanged only does a non-blocking
// enqueue (see wsQueue's doc for the full-queue drop trade-off), and the
// actual SendAll call happens on this Notifier's own goroutine, one
// notification at a time — preserving delivery ORDER (so a hub client is
// guaranteed to observe "downloading" before "ready" for the same item)
// without ever blocking the capture worker on however long SendAll itself
// takes. The relayer path above needs no equivalent change: relayer.Send
// already takes an explicit ctx and is bounded by notifySendTimeout.
type Notifier struct {
	relayer relayer.Relayer
	ws      ws.WS
	logger  *zap.Logger

	// queue, done, and workerDone are all nil only when NEITHER
	// transport is configured: no background worker is started, and
	// OnItemStateChanged returns early (see its own nil guard), so there
	// is nothing to enqueue into or wait on.
	queue chan wsNotification
	done  chan struct{}
	// workerDone is closed by runWSWorker right before it returns, so
	// Close can block until the worker has actually stopped — mirroring
	// Service.Stop's own "blocks until the worker goroutine has exited"
	// contract (service.go), which is what lets a caller rely on no
	// further SendAll calls firing after Close returns.
	workerDone chan struct{}
	closeOnce  sync.Once
}

// wsNotification pairs the exact envelope OnItemStateChanged already
// built (so runWSWorker sends byte-for-byte what a synchronous send
// would have) with the ItemStatus it was built from, purely so
// runWSWorker's own failure log keeps the same item_id/state fields
// OnItemStateChanged's inline log used to have — decoding them back out
// of the generic envelope map on the worker side would be more fragile.
type wsNotification struct {
	envelope map[string]interface{}
	status   ItemStatus
}

// NewNotifier constructs a Notifier and, when w is non-nil, starts its
// background WS-delivery worker immediately (see runWSWorker's doc for
// why the send itself must not happen on this constructor's caller's
// goroutine). Either dependency may be nil (relayer disabled, hub
// disabled) — each send path is skipped independently. Callers must call
// Close once the Notifier will no longer be used (see its doc); Bootstrap
// wires this through Runtime.Notifier for main.go's shutdown sequence.
func NewNotifier(r relayer.Relayer, w ws.WS, logger *zap.Logger) *Notifier {
	n := &Notifier{relayer: r, ws: w, logger: logger}
	// The worker now backs BOTH transports (see OnItemStateChanged), so
	// it is started whenever either one is configured — not just for the
	// hub WS as it was when only that path was queued.
	if w != nil || r != nil {
		n.queue = make(chan wsNotification, notifyQueueCapacity)
		n.done = make(chan struct{})
		n.workerDone = make(chan struct{})
		go n.runWSWorker()
	}
	return n
}

func (n *Notifier) OnItemStateChanged(status ItemStatus) {
	data := map[string]interface{}{
		"type":                 "notification",
		"notification_type":    string(relayer.NOTIFICATION_TYPE_OFFLINE_CACHE_STATUS),
		"message":              status,
		"persist_record_count": 1,
	}

	// Both transports go through the same bounded queue, so this call
	// never blocks its caller.
	//
	// The relayer send used to run inline here, write-deadline-bounded to
	// notifySendTimeout. That bound is per send, and this is called once
	// per item from enqueue — which DownloadPlaylist drives in a serial
	// loop — so a connected-but-backpressured relayer turned one
	// playlist request into (item count) x 5s of blocking inside command
	// admission, minutes past the LAN hub's own response deadline
	// (feral-file/ffos-user#229 review finding). Admission must not pay
	// for delivery.
	//
	// Queueing both also keeps the two transports in one order, and
	// keeps a slow relayer from delaying WS clients (and vice versa),
	// since one worker drains them together per notification.
	if n.queue == nil {
		return
	}
	// Non-blocking: a full queue means the worker has fallen far enough
	// behind that notifyQueueCapacity notifications are already pending
	// (see that constant's doc for how generous that bound already is) —
	// dropping this one and logging is preferable to blocking the caller
	// waiting for room, which would defeat the entire point of queueing.
	select {
	case n.queue <- wsNotification{envelope: data, status: status}:
	default:
		n.logger.Warn("offline cache: dropped offline_cache_status notification, delivery queue full",
			zap.String("item_id", status.ItemID), zap.String("state", string(status.State)))
	}
}

// deliver performs both transports' sends for one queued notification,
// on the worker goroutine. Relayer first, mirroring the order the inline
// implementation used.
func (n *Notifier) deliver(item wsNotification) {
	if n.relayer != nil && n.relayer.IsConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySendTimeout)
		if err := n.relayer.Send(ctx, item.envelope); err != nil {
			n.logger.Warn("offline cache: failed to send offline_cache_status via relayer",
				zap.String("item_id", item.status.ItemID), zap.String("state", string(item.status.State)), zap.Error(err))
		}
		cancel()
	}
	if n.ws != nil {
		n.sendWS(item)
	}
}

// runWSWorker drains the queue and performs both transports' sends one
// notification at a time, in enqueue order, until Close is called — see
// Notifier's own doc for why this runs off OnItemStateChanged's caller's
// goroutine. Select is checked between every notification (not just once
// at the top), so a Close that arrives while several notifications are
// still queued stops delivery promptly rather than draining everything
// first — see Close's own doc on why that is the intended, documented
// drop behavior, not a bug.
func (n *Notifier) runWSWorker() {
	defer close(n.workerDone)
	for {
		select {
		case notification := <-n.queue:
			n.deliver(notification)
		case <-n.done:
			return
		}
	}
}

// sendWS is the hub-WebSocket half of deliver, kept separate so the
// relayer half above reads as its own step.
func (n *Notifier) sendWS(notification wsNotification) {
	if err := n.ws.SendAll(notification.envelope); err != nil {
		n.logger.Warn("offline cache: failed to send offline_cache_status via websocket",
			zap.String("item_id", notification.status.ItemID),
			zap.String("state", string(notification.status.State)), zap.Error(err))
	}
}

// Close stops the background WS-delivery worker and blocks until it has
// actually exited — mirroring Service.Stop's own contract, and what lets
// a caller (including a test's ctrl.Finish, which must not race a mock
// method call on another goroutine) safely assume no further SendAll
// call will fire after this returns. Safe to call more than once, and
// safe to call even when w was nil at construction (no worker was ever
// started).
//
// Best-effort in what it delivers, not in whether it returns: any
// notifications still buffered in the queue when Close is called are
// simply dropped rather than drained (the worker's own select can react
// to done before or after draining what is left — see runWSWorker's
// doc), since this only runs at daemon shutdown (see main.go's defer
// ordering, registered to run once Service.Stop has already guaranteed
// no further OnItemStateChanged calls are coming) and there is no value
// in guaranteeing every buffered notification is flushed to a client the
// process is about to stop serving anyway. A SendAll call already IN
// FLIGHT when Close is called is allowed to finish normally (ws.WS.SendAll
// takes no ctx to cancel it early) — Close waits for that one call, not
// for the whole queue.
func (n *Notifier) Close() {
	if n.done == nil {
		return
	}
	n.closeOnce.Do(func() {
		close(n.done)
	})
	<-n.workerDone
}
