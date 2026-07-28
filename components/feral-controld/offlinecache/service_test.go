package offlinecache_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
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
	ctrl              *gomock.Controller
	store             offlinecache.Store
	mockClassifier    *mocks.MockOfflineCacheClassifier
	mockCapturer      *mocks.MockOfflineCacheCapturer
	mockMediaCapturer *mocks.MockOfflineCacheMediaCapturer
	service           offlinecache.Service
}

// setupService wires a Service against a real fsStore (so item/blob/GC
// round-trips are genuine, matching store_test.go's convention) plus
// mocked Classifier/Capturer/MediaCapturer, since those are the seams
// that would otherwise touch the network or a headless Chromium.
func setupService(t *testing.T, maxDiskBytes int64, observer offlinecache.ProgressObserver) *serviceTestSetup {
	t.Helper()
	ctrl := gomock.NewController(t)
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	// Stop() always closes the capturer (see service.go's Stop doc) —
	// stubbed here rather than per-test since nearly every test defers
	// Stop(); tests asserting the shutdown-close behavior itself set
	// their own tighter expectation instead (see
	// TestService_Stop_ClosesCapturer). MediaCapturer has no Close (no
	// browser process to tear down — see mediacapture.go's doc), so
	// there is nothing to stub for it here.
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, maxDiskBytes, observer, zaptest.NewLogger(t))

	return &serviceTestSetup{
		ctrl: ctrl, store: store, mockClassifier: mockClassifier, mockCapturer: mockCapturer,
		mockMediaCapturer: mockMediaCapturer, service: svc,
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
		snap, err := svc.Status(offlinecache.StatusRequest{ItemIDs: []string{itemID}})
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

func TestService_DownloadItem_RejectsStreaming(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/live.m3u8"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassStreaming, nil).Times(1)
	// No Capturer/MediaCapturer expectation: a rejected item must never
	// reach either capture pipeline.

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	err := ts.service.DownloadItem(context.Background(), item)
	assert.ErrorIs(t, err, offlinecache.ErrUnsupportedMediaClass)
}

// TestService_DownloadItem_QueuesAndCapturesMediaItem is the counterpart
// to TestService_DownloadItem_QueuesAndCapturesSoftwareItem for the new
// direct-download path: a ClassMedia item must route to mediaCapturer,
// never capturer (the headless-Chromium pipeline is neither needed nor
// invoked for a single-file media artwork — see classify.go's
// MediaClass doc).
func TestService_DownloadItem_QueuesAndCapturesMediaItem(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/video.mp4"}
	rec := &offlinecache.ItemRecord{
		ItemID: "item-1", Item: item, Entry: item.Source,
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
	waitForState(t, ts.service, "item-1", offlinecache.StateReady)
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
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockStore.EXPECT().SweepIncompleteBlobs().Return(0, int64(0), nil).Times(1)
	mockStore.EXPECT().ListItemIDs().Return(nil, assertError("permission denied")).Times(1)

	svc := offlinecache.NewService(mockStore, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, nil, zaptest.NewLogger(t))

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
		snap, err := ts.service.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
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

			snap, err := ts.service.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
			require.NoError(t, err)
			require.Len(t, snap.Items, 1)
			assert.Equal(t, wantPercent, snap.Items[0].Percent)
		})
	}
}

// TestService_DownloadPlaylist_QueuesEveryClassButStreamingAndStoresVerbatim
// pins the new mixed-class routing: a software item and a media item in
// the SAME playlist must both be queued (only ClassStreaming is
// excluded — see ErrUnsupportedMediaClass's doc), each routed to its own
// capture pipeline.
func TestService_DownloadPlaylist_QueuesEveryClassButStreamingAndStoresVerbatim(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items": []map[string]interface{}{
			{"id": "item-software", "source": "https://example.com/index.html"},
			{"id": "item-media", "source": "https://example.com/video.mp4"},
			{"id": "item-live", "source": "https://example.com/live.m3u8"},
		},
	})
	require.NoError(t, err)

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/index.html").Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/video.mp4").Return(offlinecache.ClassMedia, nil).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/live.m3u8").Return(offlinecache.ClassStreaming, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), gomock.Any(), 5000).DoAndReturn(
		func(_ context.Context, item dp1playlist.PlaylistItem, _ int) (*offlinecache.ItemRecord, error) {
			rec := &offlinecache.ItemRecord{ItemID: item.ID, Item: item, Coverage: offlinecache.Coverage{Complete: true}}
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)
	ts.mockMediaCapturer.EXPECT().Capture(gomock.Any(), gomock.Any()).DoAndReturn(
		func(_ context.Context, item dp1playlist.PlaylistItem) (*offlinecache.ItemRecord, error) {
			rec := &offlinecache.ItemRecord{ItemID: item.ID, Item: item, Coverage: offlinecache.Coverage{Complete: true}}
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	queued, total, err := ts.service.DownloadPlaylist(context.Background(), raw, "")
	require.NoError(t, err)
	assert.Equal(t, 2, queued, "software and media items must both be queued; only the streaming item is excluded")
	assert.Equal(t, 3, total)

	waitForState(t, ts.service, "item-software", offlinecache.StateReady)
	waitForState(t, ts.service, "item-media", offlinecache.StateReady)

	stored, err := ts.store.LoadPlaylist("playlist-1")
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(stored), "the playlist must be stored byte-for-byte as received, not re-marshaled")
}

// TestService_DownloadPlaylist_DoesNotDoubleCountAlreadyInFlightItems is
// the regression test for the queuedCount-overcount bug: a client that
// retries downloadPlaylist for a playlist whose item is still
// downloading from the FIRST call must see the retry's queuedCount
// reflect that nothing new was scheduled, not double-count the same
// in-flight item. enqueue's idempotent no-op (item already
// StateDownloading) must translate to DownloadPlaylist's aggregate count
// staying at 0 for the second call — see enqueue's "queued bool" doc.
func TestService_DownloadPlaylist_DoesNotDoubleCountAlreadyInFlightItems(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0", "id": "playlist-1", "title": "t",
		"items": []map[string]interface{}{
			{"id": "item-1", "source": "https://example.com/index.html"},
		},
	})
	require.NoError(t, err)

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	gate := make(chan struct{})
	// Classify runs once per DownloadPlaylist call regardless of whether
	// the item ends up newly queued, so both calls hit it.
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(2)
	// Times(1): the retry below must not schedule a second capture for
	// the same in-flight item.
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			<-gate
			rec := &offlinecache.ItemRecord{ItemID: item.ID, Item: item, Coverage: offlinecache.Coverage{Complete: true}}
			require.NoError(t, ts.store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	queued1, total1, err := ts.service.DownloadPlaylist(context.Background(), raw, "")
	require.NoError(t, err)
	assert.Equal(t, 1, queued1, "the first call must report the item as newly queued")
	assert.Equal(t, 1, total1)

	require.Eventually(t, func() bool {
		snap, err := ts.service.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
		return err == nil && len(snap.Items) == 1 && snap.Items[0].State == offlinecache.StateDownloading
	}, 2*time.Second, 10*time.Millisecond, "worker should have dequeued into downloading before the retry lands")

	queued2, total2, err := ts.service.DownloadPlaylist(context.Background(), raw, "")
	require.NoError(t, err)
	assert.Zero(t, queued2, "a retry while the only item is still downloading must report queuedCount 0, not double-count it")
	assert.Equal(t, 1, total2)

	close(gate)
	waitForState(t, ts.service, "item-1", offlinecache.StateReady)
}

