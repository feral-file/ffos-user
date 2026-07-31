package offlinecache

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/relayer"
	"github.com/feral-file/ffos-user/components/feral-controld/ws"
)

// notifySendTimeout is the deadline deliver puts on the ctx it hands
// relayer.Send.
//
// It does NOT currently bound anything: relayer.Send never reads its ctx
// (relayer.go — it takes the connection mutex, sets its own WRITE_WAIT
// write deadline, and writes). What actually bounds that leg is WRITE_WAIT
// plus however long the connection mutex is contended, not this value, so
// do not size anything (a shutdown budget, say) against it as though it
// were enforced today. Kept because the ctx parameter is part of that
// interface and a future implementation honoring it should get this bound
// rather than none; making it real belongs in relayer.Send.
//
// Delivery runs on Notifier's own worker goroutine, so an unbounded block
// no longer stalls the capture queue the way it did when the send was
// inline on OnItemStateChanged's caller — see Notifier's doc.
const notifySendTimeout = 5 * time.Second

// ShutdownCloseBudget is the budget main.go passes to CloseWithin at
// daemon shutdown. Not a "default" — CloseWithin always takes an explicit
// budget and has exactly this one caller; the name says what the value is
// for. It lives here, next to the method whose contract it bounds, rather
// than in main.go so a test asserting that the idle path fits inside it
// can name the production value instead of a generous stand-in.
//
// Deliberately a small fraction of main.go's SHUTDOWN_TIMEOUT, which
// covers every cleanup step, not just this one.
const ShutdownCloseBudget = 250 * time.Millisecond

// notifyQueueCapacity bounds how many DISTINCT items may have a pending
// notification at once — NOT how many state transitions may be pending.
// notifyQueue coalesces per item (see its doc), so this is the real
// dimension that grows.
//
// Sized at one max-size playlist (dp1MaxPlaylistItems): the largest burst
// a SINGLE command can produce, so no one downloadPlaylist or
// clearPlaylistCache can overrun it on its own.
//
// This is emphatically NOT a guarantee that drops cannot happen. Several
// producers can exceed it in aggregate, and the design accepts that:
//   - commandrouter's `heavy` rate-limit gate admits downloadPlaylist in
//     bursts, so several max-size playlists can be accepted back to back,
//     each emitting its whole "queued" burst synchronously from admission.
//   - enforceDiskLimit's eviction sweep emits one not_cached per evicted
//     item, bounded by the store's record count — which MaxStatusItems'
//     doc notes can reach tens of thousands — not by any playlist cap.
//
// Raising this to cover those would cost several times the worst-case
// memory to buy a guarantee the eviction sweep still would not honor. The
// drop path (logged, non-blocking) and getOfflineCacheStatus
// reconciliation are the deliberate answer instead — and coalescing makes
// what is lost benign, since a mega-burst's drops land on "queued"
// announcements while each item's later downloading/terminal states
// arrive at capture speed, long after slots have freed. Do not read this
// bound as "drops are impossible" and delete either of those paths.
//
// An earlier revision sized this at 256 "well above what a single
// playlist could plausibly enqueue in a burst". That was wrong in the
// direction that matters: enqueue emits one "queued" per item and
// downloadPlaylist drives it in a tight serial loop, so a full-size
// playlist produced 1024 notifications — 4x that queue — before the
// worker, which does a relayer round trip plus a hub-WS fan-out per
// notification, could plausibly drain even a fraction. Accepted items
// silently lost their state transitions on a completely healthy
// transport, not just a wedged one.
const notifyQueueCapacity = dp1MaxPlaylistItems

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
// enqueue (see notifyQueue's doc), and the actual SendAll call happens on
// this Notifier's own goroutine, one notification at a time, without ever
// blocking the capture worker on however long SendAll itself takes. The
// relayer path goes through that same worker, which is what decouples it
// too — NOT a deadline of its own: relayer.Send ignores the ctx deliver
// hands it, so that leg is bounded only by WRITE_WAIT plus however long
// the relayer's connection mutex is contended (see notifySendTimeout, and
// Close for why neither leg is fast enough for a shutdown budget).
//
// Delivery order is preserved: notifications leave in queue order
// (first-pending-first-out), one at a time, on this one worker, so a
// client never observes an item's states out of sequence. What coalescing
// changes is that intermediate states may be SKIPPED — an item can jump
// straight from nothing to "ready" with no "downloading" in between — not
// that "downloading" could arrive after "ready". See notifyQueue's doc for
// why skipping a superseded state costs a client (almost) nothing, while
// dropping an arbitrary one once a per-event buffer filled cost it real
// progress.
type Notifier struct {
	relayer relayer.Relayer
	ws      ws.WS
	logger  *zap.Logger

	// queue, done, and workerDone are all nil only when NEITHER
	// transport is configured: no background worker is started, and
	// OnItemStateChanged returns early (see its own nil guard), so there
	// is nothing to enqueue into or wait on.
	queue *notifyQueue
	done  chan struct{}
	// workerDone is closed by runWSWorker right before it returns, so
	// Close can block until the worker has actually stopped — mirroring
	// Service.Stop's own "blocks until the worker goroutine has exited"
	// contract (service.go), which is what lets a caller rely on no
	// further SendAll calls firing after Close returns.
	workerDone chan struct{}
	closeOnce  sync.Once
}

