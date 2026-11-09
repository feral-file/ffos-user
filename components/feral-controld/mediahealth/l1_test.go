package mediahealth

import (
	"context"
	"errors"
	"image"
	"image/color"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Frames: a uniform frame is blank; a checkerboard is a real render; a
// gradient varies in hue but not luma (the trap the per-channel variance
// exists for).
func uniform(c color.Color) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func checkerboard() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if (x/8+y/8)%2 == 0 {
				img.Set(x, y, color.White)
			} else {
				img.Set(x, y, color.Black)
			}
		}
	}
	return img
}

func hueGradient() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 64, 64))
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			img.Set(x, y, color.RGBA{R: uint8(x * 4), G: 0x80, B: uint8(255 - x*4), A: 255})
		}
	}
	return img
}

func TestClassify_LadderAndVariance(t *testing.T) {
	c, err := Classify(uniform(color.Black), nil, DefaultBlankVarianceThreshold)
	require.NoError(t, err)
	assert.Equal(t, RenderBlank, c.Verdict)
	assert.Less(t, c.Variance, DefaultBlankVarianceThreshold)

	c, err = Classify(uniform(color.RGBA{R: 200, G: 30, B: 90, A: 255}), nil, DefaultBlankVarianceThreshold)
	require.NoError(t, err)
	assert.Equal(t, RenderBlank, c.Verdict, "a solid color is blank")

	c, err = Classify(checkerboard(), nil, DefaultBlankVarianceThreshold)
	require.NoError(t, err)
	assert.Equal(t, RenderOK, c.Verdict)

	c, err = Classify(hueGradient(), nil, DefaultBlankVarianceThreshold)
	require.NoError(t, err)
	assert.Equal(t, RenderOK, c.Verdict, "hue without luma spread is still art, not blank")

	// A fingerprint of the checkerboard matches it, and matching comes
	// first even for a frame that is not blank.
	hash, err := PHash(checkerboard())
	require.NoError(t, err)
	fp, err := ParseFingerprint("0x"+hexOf(hash), 4, "gateway-error")
	require.NoError(t, err)
	c, err = Classify(checkerboard(), []Fingerprint{fp}, DefaultBlankVarianceThreshold)
	require.NoError(t, err)
	assert.Equal(t, RenderKnownBad, c.Verdict)
	assert.Equal(t, "gateway-error", c.Label)
	assert.Equal(t, 0, HashDistance(hash, fp.Hash))

	c, err = Classify(uniform(color.Black), []Fingerprint{fp}, DefaultBlankVarianceThreshold)
	require.NoError(t, err)
	assert.Equal(t, RenderBlank, c.Verdict, "a far fingerprint does not match")
}

func hexOf(h uint64) string {
	const digits = "0123456789abcdef"
	out := make([]byte, 16)
	for i := 15; i >= 0; i-- {
		out[i] = digits[h&0xf]
		h >>= 4
	}
	return string(out)
}

func TestParseFingerprint(t *testing.T) {
	fp, err := ParseFingerprint("0xABCDEF", 6, "x")
	require.NoError(t, err)
	assert.Equal(t, uint64(0xabcdef), fp.Hash)
	fp, err = ParseFingerprint("  ffffffffffffffff ", 0, "y")
	require.NoError(t, err)
	assert.Equal(t, uint64(0xffffffffffffffff), fp.Hash)
	for _, bad := range []string{"", "0x", "12345678901234567", "xyz"} {
		_, err := ParseFingerprint(bad, 4, "bad")
		assert.Error(t, err, bad)
	}
	_, err = ParseFingerprint("ff", 65, "far")
	assert.Error(t, err)
	_, err = ParseFingerprint("ff", -1, "neg")
	assert.Error(t, err)
}

// fakeRenderer answers by wire URL and records calls with their settle and
// whether they were canceled.
type fakeRenderer struct {
	mu     sync.Mutex
	frames map[string]*Frame
	errs   map[string]error
	calls  []renderCall
	// block, when set, makes Render wait for ctx (a slow settle).
	block bool
}

type renderCall struct {
	url      string
	settle   time.Duration
	canceled bool
}

