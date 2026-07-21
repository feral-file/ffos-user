package offlinecache

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestWaitForObservationWindow_CtxCancellationWinsRegardlessOfSelectBranch
// pins waitForObservationWindow's core guarantee: once ctx is canceled,
// the function must return ctx.Err() even in the case where its internal
// select resolves via the navCtx.Done() branch rather than the
// ctx.Done() branch — both are legal outcomes once both channels are
// ready, and Capture's real ctx/navCtx pair (navCtx derived from ctx via
// context.WithTimeout) can hit either one depending on exact goroutine
// scheduling (see capture_test.go's
// TestCapturer_Capture_ParentCancellationAfterNavigateAbortsWithoutSaving
// for the black-box version of this same hazard).
//
// ctx and navCtx here are deliberately two INDEPENDENTLY canceled
// contexts, not one derived from the other: canceling both before ever
// calling waitForObservationWindow guarantees the select's poll phase
// (not its blocking/park phase) resolves it, and the poll phase picks
// uniformly at random among every already-ready case — reliably
// exercising both branches across iterations without depending on any
// timing-sensitive goroutine race. A version of this helper that
// returned nil/no-error from the navCtx.Done() branch (rather than
// always returning ctx.Err() after the select, irrespective of which
// case fired) would fail roughly half of these iterations.
func TestWaitForObservationWindow_CtxCancellationWinsRegardlessOfSelectBranch(t *testing.T) {
	const iterations = 200
	for i := 0; i < iterations; i++ {
		ctx, cancel := context.WithCancel(context.Background())
		navCtx, navCancel := context.WithCancel(context.Background())
		cancel()
		navCancel()

		err := waitForObservationWindow(ctx, navCtx)
		require.ErrorIs(t, err, context.Canceled, "iteration %d", i)
	}
}

// TestWaitForObservationWindow_NavCtxDeadlineWithoutParentCancellationSucceeds
// pins the ordinary, non-canceled path: navCtx elapsing on its own
// (the normal end of the observation window) must return a nil error so
// Capture proceeds to resolveResources/SaveItem as usual.
func TestWaitForObservationWindow_NavCtxDeadlineWithoutParentCancellationSucceeds(t *testing.T) {
	ctx := context.Background()
	navCtx, navCancel := context.WithCancel(context.Background())
	navCancel()

	require.NoError(t, waitForObservationWindow(ctx, navCtx))
}
