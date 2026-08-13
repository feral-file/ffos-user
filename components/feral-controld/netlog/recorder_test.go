package netlog

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeClock is a manually-advanced wrapper.Clock. SleepContext parks until
// releaseSleeps is closed (or ctx dies) so the stability-window tests control
// exactly when the window "elapses". Hand-rolled for the same import-cycle
// reason as the exec fake.
type fakeClock struct {
	mu            sync.Mutex
	now           time.Time
	releaseSleeps chan struct{}
	// pruneTicks, when non-nil, backs the ticker handed out for the
	// recorder's prune interval so tests can fire retention sweeps on demand;
	// every other ticker never fires.
	pruneTicks chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Unix(1700000000, 0), releaseSleeps: make(chan struct{})}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeClock) Sleep(time.Duration) {}

func (c *fakeClock) SleepContext(ctx context.Context, _ time.Duration) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.releaseSleeps:
		return nil
	}
}

func (c *fakeClock) NewTicker(d time.Duration) wrapper.Ticker {
	if d == defaultPruneInterval && c.pruneTicks != nil {
		return fakeTicker{ch: c.pruneTicks}
	}
	return fakeTicker{}
}

type fakeTicker struct {
	ch chan time.Time // nil = never fires; tests drive edges directly
}

func (t fakeTicker) C() <-chan time.Time { return t.ch }
func (fakeTicker) Stop()                 {}
func (fakeTicker) Reset(d time.Duration) {}

// countingLadder returns a canned class and counts runs.
type countingLadder struct {
	runs  atomic.Int64
	class Class
}

func (l *countingLadder) Run(_ context.Context, trigger string) *LadderResult {
	l.runs.Add(1)
	return &LadderResult{Trigger: trigger, Class: l.class, Link: Step{Status: StatusOK}}
}

// harness builds a started recorder over a temp ring; callers drive observes
// and use waitRecords to synchronize on the async worker.
type harness struct {
	t     *testing.T
	dir   string
	rec   *Recorder
	clock *fakeClock
	lad   *countingLadder
}

func newHarness(t *testing.T, mut func(*RecorderOptions)) *harness {
	t.Helper()
	dir := t.TempDir()
	ring, err := OpenRing(dir, 0)
	require.NoError(t, err)
	clock := newFakeClock()
	lad := &countingLadder{class: ClassWANDown}
	opts := RecorderOptions{
		Ring:   ring,
		Ladder: lad,
		Clock:  clock,
		Logger: zap.NewNop(),
		stamp: func() Stamp {
			return Stamp{Wall: clock.Now(), UptimeS: 1, BootID: "test-boot"}
		},
	}
	if mut != nil {
		mut(&opts)
	}
	rec := NewRecorder(opts)
	rec.Start(context.Background())
	t.Cleanup(rec.Stop)
	return &harness{t: t, dir: dir, rec: rec, clock: clock, lad: lad}
}

// waitRecords polls the ring until pred is satisfied or times out, returning
// the records seen last. The worker is async and every append is fsynced, so
// polling the files is the honest synchronization point.
func (h *harness) waitRecords(pred func([]Record) bool) []Record {
	h.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	var recs []Record
	for time.Now().Before(deadline) {
		recs, _ = readRing(h.t, h.dir)
		if pred(recs) {
			return recs
		}
		time.Sleep(5 * time.Millisecond)
	}
	h.t.Fatalf("ring never satisfied predicate; have %d records: %+v", len(recs), recs)
	return nil
}

func kinds(recs []Record) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Kind)
	}
	return out
}

func countKind(recs []Record, kind string) int {
	n := 0
	for _, r := range recs {
		if r.Kind == kind {
			n++
		}
	}
	return n
}

// TestRecorderDedupesInternetStream: producers report levels, the recorder
// records transitions — repeated same-value observations must not spam the ring.
func TestRecorderDedupesInternetStream(t *testing.T) {
	h := newHarness(t, nil)

	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(true)

	h.waitRecords(func(r []Record) bool { return countKind(r, KindInternet) >= 1 })
	// Give the worker a beat to (wrongly) write duplicates, then re-read.
	time.Sleep(50 * time.Millisecond)
	recs, _ := readRing(t, h.dir)
	assert.Equal(t, 1, countKind(recs, KindInternet), "kinds: %v", kinds(recs))
}

