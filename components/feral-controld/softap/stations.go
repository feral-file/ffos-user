package softap

import (
	"context"
	"errors"
	"sync"

	"github.com/mdlayher/wifi"
	"go.uber.org/zap"
)

// StationCounter reports how many stations are associated with the device's
// own access-point interface: the ground truth for "is the phone that
// attached to the setup hotspot still on it". The provisioning machine polls
// it while the screen shows the portal-address QR (feral-file#3515), so a
// phone that joined, probed, and left hands the screen back to the join QR
// within seconds instead of after the portal-silence backstop.
//
// ok is false when the count is unknown: no interface is in AP mode (the
// hotspot is down or mid-teardown), the kernel query failed or timed out, or
// a previous query is still outstanding. Callers treat unknown as "no
// evidence", never as "nobody attached".
type StationCounter interface {
	AttachedStations(ctx context.Context) (n int, ok bool)
}

// errNoAPInterface marks a query that found no interface in AP mode.
var errNoAPInterface = errors.New("no wireless interface in AP mode")

// stationQuery is one open kernel session: count runs the station dump,
// close releases the socket — and, called from another goroutine, unblocks
// a count that is stuck in the kernel. The seam exists so the timeout and
// single-flight behaviour below can be tested without a radio.
type stationQuery interface {
	count() (int, error)
	close() error
}

// nl80211Stations reads the station list straight from the kernel over
// generic netlink (nl80211 GET_STATION dump). Chosen over the two obvious
// alternatives because of who controld runs as: the daemon is a user service
// (feralfile), the system bus policy denies that user every wpa_supplicant
// method AND signal (dbus-wpa_supplicant.conf, root only), and the image
// ships no `iw`. nl80211 reads need no privilege, so this works as-is on the
// package rail — field-verified on FF1 (kernel 6.18) 2026-09-08.
type nl80211Stations struct {
	logger *zap.Logger
	open   func() (stationQuery, error)

	// busy is the single-flight guard: a query that outlived its ctx is
	// closed, but the goroutine running it returns only once the kernel
	// lets go, and a poll landing before then must not open a second
	// socket on top of it. Reported unknown instead — bounded at one
	// outstanding query no matter how the kernel behaves.
	mu   sync.Mutex
	busy bool
}

// NewNL80211StationCounter builds the netlink-backed counter.
func NewNL80211StationCounter(logger *zap.Logger) StationCounter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &nl80211Stations{logger: logger, open: openNL80211}
}

// AttachedStations runs one query on its own goroutine and honors ctx: the
// netlink round trip is local and sub-millisecond, but the machine's loop
// must never inherit a stall from it. A query that outlives ctx is reported
// unknown AND closed — closing the socket unblocks the goroutine — and until
// that goroutine has returned, further calls report unknown without opening
// anything (review on 789e53d: overlapping timed-out queries accumulated
// goroutines and sockets across polls and re-raises).
func (s *nl80211Stations) AttachedStations(ctx context.Context) (int, bool) {
	s.mu.Lock()
	if s.busy {
		s.mu.Unlock()
		s.logger.Warn("softap: previous station query still outstanding; treating the count as unknown")
		return 0, false
	}
	s.busy = true
	s.mu.Unlock()

	q, err := s.open()
	if err != nil {
		s.release()
		s.logger.Warn("softap: station query could not open nl80211; treating the count as unknown", zap.Error(err))
		return 0, false
	}
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		defer s.release()
		n, err := q.count()
		// Idempotent with the timeout path's close: the second close of a
		// closed socket returns an error nobody needs.
		_ = q.close()
		done <- result{n: n, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = q.close()
		s.logger.Warn("softap: station query outlived its deadline; closed it and treating the count as unknown", zap.Error(ctx.Err()))
		return 0, false
	case r := <-done:
		if r.err != nil {
			if !errors.Is(r.err, errNoAPInterface) {
				s.logger.Warn("softap: station query failed; treating the count as unknown", zap.Error(r.err))
			}
			return 0, false
		}
		return r.n, true
	}
}

func (s *nl80211Stations) release() {
	s.mu.Lock()
	s.busy = false
	s.mu.Unlock()
}

// nl80211Query wraps one wifi.Client. A fresh socket per query (a socket
// open is cheap next to the 2 s poll cadence) keeps the timeout path simple:
// close it and forget it, no shared connection to recover.
type nl80211Query struct{ c *wifi.Client }

func openNL80211() (stationQuery, error) {
	c, err := wifi.New()
	if err != nil {
		return nil, err
	}
	return &nl80211Query{c: c}, nil
}

func (q *nl80211Query) close() error { return q.c.Close() }

// count returns the stations on the first interface in AP mode —
// NetworkManager flips the device's one radio to AP for the hotspot, so
// "the AP interface" is unambiguous on FF1.
func (q *nl80211Query) count() (int, error) {
	ifis, err := q.c.Interfaces()
	if err != nil {
		return 0, err
	}
	for _, ifi := range ifis {
		if ifi.Type != wifi.InterfaceTypeAP || ifi.Name == "" {
			continue
		}
		st, err := q.c.StationInfo(ifi)
		if err != nil {
			return 0, err
		}
		return len(st), nil
	}
	return 0, errNoAPInterface
}
