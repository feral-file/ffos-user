package offlinecache

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	dp1playlist "github.com/display-protocol/dp1-go/playlist"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
	"go.uber.org/zap/zaptest/observer"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type playbackClassifier func(context.Context, string) (MediaClass, error)

func (c playbackClassifier) Classify(ctx context.Context, source string) (MediaClass, error) {
	return c(ctx, source)
}

type unusedSoftwareCapturer struct{}

func (unusedSoftwareCapturer) Capture(context.Context, dp1playlist.PlaylistItem, int) (*ItemRecord, error) {
	return nil, errors.New("software must stay live")
}
func (unusedSoftwareCapturer) Close() error { return nil }

func playbackStore(t *testing.T) Store {
	t.Helper()
	return NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zaptest.NewLogger(t))
}

func playbackService(t *testing.T, store Store, classifier Classifier) *service {
	t.Helper()
	media := NewMediaCapturer(wrapper.NewHTTPClient(), store, wrapper.NewJSON(), wrapper.NewClock(), 0, zaptest.NewLogger(t))
	s := NewService(store, classifier, unusedSoftwareCapturer{}, media, wrapper.NewJSON(), 0, 0, nil, AdmissionOptions{RetryInterval: 10 * time.Millisecond}, zaptest.NewLogger(t)).(*service)
	s.automatic = &playbackCache{s: s}
	return s
}

func TestPlaybackCache_WarmsCurrentThenUpcomingAndReusesAfterRestart(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public, max-age=7200")
		_, _ = io.WriteString(w, "image")
	}))
	defer origin.Close()
	store := playbackStore(t)
	classify := playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil })
	items := []dp1playlist.PlaylistItem{{Source: origin.URL + "/a"}, {Source: origin.URL + "/b"}, {Source: origin.URL + "/c"}}
	s := playbackService(t, store, classify)
	require.NoError(t, s.Start(context.Background()))
	s.automatic.observe(items, 1, true)
	// Waits on the store: Status does not report automatic captures (#423).
	require.Eventually(t, func() bool {
		for _, item := range items {
			if _, err := store.LoadItem(SourceKey(item.Source)); err != nil {
				return false
			}
		}
		return true
	}, 3*time.Second, 5*time.Millisecond)
	s.Stop()
	mu.Lock()
	require.Equal(t, []string{"/b", "/c", "/a"}, paths)
	mu.Unlock()
	for _, item := range items {
		rec, err := store.LoadItem(SourceKey(item.Source))
		require.NoError(t, err)
		require.True(t, rec.Automatic)
		require.True(t, rec.freshForPlayback(time.Now()))
	}
	s = playbackService(t, store, classify)
	require.NoError(t, s.Start(context.Background()))
	defer s.Stop()
	s.automatic.observe(items, 0, true)
	// A completed playback touch proves the resumed worker consumed status.
	require.Eventually(t, func() bool { rec, _ := store.LoadItem(SourceKey(items[0].Source)); return !rec.LastPlayedAt.IsZero() }, time.Second, 5*time.Millisecond)
	mu.Lock()
	require.Len(t, paths, 3)
	mu.Unlock()
}

func TestPlaybackCache_ClearSuppressesRepopulation(t *testing.T) {
	store := playbackStore(t)
	s := playbackService(t, store, playbackClassifier(func(context.Context, string) (MediaClass, error) {
		t.Error("cleared source must not be probed")
		return ClassMedia, nil
	}))
	s.started.Store(true)
	item := dp1playlist.PlaylistItem{Source: "https://example.com/art.png"}
	require.NoError(t, store.SaveItem(&ItemRecord{Item: item, Coverage: Coverage{Complete: true}}))
	s.automatic.observe([]dp1playlist.PlaylistItem{item}, 0, true)
	require.NoError(t, s.ClearItem(item.Source))
	s.automatic.observe(nil, 0, false)
	s.automatic.prepare(context.Background())
	s.automatic.observe([]dp1playlist.PlaylistItem{item}, 0, true)
	s.automatic.prepare(context.Background())
	require.Zero(t, s.queue.len())
	require.ErrorIs(t, store.TouchItem(SourceKey(item.Source), time.Now()), ErrItemNotFound)
}

func TestPlaybackCache_IneligibleNeighborDoesNotDisableWarming(t *testing.T) {
	for _, source := range []string{"data:image/png;base64," + strings.Repeat("A", 3000), "https://example.com/" + strings.Repeat("a", 3000)} {
		s := playbackService(t, playbackStore(t), playbackClassifier(func(_ context.Context, source string) (MediaClass, error) {
			require.Equal(t, "https://example.com/art.png", source)
			return ClassMedia, nil
		}))
		s.started.Store(true)
		items := []dp1playlist.PlaylistItem{{Source: source}, {Source: "https://example.com/art.png"}}
		s.automatic.observe(items, 0, true)
		s.automatic.prepare(context.Background())
		job, ok := s.dequeueAdmitted()
		require.True(t, ok)
		require.Equal(t, items[1].Source, job.item.Source)
		require.Empty(t, s.automatic.currentKey())
	}
}

