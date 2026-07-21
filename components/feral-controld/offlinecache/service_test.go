package offlinecache_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/offlinecache"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type serviceTestSetup struct {
	ctrl           *gomock.Controller
	store          offlinecache.Store
	mockClassifier *mocks.MockOfflineCacheClassifier
	mockCapturer   *mocks.MockOfflineCacheCapturer
	service        offlinecache.Service
}

// setupService wires a Service against a real fsStore (so item/blob/GC
// round-trips are genuine, matching store_test.go's convention) plus mocked
// Classifier/Capturer, since those are the seams that would otherwise
// touch the network or a headless Chromium.
func setupService(t *testing.T, maxDiskBytes int64, observer offlinecache.ProgressObserver) *serviceTestSetup {
	t.Helper()
	ctrl := gomock.NewController(t)
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	// Stop() always closes the capturer (see service.go's Stop doc) —
	// stubbed here rather than per-test since nearly every test defers
	// Stop(); tests asserting the shutdown-close behavior itself set
	// their own tighter expectation instead (see
	// TestService_Stop_ClosesCapturer).
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, wrapper.NewJSON(), 5000, maxDiskBytes, observer, zaptest.NewLogger(t))

	return &serviceTestSetup{
		ctrl: ctrl, store: store, mockClassifier: mockClassifier, mockCapturer: mockCapturer, service: svc,
	}
}

func seedItemWithCapturedAt(t *testing.T, store offlinecache.Store, itemID, blobContent string, capturedAt time.Time) offlinecache.Resource {
	t.Helper()
	hash := writeBlobString(t, store, blobContent)
	res := offlinecache.Resource{URL: "https://example.com/" + itemID, Status: 200, SHA256: hash, ContentType: "text/html"}
	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		ItemID:     itemID,
		Item:       dp1playlist.PlaylistItem{ID: itemID, Source: res.URL},
		Entry:      res.URL,
		Resources:  []offlinecache.Resource{res},
		Coverage:   offlinecache.Coverage{Complete: true},
		CapturedAt: capturedAt,
	}))
	return res
}

func waitForState(t *testing.T, svc offlinecache.Service, itemID string, want offlinecache.ItemState) {
	t.Helper()
	require.Eventually(t, func() bool {
		snap, err := svc.Status([]string{itemID})
		return err == nil && len(snap.Items) == 1 && snap.Items[0].State == want
	}, 2*time.Second, 10*time.Millisecond, "item %s never reached state %s", itemID, want)
}

func TestService_DownloadItem_QueuesAndCapturesSoftwareItem(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{
		ItemID: "item-1", Item: item, Entry: item.Source,
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
	waitForState(t, ts.service, "item-1", offlinecache.StateReady)
}

func TestService_DownloadItem_RejectsNonSoftware(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassMedia, nil).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	err := ts.service.DownloadItem(context.Background(), item)
	assert.ErrorIs(t, err, offlinecache.ErrUnsupportedMediaClass)
}

func TestService_DownloadItem_RequiresIDAndSource(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	err := ts.service.DownloadItem(context.Background(), dp1playlist.PlaylistItem{})
	assert.Error(t, err)
}

func TestService_DownloadItem_ClassifyError(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassUnknown, assertError("network down")).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	err := ts.service.DownloadItem(context.Background(), item)
	assert.Error(t, err)
}

// TestService_DownloadItem_BeforeStartReturnsNotStarted and its
// DownloadPlaylist counterpart below are the regression tests for
// ErrServiceNotStarted: without this guard, DownloadItem/DownloadPlaylist
// would enqueue a job onto s.queue and report success even though no
// worker goroutine exists yet to ever process it (either Start was never
// called, or — see main.go's offline-cache section — Start's own setup
// failed and main.go logged it but kept the service wired into
// commandrouter rather than crashing controld).
func TestService_DownloadItem_BeforeStartReturnsNotStarted(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	// No Classify/Capture expectations: the guard must fire before
	// either is ever reached.

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	err := ts.service.DownloadItem(context.Background(), item)
	assert.ErrorIs(t, err, offlinecache.ErrServiceNotStarted)
}

func TestService_DownloadPlaylist_BeforeStartReturnsNotStarted(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw := json.RawMessage(`{"id":"playlist-1","items":[]}`)
	queued, total, err := ts.service.DownloadPlaylist(context.Background(), raw, "")
	assert.ErrorIs(t, err, offlinecache.ErrServiceNotStarted)
	assert.Zero(t, queued)
	assert.Zero(t, total)
}

