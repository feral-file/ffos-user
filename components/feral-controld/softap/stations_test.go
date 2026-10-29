package softap

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// blockingQuery is a stationQuery whose dump blocks until close is called,
// the way a netlink read stuck in the kernel does.
type blockingQuery struct {
	macs     []string
	once     sync.Once
	released chan struct{}
	closes   int
	mu       sync.Mutex
}

func newBlockingQuery(macs ...string) *blockingQuery {
	return &blockingQuery{macs: macs, released: make(chan struct{})}
}

func (q *blockingQuery) stations() ([]string, error) {
	<-q.released
	return q.macs, errors.New("socket closed under the read")
}

func (q *blockingQuery) close() error {
	q.mu.Lock()
	q.closes++
	q.mu.Unlock()
	q.once.Do(func() { close(q.released) })
	return nil
}

func (q *blockingQuery) closeCount() int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closes
}

type instantQuery struct{ macs []string }

func (q instantQuery) stations() ([]string, error) { return q.macs, nil }
func (q instantQuery) close() error                { return nil }

// TestStationQueryTimeoutClosesAndSingleFlights: a query that outlives its
// ctx is closed (which is what unblocks the kernel read), is reported
// unknown, and blocks a second query from opening until it has returned;
// once it has, the next call runs normally.
func TestStationQueryTimeoutClosesAndSingleFlights(t *testing.T) {
	opened := 0
	var current *blockingQuery
	s := &nl80211Stations{logger: zap.NewNop(), open: func() (stationQuery, error) {
		opened++
		current = newBlockingQuery()
		return current, nil
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	macs, ok := s.AttachedStations(ctx)
	assert.False(t, ok, "a timed-out query is unknown")
	assert.Empty(t, macs)
	require.Equal(t, 1, opened)
	assert.GreaterOrEqual(t, current.closeCount(), 1, "the timeout closes the socket")

	// The goroutine releases the guard once the (now unblocked) read
	// returns; a call landing first must not open a second socket.
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.busy
	}, time.Second, time.Millisecond, "the released read hands the guard back")
	assert.Equal(t, 1, opened, "no second query opened by the timeout path")

	s.open = func() (stationQuery, error) {
		opened++
		return instantQuery{macs: []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}}, nil
	}
	macs, ok = s.AttachedStations(context.Background())
	assert.True(t, ok)
	assert.Equal(t, []string{"aa:bb:cc:dd:ee:01", "aa:bb:cc:dd:ee:02"}, macs)
	assert.Equal(t, 2, opened)
}

// TestStationQueryBusyReportsUnknownWithoutOpening: while a query is
// outstanding, a concurrent call reports unknown immediately and opens
// nothing.
func TestStationQueryBusyReportsUnknownWithoutOpening(t *testing.T) {
	var opened atomic.Int32
	q := newBlockingQuery("aa:bb:cc:dd:ee:01")
	s := &nl80211Stations{logger: zap.NewNop(), open: func() (stationQuery, error) {
		opened.Add(1)
		return q, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	first := make(chan bool, 1)
	go func() {
		_, ok := s.AttachedStations(ctx)
		first <- ok
	}()
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.busy
	}, time.Second, time.Millisecond)

	macs, ok := s.AttachedStations(context.Background())
	assert.False(t, ok, "busy is unknown")
	assert.Empty(t, macs)
	assert.Equal(t, int32(1), opened.Load(), "the busy call opened nothing")

	cancel()
	assert.False(t, <-first, "the canceled first query is unknown too")
	require.Eventually(t, func() bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		return !s.busy
	}, time.Second, time.Millisecond)
}

// TestStationQueryOpenFailureReleasesTheGuard: a failed open is unknown and
// must not leave the guard held.
func TestStationQueryOpenFailureReleasesTheGuard(t *testing.T) {
	s := &nl80211Stations{logger: zap.NewNop(), open: func() (stationQuery, error) {
		return nil, errors.New("no nl80211")
	}}
	_, ok := s.AttachedStations(context.Background())
	assert.False(t, ok)
	s.mu.Lock()
	defer s.mu.Unlock()
	assert.False(t, s.busy)
}

// TestStationQueryNoAPInterfaceIsUnknown: station mode (no AP interface) is
// unknown, not zero.
func TestStationQueryNoAPInterfaceIsUnknown(t *testing.T) {
	s := &nl80211Stations{logger: zap.NewNop(), open: func() (stationQuery, error) {
		return errQuery{err: errNoAPInterface}, nil
	}}
	macs, ok := s.AttachedStations(context.Background())
	assert.False(t, ok)
	assert.Empty(t, macs)
}

type errQuery struct{ err error }

func (q errQuery) stations() ([]string, error) { return nil, q.err }
func (q errQuery) close() error                { return nil }

// TestNeighborMACReadsCompleteEntriesOnly: the neighbor table answers for a
// resolved row keyed by the exact IP, and never for an incomplete one — an
// entry still being resolved (flags 0x0) carries a placeholder address that
// would be cached as the attached phone's identity and never match a station.
func TestNeighborMACReadsCompleteEntriesOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "arp")
	require.NoError(t, os.WriteFile(path, []byte(
		"IP address       HW type     Flags       HW address            Mask     Device\n"+
			"10.42.0.22       0x1         0x2         AA:BB:CC:DD:EE:01     *        wlan0\n"+
			"10.42.0.23       0x1         0x0         00:00:00:00:00:00     *        wlan0\n"+
			"10.42.0.24       0x1         0x2         aa:bb:cc:dd:ee:04     *        wlan0\n"),
		0o600))
	old := procNetARP
	procNetARP = path
	t.Cleanup(func() { procNetARP = old })

	mac, ok := NeighborMAC("10.42.0.22")
	assert.True(t, ok)
	assert.Equal(t, "aa:bb:cc:dd:ee:01", mac, "the address is normalized to lowercase colon-hex")

	_, ok = NeighborMAC("10.42.0.23")
	assert.False(t, ok, "an incomplete entry is not an answer")

	_, ok = NeighborMAC("10.42.0.99")
	assert.False(t, ok, "no row for this address")

	_, ok = NeighborMAC("")
	assert.False(t, ok)

	// A missing table (no hotspot, no procfs) is unknown, never a panic.
	procNetARP = filepath.Join(dir, "absent")
	_, ok = NeighborMAC("10.42.0.22")
	assert.False(t, ok)
}