// TestRecorderOutageEpisode drives online→offline→online and pins the whole
// episode contract: failure edge runs the ladder, reconnect closes the
// episode with the ladder's class, seals the segment, and LastOutage serves
// the summary.
func TestRecorderOutageEpisode(t *testing.T) {
	h := newHarness(t, nil)

	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(false)
	h.clock.advance(30 * time.Second)
	h.rec.ObserveInternet(true)

	recs := h.waitRecords(func(r []Record) bool { return countKind(r, KindOutageEnd) == 1 })
	assert.Equal(t, 1, countKind(recs, KindLadder), "failure edge must run the ladder once")
	assert.EqualValues(t, 1, h.lad.runs.Load())

	var end *Record
	for i := range recs {
		if recs[i].Kind == KindOutageEnd {
			end = &recs[i]
		}
	}
	require.NotNil(t, end.Outage)
	assert.Equal(t, ClassWANDown, end.Outage.Class, "episode adopts the ladder's class")
	assert.Equal(t, 1, end.Outage.Count24h)

	last := h.rec.LastOutage()
	require.NotNil(t, last)
	assert.Equal(t, ClassWANDown, last.Class)
	assert.Equal(t, 1, last.Count24h)
	assert.True(t, last.End.After(last.Start))

	// The close must have sealed the episode's segment.
	h.rec.ring.mu.Lock()
	activeSeq := h.rec.ring.activeSeq
	h.rec.ring.mu.Unlock()
	assert.GreaterOrEqual(t, activeSeq, 2, "segment must roll when an episode closes")
}

// TestRecorderLadderRateLimit: a flapping network gets edges recorded but the
// ladder runs at most once per interval, with the suppression itself recorded.
func TestRecorderLadderRateLimit(t *testing.T) {
	h := newHarness(t, nil)

	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(false)
	h.clock.advance(10 * time.Second) // inside the 60s min interval
	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(false)

	recs := h.waitRecords(func(r []Record) bool { return countKind(r, KindInternet) == 4 })
	h.waitRecords(func(r []Record) bool {
		for _, rec := range r {
			if rec.Kind == KindNote && rec.Note == "ladder_rate_limited" {
				return true
			}
		}
		return false
	})
	assert.EqualValues(t, 1, h.lad.runs.Load(), "second failure edge inside the window must not run the ladder")
	assert.Equal(t, 1, countKind(recs, KindLadder))

	// Past the window, the next failure edge probes again.
	h.rec.ObserveInternet(true)
	h.clock.advance(2 * time.Minute)
	h.rec.ObserveInternet(false)
	h.waitRecords(func(r []Record) bool { return countKind(r, KindLadder) == 2 })
	assert.EqualValues(t, 2, h.lad.runs.Load())
}

// TestRecorderStabilityUpload: the self-upload fires only after the window
// elapses with the verdict still online.
func TestRecorderStabilityUpload(t *testing.T) {
	var uploads atomic.Int64
	h := newHarness(t, func(o *RecorderOptions) {
		o.SelfUpload = func() { uploads.Add(1) }
	})

	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(false)
	h.rec.ObserveInternet(true)
	h.waitRecords(func(r []Record) bool { return countKind(r, KindOutageEnd) == 1 })

	assert.EqualValues(t, 0, uploads.Load(), "upload must wait for the stability window")
	close(h.clock.releaseSleeps)

	// The trigger record is written just before the upload func runs; poll the
	// counter rather than racing that ordering.
	assert.Eventually(t, func() bool { return uploads.Load() == 1 },
		3*time.Second, 5*time.Millisecond, "upload must fire once the window elapses online")
}

