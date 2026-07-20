package offlinecache_test

import (
	"context"
	"encoding/json"
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
	hash, err := store.WriteBlob([]byte(blobContent))
	require.NoError(t, err)
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

	err := ts.service.DownloadItem(context.Background(), dp1playlist.PlaylistItem{})
	assert.Error(t, err)
}

func TestService_DownloadItem_ClassifyError(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassUnknown, assertError("network down")).Times(1)

	err := ts.service.DownloadItem(context.Background(), item)
	assert.Error(t, err)
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

	queued, total, err := ts.service.DownloadPlaylist(context.Background(), raw)
	require.NoError(t, err)
	assert.Equal(t, 1, queued)
	assert.Equal(t, 2, total)

	waitForState(t, ts.service, "item-software", offlinecache.StateReady)

	stored, err := ts.store.LoadPlaylist("playlist-1")
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(stored), "the playlist must be stored byte-for-byte as received, not re-marshaled")
}

func TestService_DownloadPlaylist_InvalidJSON(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	_, _, err := ts.service.DownloadPlaylist(context.Background(), json.RawMessage(`not json`))
	assert.Error(t, err)
}

func TestService_DownloadPlaylist_MissingID(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{"dpVersion": "1.0.0", "items": []interface{}{}})
	require.NoError(t, err)

	_, _, err = ts.service.DownloadPlaylist(context.Background(), raw)
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
			hash, err := ts.store.WriteBlob([]byte("payload-b"))
			require.NoError(t, err)
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

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	// Capture is deliberately given no expectation: if the queued job is
	// not removed, the worker (started below) calling it unexpectedly
	// fails the test via gomock.

	require.NoError(t, ts.service.DownloadItem(context.Background(), item))
	require.NoError(t, ts.service.ClearItem("item-1"))

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	require.Never(t, func() bool {
		_, err := ts.store.LoadItem("item-1")
		return err == nil
	}, 200*time.Millisecond, 10*time.Millisecond, "the cleared item's queued recapture must not resurrect its record")
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

	newHash, err := ts.store.WriteBlob([]byte("zzzzzzzzzz"))
	require.NoError(t, err)
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

	_, err = ts.store.LoadItem("old-1")
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