// TestService_DownloadItem_AfterStartFailureReturnsNotStarted reproduces
// the exact scenario from the PR review: Start's own index rebuild fails
// (e.g. an unreadable store root), main.go logs and continues rather than
// crashing controld, and a later DownloadItem call must not silently
// queue a job that nothing will ever drain.
func TestService_DownloadItem_AfterStartFailureReturnsNotStarted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockStore := mocks.NewMockOfflineCacheStore(ctrl)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockStore.EXPECT().ListItemIDs().Return(nil, assertError("permission denied")).Times(1)

	svc := offlinecache.NewService(mockStore, mockClassifier, mockCapturer, wrapper.NewJSON(), 5000, 0, nil, zaptest.NewLogger(t))

	err := svc.Start(context.Background())
	require.Error(t, err)

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	downloadErr := svc.DownloadItem(context.Background(), item)
	assert.ErrorIs(t, downloadErr, offlinecache.ErrServiceNotStarted)
}

// TestService_DownloadItem_ConcurrentStopDuringClassifyReturnsNotStarted
// is the regression test for the second review round's race: Classify
// does network I/O and can straddle a concurrent Stop(), so the
// started.Load() fast-fail at the top of DownloadItem is only advisory —
// this pins that enqueue()'s re-check under the same mu Stop uses to
// flip started is what actually stops a job from being pushed onto a
// queue the worker has already stopped draining.
func TestService_DownloadItem_ConcurrentStopDuringClassifyReturnsNotStarted(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}

	classifyStarted := make(chan struct{})
	releaseClassify := make(chan struct{})
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).DoAndReturn(
		func(_ context.Context, _ string) (offlinecache.MediaClass, error) {
			close(classifyStarted)
			<-releaseClassify
			return offlinecache.ClassSoftware, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))

	downloadDone := make(chan error, 1)
	go func() {
		downloadDone <- ts.service.DownloadItem(context.Background(), item)
	}()

	// Wait until DownloadItem is parked inside Classify (i.e. already
	// past its own started.Load() fast-fail), then stop the service out
	// from under it before letting Classify return.
	<-classifyStarted
	ts.service.Stop()
	close(releaseClassify)

	err := <-downloadDone
	assert.ErrorIs(t, err, offlinecache.ErrServiceNotStarted)
}

func TestService_DownloadItem_IdempotentWhileInFlight(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{ItemID: "item-1", Item: item, Coverage: offlinecache.Coverage{Complete: true}}
	gate := make(chan struct{})

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(2)
	// Times(1): a second DownloadItem while the first is still downloading
	// must not schedule a duplicate capture.
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			<-gate
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	require.Eventually(t, func() bool {
		snap, err := ts.service.Status([]string{"item-1"})
		return err == nil && len(snap.Items) == 1 && snap.Items[0].State == offlinecache.StateDownloading
	}, 2*time.Second, 10*time.Millisecond, "worker should have dequeued into downloading before the gate is released")

	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	close(gate)
	waitForState(t, ts.service, "item-1", offlinecache.StateReady)
}

func TestService_DownloadItem_FailureIsReported(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).Return(nil, assertError("capture failed")).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	waitForState(t, ts.service, "item-1", offlinecache.StateFailed)
}

func TestService_DownloadItem_CoverageClassification(t *testing.T) {
	tests := []struct {
		name      string
		reason    string
		wantState offlinecache.ItemState
	}{
		{
			name:      "pure CSP block is reported broken online",
			reason:    "loading_failed(csp_blocked):https://cdn.example/p5.min.js",
			wantState: offlinecache.StateBrokenOnline,
		},
		{
			name:      "mixed CSP and non-CSP failures stay partial",
			reason:    "loading_failed(csp_blocked):https://cdn.example/p5.min.js; fetch_failed:https://cdn.example/app.js",
			wantState: offlinecache.StatePartial,
		},
		{
			name:      "non-CSP failure is partial",
			reason:    "fetch_failed:https://cdn.example/app.js",
			wantState: offlinecache.StatePartial,
		},
	}

	// Every case above is a finished capture (the capture window
	// already ran to completion; only the resulting artwork differs in
	// how playable it is), so getOfflineCacheStatus must always report
	// percent:100 for all of them — percent must never fall back to 0
	// for StateBrokenOnline, which would read to a mobile client as
	// "still downloading" for an item that is permanently done and
	// will never progress further.
	const wantPercent = 100

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setupService(t, 0, nil)
			defer ts.ctrl.Finish()

			item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
			rec := &offlinecache.ItemRecord{
				ItemID: "item-1", Item: item, Entry: item.Source,
				Coverage: offlinecache.Coverage{Complete: false, Reason: tt.reason},
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
			waitForState(t, ts.service, "item-1", tt.wantState)

			snap, err := ts.service.Status([]string{"item-1"})
			require.NoError(t, err)
			require.Len(t, snap.Items, 1)
			assert.Equal(t, wantPercent, snap.Items[0].Percent)
		})
	}
}