// TestRecorderSelfUploadRateLimit: a flapping site closing outages every few
// minutes must not push one full log bundle per recovery — the second upload
// inside the min interval is suppressed and the suppression recorded.
func TestRecorderSelfUploadRateLimit(t *testing.T) {
	var uploads atomic.Int64
	h := newHarness(t, func(o *RecorderOptions) {
		o.SelfUpload = func() { uploads.Add(1) }
	})

	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(false)
	h.rec.ObserveInternet(true)
	h.waitRecords(func(r []Record) bool { return countKind(r, KindOutageEnd) == 1 })
	close(h.clock.releaseSleeps) // every stability window now elapses instantly
	assert.Eventually(t, func() bool { return uploads.Load() == 1 }, 3*time.Second, 5*time.Millisecond)

	// Second episode well inside the 6h min interval.
	h.clock.advance(30 * time.Minute)
	h.rec.ObserveInternet(false)
	h.rec.ObserveInternet(true)
	h.waitRecords(func(r []Record) bool {
		for _, rec := range r {
			if rec.Kind == KindUploadState && rec.Note == "self_upload_rate_limited" {
				return true
			}
		}
		return false
	})
	assert.EqualValues(t, 1, uploads.Load(), "second upload inside the min interval must be suppressed")

	// Past the interval, uploads resume.
	h.clock.advance(7 * time.Hour)
	h.rec.ObserveInternet(false)
	h.rec.ObserveInternet(true)
	assert.Eventually(t, func() bool { return uploads.Load() == 2 }, 3*time.Second, 5*time.Millisecond)
}

// TestRecorderStabilityUploadCanceledByRelapse: a relapse inside the window
// must cancel the pending upload (uploading into a flapping network burns the
// single-flight slot for nothing).
func TestRecorderStabilityUploadCanceledByRelapse(t *testing.T) {
	var uploads atomic.Int64
	h := newHarness(t, func(o *RecorderOptions) {
		o.SelfUpload = func() { uploads.Add(1) }
	})

	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(false)
	h.rec.ObserveInternet(true)
	h.waitRecords(func(r []Record) bool { return countKind(r, KindOutageEnd) == 1 })

	h.clock.advance(90 * time.Second)
	h.rec.ObserveInternet(false) // relapse while the window is pending
	h.waitRecords(func(r []Record) bool { return countKind(r, KindInternet) == 4 })

	close(h.clock.releaseSleeps) // window "elapses" after the relapse
	time.Sleep(100 * time.Millisecond)
	assert.EqualValues(t, 0, uploads.Load(), "relapse must cancel the pending upload")
}

// TestRecorderRelayerCloseCodeDedupe: the close-frame path reports
// (false, code) first and closeConn's code-less (false, 0) follows — exactly
// one teardown record survives, and it is the code-carrying one.
func TestRecorderRelayerCloseCodeDedupe(t *testing.T) {
	h := newHarness(t, nil)

	h.rec.ObserveRelayer(true, 0)
	h.rec.ObserveRelayer(false, 1006) // read loop saw the close frame
	h.rec.ObserveRelayer(false, 0)    // closeConn teardown, deduped away

	h.waitRecords(func(r []Record) bool { return countKind(r, KindRelayer) == 2 })
	time.Sleep(50 * time.Millisecond)
	recs, _ := readRing(t, h.dir)
	require.Equal(t, 2, countKind(recs, KindRelayer), "duplicate teardown must dedupe")
	var disc *RelayerEvent
	for _, rec := range recs {
		if rec.Kind == KindRelayer && !rec.Relayer.Connected {
			disc = rec.Relayer
		}
	}
	require.NotNil(t, disc)
	assert.Equal(t, 1006, disc.CloseCode, "the surviving teardown record carries the close code")
}

// TestRecorderOnDemandDiagnostics: RunDiagnostics bypasses the rate limit,
// records the run, and upgrades an open outage's class.
func TestRecorderOnDemandDiagnostics(t *testing.T) {
	h := newHarness(t, nil)
	h.lad.class = ClassUnknownProbe

	h.rec.ObserveInternet(true)
	h.rec.ObserveInternet(false) // auto ladder returns unknown-probe
	h.waitRecords(func(r []Record) bool { return countKind(r, KindLadder) == 1 })

	h.lad.class = ClassCaptivePortal
	res, err := h.rec.RunDiagnostics(context.Background())
	require.NoError(t, err)
	assert.Equal(t, ClassCaptivePortal, res.Class)

	h.rec.ObserveInternet(true)
	recs := h.waitRecords(func(r []Record) bool { return countKind(r, KindOutageEnd) == 1 })
	for _, rec := range recs {
		if rec.Kind == KindOutageEnd {
			assert.Equal(t, ClassCaptivePortal, rec.Outage.Class,
				"on-demand evidence must upgrade the episode class")
		}
	}
	assert.Equal(t, 2, countKind(recs, KindLadder), "on-demand run is recorded too")
}