func (f *fakeRenderer) Render(ctx context.Context, url string, settle time.Duration) (*Frame, error) {
	call := renderCall{url: url, settle: settle}
	if f.block {
		<-ctx.Done()
		call.canceled = true
		f.mu.Lock()
		f.calls = append(f.calls, call)
		f.mu.Unlock()
		return nil, ctx.Err()
	}
	f.mu.Lock()
	f.calls = append(f.calls, call)
	frame, err := f.frames[url], f.errs[url]
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if frame == nil {
		return nil, errors.New("no scripted frame")
	}
	return frame, nil
}

func (f *fakeRenderer) count(url string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c.url == url {
			n++
		}
	}
	return n
}

func newL1Store(t *testing.T, clock *testClock) *Store {
	t.Helper()
	return openTestStore(t, clock)
}

// seedSoftware records an L0-alive HTML source.
func seedSoftware(t *testing.T, store *Store, src string) {
	t.Helper()
	_, err := store.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html", WireURL: src})
	require.NoError(t, err)
}

func TestL1Candidates_SoftwareOnlyDueAndOrdered(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	seedSoftware(t, store, "https://o/html-a")
	seedSoftware(t, store, "https://o/html-b")
	_, err := store.ApplyProbe("https://o/image", ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "image/png", Sniffed: "image/png"})
	require.NoError(t, err)
	_, err = store.ApplyProbe("https://o/dead-html", ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus, Declared: "text/html"})
	require.NoError(t, err)
	_, err = store.ApplyOutcome(Outcome{Seq: 1, Source: "https://o/iframe", Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: "t"}, true)
	require.NoError(t, err)
	_, err = store.ApplyProbe("data:text/html,hi", ProbeResult{Verdict: VerdictInline})
	require.NoError(t, err)

	cands, err := store.L1Candidates(clock.Now(), 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"https://o/html-a", "https://o/html-b", "https://o/iframe"}, cands,
		"HTML and iframe-rendered sources only; images, a 404 and data: URIs are never rendered")

	// Rendered ones move behind never-rendered ones, and a due one comes
	// back after its window.
	_, err = store.ApplyRender("https://o/html-a", RenderResult{Verdict: RenderOK, PHash: 1, Variance: 0.1})
	require.NoError(t, err)
	cands, err = store.L1Candidates(clock.Now(), 10)
	require.NoError(t, err)
	assert.ElementsMatch(t, []string{"https://o/html-b", "https://o/iframe"}, cands)
	clock.Advance(RecheckL1OK + time.Second)
	cands, err = store.L1Candidates(clock.Now(), 10)
	require.NoError(t, err)
	assert.Len(t, cands, 3)
	assert.Equal(t, "https://o/html-a", cands[2], "the rendered one sorts last")
	one, err := store.L1Candidates(clock.Now(), 1)
	require.NoError(t, err)
	assert.Len(t, one, 1)
	assert.NotEqual(t, "https://o/html-a", one[0])
}