// TestService_DownloadPlaylist_AllItemsFailClassificationReturnsError is
// the regression test for the false-success hazard: if the classifier
// itself is broken (e.g. a transient network error) for every eligible
// item, that must be reported as an error, not silently collapse into
// the same ok:true/queuedCount:0 shape a playlist with genuinely no
// software items would produce.
//
// It also pins a second invariant on the SAME failure: the playlist
// body and its sourceURL index must NOT be persisted either.
// loadCachedPlaylistForURL's/CachedPlaylistForURL's offline-fallback
// doc promises a saved playlist record can only exist if
// downloadPlaylist actually succeeded for it — if this method saved it
// anyway despite returning an error here, a later offline
// displayPlaylist-by-URL could be handed a "last known good" playlist
// whose items were never actually queued because classification was
// down, not because the playlist genuinely has none.
func TestService_DownloadPlaylist_AllItemsFailClassificationReturnsError(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items": []map[string]interface{}{
			{"id": "item-1", "source": "https://example.com/a.html"},
			{"id": "item-2", "source": "https://example.com/b.html"},
		},
	})
	require.NoError(t, err)
	sourceURL := "https://feed.example.com/playlists/playlist-1"

	classifyErr := assertError("classifier unreachable")
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/a.html").Return(offlinecache.ClassUnknown, classifyErr).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/b.html").Return(offlinecache.ClassUnknown, classifyErr).Times(1)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	queued, total, err := ts.service.DownloadPlaylist(context.Background(), raw, sourceURL)
	require.Error(t, err, "a classifier outage must not be reported as a successful download of zero software items")
	assert.Zero(t, queued)
	assert.Equal(t, 2, total)

	_, err = ts.service.CachedPlaylistForURL(sourceURL)
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound,
		"a download this method reported as failed must never leave a 'last known good' offline fallback behind")
	_, err = ts.store.LoadPlaylist("playlist-1")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound,
		"the playlist body itself must also not be persisted on a total classification failure")
}

// TestService_DownloadPlaylist_SavePlaylistFailureStartsNoWork is the
// regression test for the review round's "enqueues captures before
// persisting the playlist" finding: if SavePlaylist itself fails (a
// rare but real I/O error path — full disk, permissions), NO item may
// have already been enqueued/started downloading by the time this
// method returns its error. Uses a mocked Store (rather than
// setupService's real fsStore) specifically so SavePlaylist can be
// made to fail; mockCapturer has no Capture expectation at all, so
// gomock's strict controller fails this test outright if the fix
// regresses and DownloadPlaylist enqueues the classified item before
// persisting.
func TestService_DownloadPlaylist_SavePlaylistFailureStartsNoWork(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockStore := mocks.NewMockOfflineCacheStore(ctrl)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockStore.EXPECT().SweepIncompleteBlobs().Return(0, int64(0), nil).Times(1)
	mockStore.EXPECT().ListItemIDs().Return(nil, nil).Times(1)

	svc := offlinecache.NewService(mockStore, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, nil, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))
	defer svc.Stop()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items": []map[string]interface{}{
			{"id": "item-1", "source": "https://example.com/a.html"},
		},
	})
	require.NoError(t, err)

	mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/a.html").Return(offlinecache.ClassSoftware, nil).Times(1)
	mockStore.EXPECT().SavePlaylist("playlist-1", json.RawMessage(raw)).Return(assertError("disk full")).Times(1)
	// Deliberately no SavePlaylistURLIndex or Capture expectation: a
	// SavePlaylist failure must short-circuit before either the URL
	// index write or any item's capture job ever starts.

	queued, total, downloadErr := svc.DownloadPlaylist(context.Background(), raw, "https://feed.example.com/playlists/playlist-1")
	require.Error(t, downloadErr)
	assert.Zero(t, queued, "no item may be reported as queued when the playlist itself failed to persist")
	assert.Equal(t, 1, total)
}

// TestService_DownloadPlaylist_PartialClassificationFailureStillQueuesSuccesses
// pins that a classify failure for SOME items must not discard the items
// that classified successfully: the caller already got real, correctly
// queued work, so this is reported as success (the partial failure was
// already logged server-side — see DownloadPlaylist's doc for why this
// case is treated differently from the all-fail case above).
func TestService_DownloadPlaylist_PartialClassificationFailureStillQueuesSuccesses(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items": []map[string]interface{}{
			{"id": "item-ok", "source": "https://example.com/ok.html"},
			{"id": "item-broken", "source": "https://example.com/broken.html"},
		},
	})
	require.NoError(t, err)

	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/ok.html").Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/broken.html").Return(offlinecache.ClassUnknown, assertError("classifier unreachable")).Times(1)
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

	// Wait for the queued item to actually capture before the deferred
	// Stop() runs. Without this the Capture expectation above was only
	// ever satisfied by the shutdown drain happening to run it, which
	// stopped being true once a drained job began skipping capture
	// entirely (see process's ctx.Err() branch).
	waitForState(t, ts.service, "item-ok", offlinecache.StateReady)
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

// TestService_IndexPlaylistForOfflineDisplay_MakesPlaylistRecoverableByURL
// is the regression test for downloadPlaylistItem's URL-fallback gap:
// this method must save+index playlistRaw exactly like DownloadPlaylist
// does, WITHOUT requiring (or performing) any classification/queuing —
// it never even touches mockClassifier/mockCapturer (setupService's
// mocks would fail this test on any unexpected call to either).
func TestService_IndexPlaylistForOfflineDisplay_MakesPlaylistRecoverableByURL(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0",
		"id":        "playlist-1",
		"title":     "t",
		"items": []map[string]interface{}{
			{"id": "item-1", "source": "https://example.com/a.html"},
		},
	})
	require.NoError(t, err)
	sourceURL := "https://feed.example.com/playlists/playlist-1"

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	// sampledEpoch matches the current (never-cleared) generation, so the
	// clear-race guard passes and the index is written.
	gen := ts.service.CurrentPlaylistClearGeneration("playlist-1")
	require.NoError(t, ts.service.IndexPlaylistForOfflineDisplay(raw, sourceURL, gen))

	cached, err := ts.service.CachedPlaylistForURL(sourceURL)
	require.NoError(t, err)
	assert.JSONEq(t, string(raw), string(cached))

	// Never queued: the ONE item in this playlist must not have been
	// classified or captured just because the playlist was indexed.
	_, loadErr := ts.store.LoadItem("item-1")
	assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound)
}

