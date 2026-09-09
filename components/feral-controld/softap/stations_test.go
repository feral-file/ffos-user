package softap

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// blockingQuery is a stationQuery whose count blocks until close is called,
// the way a netlink read stuck in the kernel does.
type blockingQuery struct {
	n        int
	once     sync.Once
	released chan struct{}
	closes   int
	mu       sync.Mutex
}

func newBlockingQuery(n int) *blockingQuery {
	return &blockingQuery{n: n, released: make(chan struct{})}
}

func (q *blockingQuery) count() (int, error) {
	<-q.released
	return q.n, errors.New("socket closed under the read")
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

type instantQuery struct{ n int }

func (q instantQuery) count() (int, error) { return q.n, nil }
func (q instantQuery) close() error        { return nil }

// TestStationQueryTimeoutClosesAndSingleFlights: a query that outlives its
// ctx is closed (which is what unblocks the kernel read), is reported
// unknown, and blocks a second query from opening until it has returned;
// once it has, the next call runs normally.
func TestStationQueryTimeoutClosesAndSingleFlights(t *testing.T) {
	opened := 0
	var current *blockingQuery
	s := &nl80211Stations{logger: zap.NewNop(), open: func() (stationQuery, error) {
		opened++
		current = newBlockingQuery(0)
		return current, nil
	}}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	n, ok := s.AttachedStations(ctx)
	assert.False(t, ok, "a timed-out query is unknown")
	assert.Equal(t, 0, n)
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

	s.open = func() (stationQuery, error) { opened++; return instantQuery{n: 2}, nil }
	n, ok = s.AttachedStations(context.Background())
	assert.True(t, ok)
	assert.Equal(t, 2, n)
	assert.Equal(t, 2, opened)
}

// TestStationQueryBusyReportsUnknownWithoutOpening: while a query is
// outstanding, a concurrent call reports unknown immediately and opens
// nothing.
func TestStationQueryBusyReportsUnknownWithoutOpening(t *testing.T) {
	var opened atomic.Int32
	q := newBlockingQuery(1)
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

	n, ok := s.AttachedStations(context.Background())
	assert.False(t, ok, "busy is unknown")
	assert.Equal(t, 0, n)
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
	n, ok := s.AttachedStations(context.Background())
	assert.False(t, ok)
	assert.Equal(t, 0, n)
}

type errQuery struct{ err error }

func (q errQuery) count() (int, error) { return 0, q.err }
func (q errQuery) close() error        { return nil }
