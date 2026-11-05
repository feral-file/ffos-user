package mediahealth

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type pushCall struct {
	castSeq int64
	hints   map[string]Hint
}

// fakeHintSender records every push and answers with err.
type fakeHintSender struct {
	mu    sync.Mutex
	calls []pushCall
	err   error
}

func (f *fakeHintSender) send(_ context.Context, castSeq int64, hints map[string]Hint) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, pushCall{castSeq: castSeq, hints: hints})
	return []int{}, f.err
}

func (f *fakeHintSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

func (f *fakeHintSender) last() pushCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

var brokenHint = Hint{State: StateBroken, Category: CategorySourceMissing, Reason: ReasonHTTPStatus}

// newTestPusher returns a pusher whose hint map is whatever *want holds.
func newTestPusher(clock *testClock, f *fakeHintSender, want *map[string]Hint) *HintPusher {
	return &HintPusher{
		Send:     f.send,
		Hints:    func([]string) map[string]Hint { return *want },
		Interval: 15 * time.Second,
		Now:      clock.Now,
	}
}

func TestHintPusher_SendsAVerdictReachedBetweenPushes(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := &fakeHintSender{}
	want := map[string]Hint{}
	p := newTestPusher(clock, f, &want)
	wall := []string{"https://o/a", "https://o/b"}

	// The cast's push carried no hints, then the poll sees its castSeq.
	p.Attached(nil)
	p.Observe(wall, 7, true)
	assert.False(t, p.step(context.Background()), "inside the settle after a new cast nothing is sent")
	clock.Advance(16 * time.Second)
	assert.False(t, p.step(context.Background()), "nothing changed since the push: no chatter")

	// The lookahead concludes b broken.
	want = map[string]Hint{"https://o/b": brokenHint}
	assert.True(t, p.step(context.Background()))
	require.Equal(t, 1, f.count())
	assert.Equal(t, int64(7), f.last().castSeq)
	assert.Equal(t, want, f.last().hints)
	assert.False(t, p.step(context.Background()), "delivered: the belief matches, nothing more is sent")

	// b heals: the empty map must reach the player too.
	want = map[string]Hint{}
	assert.True(t, p.step(context.Background()))
	assert.Empty(t, f.last().hints)
}

func TestHintPusher_AttachedMapIsTheBaselineForTheCast(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := &fakeHintSender{}
	want := map[string]Hint{"https://o/a": brokenHint}
	p := newTestPusher(clock, f, &want)
	p.Attached(map[string]Hint{"https://o/a": brokenHint})
	p.Observe([]string{"https://o/a", "https://o/b"}, 3, true)
	clock.Advance(16 * time.Second)
	assert.False(t, p.step(context.Background()), "the push already carried this map")
	assert.Zero(t, f.count())
}

func TestHintPusher_UnknownBaselineSendsOnlyWhenSomethingIsHinted(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := &fakeHintSender{}
	want := map[string]Hint{}
	p := newTestPusher(clock, f, &want)
	// A boot replay: the player put a cast up without a controld push.
	p.Observe([]string{"https://o/a"}, 1, true)
	clock.Advance(16 * time.Second)
	assert.False(t, p.step(context.Background()), "nothing to hint: no send on an unknown baseline")
	want = map[string]Hint{"https://o/a": brokenHint}
	assert.True(t, p.step(context.Background()), "the boot cast carried no hints; the push restores them")
	assert.Equal(t, 1, f.count())
}

func TestHintPusher_NewCastResetsTheSettleAndTheBaseline(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := &fakeHintSender{}
	want := map[string]Hint{"https://o/a": brokenHint}
	p := newTestPusher(clock, f, &want)
	p.Observe([]string{"https://o/a"}, 1, true)
	clock.Advance(16 * time.Second)
	require.True(t, p.step(context.Background()))

	// A re-cast of the same wall opens castSeq 2 with a push carrying the map.
	p.Attached(map[string]Hint{"https://o/a": brokenHint})
	clock.Advance(time.Second)
	p.Observe([]string{"https://o/a"}, 2, true)
	assert.False(t, p.step(context.Background()), "settling after the new cast")
	clock.Advance(16 * time.Second)
	assert.False(t, p.step(context.Background()), "the new cast's push carried the same map")
	assert.Equal(t, 1, f.count())
}

func TestHintPusher_ReplyBranches(t *testing.T) {
	clock := &testClock{t: time.Now()}
	want := map[string]Hint{"https://o/a": brokenHint}
	setup := func(err error) (*HintPusher, *fakeHintSender) {
		f := &fakeHintSender{err: err}
		p := newTestPusher(clock, f, &want)
		p.Observe([]string{"https://o/a"}, 5, true)
		clock.Advance(16 * time.Second)
		return p, f
	}

	t.Run("stale cast keeps no belief and retries on the next cast", func(t *testing.T) {
		p, f := setup(ErrStaleCast)
		assert.True(t, p.step(context.Background()))
		f.err = nil
		p.Observe([]string{"https://o/a"}, 6, true)
		clock.Advance(16 * time.Second)
		assert.True(t, p.step(context.Background()))
		assert.Equal(t, int64(6), f.last().castSeq)
	})
	t.Run("transport errors retry on the next tick", func(t *testing.T) {
		p, f := setup(errors.New("cdp down"))
		assert.True(t, p.step(context.Background()))
		f.err = nil
		assert.True(t, p.step(context.Background()), "the belief did not move, so the map is resent")
		assert.Equal(t, 2, f.count())
		assert.False(t, p.step(context.Background()))
	})
	t.Run("a refusal parks the pusher until the next cast", func(t *testing.T) {
		p, f := setup(ErrHintPushRefused)
		assert.True(t, p.step(context.Background()))
		assert.False(t, p.step(context.Background()), "never a retry loop against a refusing player")
		assert.Equal(t, 1, f.count())
		f.err = nil
		p.Observe([]string{"https://o/a"}, 6, true)
		clock.Advance(16 * time.Second)
		assert.True(t, p.step(context.Background()), "a new cast un-parks it")
	})
}

func TestHintPusher_GatesAndNoWall(t *testing.T) {
	clock := &testClock{t: time.Now()}
	f := &fakeHintSender{}
	want := map[string]Hint{"https://o/a": brokenHint}
	p := newTestPusher(clock, f, &want)
	supported := false
	p.Supported = func() bool { return supported }
	p.Observe([]string{"https://o/a"}, 1, true)
	clock.Advance(16 * time.Second)
	assert.False(t, p.step(context.Background()), "a player without the command is never sent it")
	supported = true
	p.Observe(nil, 0, false)
	assert.False(t, p.step(context.Background()), "no playlist (or no outcome ring): no wall to push for")
	assert.Zero(t, f.count())

	var nilPusher *HintPusher
	nilPusher.Observe([]string{"x"}, 1, true)
	nilPusher.Attached(nil)
	nilPusher.Run(context.Background())
}

func TestHintPusher_RunStopsOnContext(t *testing.T) {
	f := &fakeHintSender{}
	want := map[string]Hint{}
	p := &HintPusher{Send: f.send, Hints: func([]string) map[string]Hint { return want }, Interval: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { p.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return on cancel")
	}
}

// The Service wiring: every map a push path attaches is the pusher's
// baseline, and the poll feeds it the wall and the cast generation.
func TestService_FeedsThePusher(t *testing.T) {
	clock := &testClock{t: time.Now()}
	s := newTestService(t, newTableFetcher(), clock, manifestWithContract)
	defer func() { _ = s.Close() }()
	f := &fakeHintSender{}
	p := &HintPusher{Send: f.send, Hints: s.Hints, Interval: 15 * time.Second, Now: clock.Now}
	s.pusher = p
	_, err := s.Store().ApplyProbe("https://o/b", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)

	list := items("https://o/a", "https://o/b")
	require.NotNil(t, s.HintArgument(list))
	s.ObservePlayback(list, 0, nil, &Outcomes{CastSeq: 4})
	clock.Advance(16 * time.Second)
	assert.False(t, p.step(context.Background()), "the cast's own push carried b")

	_, err = s.Store().ApplyProbe("https://o/a", ProbeResult{Verdict: VerdictDead, Status: 410, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	assert.True(t, p.step(context.Background()), "a verdict reached between pushes is pushed")
	assert.Len(t, f.last().hints, 2)
	assert.Equal(t, int64(4), f.last().castSeq)

	s.ObservePlayback(nil, 0, nil, nil)
	assert.False(t, p.step(context.Background()), "nothing on the wall")
}

func TestPlayerContract_HintPushIsFeatureDetected(t *testing.T) {
	path := filepath.Join(t.TempDir(), "manifest.json")
	c := &PlayerContract{Path: path}
	require.NoError(t, os.WriteFile(path, []byte(manifestWithContract), 0o600))
	assert.True(t, c.Supported())
	assert.False(t, c.SupportsHintPush(), "v1 without the command: hints ride pushes only")

	require.NoError(t, os.WriteFile(path, []byte(`{"contracts":{"mediaHealth":{"version":1,"commands":["setMediaHealth"]}}}`), 0o600))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))
	c.checked = time.Time{}
	assert.True(t, c.SupportsHintPush())

	require.NoError(t, os.WriteFile(path, []byte(`{"contracts":{"mediaHealth":{"version":2,"commands":["setMediaHealth"]}}}`), 0o600))
	later := future.Add(time.Hour)
	require.NoError(t, os.Chtimes(path, later, later))
	c.checked = time.Time{}
	assert.False(t, c.SupportsHintPush(), "the command never outlives the contract gate")
	assert.False(t, (*PlayerContract)(nil).SupportsHintPush())
}
