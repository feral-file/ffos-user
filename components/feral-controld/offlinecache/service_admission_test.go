package offlinecache_test

import (
	"context"
	"strconv"
	"sync"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// fakeAdmission is a hand-controlled AdmissionController: each class is
// allowed or denied by explicit test toggles, standing in for the real
// metrics-driven gate (admission_test.go covers that policy on its own).
type fakeAdmission struct {
	mu     sync.Mutex
	denied map[offlinecache.MediaClass]bool
	// calls counts Admit invocations per class, so a test can pin
	// dequeueAdmitted's per-class memoization (see
	// TestService_Admission_ScanEvaluatesGateOncePerClass).
	calls map[offlinecache.MediaClass]int
}

func newFakeAdmission() *fakeAdmission {
	return &fakeAdmission{denied: make(map[offlinecache.MediaClass]bool)}
}

func (f *fakeAdmission) setDenied(class offlinecache.MediaClass, denied bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.denied[class] = denied
}

func (f *fakeAdmission) Admit(class offlinecache.MediaClass) offlinecache.AdmissionDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.calls == nil {
		f.calls = map[offlinecache.MediaClass]int{}
	}
	f.calls[class]++
	if f.denied[class] {
		return offlinecache.AdmissionDecision{Allowed: false, Reason: "test denial"}
	}
	return offlinecache.AdmissionDecision{Allowed: true, Reason: "test allow"}
}

// setupAdmissionService mirrors setupService but wires the fake gate and a
// fast retry interval so deferral loops resolve in test time rather than
// the production 3s cadence.
func setupAdmissionService(t *testing.T, gate offlinecache.AdmissionController, observer offlinecache.ProgressObserver) *serviceTestSetup {
	t.Helper()
	ts := setupService(t, 0, nil)
	ts.service = offlinecache.NewService(ts.store, ts.mockClassifier, ts.mockCapturer,
		ts.mockMediaCapturer, wrapper.NewJSON(), 5000, 0, observer,
		offlinecache.AdmissionOptions{
			Controller:    gate,
			RetryInterval: 10 * time.Millisecond,
		}, zaptest.NewLogger(t))
	return ts
}

func TestService_Admission_DeniedJobStaysQueuedThenProceeds(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	obs := &recordingObserver{}
	ts := setupAdmissionService(t, gate, obs)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{
		Item: item, Entry: item.Source,
		Coverage: offlinecache.Coverage{Complete: true},
	}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	captureStarted := make(chan struct{})
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			close(captureStarted)
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	// While denied, the job must sit in StateQueued — visibly queued, not
	// downloading, not failed — across several retry intervals.
	time.Sleep(100 * time.Millisecond)
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{item.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateQueued, snap.Items[0].State,
		"a deferred job must remain queued on the wire")
	select {
	case <-captureStarted:
		t.Fatal("capture must not start while admission is denied")
	default:
	}

	// Pressure clears: the retry tick alone (no new enqueue) must admit it.
	gate.setDenied(offlinecache.ClassSoftware, false)
	waitForState(t, ts.service, item.Source, offlinecache.StateReady)

	// Client-visible progression ran forward, exactly once each — deferral
	// added no extra notifications.
	assert.Equal(t, []offlinecache.ItemState{
		offlinecache.StateQueued, offlinecache.StateDownloading, offlinecache.StateReady,
	}, obs.statesFor(item.Source))
}

