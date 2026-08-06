package offlinecache

import (
	"context"
	"io"
	"time"
)

// Both capture paths stream artwork bodies with an HTTP client that has
// no whole-request timeout (see bootstrap.go's bodyClient), because
// http.Client.Timeout covers the response body and so kills any large
// asset outright. These two bounds are what replaces it. They are vars,
// not consts, purely so tests can compress the schedule — the same
// convention hub.go's listenRetryBase and cdpsession.go's
// newCDPSessionWithTimeout already use.
var (
	// resourceStallTimeout aborts a transfer that stops making progress.
	// This is the bound that actually matters: a wedged origin is
	// detected in a minute regardless of how large the asset is, so the
	// absolute ceiling below never has to be tight enough to double as a
	// stall detector — which is exactly the conflation that made a
	// single fixed deadline unable to serve both jobs.
	resourceStallTimeout = 60 * time.Second

	// resourceTransferTimeout is the absolute ceiling on one body
	// transfer, so a byte-per-second trickle that never technically
	// stalls still cannot run forever. Sized for what this subsystem
	// carries: ~1 GB at roughly 5 Mbps, the slow end of a real device
	// uplink, which covers the 1.1 GB video docs/offline-artwork-capture.md
	// §4.4 documents as a supported case on BOTH paths (it is a <video>
	// element inside a software artwork, fetched out-of-band by
	// capture.go, and the same shape the direct-media path downloads
	// whole).
	resourceTransferTimeout = 30 * time.Minute
)

// resourceTransfer bounds ONE body transfer, independently of whatever
// phase-level deadline the caller is also working under.
//
// The split exists because a single deadline cannot do both jobs. The
// software path's finalization window has to stop a page whose
// observation window closed with many stalled resources from
// monopolizing the single capture worker — but when that same deadline
// also bounded each individual transfer, a healthy multi-hundred-MB
// asset was canceled mid-stream at the phase limit and recorded as an
// honest-but-permanent partial, no matter how fast it was actually
// downloading. Here the phase deadline gates whether another fetch may
// START (see resolveResources), while progress-awareness bounds the
// transfer itself.
type resourceTransfer struct {
	ctx    context.Context
	cancel context.CancelFunc
	timer  *time.Timer
}

// beginResourceTransfer derives the context one request should be issued
// with. Always paired with a deferred Close.
func beginResourceTransfer(parent context.Context) *resourceTransfer {
	ctx, cancel := context.WithTimeout(parent, resourceTransferTimeout)
	return &resourceTransfer{ctx: ctx, cancel: cancel}
}

// Context is what the request must be issued with, so cancellation of
// either bound (or of the caller's own parent context) aborts the
// in-flight read.
func (t *resourceTransfer) Context() context.Context { return t.ctx }

// Body wraps a response body so the transfer is canceled once it goes
// resourceStallTimeout without delivering any bytes. Call once, on the
// body actually being streamed to the store.
func (t *resourceTransfer) Body(body io.Reader) io.Reader {
	t.timer = time.AfterFunc(resourceStallTimeout, t.cancel)
	return &stallGuardedReader{inner: body, timer: t.timer}
}

// Close stops the stall timer and releases the context. Safe to call
// whether or not Body was ever called.
func (t *resourceTransfer) Close() {
	if t.timer != nil {
		t.timer.Stop()
	}
	t.cancel()
}

// stallGuardedReader restarts the stall timer on every read that
// actually delivers bytes, so "slow" and "stalled" stay distinguishable:
// a 1 GB body arriving steadily never trips it, while an origin that
// accepts the connection and then goes quiet does, promptly, regardless
// of how much of the body already arrived.
type stallGuardedReader struct {
	inner io.Reader
	timer *time.Timer
}

func (r *stallGuardedReader) Read(p []byte) (int, error) {
	n, err := r.inner.Read(p)
	if n > 0 {
		// Racing the timer's own expiry is benign and deliberately not
		// synchronized against: if it already fired, the context is
		// canceled and this read (or the next) fails, which is the
		// outcome we want anyway. Rescheduling a fired timer here only
		// ever delays a cancellation that has already taken effect.
		r.timer.Reset(resourceStallTimeout)
	}
	return n, err
}