// TestService_IndexPlaylistForOfflineDisplay_StaleEpochDoesNotResurrect is
// the deterministic regression test for the playlist-record resurrection
// hazard on the single-item download path: IndexPlaylistForOfflineDisplay
// must refuse to (re)write a playlist record when the sampledEpoch it was
// given is older than the current clear-generation — i.e. a ClearPlaylist
// landed since the caller sampled. Here the caller samples gen BEFORE
// anything, the playlist is then cleared (bumping the generation and
// deleting the record), and a later index call carrying the STALE gen must
// leave the record deleted rather than bringing it back as an empty
// offline fallback. See Service.CurrentPlaylistClearGeneration and
// savePlaylistAndURLIndex's docs.
func TestService_IndexPlaylistForOfflineDisplay_StaleEpochDoesNotResurrect(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0", "id": "playlist-1", "title": "t",
		"items": []map[string]interface{}{{"id": "item-1", "source": "https://example.com/a.html"}},
	})
	require.NoError(t, err)
	sourceURL := "https://feed.example.com/playlists/playlist-1"

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	// Sample the generation as a caller would, before its download work.
	staleGen := ts.service.CurrentPlaylistClearGeneration("playlist-1")

	// Establish the record, then clear the playlist: this bumps the
	// clear-generation past staleGen and deletes the record.
	require.NoError(t, ts.service.IndexPlaylistForOfflineDisplay(raw, sourceURL, staleGen))
	require.NoError(t, ts.service.ClearPlaylist("playlist-1"))
	_, err = ts.service.CachedPlaylistForURL(sourceURL)
	require.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound, "sanity: the clear must have removed the record")

	// A late index write still carrying the pre-clear generation must be
	// refused (no error — a clear winning is not a failure), leaving the
	// record deleted.
	require.NoError(t, ts.service.IndexPlaylistForOfflineDisplay(raw, sourceURL, staleGen))
	_, err = ts.service.CachedPlaylistForURL(sourceURL)
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound,
		"an index write carrying a pre-clear generation must not resurrect the cleared playlist record")
}

// TestService_IndexPlaylistForOfflineDisplay_EmptySourceURLIsNotIndexed
// mirrors TestService_DownloadPlaylist_EmptySourceURLIsNotIndexed for
// this method's own sourceURL parameter.
func TestService_IndexPlaylistForOfflineDisplay_EmptySourceURLIsNotIndexed(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0", "id": "playlist-1", "title": "t", "items": []map[string]interface{}{},
	})
	require.NoError(t, err)

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	require.NoError(t, ts.service.IndexPlaylistForOfflineDisplay(raw, "", 0))

	_, err = ts.service.CachedPlaylistForURL("https://feed.example.com/playlists/playlist-1")
	assert.ErrorIs(t, err, offlinecache.ErrPlaylistNotFound)
}

// TestService_IndexPlaylistForOfflineDisplay_BeforeStartReturnsNotStarted
// mirrors DownloadPlaylist/DownloadItem's own started.Load() guard.
func TestService_IndexPlaylistForOfflineDisplay_BeforeStartReturnsNotStarted(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	err := ts.service.IndexPlaylistForOfflineDisplay(json.RawMessage(`{"id":"playlist-1"}`), "https://example.com/p.json", 0)
	assert.ErrorIs(t, err, offlinecache.ErrServiceNotStarted)
}

// TestService_IndexPlaylistForOfflineDisplay_MissingIDErrors mirrors
// DownloadPlaylist's own validation for a playlist with no id.
func TestService_IndexPlaylistForOfflineDisplay_MissingIDErrors(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	err := ts.service.IndexPlaylistForOfflineDisplay(json.RawMessage(`{"title":"no id"}`), "https://example.com/p.json", 0)
	assert.Error(t, err)
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
	// later, once Capture actually returns to process(). reserveForClear
	// reads s.state, so ClearItem can still see busy for a brief instant
	// after the disk write lands — retry until that transition catches
	// up, rather than asserting a fixed one-shot call here.
	require.Eventually(t, func() bool {
		err := ts.service.ClearItem("item-1")
		return err == nil
	}, time.Second, 5*time.Millisecond, "clear must succeed once the capture that made it busy has finished")
}

// TestService_ClearItem_ForcedRaceAgainstWorkerDequeueNeverResurrectsAfterSuccess
// is the black-box counterpart to the whitebox
// TestService_DequeueForProcessingAndReserveForClear_AreMutuallyExclusive:
// rather than calling the private methods directly, it drives the real
// public Service API and deliberately releases the worker and calls
// ClearItem at the same moment (no artificial ordering), relying on
// reserveForClear/dequeueForProcessing's shared lock to arbitrate which
// one actually goes first. Run under -race across many iterations, this
// is the test that would have caught the bug empirically even without
// knowing the exact two-step-vs-one-step internals: whichever side wins
// must be self-consistent — a successful clear must never be followed by
// a resurrected record, and a busy rejection must always be followed by
// the capture actually completing.
func TestService_ClearItem_ForcedRaceAgainstWorkerDequeueNeverResurrectsAfterSuccess(t *testing.T) {
	for i := 0; i < 20; i++ {
		ts := setupService(t, 0, nil)
		seedItemWithCapturedAt(t, ts.store, "item-1", "old payload", time.Now())

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
		// MaxTimes(1), not Times(1): whether the worker ever gets to
		// call this at all depends on which side wins the race below.
		ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).MaxTimes(1).DoAndReturn(
			func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
				rec := &offlinecache.ItemRecord{ItemID: "item-1", Item: item, Coverage: offlinecache.Coverage{Complete: true}}
				require.NoError(t, ts.store.SaveItem(rec))
				return rec, nil
			})

		require.NoError(t, ts.service.Start(context.Background()))
		require.NoError(t, ts.service.DownloadItem(context.Background(), busyItem))
		<-busyStarted
		require.NoError(t, ts.service.DownloadItem(context.Background(), item))

		clearDone := make(chan error, 1)
		go func() { clearDone <- ts.service.ClearItem("item-1") }()
		close(proceedBusy) // frees the worker to race ClearItem for item-1's queued job

		if err := <-clearDone; err == nil {
			require.Never(t, func() bool {
				_, loadErr := ts.store.LoadItem("item-1")
				return loadErr == nil
			}, 150*time.Millisecond, 5*time.Millisecond,
				"iteration %d: a clear that reported success must never be followed by a resurrected record", i)
		} else {
			require.ErrorIs(t, err, offlinecache.ErrItemBusy, "iteration %d: the only way ClearItem may fail here is busy", i)
			waitForState(t, ts.service, "item-1", offlinecache.StateReady)
		}

		ts.service.Stop()
		ts.ctrl.Finish()
	}
}

