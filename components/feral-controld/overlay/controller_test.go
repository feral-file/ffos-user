package overlay

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePainter records every delivered paint and hide in order. A paint that
// overlaps another one is counted, so serialization can be asserted.
type fakePainter struct {
	mu       sync.Mutex
	events   []string
	inFlight atomic.Int32
	overlaps atomic.Int32
	failNext error
}

func (p *fakePainter) Show(_ context.Context, o Overlay) error {
	if p.inFlight.Add(1) > 1 {
		p.overlaps.Add(1)
	}
	defer p.inFlight.Add(-1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failNext != nil {
		err := p.failNext
		p.failNext = nil
		return err
	}
	p.events = append(p.events, "show:"+string(o.Kind))
	return nil
}

func (p *fakePainter) Hide(_ context.Context, o Overlay) error {
	if p.inFlight.Add(1) > 1 {
		p.overlaps.Add(1)
	}
	defer p.inFlight.Add(-1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "hide:"+string(o.Kind))
	return nil
}

func (p *fakePainter) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
}

// waitForEvents blocks until the painter has recorded at least n deliveries.
// Delivery is asynchronous (see the package doc), so every test that expects
// a paint to land waits for it rather than asserting immediately.
func (p *fakePainter) waitForEvents(t *testing.T, n int) {
	t.Helper()
	require.Eventually(t, func() bool { return len(p.snapshot()) >= n }, time.Second, time.Millisecond)
}

// recordingListener counts terminal callbacks for one owner.
type recordingListener struct {
	overrides atomic.Int32
	closes    atomic.Int32
	// onOverride, when set, runs inside the callback so a test can call back
	// into the controller.
	onOverride func()
}

func (l *recordingListener) OnOverride(Overlay) {
	l.overrides.Add(1)
	if l.onOverride != nil {
		l.onOverride()
	}
}

func (l *recordingListener) OnClose() { l.closes.Add(1) }

const (
	kindClaimQR     Kind = "claim_qr"
	kindPairingCode Kind = "pairing_code"
)

func newController(t *testing.T, p Painter) *Controller {
	t.Helper()
	return New(p)
}

func TestShow_FirstOverlayIsCurrent(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	l := &recordingListener{}

	h, _, err := c.Show(context.Background(), l, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)

	cur, ok := c.Current()
	require.True(t, ok)
	assert.Equal(t, kindPairingCode, cur.Kind)
	assert.True(t, c.IsCurrent(h))
	p.waitForEvents(t, 1)
	assert.Equal(t, []string{"show:pairing_code"}, p.snapshot())
	assert.Zero(t, l.overrides.Load())
}

func TestShow_OwnerReplacesCurrentAndNotifiesPreviousOnce(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	pairing := &recordingListener{}
	claim := &recordingListener{}

	_, _, err := c.Show(context.Background(), pairing, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, _, err = c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	assert.Equal(t, int32(1), pairing.overrides.Load(), "the replaced owner is notified once")
	assert.Zero(t, claim.overrides.Load())
	cur, ok := c.Current()
	require.True(t, ok)
	assert.Equal(t, kindClaimQR, cur.Kind)
}

func TestShow_AutomaticOverCurrentIsRejectedAndSilent(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	claim := &recordingListener{}
	auto := &recordingListener{}

	_, _, err := c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	p.waitForEvents(t, 1)

	_, _, err = c.Show(context.Background(), auto, Overlay{Kind: kindPairingCode}, Automatic)

	assert.ErrorIs(t, err, ErrRejected)
	assert.Zero(t, claim.overrides.Load(), "a rejected automatic show must not disturb the current owner")
	assert.Zero(t, auto.overrides.Load())
	assert.Equal(t, []string{"show:claim_qr"}, p.snapshot(), "a rejected show must not paint")
	cur, _ := c.Current()
	assert.Equal(t, kindClaimQR, cur.Kind)
}

func TestShow_AutomaticWhenNothingCurrentIsAccepted(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	h, _, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindPairingCode}, Automatic)

	require.NoError(t, err)
	assert.True(t, c.IsCurrent(h))
}

