package offlinecache

import (
	"context"
	go_http "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// compressTransferBounds shrinks the two per-transfer bounds so the
// behaviors below can be pinned in milliseconds instead of the production
// minutes, restoring them afterwards.
func compressTransferBounds(t *testing.T, stall, ceiling time.Duration) {
	t.Helper()
	prevStall, prevCeiling := resourceStallTimeout, resourceTransferTimeout
	resourceStallTimeout, resourceTransferTimeout = stall, ceiling
	t.Cleanup(func() {
		resourceStallTimeout, resourceTransferTimeout = prevStall, prevCeiling
	})
}

// trickleHandler serves a body in chunks separated by gap, flushing each,
// so the response makes steady progress over a predictable wall-clock
// span. stallAfter > 0 makes it go silent (without closing) after that
// many chunks, standing in for an origin that accepts the connection and
// then wedges.
func trickleHandler(t *testing.T, chunks, chunkSize int, gap time.Duration, stallAfter int) (*httptest.Server, string) {
	t.Helper()
	payload := strings.Repeat("x", chunks*chunkSize)
	srv := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		w.WriteHeader(go_http.StatusOK)
		flusher, ok := w.(go_http.Flusher)
		require.True(t, ok)
		for i := 0; i < chunks; i++ {
			if stallAfter > 0 && i == stallAfter {
				<-r.Context().Done() // wedge until the client gives up
				return
			}
			if _, err := w.Write([]byte(payload[i*chunkSize : (i+1)*chunkSize])); err != nil {
				return
			}
			flusher.Flush()
			select {
			case <-r.Context().Done():
				return
			case <-time.After(gap):
			}
		}
	}))
	t.Cleanup(srv.Close)
	return srv, payload
}

// TestResourceTransfer_SlowButProgressingBodyOutlivesTheFinalizeWindow is
// the regression test for the finding that a large software asset could
// never reach ready. The finalization phase's fixed 60s deadline used to
// bound each individual fetch as well as the phase, so a healthy
// multi-hundred-MB transfer was canceled mid-stream at the phase limit
// and recorded as a permanent partial however fast it was downloading —
// while docs/offline-artwork-capture.md §4.4 documents a 1.1 GB video as
// supported on exactly this path.
//
// Driven through resolveResources with a real phase deadline that
// elapses WHILE the transfer is streaming, which is the only arrangement
// that can tell the two designs apart: the phase gate is passed before
// the fetch starts, then expires mid-body.
func TestResourceTransfer_SlowButProgressingBodyOutlivesTheFinalizeWindow(t *testing.T) {
	// Stall allowance comfortably above the inter-chunk gap; ceiling well
	// above the whole transfer.
	compressTransferBounds(t, 200*time.Millisecond, 10*time.Second)

	srv, payload := trickleHandler(t, 12, 64, 30*time.Millisecond, 0) // ~360ms of steady progress
	url := srv.URL + "/big.bin"

	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zaptest.NewLogger(t))
	c := &capturer{httpClient: wrapper.NewHTTPClientWithoutTimeout(), store: store, logger: zaptest.NewLogger(t)}

	tracker := newCaptureTracker()
	tracker.recordResource(url, go_http.StatusOK, "application/octet-stream", "", nil, go_http.MethodGet)

	// Long enough to let the fetch START, far too short to let it finish:
	// under the old design this deadline reached into the transfer and
	// killed it at 100ms.
	phaseCtx, phaseCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer phaseCancel()

	resources, coverage := c.resolveResources(context.Background(), phaseCtx, tracker, newCaptureDiskBudget(0, true))

	require.Len(t, resources, 1)
	// An empty SHA256 here would mean the phase gate skipped the resource
	// outright (finalization_deadline_exceeded) rather than letting the
	// fetch start — so this single assertion covers both "the fetch was
	// allowed to begin" and "the phase deadline did not kill it mid-body".
	require.NotEmpty(t, resources[0].SHA256,
		"a body that keeps delivering bytes must survive the phase deadline elapsing mid-transfer")
	require.NotContains(t, coverage.Reason, "finalization_deadline_exceeded")
	assert.True(t, coverage.Complete, "the item must be able to reach ready, not be recorded a permanent partial")

	blob, err := store.ReadBlob(resources[0].SHA256)
	require.NoError(t, err)
	assert.Equal(t, payload, string(blob), "the whole asset, not a truncated prefix")

	<-phaseCtx.Done() // the phase window really did elapse during the transfer
}

// TestResourceTransfer_StalledBodyIsCutOffPromptly pins the other half:
// progress-awareness is what lets the absolute ceiling be generous
// without a wedged origin holding the single capture worker for it.
func TestResourceTransfer_StalledBodyIsCutOffPromptly(t *testing.T) {
	// A ceiling far longer than the stall allowance, so anything but the
	// stall detector firing would make this test take 10 seconds.
	compressTransferBounds(t, 150*time.Millisecond, 10*time.Second)

	srv, _ := trickleHandler(t, 20, 64, 10*time.Millisecond, 3) // 3 chunks, then silence
	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zaptest.NewLogger(t))
	c := &capturer{httpClient: wrapper.NewHTTPClientWithoutTimeout(), store: store, logger: zaptest.NewLogger(t)}

	start := time.Now()
	_, _, err := c.fetchAndStoreBody(context.Background(), srv.URL+"/wedged.bin", go_http.MethodGet, 0)
	elapsed := time.Since(start)

	require.Error(t, err, "an origin that goes silent mid-body must fail, not hang")
	assert.Less(t, elapsed, 5*time.Second,
		"the stall detector must fire long before the absolute ceiling, or a wedged origin would hold the worker for it")
}

// TestResourceTransfer_CeilingBoundsATrickleThatNeverStalls pins that the
// absolute ceiling still exists: a body creeping along just fast enough
// to keep resetting the stall timer cannot run forever.
func TestResourceTransfer_CeilingBoundsATrickleThatNeverStalls(t *testing.T) {
	compressTransferBounds(t, 500*time.Millisecond, 200*time.Millisecond)

	srv, _ := trickleHandler(t, 100, 8, 20*time.Millisecond, 0) // never stalls, but runs ~2s
	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zaptest.NewLogger(t))
	c := &capturer{httpClient: wrapper.NewHTTPClientWithoutTimeout(), store: store, logger: zaptest.NewLogger(t)}

	start := time.Now()
	_, _, err := c.fetchAndStoreBody(context.Background(), srv.URL+"/trickle.bin", go_http.MethodGet, 0)
	elapsed := time.Since(start)

	require.Error(t, err)
	assert.Less(t, elapsed, 2*time.Second, "the ceiling must bound a never-stalling trickle")
}
