package offlinecache_test

import (
	"context"
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
	if f.denied[class] {
		return offlinecache.AdmissionDecision{Allowed: false, Reason: "test denial"}
	}
	return offlinecache.AdmissionDecision{Allowed: true, Reason: "test allow"}
}

// setupAdmissionService mirrors setupService but wires the fake gate and a
// fast retry interval so deferral loops resolve in test time rather than
// the production 3s cadence.
func setupAdmissionService(t *testing.T, gate offlinecache.AdmissionController, maxDefer time.Duration, observer offlinecache.ProgressObserver) *serviceTestSetup {
	t.Helper()
	ts := setupService(t, 0, nil)
	ts.service = offlinecache.NewService(ts.store, ts.mockClassifier, ts.mockCapturer,
		ts.mockMediaCapturer, wrapper.NewJSON(), 5000, 0, observer,
		offlinecache.AdmissionOptions{
			Controller:    gate,
			MaxDefer:      maxDefer,
			RetryInterval: 10 * time.Millisecond,
		}, zaptest.NewLogger(t))
	return ts
}

func TestService_Admission_DeniedHeadStaysQueuedThenProceeds(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	obs := &recordingObserver{}
	ts := setupAdmissionService(t, gate, 0, obs)
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
		"a deferred head must remain queued on the wire")
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

// TestService_Admission_MediaBehindDeniedSoftwareHeadWaits pins the
// head-of-queue design decision: admission is strictly FIFO, so a media
// job queued behind a deferred software head waits with it (no skip-ahead
// reordering — see dequeueAdmitted's doc).
func TestService_Admission_MediaBehindDeniedSoftwareHeadWaits(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	ts := setupAdmissionService(t, gate, 0, nil)
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

	// The media job is admissible on its own terms but must not overtake
	// the deferred software head.
	time.Sleep(100 * time.Millisecond)
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{media.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateQueued, snap.Items[0].State,
		"FIFO must hold: media queued behind a deferred software head does not start")

	gate.setDenied(offlinecache.ClassSoftware, false)
	waitForState(t, ts.service, software.Source, offlinecache.StateReady)
	waitForState(t, ts.service, media.Source, offlinecache.StateReady)
}

func TestService_Admission_DeferredHeadIsClearableWithoutBusy(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	ts := setupAdmissionService(t, gate, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	// No Capturer expectation: a cleared deferred job must never capture.

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	// Give the worker time to peek-and-defer the head at least once, so
	// the clear exercises "deferred head" rather than "never seen".
	time.Sleep(50 * time.Millisecond)

	// A deferred head is still StateQueued, so clearing it must succeed —
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
	ts := setupAdmissionService(t, gate, 0, nil)
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
		t.Fatal("Stop() blocked behind a deferred head")
	}
}

// TestService_Admission_ReDownloadAfterClearGetsFreshDeferClock is the
// regression for stale head-deferral tracking: clearing a deferred item
// and re-downloading it (exactly the retry the expiry failure reason
// suggests) must give the NEW job a full deferral budget, not inherit the
// cleared job's elapsed time and fail on its first denial.
func TestService_Admission_ReDownloadAfterClearGetsFreshDeferClock(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	const maxDefer = 600 * time.Millisecond
	ts := setupAdmissionService(t, gate, maxDefer, nil)
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

	// Re-download immediately: with stale tracking this job would "expire"
	// ~200ms in (400ms inherited + 200ms fresh > 600ms); with a fresh
	// clock it is still within budget and must remain queued.
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	time.Sleep(400 * time.Millisecond)
	snap, err := ts.service.Status(offlinecache.StatusRequest{Sources: []string{item.Source}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	require.Equal(t, offlinecache.StateQueued, snap.Items[0].State,
		"a re-downloaded item must get a fresh deferral budget, not the cleared job's")

	// And it still completes once pressure clears.
	gate.setDenied(offlinecache.ClassSoftware, false)
	waitForState(t, ts.service, item.Source, offlinecache.StateReady)
}

func TestService_Admission_MaxDeferExpiryFailsWithReason(t *testing.T) {
	gate := newFakeAdmission()
	gate.setDenied(offlinecache.ClassSoftware, true)
	obs := &recordingObserver{}
	ts := setupAdmissionService(t, gate, 50*time.Millisecond, obs)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	// No Capturer expectation: an expired deferral never captures.

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	waitForState(t, ts.service, item.Source, offlinecache.StateFailed)

	// The client sees queued then failed-with-reason; downloading is never
	// announced for a job that never started.
	states := obs.statesFor(item.Source)
	require.Equal(t, []offlinecache.ItemState{offlinecache.StateQueued, offlinecache.StateFailed}, states)
	obs.mu.Lock()
	defer obs.mu.Unlock()
	var failedReason string
	for _, st := range obs.statuses {
		if st.Source == item.Source && st.State == offlinecache.StateFailed {
			failedReason = st.Reason
		}
	}
	assert.Contains(t, failedReason, "system pressure")
}
