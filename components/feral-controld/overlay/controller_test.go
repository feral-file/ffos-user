package overlay

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fakePainter records every paint and hide in order. A paint that overlaps
// another one is counted, so serialization can be asserted.
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

func (p *fakePainter) Hide(_ context.Context, _ Overlay) error {
	if p.inFlight.Add(1) > 1 {
		p.overlaps.Add(1)
	}
	defer p.inFlight.Add(-1)
	p.mu.Lock()
	defer p.mu.Unlock()
	p.events = append(p.events, "hide")
	return nil
}

func (p *fakePainter) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.events...)
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

	h, err := c.Show(context.Background(), l, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)

	cur, ok := c.Current()
	require.True(t, ok)
	assert.Equal(t, kindPairingCode, cur.Kind)
	assert.True(t, c.IsCurrent(h))
	assert.Equal(t, []string{"show:pairing_code"}, p.snapshot())
	assert.Zero(t, l.overrides.Load())
}

func TestShow_OwnerReplacesCurrentAndNotifiesPreviousOnce(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	pairing := &recordingListener{}
	claim := &recordingListener{}

	_, err := c.Show(context.Background(), pairing, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, err = c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
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

	_, err := c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	_, err = c.Show(context.Background(), auto, Overlay{Kind: kindPairingCode}, Automatic)

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

	h, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindPairingCode}, Automatic)

	require.NoError(t, err)
	assert.True(t, c.IsCurrent(h))
}

func TestHide_OnlyTheCurrentOwnerCanHide(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	pairing := &recordingListener{}
	claim := &recordingListener{}

	old, err := c.Show(context.Background(), pairing, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, err = c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	err = c.Hide(context.Background(), old)

	assert.ErrorIs(t, err, ErrNotCurrent)
	assert.Equal(t, []string{"show:pairing_code", "show:claim_qr"}, p.snapshot(), "a replaced owner must send no hide")
}

func TestHide_CurrentOwnerClearsTheScreen(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	l := &recordingListener{}

	h, err := c.Show(context.Background(), l, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	require.NoError(t, c.Hide(context.Background(), h))

	_, ok := c.Current()
	assert.False(t, ok)
	assert.Equal(t, []string{"show:claim_qr", "hide"}, p.snapshot())
	assert.Zero(t, l.overrides.Load(), "the owner ending its own overlay is not an override")
}

func TestHide_SameHandleTwiceIsNotCurrentTheSecondTime(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	h, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	require.NoError(t, c.Hide(context.Background(), h))

	assert.ErrorIs(t, c.Hide(context.Background(), h), ErrNotCurrent)
}

func TestShow_FailedPaintLeavesStateUnchanged(t *testing.T) {
	p := &fakePainter{failNext: errors.New("cdp send failed")}
	c := newController(t, p)
	claim := &recordingListener{}
	pairing := &recordingListener{}

	_, err := c.Show(context.Background(), claim, Overlay{Kind: kindClaimQR}, Owner)
	require.Error(t, err)

	_, ok := c.Current()
	assert.False(t, ok, "a failed paint must not record an overlay")

	_, err = c.Show(context.Background(), pairing, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	assert.Zero(t, claim.overrides.Load(), "nothing was shown, so nothing was overridden")
}

// A callback runs after the controller has released its locks, so it can call
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

	_, err := c.Show(context.Background(), pairing, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, err = c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	assert.True(t, seenOK)
	assert.Equal(t, kindClaimQR, seen.Kind, "the callback sees the overlay that replaced it")
}

// Paints are serialized: concurrent shows and hides never overlap at the
// painter, and every accepted paint lands in one order.
func TestPaints_AreSerialized(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	var wg sync.WaitGroup
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _ = c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindPairingCode}, Owner)
		}()
	}
	wg.Wait()

	assert.Zero(t, p.overlaps.Load(), "the painter must never see two paints at once")
	assert.Len(t, p.snapshot(), 50)
}

// A listener replacing its own overlay is the same owner moving its display
// along. It is not an override, so it is told nothing.
func TestShow_SameOwnerReplacingItselfIsNotAnOverride(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)
	l := &recordingListener{}

	_, err := c.Show(context.Background(), l, Overlay{Kind: kindPairingCode}, Owner)
	require.NoError(t, err)
	_, err = c.Show(context.Background(), l, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	assert.Zero(t, l.overrides.Load())
	cur, _ := c.Current()
	assert.Equal(t, kindClaimQR, cur.Kind)
}

// ShowIf checks its condition under the paint lock and paints nothing when the
// condition fails.
func TestShowIf_ConditionFailureRejectsAndPaintsNothing(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	_, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	_, err = c.ShowIf(context.Background(), &recordingListener{}, Overlay{Kind: kindPairingCode}, Owner,
		func(cur Overlay, ok bool) bool { return !ok })

	assert.ErrorIs(t, err, ErrRejected)
	assert.Equal(t, []string{"show:claim_qr"}, p.snapshot())
}

func TestShowIf_ConditionSeesCurrentOverlay(t *testing.T) {
	p := &fakePainter{}
	c := newController(t, p)

	_, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	var seen Kind
	_, err = c.ShowIf(context.Background(), &recordingListener{}, Overlay{Kind: kindPairingCode}, Owner,
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

	_, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)

	err = c.HideIf(context.Background(), func(cur Overlay, ok bool) bool { return ok && cur.Kind == kindPairingCode })
	assert.ErrorIs(t, err, ErrNotCurrent)

	err = c.HideIf(context.Background(), func(cur Overlay, ok bool) bool { return ok && cur.Kind == kindClaimQR })
	require.NoError(t, err)
	assert.Equal(t, []string{"show:claim_qr", "hide"}, p.snapshot())
}

// Hide passes the overlay being cleared to the painter, so a painter that
// serves several kinds sends the right clear.
func TestHide_PainterReceivesTheOverlayBeingCleared(t *testing.T) {
	p := &kindRecordingPainter{}
	c := New(p)

	h, err := c.Show(context.Background(), &recordingListener{}, Overlay{Kind: kindClaimQR}, Owner)
	require.NoError(t, err)
	require.NoError(t, c.Hide(context.Background(), h))

	assert.Equal(t, []Kind{kindClaimQR}, p.hidden)
}

type kindRecordingPainter struct {
	hidden []Kind
}

func (p *kindRecordingPainter) Show(context.Context, Overlay) error { return nil }

func (p *kindRecordingPainter) Hide(_ context.Context, o Overlay) error {
	p.hidden = append(p.hidden, o.Kind)
	return nil
}