func TestApplyRender_Transitions(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	src := "https://o/html"
	seedSoftware(t, store, src)
	before, err := store.Get(src)
	require.NoError(t, err)

	// First blank: confirmation due now, state untouched.
	rec, err := store.ApplyRender(src, RenderResult{Verdict: RenderBlank, PHash: 7, Variance: 0.0001})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State)
	assert.Equal(t, 1, rec.L1.ConsecutiveBlank)
	assert.False(t, clock.Now().Before(rec.L1.Next), "a confirmation render is due immediately")
	assert.Equal(t, "0000000000000007", rec.L1.PHash)
	assert.Equal(t, before.RecheckAfter, rec.RecheckAfter, "L1 never touches the L0 recheck")
	cands, err := store.L1Candidates(clock.Now(), 1)
	require.NoError(t, err)
	assert.Equal(t, []string{src}, cands)

	// Second blank: suspect.
	rec, err = store.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State)
	assert.Equal(t, LayerL1, rec.Layer)
	assert.Equal(t, CategoryRenderBlank, rec.Category)
	assert.Equal(t, ReasonRenderBlank, rec.Reason)
	assert.Equal(t, 2, rec.L1.ConsecutiveBlank)
	assert.WithinDuration(t, clock.Now().Add(RecheckL1Suspect), rec.L1.Next, time.Second)
	assert.False(t, rec.Conclusive(clock.Now()), "suspect never hints")
	hints, err := store.Hints([]string{src}, clock.Now())
	require.NoError(t, err)
	assert.Empty(t, hints)
	health, err := store.ItemHealths([]string{src})
	require.NoError(t, err)
	assert.Equal(t, ItemHealth{State: StateSuspect, Category: CategoryRenderBlank, Reason: ReasonRenderBlank, Layer: LayerL1}, health[0])

	// A real render heals suspect back to ok.
	rec, err = store.ApplyRender(src, RenderResult{Verdict: RenderOK, PHash: 9, Variance: 0.2})
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State)
	assert.Equal(t, LayerL1, rec.Layer)
	assert.Empty(t, rec.Reason)
	assert.Equal(t, 0, rec.L1.ConsecutiveBlank)
	assert.WithinDuration(t, clock.Now().Add(RecheckL1OK), rec.L1.Next, time.Second)

	// Known bad: suspect at once, with the label.
	rec, err = store.ApplyRender(src, RenderResult{Verdict: RenderKnownBad, PHash: 3, Variance: 0.1, Label: "kubo-500"})
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State)
	assert.Equal(t, ReasonRenderKnownBad, rec.Reason)
	assert.Equal(t, "kubo-500", rec.L1.Label)

	// Stalled: no evidence, the suspect state and the hash stay.
	rec, err = store.ApplyRender(src, RenderResult{Verdict: RenderStalled, Err: "main document HTTP 502", MainStatus: 502})
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State)
	assert.Equal(t, RenderStalled, rec.L1.Verdict)
	assert.Equal(t, "main document HTTP 502", rec.L1.LastError)
	assert.Equal(t, 502, rec.L1.MainStatus)
	assert.Equal(t, "0000000000000003", rec.L1.PHash, "the last real hash is kept")
	assert.WithinDuration(t, clock.Now().Add(RecheckL1Stalled), rec.L1.Next, time.Second)

	// A broken verdict from L0 outranks L1 either way.
	_, err = store.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	_, err = store.ApplyRender(src, RenderResult{Verdict: RenderBlank})
	require.NoError(t, err)
	rec, err = store.ApplyRender(src, RenderResult{Verdict: RenderBlank})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State)
	assert.Equal(t, LayerL0, rec.Layer)
	assert.Equal(t, RenderBlank, rec.L1.Verdict)
	rec, err = store.ApplyRender(src, RenderResult{Verdict: RenderOK, PHash: 1, Variance: 0.2})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "a frame cannot heal a 404")
}

// fakeSleep is the AsleepSince source for tests: Store(true) stamps a new
// sleep at the clock's time (as devicectl stamps each transition into
// sleep), Store(false) wakes.
type fakeSleep struct {
	mu     sync.Mutex
	clock  *testClock
	asleep bool
	since  time.Time
}

func newFakeSleep(clock *testClock) *fakeSleep { return &fakeSleep{clock: clock} }

func (f *fakeSleep) Store(asleep bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if asleep && !f.asleep {
		f.since = f.clock.Now()
	}
	f.asleep = asleep
}

func (f *fakeSleep) Since() (time.Time, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.since, f.asleep
}

func newTestL1(t *testing.T, store *Store, r Renderer, clock *testClock, asleep *fakeSleep) *L1 {
	t.Helper()
	return NewL1(L1Config{
		Store:       store,
		Renderer:    r,
		AsleepSince: asleep.Since,
		Now:         clock.Now,
		Settle:      time.Millisecond,
		GateWatch:   time.Millisecond,
		MinAsleep:   -1, // no grace: it has its own test
	})
}