// notifyQueue is the Notifier's pending-delivery buffer: a FIFO over item
// IDs holding at most ONE pending notification per item — the latest state
// that item has reached.
//
// Coalescing, rather than buffering every transition, is what makes a
// bounded queue safe here. An item's states are successive (queued ->
// downloading -> ready/partial/failed, or -> not_cached), so a pending
// "queued" that has not been delivered by the time the item reaches
// "ready" tells a client nothing the "ready" does not already imply.
// Superseding it in place is therefore almost always a pure win, and the
// alternative — keeping both and dropping something once the buffer fills
// — is what silently lost accepted items' progress: with one slot per
// EVENT, a 1024-item playlist overran the buffer on its "queued" burst
// alone and the drops landed on whatever arrived last, terminal states
// included.
//
// "Almost always" because these notifications are attempt-level, not
// cache-level (see docs/controld-inbound-controller-messages.md): a failed
// re-download of an already-ready item reports "failed" while the earlier
// successful capture is still on disk and playable. A "failed" superseding
// an undelivered "ready" therefore does discard something. It is not a
// regression — the per-event FIFO's own last message was equally
// attempt-level — and the wire contract already directs clients to
// getOfflineCacheStatus for a cache-level answer, but it is the one case
// where the latest state is not the whole truth.
//
// With one slot per ITEM the buffer holds "the current state of everything
// not yet delivered", so its bound is a count of items in flight rather
// than of transitions, which nothing bounds. push drops only when more
// DISTINCT items than notifyQueueCapacity are pending at once — see that
// constant's doc for which producers can still reach it, and why
// getOfflineCacheStatus rather than a bigger buffer is the answer.
//
// Ordering: an item keeps the queue position of its FIRST pending
// notification rather than moving to the back on each update, so a
// steadily-updating item cannot starve items queued behind it.
type notifyQueue struct {
	mu       sync.Mutex
	capacity int
	// order holds source KEYS (SourceKey of each status's Source) awaiting
	// delivery; pending holds each one's latest status. Coalescing on the
	// key means notifications for the same source from different
	// playlists collapse into one slot — the same identity rule the rest
	// of the package keys on. The two are always mutated together under
	// mu — a key in order always has an entry in pending and vice versa.
	order   []string
	pending map[string]ItemStatus
	// wake is a 1-buffered signal channel, not a data channel: the worker
	// always re-reads order/pending under mu rather than racing two
	// sources of truth (same shape as jobQueue's own wake).
	wake chan struct{}
}

func newNotifyQueue(capacity int) *notifyQueue {
	return &notifyQueue{
		capacity: capacity,
		pending:  make(map[string]ItemStatus),
		wake:     make(chan struct{}, 1),
	}
}

// push records status as its source's pending notification, superseding
// any undelivered one for the same source. It never blocks. dropped is true only
// when this is a NEW item and capacity distinct items are already pending
// — an update to an already-pending item is always accepted, since it
// consumes no additional slot. pending is how many items are awaiting
// delivery after this call, sampled inside the same critical section so a
// caller logging it alongside dropped cannot report a count that
// contradicts the decision (a re-read afterwards could show room the
// worker has since freed).
func (q *notifyQueue) push(status ItemStatus) (dropped bool, pending int) {
	key := SourceKey(status.Source)
	q.mu.Lock()
	if _, queued := q.pending[key]; queued {
		q.pending[key] = status
		pending = len(q.order)
		q.mu.Unlock()
		// No wake: the entry was already queued, so the worker either has
		// not reached it yet (it will see this newer status) or is about
		// to be woken by the signal that queued it.
		return false, pending
	}
	if len(q.order) >= q.capacity {
		pending = len(q.order)
		q.mu.Unlock()
		return true, pending
	}
	q.order = append(q.order, key)
	q.pending[key] = status
	pending = len(q.order)
	q.mu.Unlock()

	select {
	case q.wake <- struct{}{}:
	default:
	}
	return false, pending
}