func TestPlaybackCache_SwitchCancelsTransferAndReleasesManualQueue(t *testing.T) {
	entered, canceled := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=7200")
		if r.URL.Path == "/old" {
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			close(entered)
			<-r.Context().Done()
			close(canceled)
			return
		}
		_, _ = io.WriteString(w, "requested image")
	}))
	defer origin.Close()
	s := playbackService(t, playbackStore(t), playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	obs := &recordingProgressObserver{}
	s.observer = obs
	require.NoError(t, s.Start(context.Background()))
	defer s.Stop()
	old := dp1playlist.PlaylistItem{Source: origin.URL + "/old"}
	s.automatic.observe([]dp1playlist.PlaylistItem{old}, 0, true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("automatic transfer did not start")
	}
	manual := dp1playlist.PlaylistItem{Source: origin.URL + "/manual"}
	require.NoError(t, s.DownloadItem(context.Background(), manual))
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: origin.URL + "/new"}}, 0, false)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("playlist switch did not abort the old transfer")
	}
	require.Eventually(t, func() bool { _, err := s.store.LoadItem(SourceKey(manual.Source)); return err == nil }, time.Second, time.Millisecond)
	st, err := s.Status(StatusRequest{Sources: []string{old.Source}})
	require.NoError(t, err)
	require.Equal(t, StateNotCached, st.Items[0].State)
	require.Eventually(t, func() bool { return len(obs.states()) == 3 }, time.Second, time.Millisecond)
	require.Equal(t, []ItemState{StateQueued, StateDownloading, StateReady}, obs.states(), "only the manual job emits progress")
}