func TestL1_GateCapAndVerdicts(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	seedSoftware(t, store, "https://o/blank")
	seedSoftware(t, store, "https://o/fine")
	r := &fakeRenderer{frames: map[string]*Frame{
		"https://o/blank": {Image: uniform(color.Black), MainStatus: 200},
		"https://o/fine":  {Image: checkerboard(), MainStatus: 200},
	}}
	asleep := newFakeSleep(clock)
	l1 := newTestL1(t, store, r, clock, asleep)

	assert.False(t, l1.Step(context.Background()), "awake: nothing is rendered")
	assert.Empty(t, r.calls)

	asleep.Store(true)
	// Two candidates, the blank one twice (confirmation): three renders.
	for i := 0; i < 3; i++ {
		assert.True(t, l1.Step(context.Background()), "render %d", i)
	}
	assert.False(t, l1.Step(context.Background()), "nothing left due tonight")
	assert.Equal(t, 2, r.count("https://o/blank"))
	assert.Equal(t, 1, r.count("https://o/fine"))
	var settles []time.Duration
	for _, c := range r.calls {
		if c.url == "https://o/blank" {
			settles = append(settles, c.settle)
		}
	}
	assert.Equal(t, []time.Duration{time.Millisecond, DefaultConfirmSettle}, settles, "the confirmation render uses the longer settle")
	rec, err := store.Get("https://o/blank")
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State)
	rec, err = store.Get("https://o/fine")
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State)
	assert.Equal(t, RenderOK, rec.L1.Verdict)

	// The nightly cap, reset by the calendar day.
	capped := NewL1(L1Config{Store: store, Renderer: r, AsleepSince: asleep.Since, Now: clock.Now, MaxPerNight: 1, Settle: time.Millisecond, GateWatch: time.Millisecond, MinAsleep: -1})
	clock.Advance(RecheckL1OK + time.Second)
	assert.True(t, capped.Step(context.Background()))
	assert.False(t, capped.Step(context.Background()), "one per night")
	clock.Advance(24 * time.Hour)
	assert.True(t, capped.Step(context.Background()), "a new day, a new budget")

	// Admission refusal waits.
	refused := NewL1(L1Config{Store: store, Renderer: r, AsleepSince: asleep.Since, Admit: func() bool { return false }, Now: clock.Now, Settle: time.Millisecond, MinAsleep: -1})
	clock.Advance(RecheckL1OK + time.Second)
	assert.False(t, refused.Step(context.Background()))
}

func TestL1_StalledAndUnavailableAreNotEvidence(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	seedSoftware(t, store, "https://o/502")
	seedSoftware(t, store, "https://o/down")
	seedSoftware(t, store, "https://o/nav")
	r := &fakeRenderer{
		frames: map[string]*Frame{"https://o/502": {Image: checkerboard(), MainStatus: 502}},
		errs: map[string]error{
			"https://o/down": errors.Join(ErrRendererUnavailable, errors.New("chromium exited")),
			"https://o/nav":  errors.New("navigation failed: net::ERR_NAME_NOT_RESOLVED"),
		},
	}
	asleep := newFakeSleep(clock)
	asleep.Store(true)
	l1 := newTestL1(t, store, r, clock, asleep)
	for i := 0; i < 3; i++ {
		assert.True(t, l1.Step(context.Background()))
	}
	assert.False(t, l1.Step(context.Background()))
	for _, src := range []string{"https://o/502", "https://o/down", "https://o/nav"} {
		rec, err := store.Get(src)
		require.NoError(t, err)
		assert.Equal(t, StateOK, rec.State, src)
		assert.Equal(t, RenderStalled, rec.L1.Verdict, src)
		assert.NotEmpty(t, rec.L1.LastError, src)
		assert.WithinDuration(t, clock.Now().Add(RecheckL1Stalled), rec.L1.Next, time.Second, src)
	}
}

func TestL1_WakeCancelsTheRenderAndKeepsItDue(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	seedSoftware(t, store, "https://o/slow")
	r := &fakeRenderer{block: true}
	asleep := newFakeSleep(clock)
	asleep.Store(true)
	l1 := newTestL1(t, store, r, clock, asleep)

	done := make(chan bool)
	go func() { done <- l1.Step(context.Background()) }()
	// The render is in flight; the panel wakes.
	time.Sleep(20 * time.Millisecond)
	asleep.Store(false)
	select {
	case rendered := <-done:
		assert.True(t, rendered)
	case <-time.After(5 * time.Second):
		t.Fatal("the wake did not cancel the render")
	}
	require.Len(t, r.calls, 1)
	assert.True(t, r.calls[0].canceled)
	rec, err := store.Get("https://o/slow")
	require.NoError(t, err)
	assert.Nil(t, rec.L1, "a canceled render records nothing and stays due")
	assert.Equal(t, StateOK, rec.State)
}

