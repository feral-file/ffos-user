package softap

import (
	"context"
	"errors"

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
// hotspot is down or mid-teardown), or the kernel query failed or timed out.
// Callers treat unknown as "no evidence", never as "nobody attached".
type StationCounter interface {
	AttachedStations(ctx context.Context) (n int, ok bool)
}

// errNoAPInterface marks a query that found no interface in AP mode.
var errNoAPInterface = errors.New("no wireless interface in AP mode")

// nl80211Stations reads the station list straight from the kernel over
// generic netlink (nl80211 GET_STATION dump). Chosen over the two obvious
// alternatives because of who controld runs as: the daemon is a user service
// (feralfile), the system bus policy denies that user every wpa_supplicant
// method AND signal (dbus-wpa_supplicant.conf, root only), and the image
// ships no `iw`. nl80211 reads need no privilege, so this works as-is on the
// package rail — field-verified on FF1 (kernel 6.18) 2026-09-08.
type nl80211Stations struct {
	logger *zap.Logger
}

// NewNL80211StationCounter builds the netlink-backed counter.
func NewNL80211StationCounter(logger *zap.Logger) StationCounter {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &nl80211Stations{logger: logger}
}

// AttachedStations queries the kernel on its own goroutine and honors ctx:
// the netlink round trip is local and sub-millisecond, but the machine's
// loop must never inherit a stall from it, so a query that outlives ctx is
// reported unknown and left to finish on its own.
func (s *nl80211Stations) AttachedStations(ctx context.Context) (int, bool) {
	type result struct {
		n   int
		err error
	}
	done := make(chan result, 1)
	go func() {
		n, err := countAPStations()
		done <- result{n: n, err: err}
	}()
	select {
	case <-ctx.Done():
		s.logger.Warn("softap: station query outlived its deadline; treating the count as unknown", zap.Error(ctx.Err()))
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

// countAPStations opens a fresh nl80211 client per call (a socket open is
// cheap next to the 2 s poll cadence, and a long-lived socket would need its
// own recovery path) and counts the stations on the first interface in AP
// mode — NetworkManager flips the device's one radio to AP for the hotspot,
// so "the AP interface" is unambiguous on FF1.
func countAPStations() (int, error) {
	c, err := wifi.New()
	if err != nil {
		return 0, err
	}
	// A close failure on a read-only socket changes nothing about the count
	// already taken; log-free by design, the next poll opens a fresh one.
	defer func() { _ = c.Close() }()
	ifis, err := c.Interfaces()
	if err != nil {
		return 0, err
	}
	for _, ifi := range ifis {
		if ifi.Type != wifi.InterfaceTypeAP || ifi.Name == "" {
			continue
		}
		st, err := c.StationInfo(ifi)
		if err != nil {
			return 0, err
		}
		return len(st), nil
	}
	return 0, errNoAPInterface
}
