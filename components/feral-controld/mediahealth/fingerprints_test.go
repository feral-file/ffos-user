package mediahealth

import (
	"context"
	"image"
	"image/color"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	bolt "go.etcd.io/bbolt"
	"go.uber.org/zap"
)

// errorPage is a stand-in for a gateway's 404 page: a dark field with a light
// block of "text" — a frame that renders (not blank) and is not art.
func errorPage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			c := color.RGBA{R: 20, G: 20, B: 24, A: 255}
			if y > 24 && y < 32 && x > 12 && x < 52 {
				c = color.RGBA{R: 230, G: 230, B: 230, A: 255}
			}
			img.Set(x, y, c)
		}
	}
	return img
}

// errorPagePlay is a play whose content-type check said 404 and whose frame
// on the wall rendered the gateway's error page.
func errorPagePlay(t *testing.T, svc *Service, w *WallCapture, clock *testClock, src string) {
	t.Helper()
	seedSoftware(t, svc.Store(), src)
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
	svc.ObservePlayback(items(src), 0, ready(), &Outcomes{CastSeq: 1, Entries: []Outcome{
		{Seq: clock.Now().UnixNano(), Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1, At: clock.Now().UTC().Format(time.RFC3339Nano)},
		{Seq: clock.Now().UnixNano() + 1, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: clock.Now().UTC().Format(time.RFC3339Nano)},
	}})
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
}

func TestLearned_ThreeWorksTeachTheErrorPage(t *testing.T) {
	clock := &testClock{t: time.Now()}
	frames := []*WallFrame{}
	for i := 0; i < 4; i++ {
		frames = append(frames, &WallFrame{Image: errorPage(), Clipped: true})
	}
	cap := &fakeWall{frames: frames}
	svc, w := newWallRig(t, clock, cap)

	errorPagePlay(t, svc, w, clock, "https://gw.example/ipfs/a")
	errorPagePlay(t, svc, w, clock, "https://gw.example/ipfs/b")
	assert.Empty(t, svc.Store().LearnedFingerprints(), "two works could be a coincidence")
	errorPagePlay(t, svc, w, clock, "https://gw.example/ipfs/c")
	learned := svc.Store().LearnedFingerprints()
	require.Len(t, learned, 1, "three works whose HEAD said 404 rendered the same page: learned")
	assert.Equal(t, learnedLabel, learned[0].Label)

	// A fourth work serves the same page with a 200 (the Arweave case): no
	// HEAD refusal to go by, but the frame is now recognized.
	src := "https://other.example/work"
	seedSoftware(t, svc.Store(), src)
	svc.ObservePlayback(items(src), 0, pending(), &Outcomes{CastSeq: 1})
	svc.ObservePlayback(items(src), 0, ready(), readyOutcomes(99, src))
	clock.Advance(DefaultWallSettle)
	require.True(t, w.Step(context.Background()))
	rec, err := svc.Store().Get(src)
	require.NoError(t, err)
	require.NotNil(t, rec.Wall)
	assert.Equal(t, RenderKnownBad, rec.Wall.Verdict)
	assert.Equal(t, learnedLabel, rec.Wall.Label)
	assert.Equal(t, 1, rec.Plays.ConsecutiveFail, "a counted render failure, toward a verdict")
}

func TestLearned_SurvivesAReopen(t *testing.T) {
	clock := &testClock{t: time.Now()}
	path := filepath.Join(t.TempDir(), "mh", "store.db")
	st, err := Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	h, err := PHash(errorPage())
	require.NoError(t, err)
	for _, src := range []string{"https://o/a", "https://o/b", "https://o/c"} {
		st.learnErrorPage(src, h)
	}
	require.Len(t, st.LearnedFingerprints(), 1)
	require.NoError(t, st.Close())
	st, err = Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	assert.Len(t, st.LearnedFingerprints(), 1)
}

// The nightly render learns too: a frame that renders over an open 404 run.
func TestLearned_TheNightlyRenderLearns(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openTestStore(t, clock)
	h, err := PHash(errorPage())
	require.NoError(t, err)
	for _, src := range []string{"https://o/a", "https://o/b", "https://o/c"} {
		seedSoftware(t, store, src)
		_, err := store.ApplyOutcome(Outcome{Seq: int64(len(src)), Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1, At: "t"}, true)
		require.NoError(t, err)
		rec, err := store.ApplyRender(src, RenderResult{Verdict: RenderOK, PHash: h, Variance: 0.05})
		require.NoError(t, err)
		assert.NotEqual(t, LayerL1, rec.Layer, "the HEAD wins: no ok over the 404")
	}
	assert.Len(t, store.LearnedFingerprints(), 1)
}

// The bucket is bounded: the least recently seen unpromoted cluster goes.
func TestLearned_ClustersAreBounded(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openTestStore(t, clock)
	for i := 0; i < maxLearnClusters+5; i++ {
		// Hashes far apart (a distinct bit pattern each), so each is its
		// own cluster.
		h := uint64(i) * 0x9E3779B97F4A7C15
		store.learnErrorPage("https://o/"+strconv.Itoa(i), h)
		clock.Advance(time.Second)
	}
	var n int
	require.NoError(t, store.db.View(func(tx *bolt.Tx) error { n = tx.Bucket(bucketFingerprints).Stats().KeyN; return nil }))
	assert.LessOrEqual(t, n, maxLearnClusters)
}