// TestService_ClearItem_DuringBlockedClassificationDoesNotResurrect is the
// deterministic regression test for the clear-vs-classify race: a download
// classifies its source (network I/O) BEFORE it registers any
// queued/downloading state, so for that whole window the item is untracked
// and a concurrent ClearItem sails through reserveForClear, deletes the
// on-disk record, and reports success — after which the download must NOT
// finish classifying, enqueue, capture, and write the record back. The
// classifier is blocked mid-call so the clear is guaranteed to land inside
// the classify window (no timing chance); the download must then abort at
// enqueue on the epoch mismatch. mockCapturer has NO Capture expectation,
// so gomock's strict controller fails the test if the aborted download
// ever reaches the worker and captures anyway.
func TestService_ClearItem_DuringBlockedClassificationDoesNotResurrect(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	// A record already on disk from a prior download, so the racing clear
	// has something real to delete and report success for.
	seedItemWithCapturedAt(t, ts.store, "item-1", "old payload", time.Now())

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	classifyEntered := make(chan struct{})
	releaseClassify := make(chan struct{})
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).DoAndReturn(
		func(context.Context, string) (offlinecache.MediaClass, error) {
			close(classifyEntered)
			<-releaseClassify
			return offlinecache.ClassSoftware, nil
		}).Times(1)
	// Deliberately no Capture expectation: reaching the worker is the bug.

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	dlDone := make(chan error, 1)
	go func() { dlDone <- ts.service.DownloadItem(context.Background(), item) }()

	<-classifyEntered
	require.NoError(t, ts.service.ClearItem("item-1"),
		"the clear must succeed: during classification item-1 is untracked, so nothing is busy")
	close(releaseClassify)

	// The clear won, so nothing was queued and nothing ever will be. An
	// earlier revision returned nil here and the command handler
	// answered status:"queued" for work no worker would run; reporting
	// the abort is what closes that — see ErrClearedDuringDownload.
	require.ErrorIs(t, <-dlDone, offlinecache.ErrClearedDuringDownload,
		"a download aborted by a winning clear must say so, not report success for work it never queued")

	_, err := ts.store.LoadItem("item-1")
	require.Error(t, err, "the cleared record must stay deleted")
	require.Never(t, func() bool {
		_, loadErr := ts.store.LoadItem("item-1")
		return loadErr == nil
	}, 150*time.Millisecond, 5*time.Millisecond,
		"a clear that reported success must never be followed by a resurrected record")
}

// TestService_ClearPlaylist_DuringBlockedClassificationDoesNotResurrect is
// the playlist-level twin of the clear-vs-classify regression above. It
// pins BOTH halves of the playlist fix: (1) member items must not be
// captured back (per-item epoch, downloadEpoch), and (2) the playlist
// RECORD itself must not be re-saved as an empty offline fallback
// (playlistClearEpoch) — DownloadPlaylist persists that record only after
// its classify loop, so a ClearPlaylist landing mid-classification would
// otherwise see the record deleted and then resurrected. The classifier is
// blocked mid-call so the clear is guaranteed to land in the window;
// mockCapturer has no Capture expectation, so no member item may capture.
func TestService_ClearPlaylist_DuringBlockedClassificationDoesNotResurrect(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	seedItemWithCapturedAt(t, ts.store, "item-1", "old payload", time.Now())
	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0", "id": "playlist-1", "title": "t",
		"items": []map[string]interface{}{
			{"id": "item-1", "source": "https://example.com/item-1"},
		},
	})
	require.NoError(t, err)
	require.NoError(t, ts.store.SavePlaylist("playlist-1", raw))

	classifyEntered := make(chan struct{})
	releaseClassify := make(chan struct{})
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), "https://example.com/item-1").DoAndReturn(
		func(context.Context, string) (offlinecache.MediaClass, error) {
			close(classifyEntered)
			<-releaseClassify
			return offlinecache.ClassSoftware, nil
		}).Times(1)
	// Deliberately no Capture expectation.

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	dlDone := make(chan error, 1)
	go func() {
		queued, _, dlErr := ts.service.DownloadPlaylist(context.Background(), raw, "https://feed.example.com/p1")
		assert.Zero(t, queued, "no item may be queued once the playlist is cleared mid-classification")
		dlDone <- dlErr
	}()

	<-classifyEntered
	require.NoError(t, ts.service.ClearPlaylist("playlist-1"),
		"the clear must succeed: during classification no member item is busy")
	close(releaseClassify)

	require.NoError(t, <-dlDone, "the download must return cleanly, not error")

	_, err = ts.store.LoadItem("item-1")
	require.Error(t, err, "the cleared member item record must stay deleted")
	_, err = ts.store.LoadPlaylist("playlist-1")
	require.Error(t, err, "the cleared playlist record must not be resurrected as an empty offline fallback")
	require.Never(t, func() bool {
		_, itemErr := ts.store.LoadItem("item-1")
		_, plErr := ts.store.LoadPlaylist("playlist-1")
		return itemErr == nil || plErr == nil
	}, 150*time.Millisecond, 5*time.Millisecond,
		"neither the member item nor the playlist record may reappear after a successful clear")
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

// removeFailingOS wraps the real wrapper.OS and fails Remove for any path
// containing failPathSubstr, simulating a genuine deletion failure
// (permissions, I/O) rather than store.DeleteItem's own already-handled
// "does not exist" case — see TestService_ClearPlaylist_ReturnsErrorWhenAnItemDeleteFails.
type removeFailingOS struct {
	wrapper.OS
	failPathSubstr string
}

func (o removeFailingOS) Remove(path string) error {
	if strings.Contains(path, o.failPathSubstr) {
		return errors.New("permission denied (simulated)")
	}
	return o.OS.Remove(path)
}