func TestService_DownloadPlaylist_FiltersToSoftwareAndStoresVerbatim(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items": []map[string]interface{}{
			{"id": "item-software", "source": "https://example.com/index.html"},
			{"id": "item-media", "source": "https://example.com/video.mp4"},
		},
	})
	require.NoError(t, err)

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/index.html").Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/video.mp4").Return(offlinecache.ClassMedia, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), gomock.Any(), 5000).DoAndReturn(
		func(_ context.Context, item dp1playlist.PlaylistItem, _ int) (*offlinecache.ItemRecord, error) {
			rec := &offlinecache.ItemRecord{ItemID: item.ID, Item: item, Coverage: offlinecache.Coverage{Complete: true}}
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	queued, total, err := ts.service.DownloadPlaylist(context.Background(), raw, "")
	require.NoError(t, err)
	assert.Equal(t, 1, queued)
	assert.Equal(t, 2, total)

	waitForState(t, ts.service, "item-software", offlinecache.StateReady)

	stored, err := ts.store.LoadPlaylist("playlist-1")
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(stored), "the playlist must be stored byte-for-byte as received, not re-marshaled")
}

// TestService_DownloadPlaylist_IndexesBySourceURLForOfflineDisplayFallback
// is the regression test pinning that a non-empty sourceURL must make the
// downloaded playlist recoverable by that same URL via
// CachedPlaylistForURL, so displayPlaylist by URL can use the downloaded
// cache offline — for commandrouter's offline displayPlaylist fallback
// (see handler.go).
func TestService_DownloadPlaylist_IndexesBySourceURLForOfflineDisplayFallback(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items":     []map[string]interface{}{},
	})
	require.NoError(t, err)
	sourceURL := "https://feed.example.com/playlists/playlist-1"

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	_, _, err = ts.service.DownloadPlaylist(context.Background(), raw, sourceURL)
	require.NoError(t, err)

	cached, err := ts.service.CachedPlaylistForURL(sourceURL)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(cached))
}

// TestService_DownloadPlaylist_EmptySourceURLIsNotIndexed pins the inline
// dp1_call download path (empty sourceURL): it must not become spuriously
// recoverable by any URL, since it was never actually downloaded from one.
func TestService_DownloadPlaylist_EmptySourceURLIsNotIndexed(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items":     []map[string]interface{}{},
	})
	require.NoError(t, err)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	_, _, err = ts.service.DownloadPlaylist(context.Background(), raw, "")
	require.NoError(t, err)

	_, err = ts.service.CachedPlaylistForURL("https://feed.example.com/playlists/playlist-1")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

// TestService_CachedPlaylistForURL_UnknownURLReturnsNotFound covers the
// "never downloaded" case directly (as opposed to the "downloaded, but
// via a different/no URL" cases above).
func TestService_CachedPlaylistForURL_UnknownURLReturnsNotFound(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	_, err := ts.service.CachedPlaylistForURL("https://feed.example.com/never-downloaded")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

func TestService_DownloadPlaylist_InvalidJSON(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	_, _, err := ts.service.DownloadPlaylist(context.Background(), json.RawMessage(`not json`), "")
	assert.Error(t, err)
}

func TestService_DownloadPlaylist_MissingID(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{"dpVersion": "1.0.0", "items": []interface{}{}})
	require.NoError(t, err)

	_, _, err = ts.service.DownloadPlaylist(context.Background(), raw, "")
	assert.Error(t, err)
}