func TestPlaybackCache_SwitchBeforeCaptureDropsQueuedSpeculation(t *testing.T) {
	s := playbackService(t, playbackStore(t), playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	s.started.Store(true)
	s.mediaCapturer = playbackMediaCapturer(func(context.Context, dp1playlist.PlaylistItem) (*ItemRecord, error) {
		t.Fatal("obsolete queued artwork must not be fetched")
		return nil, nil
	})
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/old.png"}}, 0, true)
	s.automatic.prepare(context.Background())
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/new.png"}}, 0, false)
	j, ok := s.dequeueAdmitted()
	require.True(t, ok)
	s.process(context.Background(), j)
	require.Empty(t, s.state)
}

func TestPlaybackCache_ManualJoinAfterSwitchUsesLiveWorkerContext(t *testing.T) {
	entered, canceled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	releaseCapture := sync.OnceFunc(func() { close(release) })
	s := playbackService(t, playbackStore(t), playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	var calls atomic.Int32
	s.mediaCapturer = playbackMediaCapturer(func(ctx context.Context, item dp1playlist.PlaylistItem) (*ItemRecord, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			close(canceled)
			<-release
			return nil, ctx.Err()
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		rec := &ItemRecord{Item: item, Coverage: Coverage{Complete: true}}
		return rec, s.store.SaveItem(rec)
	})
	require.NoError(t, s.Start(context.Background()))
	defer s.Stop()
	defer releaseCapture()
	item := dp1playlist.PlaylistItem{Source: "https://example.com/old.png"}
	s.automatic.observe([]dp1playlist.PlaylistItem{item}, 0, true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("automatic capture did not start")
	}
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/new.png"}}, 0, false)
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("old capture was not canceled")
	}
	require.NoError(t, s.DownloadItem(context.Background(), item))
	releaseCapture()
	require.Eventually(t, func() bool { rec, err := s.store.LoadItem(SourceKey(item.Source)); return err == nil && !rec.Automatic }, time.Second, time.Millisecond)
	require.Equal(t, int32(2), calls.Load())
}

func TestPlaybackCache_StatusHidesSpeculationUntilManualJoin(t *testing.T) {
	for _, prior := range []string{"missing", "expired", "ready"} {
		t.Run(prior, func(t *testing.T) {
			s := playbackService(t, playbackStore(t), nil)
			s.started.Store(true)
			item := dp1playlist.PlaylistItem{Source: "https://example.com/art.png"}
			if prior != "missing" {
				freshUntil := time.Now().Add(time.Hour)
				if prior == "expired" {
					freshUntil = time.Now().Add(-time.Hour)
				}
				require.NoError(t, s.store.SaveItem(&ItemRecord{Item: item, Automatic: true, Coverage: Coverage{Complete: true}, Resources: []Resource{{URL: item.Source, FreshUntil: freshUntil}}}))
			}
			_, err := s.enqueueCapture(item, 0, ClassMedia, true)
			require.NoError(t, err)
			for _, downloading := range []bool{false, true} {
				if downloading {
					_, ok := s.dequeueAdmitted()
					require.True(t, ok)
				}
				// Even a fresh automatic capture is the FF1's own, so it
				// reads as not_cached and stays out of the listing (#423).
				st, err := s.Status(StatusRequest{Sources: []string{item.Source}})
				require.NoError(t, err)
				require.Equal(t, []ItemStatus{{Source: item.Source, State: StateNotCached}}, st.Items)
				st, err = s.Status(StatusRequest{})
				require.NoError(t, err)
				require.Empty(t, st.Items)
				require.Equal(t, StatusTotals{}, *st.Totals)
			}
			_, err = s.enqueue(item, 0, ClassMedia)
			require.NoError(t, err)
			// Someone has now asked: the explicit job shows over any record.
			st, err := s.Status(StatusRequest{Sources: []string{item.Source}})
			require.NoError(t, err)
			require.Equal(t, StateDownloading, st.Items[0].State)
			st, err = s.Status(StatusRequest{})
			require.NoError(t, err)
			require.Equal(t, []ItemStatus{{Source: item.Source, State: StateDownloading}}, st.Items)
			require.Equal(t, StatusTotals{Total: 1, Downloading: 1}, *st.Totals)
		})
	}
}

// Downloads lists only the works someone asked the FF1 to keep (#423). A
// finished automatic capture is absent from every report while the FF1
// still uses it, and an explicit Download of it then reports like any
// download.
func TestPlaybackCache_AutomaticCapturesStayOutOfReports(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=7200")
		_, _ = io.WriteString(w, "image")
	}))
	defer origin.Close()
	store := playbackStore(t)
	kept := ItemStatus{Source: "https://example.com/kept.png", State: StateReady, Percent: 100, CoverageComplete: true}
	require.NoError(t, store.SaveItem(&ItemRecord{Item: dp1playlist.PlaylistItem{Source: kept.Source}, Coverage: Coverage{Complete: true}}))
	s := playbackService(t, store, playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	s.started.Store(true)
	obs := &recordingProgressObserver{}
	s.observer = obs
	played := dp1playlist.PlaylistItem{Source: origin.URL + "/played.png"}
	s.automatic.observe([]dp1playlist.PlaylistItem{played}, 0, true)
	playbackPass(s)
	rec, err := store.LoadItem(SourceKey(played.Source))
	require.NoError(t, err)
	require.True(t, rec.Automatic)
	require.True(t, rec.freshForPlayback(time.Now()))

	require.Empty(t, obs.states(), "an automatic capture is never announced")
	st, err := s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Equal(t, []ItemStatus{kept}, st.Items)
	require.Equal(t, StatusTotals{Total: 1, Ready: 1}, *st.Totals)
	st, err = s.Status(StatusRequest{TotalsOnly: true})
	require.NoError(t, err)
	require.Equal(t, StatusTotals{Total: 1, Ready: 1}, *st.Totals)
	st, err = s.Status(StatusRequest{Sources: []string{played.Source, kept.Source}})
	require.NoError(t, err)
	require.ElementsMatch(t, []ItemStatus{kept, {Source: played.Source, State: StateNotCached}}, st.Items)
	// The FF1's own use is unchanged: the offline rescue still replays it.
	require.True(t, s.HasReplayableItem(played.Source))

	// A Download makes it a work someone asked the FF1 to keep.
	require.NoError(t, s.DownloadItem(context.Background(), played))
	st, err = s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Equal(t, StatusTotals{Total: 2, Ready: 1, Downloading: 1}, *st.Totals)
	job, ok := s.dequeueAdmitted()
	require.True(t, ok)
	s.process(context.Background(), job)
	rec, err = store.LoadItem(SourceKey(played.Source))
	require.NoError(t, err)
	require.False(t, rec.Automatic)
	require.Equal(t, []ItemState{StateQueued, StateDownloading, StateReady}, obs.states())
	st, err = s.Status(StatusRequest{Sources: []string{played.Source}})
	require.NoError(t, err)
	require.Equal(t, StateReady, st.Items[0].State)
	st, err = s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Len(t, st.Items, 2)
	require.Equal(t, StatusTotals{Total: 2, Ready: 2}, *st.Totals)
}

// Expire retry backoff without sleeping five minutes. These tests drive the
// worker synchronously, so no background goroutine owns the attempt map.
func expirePlaybackAttempts(s *service) {
	for key, attempt := range s.automatic.attempts {
		attempt.at = time.Now().Add(-2 * automaticRetryInterval)
		s.automatic.attempts[key] = attempt
	}
}