func TestL1_RunStopsOnContext(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	asleep := newFakeSleep(clock)
	l1 := NewL1(L1Config{Store: store, Renderer: &fakeRenderer{}, AsleepSince: asleep.Since, Now: clock.Now, Tick: time.Millisecond, MinAsleep: -1})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { l1.Run(ctx); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return after cancel")
	}
}

// Regressions from the PR #443 review (feralfile-bot, round 1).

// F19: the render asks for the record's OWN source, fragment kept — never
// the wire URL the cast pass copied from a sibling. Routing it like the
// kiosk (policy, recorded fallback, bare-CID rule) is the renderer's job,
// per request (offlinecache TestMediaHealthRenderer_RoutesLikeTheKiosk).
func TestL1_RendersTheRecordsOwnSource(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	// Two fragment siblings share one L0 request and, with it, one WireURL.
	for _, src := range []string{"https://o/work#one", "https://o/work#two"} {
		_, err := store.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html", WireURL: "https://o/work#one"})
		require.NoError(t, err)
	}
	r := &fakeRenderer{frames: map[string]*Frame{
		"https://o/work#one": {Image: checkerboard(), MainStatus: 200},
		"https://o/work#two": {Image: checkerboard(), MainStatus: 200},
	}}
	asleep := newFakeSleep(clock)
	asleep.Store(true)
	l1 := newTestL1(t, store, r, clock, asleep)
	assert.True(t, l1.Step(context.Background()))
	assert.True(t, l1.Step(context.Background()))
	assert.Equal(t, 1, r.count("https://o/work#one"))
	assert.Equal(t, 1, r.count("https://o/work#two"), "each sibling renders as itself")
}

// F21: a stalled confirmation keeps the blank pending, so the next render
// is still the long confirmation and a short blank can never conclude.
func TestL1_StalledConfirmationKeepsTheLongSettle(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	seedSoftware(t, store, "https://o/slow")
	r := &fakeRenderer{frames: map[string]*Frame{"https://o/slow": {Image: uniform(color.Black), MainStatus: 200}}}
	asleep := newFakeSleep(clock)
	asleep.Store(true)
	l1 := newTestL1(t, store, r, clock, asleep)

	assert.True(t, l1.Step(context.Background()), "first render: blank, confirmation due")
	r.mu.Lock()
	r.errs = map[string]error{"https://o/slow": errors.New("navigation failed")}
	r.mu.Unlock()
	assert.True(t, l1.Step(context.Background()), "the confirmation stalls")
	rec, err := store.Get("https://o/slow")
	require.NoError(t, err)
	assert.Equal(t, RenderStalled, rec.L1.Verdict)
	assert.Equal(t, 1, rec.L1.ConsecutiveBlank, "the blank stays pending")
	assert.Equal(t, StateOK, rec.State)

	clock.Advance(RecheckL1Stalled + time.Second)
	r.mu.Lock()
	r.errs = nil
	r.mu.Unlock()
	assert.True(t, l1.Step(context.Background()), "tomorrow's render")
	settles := []time.Duration{}
	for _, c := range r.calls {
		settles = append(settles, c.settle)
	}
	assert.Equal(t, []time.Duration{time.Millisecond, DefaultConfirmSettle, DefaultConfirmSettle}, settles,
		"the render after a stalled confirmation is still the long confirmation")
	rec, err = store.Get("https://o/slow")
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State, "a long blank after a short blank concludes")
}