func TestService_ClearItem_RemovesRecordAndBlob(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	res := seedItemWithCapturedAt(t, ts.store, "item-1", "payload", time.Now())

	require.NoError(t, ts.service.ClearItem("item-1"))

	_, err := ts.store.LoadItem("item-1")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
	_, err = ts.store.ReadBlob(res.SHA256)
	assert.ErrorIs(t, err, offlinecache.ErrBlobNotFound, "GC should reclaim the now-orphaned blob")
}

func TestService_ClearItem_MissingReturnsNotFound(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	// Matches docs/controld-inbound-controller-messages.md's documented
	// not_found contract for clearPlaylistItemCache, and ClearPlaylist's
	// existing not-cached behavior below.
	err := ts.service.ClearItem("does-not-exist")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
}

// TestService_ClearItem_WaitsForUnrelatedActiveCaptureBeforeGC is the
// regression test for the captureMu fence: without it, GC() treats any
// blob not yet referenced by a saved item record as an orphan, and
// capturer.Capture writes blobs well before it calls SaveItem, so a clear
// for an unrelated item could delete another item's in-flight capture's
// blobs before that capture ever gets to save its record.
func TestService_ClearItem_WaitsForUnrelatedActiveCaptureBeforeGC(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-a", "payload-a", time.Now())

	itemB := dp1playlist.PlaylistItem{ID: "item-b", Source: "https://example.com/item-b"}
	blobWritten := make(chan struct{})
	proceedToSave := make(chan struct{})

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), itemB.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), itemB, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			// Mirrors capture.go's real shape: the blob lands on disk
			// well before SaveItem persists any record referencing it.
			hash := writeBlobString(t, ts.store, "payload-b")
			close(blobWritten)
			<-proceedToSave
			rec := &offlinecache.ItemRecord{
				ItemID: "item-b", Item: itemB, Entry: itemB.Source,
				Resources: []offlinecache.Resource{{URL: itemB.Source, Status: 200, SHA256: hash, ContentType: "text/html"}},
				Coverage:  offlinecache.Coverage{Complete: true},
			}
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), itemB))

	<-blobWritten // item-b's blob now exists on disk, but its record does not yet.

	clearDone := make(chan error, 1)
	go func() { clearDone <- ts.service.ClearItem("item-a") }()

	select {
	case <-clearDone:
		t.Fatal("ClearItem(\"item-a\") returned before the unrelated in-flight capture of item-b finished; GC was not fenced against it")
	case <-time.After(100 * time.Millisecond):
	}

	close(proceedToSave)
	require.NoError(t, <-clearDone)

	_, err := ts.store.LoadItem("item-a")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound, "item-a should still be cleared once the fence releases")

	rec, err := ts.store.LoadItem("item-b")
	require.NoError(t, err, "item-b's capture must have completed normally")
	_, err = ts.store.ReadBlob(rec.Resources[0].SHA256)
	assert.NoError(t, err, "item-b's blob must not have been GC'd out from under its own in-flight capture")
}