func TestHide_OnlyTheCurrentOwnerCanHide(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	pairing := &recordingListener{}
	claim := &recordingListener{}

	old, _, err := c.Show(context.Background(), pairing, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, _, err = c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	p.waitForEvents(t, 2)

	err = c.Hide(context.Background(), old)

	assert.ErrorIs(t, err, ErrNotCurrent)
	assert.Equal(t, []string{"show:pairing_code", "show:claim_qr"}, p.snapshot(), "a replaced owner must send no hide")
}

func TestHide_CurrentOwnerClearsTheScreen(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	l := &recordingListener{}

	h, _, err := c.Show(context.Background(), l, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	require.NoError(t, c.Hide(context.Background(), h))

	_, ok := c.Current()
	assert.False(t, ok)
	p.waitForEvents(t, 2)
	assert.Equal(t, []string{"show:claim_qr", "hide:claim_qr"}, p.snapshot())
	assert.Zero(t, l.overrides.Load(), "the owner ending its own overlay is not an override")
}

func TestHide_SameHandleTwiceIsNotCurrentTheSecondTime(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	h, _, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	require.NoError(t, c.Hide(context.Background(), h))

	assert.ErrorIs(t, c.Hide(context.Background(), h), ErrNotCurrent)
}

// A failed delivery is logged by the painter and never retried; the decision
// that was already made — the overlay is current — is not rolled back. See
// the package doc on decision versus delivery.
func TestShow_FailedDeliveryStillCommitsAndIsNotRetried(t *testing.T) {
	p := &fakePainter{failNext: errors.New("cdp send failed")}
	c := newController(t, p)
	claim := &recordingListener{}

	h, _, err := c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err, "Show decides ownership; it does not wait on the painter")

	assert.True(t, c.IsCurrent(h))
	cur, ok := c.Current()
	require.True(t, ok)
	assert.Equal(t, kindClaimQR, cur.Kind)

	// The failed delivery leaves nothing in the painter's log and is not retried.
	time.Sleep(20 * time.Millisecond)
	assert.Empty(t, p.snapshot())
}

// A callback runs after the controller has released its lock, so it can call
// back into the controller without deadlocking.
func TestOverrideCallback_MayCallBackIntoController(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	pairing := &recordingListener{}
	var seen Overlay
	var seenOK bool
	pairing.onOverride = func() {
		seen, seenOK = c.Current()
	}

	_, _, err := c.Show(context.Background(), pairing, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, _, err = c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	assert.True(t, seenOK)
	assert.Equal(t, kindClaimQR, seen.Kind, "the callback sees the overlay that replaced it")
}

// Deliveries are serialized by the controller's own worker: concurrent shows
// never reach the painter at once, and every DISTINCT kind is eventually
// delivered exactly once. (Same-kind shows coalesce to the trailing one by
// design — see TestDelivery_CoalescesTrailingSameKindButKeepsDistinctStates —
// so this test uses one kind per show to isolate the serialization claim.)
func TestPaints_AreSerialized(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	// Fewer than maxPendingDeliveries: this test is about concurrent safety, not
	// the queue's drop-oldest leak guard (see TestDelivery_DropsOldestBeyondBound).
	const n = 10
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _, _ = c.Show(context.Background(), &recordingListener{}, Overlay{Kind: Kind(fmt.Sprintf("kind-%02d", i))}, Owner)
		}(i)
	}
	wg.Wait()

	p.waitForEvents(t, n)
	assert.Zero(t, p.overlaps.Load(), "the painter must never see two deliveries at once")
	assert.Len(t, p.snapshot(), n)
}

// Beyond the bound, the queue drops the oldest pending delivery rather than
// growing without limit — the correct staleness policy for a courtesy
// overlay, carried over from setupui's own pre-controller queue.
func TestDelivery_DropsOldestBeyondBound(t *testing.T) {
	p := &fakePainter{}
	release := make(chan struct{})
	blocking := &blockingPainter{inner: p, release: release, entered: make(chan struct{})}
	c := New(blocking)
	l := &recordingListener{}

	// One delivery is popped and blocked in flight; the rest queue up behind it.
	_, _, err := c.Show(context.Background(), l, Overlay{Kind: "kind-first"}, Owner)
	require.NoError(t, err)
	<-blocking.entered

	for i := 0; i < maxPendingDeliveries+5; i++ {
		_, _, err := c.Show(context.Background(), l, Overlay{Kind: Kind(fmt.Sprintf("kind-%02d", i))}, Owner)
		require.NoError(t, err)
	}

	close(release)
	p.waitForEvents(t, 1+maxPendingDeliveries)
	events := p.snapshot()
	assert.Len(t, events, 1+maxPendingDeliveries, "the oldest queued entries beyond the bound were dropped")
	assert.Equal(t, "show:kind-first", events[0], "the in-flight delivery was not affected by the bound")
}