// F23 (round 2): an L0 success refreshes a suspect record's L0 metadata but
// never its state; a play success or an L1 render heals it, an L0 failure
// still outranks it.
func TestApplyProbe_AliveKeepsSuspect(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	src := "https://o/html"
	seedSoftware(t, store, src)
	_, err := store.ApplyRender(src, RenderResult{Verdict: RenderKnownBad, PHash: 3, Variance: 0.1, Label: "kubo-500"})
	require.NoError(t, err)
	clock.Advance(RecheckOK + time.Second)

	rec, err := store.ApplyProbe(src, ProbeResult{Verdict: VerdictAlive, Status: 200, Declared: "text/html", Sniffed: "text/html"})
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State, "valid bytes do not heal a render verdict")
	assert.Equal(t, LayerL1, rec.Layer)
	assert.Equal(t, ReasonRenderKnownBad, rec.Reason)
	assert.Equal(t, 200, rec.HTTPStatus, "but the L0 metadata is refreshed")
	assert.WithinDuration(t, clock.Now().Add(RecheckOK), rec.RecheckAfter, time.Second, "and so is the L0 recheck")
	assert.Equal(t, RenderKnownBad, rec.L1.Verdict)

	// A play that proves a render (readyIsEvidence); an iframe's ready would
	// not (TestL1_PlaySuccessResetsTheBlankRun).
	rec, err = store.ApplyOutcome(Outcome{Seq: 1, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "model", At: "t"}, true)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "a real play heals it")

	_, err = store.ApplyRender(src, RenderResult{Verdict: RenderBlank})
	require.NoError(t, err)
	_, err = store.ApplyRender(src, RenderResult{Verdict: RenderBlank})
	require.NoError(t, err)
	rec, err = store.ApplyProbe(src, ProbeResult{Verdict: VerdictDead, Status: 404, Reason: ReasonHTTPStatus})
	require.NoError(t, err)
	assert.Equal(t, StateBroken, rec.State, "an L0 failure still outranks suspect")
}

// F25 (round 3): a successful play ends an L1 blank run, so a recovered
// work needs a fresh long confirmation before it can be suspect again.
// "Successful" is a play that proves a render (readyIsEvidence): an
// iframe's ready fires for a blank frame too, so it neither heals the
// suspect verdict nor ends the run — for an HTML work only the next L1
// render can.
func TestL1_PlaySuccessResetsTheBlankRun(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	src := "https://o/html"
	seedSoftware(t, store, src)
	for i := 0; i < 2; i++ {
		_, err := store.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
		require.NoError(t, err)
	}
	rec, err := store.Get(src)
	require.NoError(t, err)
	require.Equal(t, StateSuspect, rec.State)

	rec, err = store.ApplyOutcome(Outcome{Seq: 1, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "iframe", At: "t"}, true)
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State, "an iframe ready proves the mount, not the render")
	assert.Equal(t, 2, rec.L1.ConsecutiveBlank, "and does not end the blank run")

	rec, err = store.ApplyOutcome(Outcome{Seq: 2, Source: src, Kind: OutcomeOK, Stage: StageReady, MediaType: "model", At: "t"}, true)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "a play that proves a render heals")
	assert.Equal(t, 0, rec.L1.ConsecutiveBlank, "the blank run ended with the play")

	// The next due render: a short blank only schedules a confirmation.
	clock.Advance(RecheckL1Suspect + time.Second)
	r := &fakeRenderer{frames: map[string]*Frame{src: {Image: uniform(color.Black), MainStatus: 200}}}
	asleep := newFakeSleep(clock)
	asleep.Store(true)
	l1 := newTestL1(t, store, r, clock, asleep)
	assert.True(t, l1.Step(context.Background()))
	assert.Equal(t, time.Millisecond, r.calls[0].settle, "a fresh render, not a confirmation")
	rec, err = store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "one short blank cannot reverse the recovery")
	assert.Equal(t, 1, rec.L1.ConsecutiveBlank)
	assert.True(t, l1.Step(context.Background()))
	assert.Equal(t, DefaultConfirmSettle, r.calls[1].settle, "then the long confirmation")
	rec, err = store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateSuspect, rec.State)
}