// TestService_ClearPlaylist_ReturnsErrorWhenAnItemDeleteFails is the
// regression test pinning that ClearPlaylist must not report ok:true
// (nil error) when one of its per-item store.DeleteItem calls genuinely
// fails: store.DeleteItem already treats "does not exist" as success, so
// any error it returns here is a real deletion failure, and silently
// continuing to report success would leave the caller believing the
// whole playlist was cleared while item-2's record (and its blobs) is
// still on disk. The rest of the sweep (item-1, the playlist record, GC)
// must still run — a caller retrying the clear should not have to
// re-clear things that already succeeded — but the failure must surface.
func TestService_ClearPlaylist_ReturnsErrorWhenAnItemDeleteFails(t *testing.T) {
	root := t.TempDir()
	logger := zaptest.NewLogger(t)
	failingOS := removeFailingOS{OS: wrapper.NewOS(), failPathSubstr: "item-2.json"}
	store := offlinecache.NewStore(root, failingOS, wrapper.NewJSON(), logger)

	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	svc := offlinecache.NewService(store, mocks.NewMockOfflineCacheClassifier(ctrl), mockCapturer,
		mocks.NewMockOfflineCacheMediaCapturer(ctrl), wrapper.NewJSON(), 5000, 0, nil, logger)

	seedItemWithCapturedAt(t, store, "item-1", "payload-1", time.Now())
	seedItemWithCapturedAt(t, store, "item-2", "payload-2", time.Now())

	raw, err := json.Marshal(map[string]interface{}{
		"dpVersion": "1.0.0", "id": "playlist-1",
		"items": []map[string]interface{}{{"id": "item-1", "source": "x"}, {"id": "item-2", "source": "y"}},
	})
	require.NoError(t, err)
	require.NoError(t, store.SavePlaylist("playlist-1", raw))

	err = svc.ClearPlaylist("playlist-1")
	assert.Error(t, err, "a genuine per-item delete failure must not be swallowed into a successful clear")

	_, loadErr := store.LoadItem("item-1")
	assert.ErrorIs(t, loadErr, offlinecache.ErrItemNotFound, "item-1's delete succeeded and must still take effect")
	_, loadErr = store.LoadItem("item-2")
	assert.NoError(t, loadErr, "item-2's record must remain since its delete genuinely failed")
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

	snap, err := ts.service.Status(offlinecache.StatusRequest{ItemIDs: []string{"missing"}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateNotCached, snap.Items[0].State)
}

func TestService_Status_AggregatesTotalsAndDiskUsage(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "0123456789", time.Now())
	seedItemWithCapturedAt(t, ts.store, "item-2", "abcdefghij", time.Now())

	snap, err := ts.service.Status(offlinecache.StatusRequest{})
	require.NoError(t, err)
	assert.Len(t, snap.Items, 2)
	require.NotNil(t, snap.Totals)
	assert.Equal(t, 2, snap.Totals.Total)
	assert.Equal(t, 2, snap.Totals.Ready)
	require.NotNil(t, snap.DiskUsedBytes)
	assert.Equal(t, int64(20), *snap.DiskUsedBytes)
	assert.False(t, snap.Truncated)
	assert.Empty(t, snap.NextCursor)
}

// seedItemWithReason writes a record whose capture came back incomplete,
// so Status has a Coverage.Reason to report (and truncate).
func seedItemWithReason(t *testing.T, store offlinecache.Store, itemID, reason string) {
	t.Helper()
	require.NoError(t, store.SaveItem(&offlinecache.ItemRecord{
		ItemID:     itemID,
		Item:       dp1playlist.PlaylistItem{ID: itemID, Source: "https://example.com/" + itemID},
		Entry:      "https://example.com/" + itemID,
		Coverage:   offlinecache.Coverage{Complete: false, Reason: reason},
		CapturedAt: time.Now(),
	}))
}

func TestService_Status_TruncatesLongReason(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	// The shape a capture that ran with no network produces: one entry
	// per resource, each carrying that resource's full URL.
	entries := make([]string, 200)
	for i := range entries {
		entries[i] = fmt.Sprintf("fetch_failed:https://cdn.example.com/assets/very/long/path/chunk-%03d.js", i)
	}
	reason := strings.Join(entries, "; ")
	require.Greater(t, len(reason), 10_000, "fixture should be far over the wire budget")
	seedItemWithReason(t, ts.store, "item-1", reason)

	snap, err := ts.service.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)

	got := snap.Items[0].Reason
	assert.Less(t, len(got), 700, "reason should be bounded, not the full %d-byte list", len(reason))
	assert.Contains(t, got, entries[0], "kept entries must stay whole so clients can still parse the prefix")
	assert.Regexp(t, `…\(\+\d+ more\)$`, got, "a truncated list must say how much it dropped")

	// The record on disk keeps the complete reason for debugging.
	rec, err := ts.store.LoadItem("item-1")
	require.NoError(t, err)
	assert.Equal(t, reason, rec.Coverage.Reason)
}

func TestService_Status_PagesWithCursor(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	for _, id := range []string{"item-1", "item-2", "item-3", "item-4", "item-5"} {
		seedItemWithCapturedAt(t, ts.store, id, id+"-blob", time.Now())
	}

	first, err := ts.service.Status(offlinecache.StatusRequest{Limit: 2})
	require.NoError(t, err)
	require.Len(t, first.Items, 2)
	assert.Equal(t, "item-1", first.Items[0].ItemID)
	assert.Equal(t, "item-2", first.Items[1].ItemID)
	assert.True(t, first.Truncated)
	assert.Equal(t, "item-2", first.NextCursor)
	// Totals cover the whole set, not just this page.
	require.NotNil(t, first.Totals)
	assert.Equal(t, 5, first.Totals.Total)
	assert.Equal(t, 5, first.Totals.Ready)
	require.NotNil(t, first.DiskUsedBytes)

	second, err := ts.service.Status(offlinecache.StatusRequest{Limit: 2, Cursor: first.NextCursor})
	require.NoError(t, err)
	require.Len(t, second.Items, 2)
	assert.Equal(t, "item-3", second.Items[0].ItemID)
	assert.Equal(t, "item-4", second.Items[1].ItemID)
	assert.True(t, second.Truncated)
	// Continuation pages skip the whole-set pass, so they carry no
	// totals — see StatusSnapshot's doc.
	assert.Nil(t, second.Totals)
	assert.Nil(t, second.DiskUsedBytes)

	last, err := ts.service.Status(offlinecache.StatusRequest{Limit: 2, Cursor: second.NextCursor})
	require.NoError(t, err)
	require.Len(t, last.Items, 1)
	assert.Equal(t, "item-5", last.Items[0].ItemID)
	assert.False(t, last.Truncated)
	assert.Empty(t, last.NextCursor)
}

func TestService_Status_CursorSurvivesEvictedItem(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "one", time.Now())
	seedItemWithCapturedAt(t, ts.store, "item-2", "two", time.Now())

	// The cursor names an item that no longer exists (cleared or evicted
	// between pages); paging is by sort order, so the next page still
	// resolves rather than erroring or restarting.
	snap, err := ts.service.Status(offlinecache.StatusRequest{Cursor: "item-1-gone-since"})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, "item-2", snap.Items[0].ItemID)
}

func TestService_Status_TotalsOnlyOmitsItems(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "0123456789", time.Now())
	seedItemWithReason(t, ts.store, "item-2", "csp_blocked")

	snap, err := ts.service.Status(offlinecache.StatusRequest{TotalsOnly: true})
	require.NoError(t, err)
	assert.Empty(t, snap.Items)
	assert.False(t, snap.Truncated)
	require.NotNil(t, snap.Totals)
	assert.Equal(t, 2, snap.Totals.Total)
	assert.Equal(t, 1, snap.Totals.Ready)
	require.NotNil(t, snap.DiskUsedBytes)
	assert.Equal(t, int64(10), *snap.DiskUsedBytes)
}

func TestService_Status_SortsAndDedupesRequestedIDs(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "one", time.Now())
	seedItemWithCapturedAt(t, ts.store, "item-2", "two", time.Now())

	snap, err := ts.service.Status(offlinecache.StatusRequest{
		ItemIDs: []string{"item-2", "item-1", "item-2", ""},
	})
	require.NoError(t, err)
	require.Len(t, snap.Items, 2)
	assert.Equal(t, "item-1", snap.Items[0].ItemID)
	assert.Equal(t, "item-2", snap.Items[1].ItemID)
}