func playbackPass(s *service) {
	s.automatic.prepare(context.Background())
	if job, ok := s.dequeueAdmitted(); ok {
		s.process(context.Background(), job)
	}
}

func TestPlaybackCache_OversizedPlaylistPreservesUpcomingAndBoundsDownloads(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=7200")
		w.Header().Set("Content-Length", "100000")
		// Distinct blobs exercise actual storage pressure rather than dedup.
		_, _ = io.WriteString(w, strings.Repeat(string(r.URL.Path[len(r.URL.Path)-1]), 100000))
	}))
	defer origin.Close()
	policy := &storagePolicy{percent: 50, maxBytes: 400000, space: func(string) (int64, int64, error) { return 1 << 30, 1 << 30, nil }}
	store := &budgetStore{Store: playbackStore(t), policy: policy}
	s := playbackService(t, store, playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	s.storage = policy
	s.started.Store(true)
	items := []dp1playlist.PlaylistItem{{Source: origin.URL + "/a"}, {Source: origin.URL + "/b"}, {Source: origin.URL + "/c"}, {Source: origin.URL + "/d"}, {Source: origin.URL + "/e"}}
	s.automatic.observe(items, 0, true)
	for i := range 30 {
		s.automatic.observe(items, i%len(items), true)
		expirePlaybackAttempts(s)
		playbackPass(s)
	}
	require.LessOrEqual(t, int(requests.Load()), len(items))
	for _, item := range items[:3] {
		_, err := store.LoadItem(SourceKey(item.Source))
		require.NoError(t, err, "warming must not evict the upcoming working set")
	}
	before := requests.Load()
	// An external writer can force hard-budget eviction; even then a loop
	// must not immediately download the evicted bytes again.
	policy.maxBytes = 1
	s.enforceDiskLimit("", "")
	policy.maxBytes = 400000
	for range 10 {
		expirePlaybackAttempts(s)
		playbackPass(s)
	}
	require.Equal(t, before, requests.Load())
}

func TestPlaybackCache_RefusalsStayQuietAcrossPollGaps(t *testing.T) {
	for _, mode := range []string{"no-store", "transient-failure"} {
		t.Run(mode, func(t *testing.T) {
			var requests atomic.Int32
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				requests.Add(1)
				w.Header().Set("Content-Type", "image/png")
				w.Header().Set("Cache-Control", "no-store")
				if mode == "transient-failure" {
					w.WriteHeader(503)
				}
			}))
			defer origin.Close()
			s := playbackService(t, playbackStore(t), playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
			s.started.Store(true)
			observer := &recordingProgressObserver{}
			s.observer = observer
			items := []dp1playlist.PlaylistItem{{Source: origin.URL}}
			s.automatic.observe(items, 0, true)
			playbackPass(s)
			for range 3 {
				s.automatic.observe(nil, 0, false)
				s.automatic.observe(items, 0, true)
				expirePlaybackAttempts(s)
				playbackPass(s)
			}
			if mode == "no-store" {
				require.Equal(t, int32(1), requests.Load())
			} else {
				require.Equal(t, int32(4), requests.Load(), "transient errors retain bounded retry")
			}
			require.Empty(t, observer.states())
			require.Empty(t, s.state)
			require.Empty(t, s.sourceByKey)
			st, err := s.Status(StatusRequest{})
			require.NoError(t, err)
			require.Empty(t, st.Items)
			// Existing manual failures must survive an automatic attempt.
			key := SourceKey(items[0].Source)
			s.state[key], s.sourceByKey[key] = StateFailed, items[0].Source
			_, err = s.enqueueCapture(items[0], s.currentEpoch(key), ClassMedia, true)
			require.NoError(t, err)
			job, ok := s.dequeueAdmitted()
			require.True(t, ok)
			s.process(context.Background(), job)
			require.Equal(t, StateFailed, s.state[key])
		})
	}
}

type deferSoftwareAdmission struct{}

func TestPlaybackCache_ExpiredResidentEntryCanRefresh(t *testing.T) {
	var requests atomic.Int32
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=7200")
		_, _ = io.WriteString(w, "image")
	}))
	defer origin.Close()
	s := playbackService(t, playbackStore(t), playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	s.started.Store(true)
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: origin.URL}}, 0, true)
	playbackPass(s)
	rec, err := s.store.LoadItem(SourceKey(origin.URL))
	require.NoError(t, err)
	rec.Resources[0].FreshUntil = time.Now().Add(-time.Minute)
	require.NoError(t, s.store.SaveItem(rec))
	expirePlaybackAttempts(s)
	playbackPass(s)
	require.Equal(t, int32(2), requests.Load())
	rec, err = s.store.LoadItem(SourceKey(origin.URL))
	require.NoError(t, err)
	require.True(t, rec.freshForPlayback(time.Now()))
	rec.Resources[0].FreshUntil = time.Now().Add(-time.Minute)
	require.NoError(t, s.store.SaveItem(rec))
	s.evictDownTo(0, "", "", false, nil)
	expirePlaybackAttempts(s)
	playbackPass(s)
	require.Equal(t, int32(3), requests.Load(), "expiry reclamation must permit a later fresh capture")
}