// TestService_Admission_MediaSkipsPastDeferredSoftware pins the skip-scan
// selection policy. The gate's verdict depends only on class, and the two
// classes have very different thresholds — software is gated strictly on
// temperature, media (a plain HTTP GET) permissively — so a thermally
// deferred software job must NOT hold up media work behind it in the
// shared FIFO. Displaying artwork is the priority; caching it is
// deferrable, and the two must not be coupled by queue position.
func TestService_Admission_MediaSkipsPastDeferredSoftware(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	ts := setupAdmissionService(t, gate, nil)
	defer ts.ctrl.Finish()

	software := dp1playlist.PlaylistItem{ID: "item-software", Source: "https://example.com/index.html"}
	media := dp1playlist.PlaylistItem{ID: "item-media", Source: "https://example.com/video.mp4"}
	softwareRec := &offlinecache.ItemRecord{
		Item: software, Entry: software.Source,
		Coverage: offlinecache.Coverage{Complete: true},
	}
	mediaRec := &offlinecache.ItemRecord{
		Item: media, Entry: media.Source,
		Coverage: offlinecache.Coverage{Complete: true},
	}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), software.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), media.Source).Return(offlinecache.ClassMedia, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), software, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(softwareRec))
			return softwareRec, nil
		}).Times(1)
	ts.mockMediaCapturer.EXPECT().Capture(gomock.Any(), media).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(mediaRec))
			return mediaRec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), software))
	require.NoError(t, ts.service.DownloadItem(context.Background(), media))

	// The media job is admissible on its own terms and must run straight
	// away, even though a deferred software job sits ahead of it.
	waitForState(t, ts.service, media.Source, offlinecache.StateReady)

	// The software job is still waiting — not failed, not started.
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{software.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateQueued, snap.Items[0].State,
		"a thermally deferred software job waits rather than failing")

	// It runs once pressure clears.
	gate.setDenied(offlinecache.ClassSoftware, false)
	waitForState(t, ts.service, software.Source, offlinecache.StateReady)
}