// TestService_ClearItem_RemovesQueuedRecaptureJobBeforeItRuns is the
// regression test for jobQueue.removeItems: clearing an item that has a
// re-download still sitting in the queue (not yet started) must prevent
// that queued job from silently resurrecting the record once it
// eventually runs, or "clear" would not actually stick.
func TestService_ClearItem_RemovesQueuedRecaptureJobBeforeItRuns(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "old payload", time.Now())

	// A separate busyItem occupies the single worker goroutine so
	// item-1's job provably sits in the queue (not yet popped) for the
	// DownloadItem/ClearItem sequence below — DownloadItem now requires
	// Start to have already succeeded (see ErrServiceNotStarted), so
	// this test can no longer enqueue before starting the worker the
	// way it used to.
	busyItem := dp1playlist.PlaylistItem{ID: "busy-item", Source: "https://example.com/busy-item"}
	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	busyStarted := make(chan struct{})
	proceedBusy := make(chan struct{})

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), busyItem.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), busyItem, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			close(busyStarted)
			<-proceedBusy
			return &offlinecache.ItemRecord{ItemID: busyItem.ID, Item: busyItem, Coverage: offlinecache.Coverage{Complete: true}}, nil
		}).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	// Capture is deliberately given no expectation for item-1: if the
	// queued job is not removed, the worker calling it unexpectedly
	// fails the test via gomock.

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	require.NoError(t, ts.service.DownloadItem(context.Background(), busyItem))
	<-busyStarted // worker is now blocked on busyItem; item-1's job below is guaranteed to still be queued, not running

	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	// ClearItem cannot be called synchronously here: it always GCs (see
	// service.gc's doc), and that GC call blocks on captureMu until
	// busyItem's in-flight capture finishes — the same fence
	// TestService_ClearItem_WaitsForUnrelatedActiveCaptureBeforeGC
	// covers directly. Run it in the background instead.
	clearDone := make(chan error, 1)
	go func() { clearDone <- ts.service.ClearItem("item-1") }()

	// ClearItem's queue removal (jobQueue.removeItems) happens before
	// its blocking GC call, and item-1's on-disk delete happens
	// immediately before that removal in the same, unblocked call —
	// so waiting for the delete to land is a safe proxy for "the queued
	// job has already been removed" before releasing busyItem, which
	// is what would let the worker's run loop reach queue.pop() next.
	require.Eventually(t, func() bool {
		_, err := ts.store.LoadItem("item-1")
		return errors.Is(err, offlinecache.ErrItemNotFound)
	}, time.Second, 5*time.Millisecond, "ClearItem must delete item-1's record before this test releases busyItem")

	close(proceedBusy) // let the worker finish busyItem and advance to item-1's (removed) queue slot
	require.NoError(t, <-clearDone)

	require.Never(t, func() bool {
		_, err := ts.store.LoadItem("item-1")
		return err == nil
	}, 200*time.Millisecond, 10*time.Millisecond, "the cleared item's queued recapture must not resurrect its record")
}

// TestService_ClearItem_ActiveCaptureOfSameItemReturnsBusyWithoutDeleting
// is the regression test for ErrItemBusy: ClearItem must not proceed
// unconditionally when itemID's own re-download is actively in flight (as
// opposed to merely queued, which
// TestService_ClearItem_RemovesQueuedRecaptureJobBeforeItRuns already
// covers) — proceeding anyway would report the clear as a success while
// the in-flight capture still saves a fresh record afterward, making the
// item "legitimately reappear" with no signal to the caller. ClearItem
// must instead reject immediately (without touching the store or
// blocking on captureMu/GC).
func TestService_ClearItem_ActiveCaptureOfSameItemReturnsBusyWithoutDeleting(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "old payload", time.Now())

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	captureStarted := make(chan struct{})
	proceed := make(chan struct{})

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			close(captureStarted)
			<-proceed
			rec := &offlinecache.ItemRecord{ItemID: "item-1", Item: item, Coverage: offlinecache.Coverage{Complete: true}}
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	<-captureStarted // item-1's recapture is now active, past the queue.

	err := ts.service.ClearItem("item-1")
	assert.ErrorIs(t, err, offlinecache.ErrItemBusy)

	rec, loadErr := ts.store.LoadItem("item-1")
	require.NoError(t, loadErr, "the rejected clear must leave the old record untouched")
	blob, err := ts.store.ReadBlob(rec.Resources[0].SHA256)
	require.NoError(t, err, "the old blob must survive a rejected clear too")
	assert.Equal(t, "old payload", string(blob))

	close(proceed)
	waitForState(t, ts.service, "item-1", offlinecache.StateReady)
	// waitForState only proves the *disk* record is ready (SaveItem runs
	// inside the mocked Capture above, before process() calls notify());
	// s.state's StateDownloading->StateReady transition happens slightly
	// later, once Capture actually returns to process(). isDownloading
	// reads s.state, so ClearItem can still see busy for a brief instant
	// after the disk write lands — retry until that transition catches
	// up, rather than asserting a fixed one-shot call here.
	require.Eventually(t, func() bool {
		err := ts.service.ClearItem("item-1")
		return err == nil
	}, time.Second, 5*time.Millisecond, "clear must succeed once the capture that made it busy has finished")
}