func TestEviction_ExpiredAutomaticFirstAndNoGhostStatus(t *testing.T) {
	store := playbackStore(t)
	manual := "https://example.com/offline.png"
	stale := "https://example.com/expired.png"
	old := time.Now().Add(-time.Hour)
	for i, source := range []string{manual, stale} {
		hash, err := store.WriteBlob(strings.NewReader(source), 0)
		require.NoError(t, err)
		rec := &ItemRecord{Item: dp1playlist.PlaylistItem{Source: source}, CapturedAt: old.Add(time.Duration(i) * time.Minute), LastPlayedAt: old, Coverage: Coverage{Complete: true}, Resources: []Resource{{URL: source, SHA256: hash}}}
		if source == stale {
			rec.Automatic = true
			rec.Resources[0].FreshUntil = old
		}
		require.NoError(t, store.SaveItem(rec))
	}
	s := playbackService(t, store, playbackClassifier(func(context.Context, string) (MediaClass, error) {
		t.Fatal("refused expired item must remain live")
		return ClassMedia, nil
	}))
	s.started.Store(true)
	key := SourceKey(stale)
	s.state[key], s.sourceByKey[key] = StateReady, stale
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: stale}}, 0, true)
	s.automatic.attempts[key] = automaticAttempt{cached: true, blocked: true}
	s.automatic.prepare(context.Background())
	rec, err := store.LoadItem(key)
	require.NoError(t, err)
	require.True(t, rec.LastPlayedAt.Equal(old), "network playback must not freshen an unusable cache record's recency")
	_, source, expired, ok := s.oldestEvictableItem("", nil)
	require.True(t, ok)
	require.True(t, expired)
	require.Equal(t, stale, source)
	used, err := store.DiskUsage()
	require.NoError(t, err)
	// Even an active expired source is reclaimable; the explicit snapshot is
	// protected from speculative reclaim regardless of its older capture date.
	s.evictDownTo(used-1, key, stale, false, s.automatic.protectedKeys())
	require.NotContains(t, s.state, key)
	require.NotContains(t, s.sourceByKey, key)
	st, err := s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Len(t, st.Items, 1)
	require.Equal(t, manual, st.Items[0].Source)
	st, err = s.Status(StatusRequest{Sources: []string{stale}})
	require.NoError(t, err)
	require.Equal(t, StateNotCached, st.Items[0].State)
	_, _, _, ok = s.oldestEvictableItem("", map[string]bool{})
	require.False(t, ok, "speculative warming cannot reclaim an explicit offline snapshot")
	_, _, _, ok = s.oldestEvictableItem("", nil)
	require.True(t, ok, "hard-ceiling enforcement can still reclaim space")
	// An automatic failure after reclaim cannot resurrect a prior ready state.
	s.restoreAutomaticTracking(captureJob{sourceKey: key, previouslyTracked: true, previousState: StateReady, previousSource: stale})
	require.NotContains(t, s.state, key)
}

func TestPlaybackCache_ExpiredRecordsPrunedWithoutDiskPressure(t *testing.T) {
	store := playbackStore(t)
	source := "https://example.com/expired.png"
	hash, err := store.WriteBlob(strings.NewReader("expired image"), 0)
	require.NoError(t, err)
	require.NoError(t, store.SaveItem(&ItemRecord{Item: dp1playlist.PlaylistItem{Source: source}, Automatic: true, Coverage: Coverage{Complete: true}, Resources: []Resource{{URL: source, SHA256: hash, FreshUntil: time.Now().Add(-time.Hour)}}}))
	s := playbackService(t, store, nil)
	s.storage = &storagePolicy{percent: 50, minFree: 5 << 30, space: func(string) (int64, int64, error) { return 128 << 30, 100 << 30, nil }}
	// Unlisted before any sweep runs: an automatic capture is never
	// reported (#423), expired or not.
	st, err := s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Empty(t, st.Items)
	require.NoError(t, s.Start(context.Background()))
	defer s.Stop()
	// Usage reaches 0 only at the sweep's closing GC, after the eviction
	// has dropped the tracked state Start seeded from the record.
	require.Eventually(t, func() bool { used, _ := store.DiskUsage(); return used == 0 }, time.Second, time.Millisecond)
	st, err = s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Empty(t, st.Items)
	st, err = s.Status(StatusRequest{Sources: []string{source}})
	require.NoError(t, err)
	require.Equal(t, StateNotCached, st.Items[0].State)
}