func TestService_Admission_DeferredJobIsClearableWithoutBusy(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	ts := setupAdmissionService(t, gate, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	// No Capturer expectation: a cleared deferred job must never capture.

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	// Give the worker time to scan-and-defer this job at least once, so
	// the clear exercises "deferred" rather than "never seen".
	time.Sleep(50 * time.Millisecond)

	// A deferred job is still StateQueued, so clearing it must succeed —
	// deferral must never surface as ErrItemBusy (that error is reserved
	// for an item actively mid-capture).
	require.NoError(t, ts.service.ClearItem(item.Source))

	// Even once admission opens up, the cleared job must not resurrect.
	gate.setDenied(offlinecache.ClassSoftware, false)
	time.Sleep(100 * time.Millisecond)
	waitForState(t, ts.service, item.Source, offlinecache.StateNotCached)
}

func TestService_Admission_StopReturnsPromptlyWhileDeferred(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	ts := setupAdmissionService(t, gate, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	time.Sleep(50 * time.Millisecond)

	// Stop must not wait out the deferral: the shutdown drain bypasses the
	// admission gate (see run's drain comment).
	done := make(chan struct{})
	go func() {
		ts.service.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Stop() blocked behind a deferred job")
	}
}

// TestService_Admission_ReDownloadAfterClearWhileDeferred covers
// clear-then-re-download of an item that is currently deferred. This used
// to guard a per-job deferral clock the clear had to reset (a stale one
// would expire the fresh job almost immediately); with no deadline left
// there is no clock to inherit, but the underlying race — a clear removing
// a queued job while the worker is scanning it — is still worth pinning.
func TestService_Admission_ReDownloadAfterClearWhileDeferred(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	ts := setupAdmissionService(t, gate, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{
		Item: item, Entry: item.Source,
		Coverage: offlinecache.Coverage{Complete: true},
	}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(2)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	// First download defers for most of the budget, then is cleared.
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	time.Sleep(400 * time.Millisecond)
	require.NoError(t, ts.service.ClearItem(item.Source))

	// Re-download immediately: the fresh job is queued and deferred like
	// any other, unaffected by the cleared one.
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	time.Sleep(400 * time.Millisecond)
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{item.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	require.Equal(t, offlinecache.StateQueued, snap.Items[0].State,
		"a re-downloaded item waits on pressure like any other, and is never dropped")

	// And it still completes once pressure clears.
	gate.setDenied(offlinecache.ClassSoftware, false)
	waitForState(t, ts.service, item.Source, offlinecache.StateReady)
}

// TestService_Admission_DeferredMediaNeverDropped is the media half of the
// invariant: a job leaves the queue only by being processed or by an
// explicit clear — never on a timer, for any class. An earlier revision
// failed a job deferred past a maxDefer bound; that bound existed to
// unwedge a head-of-line-blocked FIFO, which skip-scan already solved, and
// keeping it for one class made "deferred" mean different things depending
// on what you were downloading.
func TestService_Admission_DeferredMediaNeverDropped(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassMedia, true)
	obs := &recordingObserver{}
	ts := setupAdmissionService(t, gate, obs)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	rec := &offlinecache.ItemRecord{
		Item: item, Entry: item.Source,
		Coverage: offlinecache.Coverage{Complete: true},
	}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassMedia, nil).Times(1)
	ts.mockMediaCapturer.EXPECT().Capture(gomock.Any(), item).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	time.Sleep(300 * time.Millisecond)
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{item.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateQueued, snap.Items[0].State)
	assert.NotContains(t, obs.statesFor(item.Source), offlinecache.StateFailed,
		"deferral is a scheduling delay, never a terminal state")

	gate.setDenied(offlinecache.ClassMedia, false)
	waitForState(t, ts.service, item.Source, offlinecache.StateReady)
	// The client saw queued -> downloading -> ready; no failure was ever
	// announced for a job that only had to wait.
	assert.Equal(t, []offlinecache.ItemState{
		offlinecache.StateQueued, offlinecache.StateDownloading, offlinecache.StateReady,
	}, obs.statesFor(item.Source))
}

// TestService_Admission_DeferredSoftwareNeverExpires pins the no-deadline
// half of the policy: caching an artwork is optional, deferrable work,
// while keeping the panel stable is not. A software job denied on
// temperature must therefore wait for the device to recover rather than
// being failed out of the queue, however long that takes. Failing it
// would turn "we will cache this later" into a client-visible error the
// device could not avoid.
func TestService_Admission_DeferredSoftwareNeverExpires(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	obs := &recordingObserver{}
	ts := setupAdmissionService(t, gate, obs)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{
		Item: item, Entry: item.Source,
		Coverage: offlinecache.Coverage{Complete: true},
	}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	// Many retry ticks later: still queued, never failed.
	time.Sleep(300 * time.Millisecond)
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{item.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateQueued, snap.Items[0].State,
		"software deferral has no deadline: it waits for the device, it does not fail")
	assert.NotContains(t, obs.statesFor(item.Source), offlinecache.StateFailed)

	// And it still runs when the device recovers.
	gate.setDenied(offlinecache.ClassSoftware, false)
	waitForState(t, ts.service, item.Source, offlinecache.StateReady)
}

// TestService_Admission_SoftwareOrderPreservedAmongItself checks the skip
// scan did not turn into reordering WITHIN a class. Only cross-class
// overtaking is intended; two software jobs must still run in the order
// they were queued, since that is the order a playlist implies.
func TestService_Admission_SoftwareOrderPreservedAmongItself(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	ts := setupAdmissionService(t, gate, nil)
	defer ts.ctrl.Finish()

	first := dp1playlist.PlaylistItem{ID: "sw-1", Source: "https://example.com/a.html"}
	second := dp1playlist.PlaylistItem{ID: "sw-2", Source: "https://example.com/b.html"}
	for _, it := range []dp1playlist.PlaylistItem{first, second} {
		ts.mockClassifier.EXPECT().Classify(gomock.Any(), it.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	}

	var mu sync.Mutex
	var order []string
	for _, it := range []dp1playlist.PlaylistItem{first, second} {
		item := it
		rec := &offlinecache.ItemRecord{
			Item: item, Entry: item.Source,
			Coverage: offlinecache.Coverage{Complete: true},
		}
		ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
			func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
				mu.Lock()
				order = append(order, item.ID)
				mu.Unlock()
				require.NoError(t, ts.store.SaveItem(rec))
				return rec, nil
			}).Times(1)
	}

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), first))
	require.NoError(t, ts.service.DownloadItem(context.Background(), second))

	gate.setDenied(offlinecache.ClassSoftware, false)
	waitForState(t, ts.service, first.Source, offlinecache.StateReady)
	waitForState(t, ts.service, second.Source, offlinecache.StateReady)

	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"sw-1", "sw-2"}, order,
		"FIFO must still hold within a class; only cross-class overtaking is intended")
}

// callsFor reports how many times Admit was asked about a class.
func (f *fakeAdmission) callsFor(class offlinecache.MediaClass) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[class]
}