// The owner's own Show over itself is a replacement: the previous handle is
// overridden exactly once, even when the same listener shows twice.
func TestShow_SameOwnerReplacingItselfIsNotAnOverride(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	l := &recordingListener{}

	_, _, err := c.Show(context.Background(), l, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, _, err = c.Show(context.Background(), l, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	assert.Zero(t, l.overrides.Load())
	cur, _ := c.Current()
	assert.Equal(t, kindClaimQR, cur.Kind)
}

// ShowIf checks its condition under the decision lock and queues nothing when
// the condition fails.
func TestShowIf_ConditionFailureRejectsAndPaintsNothing(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	_, _, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	p.waitForEvents(t, 1)

	_, _, err = c.ShowIf(context.Background(), &recordingListener{}, Overlay{Kind: kindPairingCode}, Owner,
		func(cur Overlay, ok bool) bool { return !ok })

	assert.ErrorIs(t, err, ErrRejected)
	assert.Equal(t, []string{"show:claim_qr"}, p.snapshot())
}

func TestShowIf_ConditionSeesCurrentOverlay(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	_, _, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	var seen Kind
	_, _, err = c.ShowIf(context.Background(), &recordingListener{}, Overlay{Kind: kindPairingCode}, Owner,
		func(cur Overlay, ok bool) bool {
			seen = cur.Kind
			return ok
		})

	require.NoError(t, err)
	assert.Equal(t, kindClaimQR, seen)
}

// HideIf clears only when its condition holds on the current overlay.
func TestHideIf_ClearsOnlyWhenConditionHolds(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	_, _, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	err = c.HideIf(context.Background(), func(cur Overlay, ok bool) bool { return ok && cur.Kind == kindPairingCode })
	assert.ErrorIs(t, err, ErrNotCurrent)

	err = c.HideIf(context.Background(), func(cur Overlay, ok bool) bool { return ok && cur.Kind == kindClaimQR })
	require.NoError(t, err)
	p.waitForEvents(t, 2)
	assert.Equal(t, []string{"show:claim_qr", "hide:claim_qr"}, p.snapshot())
}

// A condition that only passes on a present overlay is ErrNotCurrent when
// nothing is current — there is no bypass for "nothing is current" (see
// TestHide_SameHandleTwiceIsNotCurrentTheSecondTime).
func TestHideIf_NothingCurrentIsErrNotCurrent(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	err := c.HideIf(context.Background(), func(_ Overlay, ok bool) bool { return ok })
	assert.ErrorIs(t, err, ErrNotCurrent)
	assert.Empty(t, p.snapshot())
}

// Hide passes the overlay being cleared to the painter, so a painter that
// serves several kinds sends the right clear.
func TestHide_PainterReceivesTheOverlayBeingCleared(t *testing.T) {
	p := &kindRecordingPainter{}
	c := New(p)

	h, _, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	require.NoError(t, c.Hide(context.Background(), h))

	require.Eventually(t, func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return len(p.hidden) == 1
	}, time.Second, time.Millisecond)
	p.mu.Lock()
	defer p.mu.Unlock()
	assert.Equal(t, []Kind{kindClaimQR}, p.hidden)
}

type kindRecordingPainter struct {
	mu     sync.Mutex
	hidden []Kind
}

func (p *kindRecordingPainter) Show(context.Context, Overlay) error { return nil }

func (p *kindRecordingPainter) Hide(_ context.Context, o Overlay) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.hidden = append(p.hidden, o.Kind)
	return nil
}

// A burst of the same kind, still sitting in the queue, collapses to its
// trailing payload; a distinct kind queued behind it still delivers on its
// own — the coalescing rule the queue took over from setupui's own,
// pre-controller queue. An item already popped and in flight is a separate
// delivery, never merged away — see TestDelivery_DropsOldestBeyondBound for
// the queue's other edge, the bound.
func TestDelivery_CoalescesTrailingSameKindButKeepsDistinctStates(t *testing.T) {
	p := &fakePainter{}
	release := make(chan struct{})
	blocking := &blockingPainter{inner: p, release: release, entered: make(chan struct{})}
	c := New(blocking)
	l := &recordingListener{}

	// The first delivery is popped and blocked in flight before the burst is
	// enqueued, so the burst coalesces in the queue, not against it.
	_, _, err := c.Show(context.Background(), l, Overlay{Kind: kindPairingCode, Payload: "first"}, Owner)
	require.NoError(t, err)
	<-blocking.entered

	// These coalesce into the trailing entry under kindPairingCode, mirroring
	// an OTA progress burst.
	_, _, err = c.Show(context.Background(), l, Overlay{Kind: kindPairingCode, Payload: "burst-1"}, Owner)
	require.NoError(t, err)
	_, _, err = c.Show(context.Background(), l, Overlay{Kind: kindPairingCode, Payload: "burst-2"}, Owner)
	require.NoError(t, err)
	// A distinct kind still gets its own delivery, not coalesced away.
	_, _, err = c.Show(context.Background(), l, Overlay{Kind: kindClaimQR, Payload: "claim"}, Owner)
	require.NoError(t, err)

	close(release)
	p.waitForEvents(t, 3)
	assert.Equal(t, []string{"show:pairing_code", "show:pairing_code", "show:claim_qr"}, p.snapshot(),
		"the in-flight first delivery stands on its own; the queued burst collapsed to one; the distinct kind still delivered")
}