func TestStorageBudget_WarnsOncePerPressureEpisodeWithEffectiveLimit(t *testing.T) {
	store := playbackStore(t)
	// Unreferenced bytes stand in for metadata that item eviction cannot remove.
	_, err := store.WriteBlob(strings.NewReader("unreclaimable by item eviction"), 0)
	require.NoError(t, err)
	s := playbackService(t, store, nil)
	s.maxDiskBytes = 80 << 30
	available := int64(1 << 30)
	s.storage = &storagePolicy{percent: 50, minFree: 5 << 30, space: func(string) (int64, int64, error) { return 128 << 30, available, nil }}
	core, logs := observer.New(zap.WarnLevel)
	s.logger = zap.New(core)
	for range 5 {
		s.enforceDiskLimit("", "")
	}
	require.Equal(t, 1, logs.Len())
	require.Equal(t, int64(0), logs.All()[0].ContextMap()["max_bytes"])
	available = 100 << 30
	s.enforceDiskLimit("", "")
	available = 1 << 30
	s.enforceDiskLimit("", "")
	require.Equal(t, 2, logs.Len(), "a new pressure episode must be observable")
}

func TestAutomaticCapture_DeclinesShortFreshness(t *testing.T) {
	for _, policy := range []string{"max-age=60", "max-age=1800"} {
		t.Run(policy, func(t *testing.T) {
			origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "video/mp4")
				w.Header().Set("Cache-Control", policy)
				_, _ = io.WriteString(w, "video")
			}))
			defer origin.Close()
			store := playbackStore(t)
			media := NewMediaCapturer(wrapper.NewHTTPClient(), store, wrapper.NewJSON(), wrapper.NewClock(), 0, zaptest.NewLogger(t))
			intent := &atomic.Bool{}
			intent.Store(true)
			_, err := media.Capture(context.WithValue(context.Background(), automaticCaptureKey{}, intent), dp1playlist.PlaylistItem{Source: origin.URL})
			require.ErrorIs(t, err, errAutomaticUncacheable)
			used, err := store.DiskUsage()
			require.NoError(t, err)
			require.Zero(t, used)
		})
	}
}

type playbackMediaCapturer func(context.Context, dp1playlist.PlaylistItem) (*ItemRecord, error)

func (f playbackMediaCapturer) Capture(ctx context.Context, item dp1playlist.PlaylistItem) (*ItemRecord, error) {
	return f(ctx, item)
}

func TestPlaybackCache_TransferOutlivingTTLDoesNotRepeat(t *testing.T) {
	store := playbackStore(t)
	s := playbackService(t, store, playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	s.started.Store(true)
	captures := 0
	s.mediaCapturer = playbackMediaCapturer(func(_ context.Context, item dp1playlist.PlaylistItem) (*ItemRecord, error) {
		captures++
		hash, err := store.WriteBlob(strings.NewReader("slowly transferred image"), 0)
		if err != nil {
			return nil, err
		}
		rec := &ItemRecord{Item: item, Automatic: true, Coverage: Coverage{Complete: true}, Resources: []Resource{{URL: item.Source, SHA256: hash, FreshUntil: time.Now().Add(-time.Second)}}}
		return rec, store.SaveItem(rec)
	})
	obs := &recordingProgressObserver{}
	s.observer = obs
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/slow.png"}}, 0, true)
	playbackPass(s)
	s.pruneExpiredAutomatic()
	for range 5 {
		expirePlaybackAttempts(s)
		playbackPass(s)
	}
	require.Equal(t, 1, captures)
	require.NotContains(t, obs.states(), StateReady)
	used, err := store.DiskUsage()
	require.NoError(t, err)
	require.Zero(t, used)
}

func (deferSoftwareAdmission) Admit(class MediaClass) AdmissionDecision {
	return AdmissionDecision{Allowed: class == ClassMedia}
}

func TestPlaybackCache_DeferredSoftwareDoesNotStarveMedia(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=7200")
		_, _ = io.WriteString(w, "image")
	}))
	defer origin.Close()
	s := playbackService(t, playbackStore(t), playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	s.admission = deferSoftwareAdmission{}
	require.NoError(t, s.Start(context.Background()))
	defer s.Stop()
	software := dp1playlist.PlaylistItem{Source: "https://example.com/software.html"}
	_, err := s.enqueueCapture(software, 0, ClassSoftware, false)
	require.NoError(t, err)
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: origin.URL}}, 0, true)
	require.Eventually(t, func() bool { _, err := s.store.LoadItem(SourceKey(origin.URL)); return err == nil }, time.Second, time.Millisecond)
	s.mu.RLock()
	defer s.mu.RUnlock()
	require.Equal(t, StateQueued, s.state[SourceKey(software.Source)])
}