// TestService_ClearPlaylist_ActiveCaptureOfMemberItemReturnsBusyWithoutPartialClear
// covers the same busy rejection at the playlist level: the whole call
// must fail before deleting anything if any member item is actively
// capturing, rather than clearing the rest of the playlist and leaving
// just the busy item to be resurrected once its capture finishes.
func TestService_ClearPlaylist_ActiveCaptureOfMemberItemReturnsBusyWithoutPartialClear(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "payload-1", time.Now())
	seedItemWithCapturedAt(t, ts.store, "item-2", "payload-2", time.Now())

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0", "id": "playlist-1",
		"items": []map[string]interface{}{{"id": "item-1", "source": "x"}, {"id": "item-2", "source": "y"}},
	})
	require.NoError(t, err)
	require.NoError(t, ts.store.SavePlaylist("playlist-1", raw))

	item2 := dp1playlist.PlaylistItem{ID: "item-2", Source: "y"}
	captureStarted := make(chan struct{})
	proceed := make(chan struct{})
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item2.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item2, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			close(captureStarted)
			<-proceed
			rec := &offlinecache.ItemRecord{ItemID: "item-2", Item: item2, Coverage: offlinecache.Coverage{Complete: true}}
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item2))
	<-captureStarted

	err = ts.service.ClearPlaylist("playlist-1")
	assert.ErrorIs(t, err, offlinecache.ErrItemBusy)

	_, loadErr := ts.store.LoadItem("item-1")
	assert.NoError(t, loadErr, "a rejected playlist clear must not partially delete an unrelated, idle sibling item")
	_, loadErr = ts.store.LoadPlaylist("playlist-1")
	assert.NoError(t, loadErr, "a rejected playlist clear must leave the playlist record itself untouched too")

	close(proceed)
	waitForState(t, ts.service, "item-2", offlinecache.StateReady)
}

func TestService_ClearPlaylist_RemovesPlaylistAndItsItems(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "payload-1", time.Now())
	seedItemWithCapturedAt(t, ts.store, "item-2", "payload-2", time.Now())

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0", "id": "playlist-1",
		"items": []map[string]interface{}{{"id": "item-1", "source": "x"}, {"id": "item-2", "source": "y"}},
	})
	require.NoError(t, err)
	require.NoError(t, ts.store.SavePlaylist("playlist-1", raw))

	require.NoError(t, ts.service.ClearPlaylist("playlist-1"))

	_, err = ts.store.LoadItem("item-1")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
	_, err = ts.store.LoadItem("item-2")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound)
	_, err = ts.store.LoadPlaylist("playlist-1")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

func TestService_ClearPlaylist_NotFound(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	err := ts.service.ClearPlaylist("missing-playlist")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

func TestService_Status_UnknownItemIsNotCached(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	snap, err := ts.service.Status([]string{"missing"})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateNotCached, snap.Items[0].State)
}

func TestService_Status_AggregatesTotalsAndDiskUsage(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "0123456789", time.Now())
	seedItemWithCapturedAt(t, ts.store, "item-2", "abcdefghij", time.Now())

	snap, err := ts.service.Status(nil)
	require.NoError(t, err)
	assert.Len(t, snap.Items, 2)
	assert.Equal(t, 2, snap.Totals.Total)
	assert.Equal(t, 2, snap.Totals.Ready)
	assert.Equal(t, int64(20), snap.DiskUsedBytes)
}

func TestService_Start_RebuildsIndexFromExistingDiskState(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "payload", time.Now())

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	snap, err := ts.service.Status([]string{"item-1"})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateReady, snap.Items[0].State)
}

func TestService_Start_PropagatesListItemIDsError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockStore := mocks.NewMockOfflineCacheStore(ctrl)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockStore.EXPECT().ListItemIDs().Return(nil, assertError("disk error")).Times(1)

	svc := offlinecache.NewService(mockStore, mockClassifier, mockCapturer, wrapper.NewJSON(), 5000, 0, nil, zaptest.NewLogger(t))
	err := svc.Start(context.Background())
	assert.Error(t, err)
}

func TestService_Stop_WithoutStart_Noop(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	assert.NotPanics(t, func() { ts.service.Stop() })
}

// TestService_Stop_ClosesCapturer is the regression test for the
// downloader-shutdown fix: Stop must close the Capturer (which tears
// down Downloader's headless Chromium — see capturer.Close and
// downloader.go's Close doc) so main.go's shutdown sequence actually
// reaches that second Chromium process instead of relying solely on
// systemd's cgroup kill to clean it up.
func TestService_Stop_ClosesCapturer(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).Times(1)

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, wrapper.NewJSON(), 5000, 0, nil, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))
	svc.Stop()
}