// pop removes and returns the next pending notification in queue order.
func (q *notifyQueue) pop() (ItemStatus, bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.order) == 0 {
		return ItemStatus{}, false
	}
	key := q.order[0]
	q.order = q.order[1:]
	status := q.pending[key]
	delete(q.pending, key)
	return status, true
}

// len reports how many items are currently awaiting delivery.
func (q *notifyQueue) len() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.order)
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
		n.queue = newNotifyQueue(notifyQueueCapacity)
		n.done = make(chan struct{})
		n.workerDone = make(chan struct{})
		go n.runWSWorker()
	}
	return n
}

func (n *Notifier) OnItemStateChanged(status ItemStatus) {
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
	// Non-blocking. A drop here means more DISTINCT items than
	// notifyQueueCapacity are pending delivery at once — more than a whole
	// max-size playlist — since an update to an already-pending item is
	// coalesced into its existing slot rather than needing a new one (see
	// notifyQueue's doc). Dropping and logging beats blocking the caller
	// for room, which would defeat the entire point of queueing; the
	// client's reconciliation path is getOfflineCacheStatus.
	if dropped, pending := n.queue.push(status); dropped {
		n.logger.Warn("offline cache: dropped offline_cache_status notification, delivery queue full",
			zap.String("source", status.Source), zap.String("state", string(status.State)),
			zap.Int("pending_items", pending))
	}
}

// envelopeFor builds the wire envelope for one notification. Built at
// DELIVERY time rather than at enqueue time so notifyQueue stores only the
// small ItemStatus value it coalesces on, not a per-notification map that
// would be thrown away the moment a newer state superseded it. Mirrors
// status.go's sendNotification envelope shape exactly so mobile clients
// can reuse the same parsing.
func envelopeFor(status ItemStatus) map[string]interface{} {
	return map[string]interface{}{
		"type":                 "notification",
		"notification_type":    string(relayer.NOTIFICATION_TYPE_OFFLINE_CACHE_STATUS),
		"message":              status,
		"persist_record_count": 1,
	}
}

// deliver performs both transports' sends for one queued notification,
// on the worker goroutine. Relayer first, mirroring the order the inline
// implementation used.
func (n *Notifier) deliver(status ItemStatus) {
	envelope := envelopeFor(status)
	if n.relayer != nil && n.relayer.IsConnected() {
		ctx, cancel := context.WithTimeout(context.Background(), notifySendTimeout)
		if err := n.relayer.Send(ctx, envelope); err != nil {
			n.logger.Warn("offline cache: failed to send offline_cache_status via relayer",
				zap.String("source", status.Source), zap.String("state", string(status.State)), zap.Error(err))
		}
		cancel()
	}
	if n.ws != nil {
		n.sendWS(envelope, status)
	}
}

// runWSWorker drains the queue and performs both transports' sends one
// notification at a time, in first-pending order (an item's position is
// the one its earliest undelivered notification took — see notifyQueue),
// until Close is called; see Notifier's own doc for why this runs off
// OnItemStateChanged's caller's goroutine. done is checked before every
// single delivery, not just once per wake, so a Close arriving while many
// notifications are still queued stops delivery at the next notification
// rather than draining everything first — see Close's own doc on why that
// is the intended, documented drop behavior, not a bug.
func (n *Notifier) runWSWorker() {
	defer close(n.workerDone)
	for {
		// Drain everything currently pending before waiting again: wake is
		// a 1-buffered signal, so a burst of pushes can coalesce into a
		// single token and waiting per token would strand the rest.
		for {
			select {
			case <-n.done:
				return
			default:
			}
			status, ok := n.queue.pop()
			if !ok {
				break
			}
			n.deliver(status)
		}
		select {
		case <-n.queue.wake:
		case <-n.done:
			return
		}
	}
}