func TestPlaybackCache_SwitchDuringClassificationDoesNotQueueOldPlaylist(t *testing.T) {
	store := playbackStore(t)
	var s *service
	s = playbackService(t, store, playbackClassifier(func(context.Context, string) (MediaClass, error) {
		s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/new.png"}}, 0, true)
		return ClassMedia, nil
	}))
	s.started.Store(true)
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/old.png"}}, 0, true)
	s.automatic.prepare(context.Background())
	require.Zero(t, s.queue.len())
}

func TestPlaybackCache_ProbeBackoffAndSoftwareRemainLive(t *testing.T) {
	var probes int
	s := playbackService(t, playbackStore(t), playbackClassifier(func(context.Context, string) (MediaClass, error) { probes++; return ClassSoftware, nil }))
	s.started.Store(true)
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/live.html"}}, 0, true)
	for range 20 {
		s.automatic.prepare(context.Background())
	}
	require.Equal(t, 1, probes)
	require.Zero(t, s.queue.len())
}

func TestAutomaticCapture_RejectsNoStoreButManualDownloadStillWorks(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = io.WriteString(w, "private image")
	}))
	defer origin.Close()
	store := playbackStore(t)
	media := NewMediaCapturer(wrapper.NewHTTPClient(), store, wrapper.NewJSON(), wrapper.NewClock(), 0, zaptest.NewLogger(t))
	intent := &atomic.Bool{}
	intent.Store(true)
	item := dp1playlist.PlaylistItem{Source: origin.URL}
	_, err := media.Capture(context.WithValue(context.Background(), automaticCaptureKey{}, intent), item)
	require.ErrorIs(t, err, errAutomaticUncacheable)
	_, err = store.LoadItem(SourceKey(item.Source))
	require.ErrorIs(t, err, ErrItemNotFound)
	rec, err := media.Capture(context.Background(), item)
	require.NoError(t, err)
	require.False(t, rec.Automatic)
	require.True(t, rec.Resources[0].FreshUntil.IsZero())
}

func TestEviction_UsesPersistedLastPlayed(t *testing.T) {
	store := playbackStore(t)
	old := time.Now().Add(-time.Hour)
	for i, source := range []string{"https://example.com/a", "https://example.com/b"} {
		require.NoError(t, store.SaveItem(&ItemRecord{Item: dp1playlist.PlaylistItem{Source: source}, CapturedAt: old.Add(time.Duration(i) * time.Minute)}))
	}
	require.NoError(t, store.TouchItem(SourceKey("https://example.com/a"), time.Now()))
	s := playbackService(t, store, nil)
	_, victim, _, ok := s.oldestEvictableItem("", nil)
	require.True(t, ok)
	require.Equal(t, "https://example.com/b", victim)
}

func TestStorageGuard_AbortsStreamingAndRemovesPartialBlob(t *testing.T) {
	store := playbackStore(t)
	calls := 0
	p := &storagePolicy{percent: 50, minFree: 5 << 30, space: func(string) (int64, int64, error) {
		calls++
		if calls > 2 {
			return 128 << 30, 1 << 30, nil
		}
		return 128 << 30, 100 << 30, nil
	}}
	bounded := &budgetStore{Store: store, policy: p}
	_, err := bounded.WriteBlob(strings.NewReader(strings.Repeat("x", 1<<20)), 0)
	require.ErrorIs(t, err, errCacheDiskPressure)
	used, err := store.DiskUsage()
	require.NoError(t, err)
	require.Zero(t, used)
}