func TestService_EnforceDiskLimit_EvictsOldestItemsFirst(t *testing.T) {
	ts := setupService(t, 12, nil)
	defer ts.ctrl.Finish()

	seedItemWithCapturedAt(t, ts.store, "old-1", "0123456789", time.Now().Add(-2*time.Hour))
	seedItemWithCapturedAt(t, ts.store, "old-2", "abcdefghij", time.Now().Add(-1*time.Hour))

	newHash := writeBlobString(t, ts.store, "zzzzzzzzzz")
	newItem := dp1playlist.PlaylistItem{ID: "new-item", Source: "https://example.com/new"}
	newRec := &offlinecache.ItemRecord{
		ItemID: "new-item", Item: newItem, Entry: newItem.Source,
		Resources:  []offlinecache.Resource{{URL: newItem.Source, Status: 200, SHA256: newHash, ContentType: "text/html"}},
		Coverage:   offlinecache.Coverage{Complete: true},
		CapturedAt: time.Now(),
	}

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), newItem.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), newItem, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			require.NoError(t, ts.store.SaveItem(newRec))
			return newRec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	require.NoError(t, ts.service.DownloadItem(context.Background(), newItem))
	waitForState(t, ts.service, "new-item", offlinecache.StateReady)

	require.Eventually(t, func() bool {
		usage, err := ts.store.DiskUsage()
		return err == nil && usage <= 12
	}, 2*time.Second, 10*time.Millisecond, "disk usage should settle back under budget")

	_, err := ts.store.LoadItem("old-1")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound, "oldest item should be evicted first")
	_, err = ts.store.LoadItem("old-2")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound, "second-oldest item should also be evicted to get under budget")
	_, err = ts.store.LoadItem("new-item")
	assert.NoError(t, err, "the item that was just captured must never be evicted by its own capture")
}

func TestService_Notify_ReportsQueuedDownloadingThenReadyInOrder(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{ItemID: "item-1", Item: item, Coverage: offlinecache.Coverage{Complete: true}}

	var seen []offlinecache.ItemState
	done := make(chan struct{})
	mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
		seen = append(seen, status.State)
		if status.State == offlinecache.StateReady {
			close(done)
		}
	}).Times(3)

	mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			require.NoError(t, store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))
	defer svc.Stop()

	require.NoError(t, svc.DownloadItem(context.Background(), item))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the observer to see the terminal ready state")
	}

	assert.Equal(t, []offlinecache.ItemState{
		offlinecache.StateQueued, offlinecache.StateDownloading, offlinecache.StateReady,
	}, seen)
}

// TestService_Notify_FailedRecaptureNotificationDivergesFromStillReadyDiskStatus
// pins the intentional attempt-level-vs-cache-level split: a failed
// *re*-capture of an item that was already cached
// must still notify state:"failed" for that one attempt (so the mobile
// app's in-flight progress UI resolves), while itemStatus's own doc
// ("a record on disk always wins over in-memory state") means
// getOfflineCacheStatus/Status must keep reporting the earlier
// successful capture's ready/partial state, since the failed attempt
// never touched the still-valid old record or blob. Without this test,
// a future edit that made itemStatus prefer in-memory state over disk
// (e.g. to "fix" this exact-looking divergence) would silently make
// Status flicker to failed for an item that is still fully playable
// offline.
func TestService_Notify_FailedRecaptureNotificationDivergesFromStillReadyDiskStatus(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	seedItemWithCapturedAt(t, store, "item-1", "already cached payload", time.Now())

	var seenFailed atomic.Bool
	done := make(chan struct{})
	mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
		if status.State == offlinecache.StateFailed {
			seenFailed.Store(true)
			close(done)
		}
	}).AnyTimes()

	mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).Return(nil, assertError("recapture failed")).Times(1)

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))
	defer svc.Stop()

	require.NoError(t, svc.DownloadItem(context.Background(), item))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the observer to see the failed recapture attempt")
	}
	require.True(t, seenFailed.Load())

	// The notification said "failed", but the disk-backed status must
	// still say "ready": the old record/blob were never touched.
	snap, err := svc.Status([]string{"item-1"})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateReady, snap.Items[0].State,
		"getOfflineCacheStatus must keep reporting the earlier successful capture, not the failed re-download attempt")
	assert.Equal(t, 100, snap.Items[0].Percent)
}