func TestService_Status_InFlightItemsSortWithOnDiskOnes(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	// "item-b" only exists in memory (queued, no record written yet), so
	// it must still land in id order rather than after every on-disk id —
	// paging by "id greater than cursor" would otherwise skip it.
	seedItemWithCapturedAt(t, ts.store, "item-a", "a", time.Now())
	seedItemWithCapturedAt(t, ts.store, "item-c", "c", time.Now())

	item := dp1playlist.PlaylistItem{ID: "item-b", Source: "https://example.com/b"}
	ts.mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	ts.mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(ctx context.Context, _ dp1playlist.PlaylistItem, _ int) (*offlinecache.ItemRecord, error) {
			<-ctx.Done() // hold the capture open so the item stays in flight
			return nil, ctx.Err()
		}).AnyTimes()

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()
	require.NoError(t, ts.service.DownloadItem(context.Background(), item))

	require.Eventually(t, func() bool {
		snap, err := ts.service.Status(offlinecache.StatusRequest{})
		return err == nil && len(snap.Items) == 3
	}, 2*time.Second, 10*time.Millisecond)

	snap, err := ts.service.Status(offlinecache.StatusRequest{})
	require.NoError(t, err)
	require.Len(t, snap.Items, 3)
	assert.Equal(t, []string{"item-a", "item-b", "item-c"},
		[]string{snap.Items[0].ItemID, snap.Items[1].ItemID, snap.Items[2].ItemID})
}

func TestService_Start_RebuildsIndexFromExistingDiskState(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()
	seedItemWithCapturedAt(t, ts.store, "item-1", "payload", time.Now())

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	snap, err := ts.service.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
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
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockStore.EXPECT().SweepIncompleteBlobs().Return(0, int64(0), nil).Times(1)
	mockStore.EXPECT().ListItemIDs().Return(nil, assertError("disk error")).Times(1)

	svc := offlinecache.NewService(mockStore, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, nil, zaptest.NewLogger(t))
	err := svc.Start(context.Background())
	assert.Error(t, err)
}

// TestService_Start_SweepsStaleIncompleteBlobLeftByPreviousCrash is the
// regression test for the disk-accounting leak a killed daemon (SIGKILL,
// power loss) could otherwise cause: WriteBlob's own cleanup defer never
// runs in that case, and neither GC nor DiskUsage ever reclaim or count
// blobs/*.tmp (by design), so without Start sweeping them, a stray temp
// file from a previous run would sit on disk forever.
func TestService_Start_SweepsStaleIncompleteBlobLeftByPreviousCrash(t *testing.T) {
	ts := setupService(t, 0, nil)
	defer ts.ctrl.Finish()

	os := wrapper.NewOS()
	blobsDir := filepath.Join(ts.store.RootDir(), "blobs")
	require.NoError(t, os.MkdirAll(blobsDir, 0o755))
	stalePath := filepath.Join(blobsDir, "incoming-crashed.tmp")
	require.NoError(t, os.WriteFile(stalePath, []byte("half-written"), 0o644))

	require.NoError(t, ts.service.Start(context.Background()))
	defer ts.service.Stop()

	_, statErr := os.Stat(stalePath)
	assert.True(t, os.IsNotExist(statErr), "Start must sweep away a stale incomplete blob left by a previous run")
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
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, nil, zaptest.NewLogger(t))
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
	assert.NoError(t, err, "the item that was just captured must never be evicted by its own capture when evicting OTHER items already brings usage back under budget")
}

// TestService_EnforceDiskLimit_EvictsJustCapturedItemWhenNoOtherVictimRemains
// is the regression test for enforceDiskLimit's fallback: capture only
// caps a single resource's size (maxResourceBytes), never an item's
// TOTAL across all its resources, so a multi-resource artwork can
// exceed the whole cache's maxDiskBytes on its own even with no older
// item left to evict instead. Without the fallback, maxDiskBytes would
// not actually be a hard ceiling in that case — this seeds NO older
// items at all, so the only possible victim is the one that was just
// captured.
func TestService_EnforceDiskLimit_EvictsJustCapturedItemWhenNoOtherVictimRemains(t *testing.T) {
	ts := setupService(t, 5, nil) // budget smaller than the new item's own 10-byte blob, and nothing older exists to evict instead
	defer ts.ctrl.Finish()

	newHash := writeBlobString(t, ts.store, "0123456789")
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
	waitForState(t, ts.service, "new-item", offlinecache.StateNotCached)

	usage, err := ts.store.DiskUsage()
	require.NoError(t, err)
	assert.LessOrEqual(t, usage, int64(5), "the cache must never be left permanently over budget just because the only oversized item was also the most recent capture")

	_, err = ts.store.LoadItem("new-item")
	assert.ErrorIs(t, err, offlinecache.ErrItemNotFound,
		"an item that alone exceeds maxDiskBytes with no older item to evict instead must be rejected, not silently kept over budget forever")
}

func TestService_Notify_ReportsQueuedDownloadingThenReadyInOrder(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
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

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
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

// TestService_Notify_TruncatesReason pins that the notification path
// bounds Coverage.Reason the same way Status does: this ItemStatus goes
// straight out over the relayer and the hub WebSocket, both of which
// bound how long a write may take but not how large it may be.
func TestService_Notify_TruncatesReason(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

	entries := make([]string, 200)
	for i := range entries {
		entries[i] = fmt.Sprintf("unresolved_at_deadline:https://cdn.example.com/assets/chunk-%03d.js", i)
	}
	reason := strings.Join(entries, "; ")

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{
		ItemID: "item-1", Item: item,
		Coverage: offlinecache.Coverage{Complete: false, Reason: reason},
	}

	var terminal offlinecache.ItemStatus
	done := make(chan struct{})
	mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
		if status.State == offlinecache.StatePartial {
			terminal = status
			close(done)
		}
	}).Times(3)

	mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
			require.NoError(t, store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))
	defer svc.Stop()

	require.NoError(t, svc.DownloadItem(context.Background(), item))

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the observer to see the terminal partial state")
	}

	assert.Less(t, len(terminal.Reason), 700, "notification reason should be bounded, not the full %d bytes", len(reason))
	assert.Regexp(t, `…\(\+\d+ more\)$`, terminal.Reason)
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
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
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

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
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
	snap, err := svc.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateReady, snap.Items[0].State,
		"getOfflineCacheStatus must keep reporting the earlier successful capture, not the failed re-download attempt")
	assert.Equal(t, 100, snap.Items[0].Percent)
}