// TestRecorderBootDuringOutage: the first observation being offline must open
// an episode (a device booting mid-outage is the MoMA case).
func TestRecorderBootDuringOutage(t *testing.T) {
	h := newHarness(t, nil)

	h.rec.ObserveInternet(false)
	h.waitRecords(func(r []Record) bool { return countKind(r, KindLadder) == 1 })

	h.rec.ObserveInternet(true)
	recs := h.waitRecords(func(r []Record) bool { return countKind(r, KindOutageEnd) == 1 })
	for _, rec := range recs {
		if rec.Kind == KindInternet && rec.Internet != nil && !rec.Internet.Connected {
			assert.Equal(t, "initial", rec.Internet.Source)
		}
	}
}

// TestRecorderProvTransitionRecordedOnce: one observed transition produces
// exactly one KindProv record (pins the single-append contract a PR review
// claimed was violated — it is not, and now cannot silently become so).
func TestRecorderProvTransitionRecordedOnce(t *testing.T) {
	h := newHarness(t, nil)

	h.rec.ObserveProvTransition("online", "offline_retrying", "", "clear-offline")

	h.waitRecords(func(r []Record) bool { return countKind(r, KindProv) >= 1 })
	time.Sleep(50 * time.Millisecond) // window for a (wrong) duplicate to land
	recs, _ := readRing(t, h.dir)
	require.Equal(t, 1, countKind(recs, KindProv), "one transition = exactly one record")
	for _, rec := range recs {
		if rec.Kind == KindProv {
			assert.Equal(t, "offline_retrying", rec.Prov.To)
			assert.Equal(t, "clear-offline", rec.Prov.Reason)
		}
	}
}

// TestRecorderDropAccounting: producer overflow is counted and surfaced
// in-band, never silently swallowed.
func TestRecorderDropAccounting(t *testing.T) {
	dir := t.TempDir()
	ring, err := OpenRing(dir, 0)
	require.NoError(t, err)
	clock := newFakeClock()
	rec := NewRecorder(RecorderOptions{
		Ring: ring, Ladder: nil, Clock: clock, Logger: zap.NewNop(),
		stamp: func() Stamp { return Stamp{Wall: clock.Now()} },
	})
	// Flood BEFORE starting the worker so the queue genuinely overflows.
	for i := 0; i < eventQueueDepth+50; i++ {
		rec.ObserveProvTransition("a", "b", "", "flood")
	}
	rec.Start(context.Background())
	t.Cleanup(rec.Stop)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		recs, _ := readRing(t, dir)
		for _, r := range recs {
			if r.Kind == KindNote && r.Note == "events_dropped" {
				assert.GreaterOrEqual(t, r.Dropped, int64(50))
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("dropped events were never surfaced in-band")
}

// TestRecorderPeriodicPruneEnforcesRetention: retention must not depend on
// outage activity. A device that stays healthy after its last outage never
// appends (dedupe) and so never rolls — the worker's periodic tick is the
// only thing standing between a sealed segment and outliving the 7-day
// window into every support bundle.
func TestRecorderPeriodicPruneEnforcesRetention(t *testing.T) {
	pruneTicks := make(chan time.Time)
	h := newHarness(t, func(opts *RecorderOptions) {
		opts.Clock.(*fakeClock).pruneTicks = pruneTicks
	})

	// Plant a stale sealed segment AFTER open (open-time pruning already ran).
	// A far-off seq so it can never collide with the live active segment.
	stale := filepath.Join(h.dir, segmentName(999999))
	require.NoError(t, os.WriteFile(stale, []byte("{}\n"), 0o600))
	old := time.Now().Add(-8 * 24 * time.Hour)
	require.NoError(t, os.Chtimes(stale, old, old))

	pruneTicks <- time.Now()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(stale); os.IsNotExist(err) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("stale sealed segment survived the periodic prune tick")
}
