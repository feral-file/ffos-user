package mediahealth

import (
	"context"
	"encoding/json"
	"fmt"
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

// The nightly render never learns: it counts a frame only when its own GET
// of the document was served 2xx, and a served GET outranks the HEAD — the
// frame is the document, not an error page (PR #450 review, F1).
func TestLearned_TheNightlyRenderNeverLearns(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := openTestStore(t, clock)
	h, err := PHash(errorPage())
	require.NoError(t, err)
	for i, src := range []string{"https://o/a", "https://o/b", "https://o/c"} {
		seedSoftware(t, store, src)
		_, err := store.ApplyOutcome(Outcome{Seq: int64(i + 1), Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1}, true)
		require.NoError(t, err)
		rec, err := store.ApplyRender(src, RenderResult{Verdict: RenderOK, PHash: h, Variance: 0.05})
		require.NoError(t, err)
		assert.NotEqual(t, LayerL1, rec.Layer, "the HEAD still wins the record: no ok over the 404")
	}
	assert.Empty(t, store.LearnedFingerprints())
}

// wallFrameOK is the wall capture's delivery of a frame that rendered.
func wallFrameOK(src string, h uint64) Outcome {
	return Outcome{Source: src, Kind: OutcomeOK, Stage: StageFrame, Code: string(RenderOK), frame: &FrameEvidence{PHash: h, Variance: 0.05, Clipped: true}}
}

// A refusal is learnable only against a frame of the same play through the
// same gateway (PR #450 review, F1): the HEAD 404'd on the source's own
// gateway, L0 then found the work served by a fallback, and the wall now
// shows the work through that fallback — the healthy frame must not become
// a known-bad fingerprint. A frame from a later play is not the conflict
// either. The control (same play, same gateway) still learns, so the guard
// is what stops the other two.
func TestLearned_OnlyTheSamePlayOnTheSameGateway(t *testing.T) {
	h, err := PHash(errorPage())
	require.NoError(t, err)
	refuse := func(t *testing.T, store *Store, seq int64, src string) {
		t.Helper()
		seedSoftware(t, store, src)
		_, err := store.ApplyOutcome(Outcome{Seq: seq, Source: src, Kind: OutcomeFail, Stage: StageDetect, Code: "404", Attempt: 1}, true)
		require.NoError(t, err)
	}
	srcs := []string{"https://gw.example/ipfs/a", "https://gw.example/ipfs/b", "https://gw.example/ipfs/c"}
	cases := []struct {
		name    string
		between func(t *testing.T, store *Store, clock *testClock, src string)
		learns  bool
	}{
		{"same play, same gateway", func(*testing.T, *Store, *testClock, string) {}, true},
		{"served by a fallback since", func(t *testing.T, store *Store, _ *testClock, src string) {
			_, err := store.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html", ServedBy: "https://ipfs.filebase.io"})
			require.NoError(t, err)
		}, false},
		{"a later play", func(_ *testing.T, _ *Store, clock *testClock, _ string) {
			clock.Advance(learnConflictWindow + time.Second)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clock := &testClock{t: time.Now()}
			store := openTestStore(t, clock)
			for i, src := range srcs {
				refuse(t, store, int64(i+1), src)
				tc.between(t, store, clock, src)
				_, err := store.ApplyOutcome(wallFrameOK(src, h), true)
				require.NoError(t, err)
			}
			if tc.learns {
				assert.Len(t, store.LearnedFingerprints(), 1)
			} else {
				assert.Empty(t, store.LearnedFingerprints())
			}
		})
	}
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

// The cluster cap holds when every cluster is promoted: a learned
// fingerprint is never evicted, so a new hash is dropped (PR #450 review,
// F7).
func TestLearned_TheCapHoldsWhenEveryClusterIsPromoted(t *testing.T) {
	clock := &testClock{t: time.Now()}
	path := filepath.Join(t.TempDir(), "mh", "store.db")
	st, err := Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	require.NoError(t, st.db.Update(func(tx *bolt.Tx) error {
		b := tx.Bucket(bucketFingerprints)
		for i := 0; i < maxLearnClusters; i++ {
			c := learnCluster{Hash: fmt.Sprintf("%016x", uint64(i)<<40), Promoted: true, Sources: []string{"a", "b", "c"}, First: clock.Now(), Last: clock.Now()}
			raw, err := json.Marshal(c)
			if err != nil {
				return err
			}
			if err := b.Put([]byte(c.Hash), raw); err != nil {
				return err
			}
		}
		return nil
	}))
	require.NoError(t, st.Close())
	st, err = Open(path, Options{Now: clock.Now, Logger: zap.NewNop()})
	require.NoError(t, err)
	defer func() { _ = st.Close() }()
	require.Len(t, st.LearnedFingerprints(), maxLearnClusters)

	fresh := ^uint64(0) // far from every seeded hash
	for _, src := range []string{"https://o/x", "https://o/y", "https://o/z"} {
		st.learnErrorPage(src, fresh)
	}
	var n int
	require.NoError(t, st.db.View(func(tx *bolt.Tx) error { n = tx.Bucket(bucketFingerprints).Stats().KeyN; return nil }))
	assert.Equal(t, maxLearnClusters, n)
	assert.Len(t, st.LearnedFingerprints(), maxLearnClusters)
}