// TestService_Stop_DuringInFlightRecaptureLeavesReadyStatusUntouched is
// the service-level counterpart to capture_wedge_test.go's
// waitForObservationWindow unit tests: it exercises the real
// Service.Stop() -> ctx-cancel -> Capture path end to end (rather than
// a canned mock error standing in for it) to confirm that when
// shutdown races an in-flight *re*-capture of an already-ready item,
// the disk-backed record survives untouched. Capture itself (see
// capture.go's waitForObservationWindow) is what guarantees the
// canceled attempt never reaches SaveItem; this test pins that
// Service.Stop's ctx-cancellation is what actually reaches Capture in
// the first place, and that the observer's attempt-level "failed"
// notification for the aborted attempt does not regress
// getOfflineCacheStatus's disk-backed "ready" answer for the still-
// valid earlier capture.
func TestService_Stop_DuringInFlightRecaptureLeavesReadyStatusUntouched(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).Times(1)
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	seedItemWithCapturedAt(t, store, "item-1", "already cached payload", time.Now())

	captureStarted := make(chan struct{})
	var seenFailed atomic.Bool
	failedSeen := make(chan struct{})
	mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
		if status.State == offlinecache.StateFailed && seenFailed.CompareAndSwap(false, true) {
			close(failedSeen)
		}
	}).AnyTimes()

	mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	// Blocks on the SAME ctx Service.Start(ctx)/run(ctx) thread the
	// worker calls Capture with — this is the real cancellation path
	// (Service.Stop -> s.cancel()), not a stand-in error, so a future
	// edit that broke that plumbing (e.g. Capture called with a
	// context detached from Stop's cancel) would fail this test by
	// timing out rather than silently passing.
	mockCapturer.EXPECT().Capture(gomock.Any(), item, gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ dp1playlist.PlaylistItem, _ int) (*offlinecache.ItemRecord, error) {
			close(captureStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}).Times(1)

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))

	require.NoError(t, svc.DownloadItem(context.Background(), item))

	select {
	case <-captureStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the recapture's Capture call to start")
	}

	// Stop() cancels the worker's ctx then blocks on doneCh, which only
	// closes once the blocked Capture call above actually returns — so
	// by the time Stop() returns here, the canceled recapture attempt
	// has already been through service.process's error path.
	svc.Stop()

	select {
	case <-failedSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the observer to see the canceled recapture attempt")
	}

	snap, err := store.LoadItem("item-1")
	require.NoError(t, err, "the pre-existing ready record must still be on disk, untouched by the canceled recapture")
	assert.Equal(t, "already cached payload", func() string {
		blob, blobErr := store.ReadBlob(snap.Resources[0].SHA256)
		require.NoError(t, blobErr)
		return string(blob)
	}())

	status, err := svc.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
	require.NoError(t, err)
	require.Len(t, status.Items, 1)
	assert.Equal(t, offlinecache.StateReady, status.Items[0].State,
		"a recapture aborted by shutdown must never regress an already-ready item's disk-backed status")
	assert.Equal(t, 100, status.Items[0].Percent)
}

// TestService_DownloadItem_ClearWinningTheRaceNotifiesNothingAndReportsIt
// pins BOTH halves of the false-"queued" fix, at the seam the mobile app
// actually observes:
//
//  1. DownloadItem must report ErrClearedDuringDownload rather than nil.
//     The commandrouter turns a nil here into a flat status:"queued",
//     which claimed scheduled work that no worker would ever run.
//  2. No offline_cache_status state:"queued" may be emitted for that
//     item. An earlier revision notified the observer BEFORE enqueue's
//     authoritative epoch re-check, so a clear landing in that window
//     produced a queued notification for an item that was then never
//     queued — and since nothing further would ever be sent for it, the
//     app's progress UI waited on it indefinitely.
//
// The clear is forced to land inside the classify window (blocking
// classifier, no timing chance), exactly like
// TestService_ClearItem_DuringBlockedClassificationDoesNotResurrect.
func TestService_DownloadItem_ClearWinningTheRaceNotifiesNothingAndReportsIt(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

	seedItemWithCapturedAt(t, store, "item-1", "old payload", time.Now())

	var mu sync.Mutex
	var notified []offlinecache.ItemStatus
	mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
		mu.Lock()
		defer mu.Unlock()
		notified = append(notified, status)
	}).AnyTimes()

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/item-1"}
	classifyEntered := make(chan struct{})
	releaseClassify := make(chan struct{})
	mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).DoAndReturn(
		func(context.Context, string) (offlinecache.MediaClass, error) {
			close(classifyEntered)
			<-releaseClassify
			return offlinecache.ClassSoftware, nil
		}).Times(1)
	// No Capture expectation: reaching the worker at all is the bug.

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))
	defer svc.Stop()

	dlDone := make(chan error, 1)
	go func() { dlDone <- svc.DownloadItem(context.Background(), item) }()

	<-classifyEntered
	require.NoError(t, svc.ClearItem("item-1"))
	close(releaseClassify)

	require.ErrorIs(t, <-dlDone, offlinecache.ErrClearedDuringDownload,
		"the caller must learn nothing was queued; the command layer maps this to a retryable busy, not ok/queued")

	// Give any stray asynchronous notification a chance to land before
	// asserting none did.
	require.Never(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(notified) > 0
	}, 200*time.Millisecond, 10*time.Millisecond,
		"a clear that won the race must produce no state notification at all for that item, least of all state:\"queued\"")

	_, err := store.LoadItem("item-1")
	require.Error(t, err, "the cleared record must stay deleted")
}

// TestService_Enqueue_NotifiesQueuedOnlyAfterTheJobIsCommitted is the
// ordering half of the same fix, on the happy path: the observer must see
// state:"queued" only once the job is genuinely in the queue, so every
// queued notification a client receives corresponds to real scheduled
// work.
func TestService_Enqueue_NotifiesQueuedOnlyAfterTheJobIsCommitted(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

	item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
	rec := &offlinecache.ItemRecord{ItemID: "item-1", Item: item, Coverage: offlinecache.Coverage{Complete: true}}

	queuedSeen := make(chan struct{})
	mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
		if status.State == offlinecache.StateQueued {
			close(queuedSeen)
		}
	}).AnyTimes()

	mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	// captureStarted is what makes this test deterministic: the deferred
	// Stop() must not run until the worker has actually dequeued this
	// job, or the shutdown drain skips the capture entirely (see
	// process's ctx.Err() branch) and the Times(1) expectation below is
	// never satisfied — which is exactly how this test failed on CI,
	// where the worker lost that race.
	//
	// blockCapture then holds the capture open so the item is observably
	// mid-flight when Status is asserted. It must release on ctx
	// cancellation too: Stop() blocks until the worker exits, so a
	// capture that only ever waited on the channel would deadlock the
	// test's own teardown.
	captureStarted := make(chan struct{})
	blockCapture := make(chan struct{})
	mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
		func(ctx context.Context, _ dp1playlist.PlaylistItem, _ int) (*offlinecache.ItemRecord, error) {
			close(captureStarted)
			select {
			case <-blockCapture:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			require.NoError(t, store.SaveItem(rec))
			return rec, nil
		}).Times(1)

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))
	defer svc.Stop()

	require.NoError(t, svc.DownloadItem(context.Background(), item))

	select {
	case <-queuedSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("a genuinely queued item must still produce its queued notification")
	}

	select {
	case <-captureStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the queued item must actually reach the capture worker")
	}

	// And the state the notification claims is real: Status agrees the
	// item is scheduled. Deterministically downloading, since the capture
	// above has provably started and is held open.
	snap, err := svc.Status(offlinecache.StatusRequest{ItemIDs: []string{"item-1"}})
	require.NoError(t, err)
	require.Len(t, snap.Items, 1)
	assert.Equal(t, offlinecache.StateDownloading, snap.Items[0].State)
}

