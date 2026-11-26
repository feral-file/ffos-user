package relayer

import (
	"sync"
	"time"

	"go.uber.org/zap"
)

// dropLogInterval bounds how often one reason is logged.
const dropLogInterval = 30 * time.Second

// dropLog records relay v2 traffic the device discarded before any handler
// saw it: a frame from a source it does not know, a malformed frame or chunk,
// a chunk out of order or past its deadline, a body that is not JSON, and a
// chunked request its sender left unfinished.
//
// These were discarded in silence, so a request that never completed left no
// line on the device and its sender timed out with nothing to compare
// against (feral-file/feral-file#3644). They cannot be logged one line each
// either: the channel is public and any stranger can send them. So the first
// drop of a reason is logged at once and later ones are counted, with at most
// one line per reason per dropLogInterval carrying the count since the last.
// A line never carries a payload, only the reason, the count and the source's
// connection id.
type dropLog struct {
	mu     sync.Mutex
	logger *zap.Logger
	now    func() time.Time
	last   map[string]time.Time
	held   map[string]int
}

func newDropLog(logger *zap.Logger, now func() time.Time) *dropLog {
	return &dropLog{logger: logger, now: now, last: map[string]time.Time{}, held: map[string]int{}}
}

// note counts one drop and logs it when its reason is due a line.
func (d *dropLog) note(reason string, fields ...zap.Field) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.held[reason]++
	now := d.now()
	if last, seen := d.last[reason]; seen && now.Sub(last) < dropLogInterval {
		d.mu.Unlock()
		return
	}
	count := d.held[reason]
	d.held[reason] = 0
	d.last[reason] = now
	d.mu.Unlock()
	d.logger.Warn("Relay v2 request dropped before dispatch",
		append([]zap.Field{zap.String("reason", reason), zap.Int("count", count)}, fields...)...)
}