// F27 (round 6): a suspect record's recheck is a recovery probe and gets
// the longer settle, so a work that paints between the two windows heals.
func TestL1_SuspectRecheckUsesTheLongSettle(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	src := "https://o/slow-paint"
	seedSoftware(t, store, src)
	for i := 0; i < 2; i++ {
		_, err := store.ApplyRender(src, RenderResult{Verdict: RenderBlank, Variance: 0.0001})
		require.NoError(t, err)
	}
	rec, err := store.Get(src)
	require.NoError(t, err)
	require.Equal(t, StateSuspect, rec.State)

	// The page paints only under the long window.
	r := &fakeRenderer{frames: map[string]*Frame{}}
	r.frames[src] = &Frame{Image: uniform(color.Black), MainStatus: 200}
	asleep := newFakeSleep(clock)
	asleep.Store(true)
	l1 := NewL1(L1Config{Store: store, Renderer: r, AsleepSince: asleep.Since, Now: clock.Now, Settle: time.Millisecond, ConfirmSettle: 7 * time.Millisecond, GateWatch: time.Millisecond, MinAsleep: -1})
	clock.Advance(RecheckL1Suspect + time.Second)
	r.frames[src] = &Frame{Image: checkerboard(), MainStatus: 200} // what a 7 ms render shows
	assert.True(t, l1.Step(context.Background()))
	require.Len(t, r.calls, 1)
	assert.Equal(t, 7*time.Millisecond, r.calls[0].settle, "a suspect recheck is a recovery probe: the long window")
	rec, err = store.Get(src)
	require.NoError(t, err)
	assert.Equal(t, StateOK, rec.State, "the recovery heals it")
	assert.Equal(t, 0, rec.L1.ConsecutiveBlank)

	// A known-bad suspect record rechecks the same way.
	_, err = store.ApplyRender(src, RenderResult{Verdict: RenderKnownBad, Label: "x"})
	require.NoError(t, err)
	clock.Advance(RecheckL1Suspect + time.Second)
	assert.True(t, l1.Step(context.Background()))
	assert.Equal(t, 7*time.Millisecond, r.calls[1].settle)
}

// The grace period: the panel must have been confirmed asleep for
// MinAsleep before the first render, counted from the first asleep
// sighting; any wake restarts it, so a short manual nap never starts the
// headless browser.
func TestL1_GracePeriodAfterSleep(t *testing.T) {
	clock := &testClock{t: time.Now()}
	store := newL1Store(t, clock)
	seedSoftware(t, store, "https://o/html")
	r := &fakeRenderer{frames: map[string]*Frame{"https://o/html": {Image: checkerboard(), MainStatus: 200}}}
	asleep := newFakeSleep(clock)
	l1 := NewL1(L1Config{Store: store, Renderer: r, AsleepSince: asleep.Since, Now: clock.Now, Settle: time.Millisecond, GateWatch: time.Millisecond, MinAsleep: 2 * time.Minute})

	asleep.Store(true)
	assert.False(t, l1.Step(context.Background()), "just asleep: inside the grace period")
	clock.Advance(90 * time.Second)
	assert.False(t, l1.Step(context.Background()), "still inside it")
	assert.Empty(t, r.calls, "no headless render was started")

	// A nap that ends inside the grace: the clock restarts on the next sleep.
	asleep.Store(false)
	assert.False(t, l1.Step(context.Background()))
	asleep.Store(true)
	assert.False(t, l1.Step(context.Background()), "the new sleep's first sighting starts a new grace")
	clock.Advance(90 * time.Second)
	assert.False(t, l1.Step(context.Background()), "the grace restarted with the new sleep")
	clock.Advance(31 * time.Second)
	assert.True(t, l1.Step(context.Background()), "past the grace: the render runs")
	assert.Len(t, r.calls, 1)

	// F28: a wake and a new sleep BETWEEN two gate checks is a new sleep —
	// the source's stamp moved — so the grace restarts although the runner
	// never saw the panel awake.
	asleep.Store(false)
	asleep.Store(true)
	assert.False(t, l1.Step(context.Background()), "a sleep that began since the last check has not earned the grace")
	clock.Advance(2*time.Minute + time.Second)
	clock.Advance(RecheckL1OK) // make the record due again
	assert.True(t, l1.Step(context.Background()))

	// The default applies when unset.
	assert.Equal(t, DefaultMinAsleep, NewL1(L1Config{}).cfg.MinAsleep)
}