// TestService_Notify_QueuedPrecedesDownloadingEvenWithAnIdleWorker pins
// the ordering barrier (captureJob.queuedNotified). The two notifications
// are produced by different goroutines — queued by the enqueuing caller,
// downloading by the capture worker — and enqueue can only emit its own
// after committing the job, at which point an idle worker is free to pop
// it immediately. Without the barrier a client could see progress run
// backwards (downloading, then queued) for an item progressing normally.
//
// The worker is deliberately idle and the capture instant, so the worker
// reaches its notification as early as it ever can.
func TestService_Notify_QueuedPrecedesDownloadingEvenWithAnIdleWorker(t *testing.T) {
	for i := 0; i < 20; i++ {
		t.Run(fmt.Sprintf("attempt-%d", i), func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()
			store, _ := newTestStore(t)
			mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
			mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
			mockCapturer.EXPECT().Close().Return(nil).AnyTimes()
			mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
			mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

			item := dp1playlist.PlaylistItem{ID: "item-1", Source: "https://example.com/index.html"}
			rec := &offlinecache.ItemRecord{ItemID: "item-1", Item: item, Coverage: offlinecache.Coverage{Complete: true}}

			var mu sync.Mutex
			var seen []offlinecache.ItemState
			done := make(chan struct{})
			mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
				mu.Lock()
				defer mu.Unlock()
				seen = append(seen, status.State)
				if status.State == offlinecache.StateReady {
					close(done)
				}
			}).AnyTimes()

			mockClassifier.EXPECT().Classify(gomock.Any(), item.Source).Return(offlinecache.ClassSoftware, nil).Times(1)
			mockCapturer.EXPECT().Capture(gomock.Any(), item, 5000).DoAndReturn(
				func(context.Context, dp1playlist.PlaylistItem, int) (*offlinecache.ItemRecord, error) {
					require.NoError(t, store.SaveItem(rec))
					return rec, nil
				}).Times(1)

			svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
			require.NoError(t, svc.Start(context.Background()))
			defer svc.Stop()

			require.NoError(t, svc.DownloadItem(context.Background(), item))

			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for the terminal ready notification")
			}

			mu.Lock()
			defer mu.Unlock()
			assert.Equal(t, []offlinecache.ItemState{
				offlinecache.StateQueued, offlinecache.StateDownloading, offlinecache.StateReady,
			}, seen)
		})
	}
}

// TestService_Stop_WithQueuedBacklogDoesNotNotifyPerJob is the shutdown
// regression test: Stop() must not take time proportional to the queue
// depth. run()'s cancellation path drains every remaining job through
// process(), and each drained job used to emit downloading and then
// failed — both synchronous relayer sends with a 5s deadline built from
// context.Background(), so cancellation could not shorten them. A
// backlogged queue therefore pushed shutdown past systemd's
// TimeoutStopSec into a SIGKILL, skipping the capturer.Close() that Stop
// runs after the drain.
//
// The observer here fails the test outright if a drained job notifies:
// the blocking send is the cost being removed, so counting calls would
// pin the symptom rather than the cause.
func TestService_Stop_WithQueuedBacklogDoesNotNotifyPerJob(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	store, _ := newTestStore(t)
	mockClassifier := mocks.NewMockOfflineCacheClassifier(ctrl)
	mockCapturer := mocks.NewMockOfflineCacheCapturer(ctrl)
	mockCapturer.EXPECT().Close().Return(nil).Times(1)
	mockMediaCapturer := mocks.NewMockOfflineCacheMediaCapturer(ctrl)
	mockObserver := mocks.NewMockOfflineCacheProgressObserver(ctrl)

	const backlog = 50

	// The first item's capture blocks until cancellation, holding the
	// single worker so every other item stays queued behind it — which
	// is exactly the backlog state shutdown has to get through.
	blocking := dp1playlist.PlaylistItem{ID: "item-000", Source: "https://example.com/item-000"}
	captureStarted := make(chan struct{})
	mockCapturer.EXPECT().Capture(gomock.Any(), blocking, gomock.Any()).DoAndReturn(
		func(ctx context.Context, _ dp1playlist.PlaylistItem, _ int) (*offlinecache.ItemRecord, error) {
			close(captureStarted)
			<-ctx.Done()
			return nil, ctx.Err()
		}).Times(1)
	// Any OTHER item reaching Capture means the drain ran real work.
	mockCapturer.EXPECT().Capture(gomock.Any(), gomock.Not(gomock.Eq(blocking)), gomock.Any()).Times(0)

	var mu sync.Mutex
	notifiedFor := map[string]int{}
	mockObserver.EXPECT().OnItemStateChanged(gomock.Any()).Do(func(status offlinecache.ItemStatus) {
		mu.Lock()
		defer mu.Unlock()
		notifiedFor[status.ItemID]++
	}).AnyTimes()

	items := make([]dp1playlist.PlaylistItem, backlog)
	for i := range items {
		items[i] = dp1playlist.PlaylistItem{ID: fmt.Sprintf("item-%03d", i), Source: fmt.Sprintf("https://example.com/item-%03d", i)}
		mockClassifier.EXPECT().Classify(gomock.Any(), items[i].Source).Return(offlinecache.ClassSoftware, nil).Times(1)
	}

	svc := offlinecache.NewService(store, mockClassifier, mockCapturer, mockMediaCapturer, wrapper.NewJSON(), 5000, 0, mockObserver, zaptest.NewLogger(t))
	require.NoError(t, svc.Start(context.Background()))

	require.NoError(t, svc.DownloadItem(context.Background(), items[0]))
	select {
	case <-captureStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for the first capture to occupy the worker")
	}
	for _, item := range items[1:] {
		require.NoError(t, svc.DownloadItem(context.Background(), item))
	}

	svc.Stop()

	mu.Lock()
	defer mu.Unlock()
	// Every backlogged item announced itself as queued when it was
	// enqueued; that is the enqueue-side notification and is expected.
	// What must NOT happen is the drain adding downloading/failed on top
	// of it for jobs that never ran.
	for _, item := range items[1:] {
		assert.Equal(t, 1, notifiedFor[item.ID],
			"drained job %s must carry only its original queued notification, not a downloading/failed pair emitted during shutdown", item.ID)
	}

	// The item that genuinely was capturing keeps its full attempt-level
	// story (queued, downloading, failed) — see
	// TestService_Stop_DuringInFlightRecaptureLeavesReadyStatusUntouched.
	assert.Equal(t, 3, notifiedFor[blocking.ID],
		"the in-flight capture is a real attempt and must still report its outcome")
}