func TestPlaybackCache_ExplicitDownloadUpgradesInflightCapture(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "max-age=7200")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		close(entered)
		<-release
		_, _ = io.WriteString(w, "image")
	}))
	defer origin.Close()
	store := playbackStore(t)
	s := playbackService(t, store, playbackClassifier(func(context.Context, string) (MediaClass, error) { return ClassMedia, nil }))
	obs := &recordingProgressObserver{}
	s.observer = obs
	require.NoError(t, s.Start(context.Background()))
	defer s.Stop()
	item := dp1playlist.PlaylistItem{Source: origin.URL}
	s.automatic.observe([]dp1playlist.PlaylistItem{item}, 0, true)
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("automatic capture did not start")
	}
	st, err := s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Empty(t, st.Items)
	require.NoError(t, s.DownloadItem(context.Background(), item))
	st, err = s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Equal(t, []ItemStatus{{Source: item.Source, State: StateDownloading}}, st.Items)
	s.automatic.observe([]dp1playlist.PlaylistItem{{Source: "https://example.com/new.png"}}, 0, false)
	close(release)
	require.Eventually(t, func() bool {
		rec, err := store.LoadItem(SourceKey(item.Source))
		return err == nil && !rec.Automatic && rec.Resources[0].FreshUntil.IsZero()
	}, time.Second, time.Millisecond)
	// The joined capture is now the requested download (the flag cleared in
	// process), so it is announced and listed like any download (#423).
	require.Eventually(t, func() bool { return slices.Contains(obs.states(), StateReady) }, time.Second, time.Millisecond)
	st, err = s.Status(StatusRequest{})
	require.NoError(t, err)
	require.Len(t, st.Items, 1)
	require.Equal(t, StateReady, st.Items[0].State)
	require.Equal(t, StatusTotals{Total: 1, Ready: 1}, *st.Totals)
}

func TestStorageBudget_EnforcedAtStartupWithoutPlayback(t *testing.T) {
	store := playbackStore(t)
	item := dp1playlist.PlaylistItem{Source: "https://example.com/old.png"}
	hash, err := store.WriteBlob(strings.NewReader("old image"), 0)
	require.NoError(t, err)
	require.NoError(t, store.SaveItem(&ItemRecord{Item: item, Resources: []Resource{{URL: item.Source, SHA256: hash}}, Coverage: Coverage{Complete: true}}))
	s := playbackService(t, store, nil)
	s.storage = &storagePolicy{percent: 50, minFree: 5 << 30, space: func(string) (int64, int64, error) { return 128 << 30, 1 << 30, nil }}
	require.NoError(t, s.Start(context.Background()))
	defer s.Stop()
	require.Eventually(t, func() bool { _, err := store.LoadItem(SourceKey(item.Source)); return errors.Is(err, ErrItemNotFound) }, time.Second, time.Millisecond)
}

func TestAutomaticStore_OldReadersRejectAndOldClearCannotResurrect(t *testing.T) {
	store := playbackStore(t)
	item := dp1playlist.PlaylistItem{Source: "https://example.com/changing.png"}
	hash, err := store.WriteBlob(strings.NewReader("automatic image"), 0)
	require.NoError(t, err)
	rec := &ItemRecord{Item: item, Automatic: true, Resources: []Resource{{URL: item.Source, SHA256: hash, FreshUntil: time.Now().Add(time.Hour)}}}
	require.NoError(t, store.SaveItem(rec))
	key := SourceKey(item.Source)
	path := filepath.Join(store.RootDir(), "items", key+".json")
	data, err := os.ReadFile(path) //nolint:gosec // path is inside this test's t.TempDir.
	require.NoError(t, err)
	// This is the old reader: plain ItemRecord JSON followed by its source-key
	// check. The envelope must be rejected rather than interpreted as a snapshot.
	var legacy ItemRecord
	require.NoError(t, json.Unmarshal(data, &legacy))
	require.NotEqual(t, key, SourceKey(legacy.Item.Source))
	_, err = store.LoadItem(key)
	require.NoError(t, err)
	_, _, err = store.GC()
	require.NoError(t, err)
	_, err = store.BlobSize(hash)
	require.NoError(t, err)
	// Old download writes a bare snapshot to the same path. Old clear removes
	// that path. Keep the blob (shared by another artwork) to test resurrection.
	rec.Automatic = false
	data, err = json.Marshal(rec)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0600))
	require.NoError(t, os.Remove(path))
	_, err = store.LoadItem(key)
	require.ErrorIs(t, err, ErrItemNotFound)
	keys, err := store.ListItemKeys()
	require.NoError(t, err)
	require.Empty(t, keys)
}

func TestAutomaticCapture_DoesNotSnapshotMutableRedirect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/current" {
			w.Header().Set("Cache-Control", "no-store")
			http.Redirect(w, r, "/immutable.png", http.StatusFound)
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.Header().Set("Cache-Control", "public,max-age=7200")
		_, _ = io.WriteString(w, "image")
	}))
	defer origin.Close()
	store := playbackStore(t)
	media := NewMediaCapturer(wrapper.NewHTTPClient(), store, wrapper.NewJSON(), wrapper.NewClock(), 0, zaptest.NewLogger(t))
	intent := &atomic.Bool{}
	intent.Store(true)
	_, err := media.Capture(context.WithValue(context.Background(), automaticCaptureKey{}, intent), dp1playlist.PlaylistItem{Source: origin.URL + "/current"})
	require.ErrorIs(t, err, errAutomaticUncacheable)
	used, err := store.DiskUsage()
	require.NoError(t, err)
	require.Zero(t, used)
}