// blockingPainter holds the worker on the first Show until release closes, so
// a test can enqueue several deliveries before any of them drain. entered
// closes right as that first call arrives, so a test can wait for the item to
// be popped-and-in-flight before enqueueing more, making the race deterministic.
type blockingPainter struct {
	inner   Painter
	release chan struct{}
	once    sync.Once
	entered chan struct{}
}

func (p *blockingPainter) Show(ctx context.Context, o Overlay) error {
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return p.inner.Show(ctx, o)
}

func (p *blockingPainter) Hide(ctx context.Context, o Overlay) error {
	return p.inner.Hide(ctx, o)
}

// Result.Wait reports the painter's own outcome for the caller's specific
// delivery, success and failure alike, without any controller lock held
// while it blocks.
func TestResultWait_ReportsThePainterOutcome(t *testing.T) {
	ok := &fakePainter{}
	c := New(ok)
	_, result, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	assert.NoError(t, result.Wait(context.Background()))

	failing := &fakePainter{failNext: errors.New("cdp send failed")}
	c = New(failing)
	_, result, err = c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	assert.ErrorContains(t, result.Wait(context.Background()), "cdp send failed")
}

// A delivery that coalescing replaces before the worker ever sends it
// resolves its Result with ErrSuperseded, not a hang — a waiter must not
// block forever on a send that will never happen.
func TestResultWait_SupersededDeliveryResolvesWithErrSuperseded(t *testing.T) {
	p := &fakePainter{}
	release := make(chan struct{})
	blocking := &blockingPainter{inner: p, release: release, entered: make(chan struct{})}
	c := New(blocking)
	l := &recordingListener{}

	_, _, err := c.Show(context.Background(), l, Overlay{Kind: kindPairingCode, Payload: "first"}, Owner)
	require.NoError(t, err)
	<-blocking.entered

	_, superseded, err := c.Show(context.Background(), l, Overlay{Kind: kindPairingCode, Payload: "burst-1"}, Owner)
	require.NoError(t, err)
	_, _, err = c.Show(context.Background(), l, Overlay{Kind: kindPairingCode, Payload: "burst-2"}, Owner)
	require.NoError(t, err)

	assert.ErrorIs(t, superseded.Wait(context.Background()), ErrSuperseded)
	close(release)
}

// The zero Result — from an ErrRejected Show — has nothing to wait for: Wait
// returns immediately rather than blocking on a delivery that was never queued.
func TestResultWait_ZeroResultReturnsImmediately(t *testing.T) {
	var r Result
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already done; a real wait would return ctx.Err() here instead of nil
	assert.NoError(t, r.Wait(ctx))
}

// Round 3 review, F1: an Automatic show may replace its own listener's
// earlier overlay — without this, setupui's ShowFinalizing (Owner) followed
// by the auto-claim loop's ShowClaimQRAutomatic (Automatic) would starve the
// claim QR forever, since both are the same listener and the second call
// would otherwise find "something current" and be rejected.
func TestShow_AutomaticMayReplaceItsOwnListenersEarlierOverlay(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	l := &recordingListener{}

	_, _, err := c.Show(context.Background(), l, Overlay{Kind: "finalizing"}, Owner)
	require.NoError(t, err)

	h, _, err := c.Show(context.Background(), l, Overlay{Kind: kindClaimQR}, Automatic)

	require.NoError(t, err, "the same listener's own Automatic show must not be rejected")
	assert.True(t, c.IsCurrent(h))
	assert.Zero(t, l.overrides.Load(), "replacing its own overlay is not an override")
}

// An Automatic show is still rejected when a DIFFERENT listener owns the
// screen, same listener or not notwithstanding — this is the other half of
// F1's fix, already covered by TestShow_AutomaticOverCurrentIsRejectedAndSilent
// above with two distinct listeners; this test pins the boundary explicitly.
func TestShow_AutomaticStillRejectedByADifferentListener(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	owner := &recordingListener{}
	auto := &recordingListener{}

	_, _, err := c.Show(context.Background(), owner, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	_, _, err = c.Show(context.Background(), auto, Overlay{Kind: kindPairingCode}, Automatic)

	assert.ErrorIs(t, err, ErrRejected)
	cur, _ := c.Current()
	assert.Equal(t, kindClaimQR, cur.Kind)
}