// sendWS is the hub-WebSocket half of deliver, kept separate so the
// relayer half above reads as its own step.
func (n *Notifier) sendWS(envelope map[string]interface{}, status ItemStatus) {
	if err := n.ws.SendAll(envelope); err != nil {
		n.logger.Warn("offline cache: failed to send offline_cache_status via websocket",
			zap.String("source", status.Source),
			zap.String("state", string(status.State)), zap.Error(err))
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
// simply dropped rather than drained — the worker checks done before
// every delivery (see runWSWorker), so it stops at the next notification
// and abandons the rest — since this only runs at daemon shutdown (see main.go's defer
// ordering, registered to run once Service.Stop has already guaranteed
// no further OnItemStateChanged calls are coming) and there is no value
// in guaranteeing every buffered notification is flushed to a client the
// process is about to stop serving anyway. A SendAll call already IN
// FLIGHT when Close is called is allowed to finish normally (ws.WS.SendAll
// takes no ctx to cancel it early) — Close waits for that one call, not
// for the whole queue.
//
// Neither leg of that call finishes fast enough for a shutdown budget,
// though they fail differently and the difference matters:
//
//   - The relayer leg IS bounded, just far too loosely. deliver waits on
//     that connection's mutex (IsConnected takes it before Send is even
//     reached), and every holder bounds its own critical section —
//     WRITE_WAIT for a write, an explicit deadline for ping, the dialer's
//     HandshakeTimeout for a reconnect — and Go's mutex hands off FIFO
//     once a waiter has blocked long enough, so nothing can starve this
//     wait indefinitely. But a waiter joins the TAIL of that queue: the
//     floor is WRITE_WAIT plus one contended section, and it grows with
//     however many holders are already queued ahead. Even the floor is
//     several times the whole daemon shutdown timeout. Note it is NOT
//     notifySendTimeout that produces any of this: relayer.Send ignores
//     the ctx deliver builds (see that constant's doc).
//   - The hub leg is unbounded in aggregate, which is the categorically
//     worse shape: ws.WS.SendAll caps each per-connection write at its own
//     sendWriteWait but not the loop, so the wait grows with however many
//     clients are connected. Interrupting an in-flight write in ws is the
//     only thing that would fix that, which is why it is the real
//     follow-up rather than a bigger number here.
//
// Daemon shutdown must not spend its whole budget on either — see
// CloseWithin, which is what main.go actually calls.
func (n *Notifier) Close() {
	if n.done == nil {
		return
	}
	n.closeOnce.Do(func() {
		close(n.done)
	})
	<-n.workerDone
}

// CloseWithin is Close bounded to budget, reporting whether the worker
// actually stopped in time. It signals the worker exactly as Close does —
// so no further notification is ever picked up either way — and only the
// WAIT is abandoned when budget expires.
//
// It exists because Close's wait is far longer than the daemon's shutdown
// allows (see its doc): main.go force-exits SHUTDOWN_TIMEOUT after
// cancellation, and every cleanup step registered before the notifier's
// runs AFTER it in defer order, so blocking here strands all of them.
//
// It does NOT make shutdown bounded end to end, and must not be read that
// way. Both transports take, for their own teardown, the very mutex the
// abandoned delivery still holds — hub.Stop -> ws.Close takes each
// connection's write mutex, and relayer.Close takes the relayer's — so a
// wedge simply blocks at whichever of those comes next in LIFO order. The
// slow step is relocated, not removed; removing it means interrupting an
// in-flight write, which belongs in ws and relayer, not here.
//
// What giving up actually buys is therefore leg-dependent. A wedge on the
// hub leg lets mint-pairing, the playlist refresher, the relayer,
// provisioning and the mDNS advertiser run before blocking at hub.Stop; a
// wedge on the relayer leg lets only mint-pairing and the playlist
// refresher run before blocking at relayer.Close. Those two are the only
// steps guaranteed to run in both cases — do not extend this list without
// re-checking main.go's defer registration order against which mutex the
// wedged leg holds.
//
// The trade when budget expires: an in-flight delivery may still be
// running on the worker goroutine after this returns, so the "no further
// SendAll after close" guarantee Close provides no longer holds. That is
// fine for the one caller that accepts it — a process on its way out — and
// is exactly why Close itself is left strict for tests and any caller that
// needs the guarantee rather than the bound.
func (n *Notifier) CloseWithin(budget time.Duration) (stopped bool) {
	if n.done == nil {
		return true
	}
	n.closeOnce.Do(func() {
		close(n.done)
	})
	timer := time.NewTimer(budget)
	defer timer.Stop()
	select {
	case <-n.workerDone:
		return true
	case <-timer.C:
		return false
	}
}