// TestService_Admission_ScanEvaluatesGateOncePerClass pins the
// memoization the skip-scan depends on. The scan walks EVERY queued job
// looking for an admissible one, so without a per-class memo a queue of N
// same-class jobs would cost N gate evaluations per pass — and since
// deferral is now unbounded, that pass repeats every retryInterval
// forever, all of it under s.mu. Deferral must cost O(classes), not
// O(queue length), per tick.
func TestService_Admission_ScanEvaluatesGateOncePerClass(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	gate.setDenied(offlinecache.ClassMedia, true)
	ts := setupAdmissionService(t, gate, nil)
	defer ts.ctrl.Finish()

	// The queue is deliberately LOPSIDED — six software jobs to one media
	// job — because a balanced queue cannot tell the two implementations
	// apart: per-job evaluation and per-class memoization would both yield
	// equal counts. Skewed, per-job evaluation costs 6 software calls per
	// tick against 1 media call, while memoization costs exactly one each.
	var items []dp1playlist.PlaylistItem
	for i := range 6 {
		sw := dp1playlist.PlaylistItem{ID: "sw-" + strconv.Itoa(i), Source: "https://example.com/a" + strconv.Itoa(i) + ".html"}
		ts.mockClassifier.EXPECT().Classify(gomock.Any(), sw.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
		items = append(items, sw)
	}
	md := dp1playlist.PlaylistItem{ID: "md-0", Source: "https://example.com/v0.mp4"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), md.Source).Return(offlinecache.ClassMedia, nil).Times(1)
	items = append(items, md)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	for _, it := range items {
		require.NoError(t, ts.service.DownloadItem(context.Background(), it))
	}

	// Let a bounded number of retry ticks (10ms each) run against the
	// fully deferred queue.
	time.Sleep(200 * time.Millisecond)

	swCalls := gate.callsFor(offlinecache.ClassSoftware)
	mdCalls := gate.callsFor(offlinecache.ClassMedia)
	require.Positive(t, mdCalls, "the gate must actually have been consulted")
	// Memoized, every tick costs exactly one call per class, so the two
	// counts track 1:1 despite the 6:1 job skew. Per-job evaluation would
	// put software at ~6x media.
	assert.InDelta(t, mdCalls, swCalls, float64(mdCalls),
		"each scan must evaluate each class once, not once per queued job")
}

// TestService_Admission_SoftwareSkipsPastDeferredMedia is the mirror of
// MediaSkipsPastDeferredSoftware. The two buckets latch independently, so
// overtaking in the other direction (media deferred on memory, software
// admissible) is a distinct path through the scan and must work the same
// way — the policy is "take the first admissible job", not "software
// yields to media".
func TestService_Admission_SoftwareSkipsPastDeferredMedia(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassMedia, true)
	ts := setupAdmissionService(t, gate, nil)
	defer ts.ctrl.Finish()

	media := dp1playlist.PlaylistItem{ID: "item-media", Source: "https://example.com/video.mp4"}
	software := dp1playlist.PlaylistItem{ID: "item-software", Source: "https://example.com/index.html"}
	mediaRec := &offlinecache.ItemRecord{Item: media, Entry: media.Source, Coverage: offlinecache.Coverage{Complete: true}}
	softwareRec := &offlinecache.ItemRecord{Item: software, Entry: software.Source, Coverage: offlinecache.Coverage{Complete: true}}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), media.Source).Return(offlinecache.ClassMedia, nil).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), software.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockMediaCapturer.EXPECT().Capture(gomock.Any(), media).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(mediaRec))
			return mediaRec, nil
		}).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), software, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(softwareRec))
			return softwareRec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	// Media first in the queue, and denied.
	require.NoError(t, ts.service.DownloadItem(context.Background(), media))
	require.NoError(t, ts.service.DownloadItem(context.Background(), software))

	waitForState(t, ts.service, software.Source, offlinecache.StateReady)
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{media.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateQueued, snap.Items[0].State)

	gate.setDenied(offlinecache.ClassMedia, false)
	waitForState(t, ts.service, media.Source, offlinecache.StateReady)
}
