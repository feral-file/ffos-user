package offlinecache

import (
	"context"
	"encoding/json"
	"fmt"
	go_io "io"
	go_http "net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
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

// countingHTTPClient is a minimal hand-rolled wrapper.HTTPClient fake
// (not a gomock mock: this file is package offlinecache, and mocks
// imports offlinecache, so importing mocks here would be a cycle).
// doFunc lets each test script exactly what a fetch should do (succeed
// immediately, sleep to simulate a slow origin, or fail the test if
// called at all).
type countingHTTPClient struct {
	doFunc func(req *go_http.Request) (*go_http.Response, error)
	calls  int
}

func (c *countingHTTPClient) NewRequest(method, url string, body go_io.Reader) (*go_http.Request, error) {
	return go_http.NewRequest(method, url, body)
}
func (c *countingHTTPClient) Do(req *go_http.Request) (*go_http.Response, error) {
	c.calls++
	return c.doFunc(req)
}
func (c *countingHTTPClient) Get(url string) (*go_http.Response, error) { return nil, nil }
func (c *countingHTTPClient) Post(url, contentType string, body go_io.Reader) (*go_http.Response, error) {
	return nil, nil
}

// TestResolveResources_ExpiredFinalizeContextSkipsEveryFetchWithoutNetworkCalls
// is the regression test for the P1 finding that finalization had no
// total deadline (see captureFinalizeWindowDefault's doc): with an
// already-expired ctx (standing in for captureFinalizeWindowDefault
// having elapsed), resolveResources must never attempt ANY of the
// remaining resources' network fetches, no matter how many are still
// outstanding — proving the loop's cost after the deadline is O(remaining
// resources) in pure bookkeeping, not O(remaining resources) in blocking
// network calls. This is what actually bounds capture's single worker
// slot: without this short-circuit, a page with many stalled resources
// could keep the worker busy for (stalled count) x (up to the shared
// HTTP client's own 30s timeout) after the deadline, well beyond the
// intended cap, starving every other queued download behind it.
func TestResolveResources_ExpiredFinalizeContextSkipsEveryFetchWithoutNetworkCalls(t *testing.T) {
	client := &countingHTTPClient{
		doFunc: func(*go_http.Request) (*go_http.Response, error) {
			t.Fatal("Do must never be called once the finalize deadline has already elapsed")
			return nil, nil
		},
	}
	c := &capturer{httpClient: client, logger: zaptest.NewLogger(t)}

	tracker := newCaptureTracker()
	const resourceCount = 5
	for i := 0; i < resourceCount; i++ {
		tracker.recordResource(fmt.Sprintf("https://example.com/r%d.bin", i), go_http.StatusOK, "application/octet-stream", "", nil, go_http.MethodGet)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // stands in for captureFinalizeWindowDefault having already elapsed

	resources, coverage := c.resolveResources(ctx, tracker, newCaptureDiskBudget(0, true))

	require.Len(t, resources, resourceCount)
	for _, res := range resources {
		assert.Empty(t, res.SHA256, "a resource skipped by the finalize deadline must never have a blob written for it")
	}
	assert.False(t, coverage.Complete)
	assert.Equal(t, 0, client.calls, "no fetch may be attempted once the finalize deadline has elapsed")
	for i := 0; i < resourceCount; i++ {
		assert.Contains(t, coverage.Reason, fmt.Sprintf("finalization_deadline_exceeded:https://example.com/r%d.bin", i))
	}
}

// TestResolveResources_DeadlineExpiringMidFinalizationPreservesEarlierFetches
// pins the other half of the same fix: the finalize deadline is checked
// once per resource, at the START of that resource's turn — a resource
// that already finished fetching before the deadline elapsed keeps its
// result; only a LATER resource, whose turn has not started by the time
// the deadline elapses, is skipped. This is what lets an otherwise-fast
// capture still save everything it already fetched (an explicitly
// PARTIAL record, not a fully-empty one) when only a LATER resource is
// the slow/stalled one.
//
// This deliberately does NOT model the deadline elapsing WHILE fastURL's
// own fetch is still in flight: a real net/http request passed
// req.WithContext(ctx) (see fetchAndStoreBody) is itself torn down by the
// transport once ctx's deadline fires, so a genuinely in-flight fetch
// straddling the deadline would typically fail via the ordinary
// fetch_failed path, not succeed — that is unaffected, pre-existing
// behavior this fix does not change. What this test isolates is
// resolveResources' OWN per-iteration gate: fastURL's fetch call
// completes and returns success first (well inside the deadline), and
// only THEN does the test cancel ctx directly (rather than racing a real
// sleep against a real timer, which would make the exact iteration count
// after cancellation execution-order-dependent, and flaky) — guaranteeing
// deterministically that slowURL's turn starts strictly after
// cancellation, with no reliance on wall-clock timing at all.
func TestResolveResources_DeadlineExpiringMidFinalizationPreservesEarlierFetches(t *testing.T) {
	fastURL := "https://example.com/fast.bin"
	slowURL := "https://example.com/slow.bin" // never reached: skipped once ctx is canceled

	ctx, cancel := context.WithCancel(context.Background())
	client := &countingHTTPClient{
		doFunc: func(req *go_http.Request) (*go_http.Response, error) {
			resp := &go_http.Response{
				StatusCode: go_http.StatusOK,
				Body:       go_io.NopCloser(strings.NewReader("payload")),
			}
			// fastURL's fetch has already fully succeeded at this point
			// (the response above is what fetchAndStoreBody will read
			// and store) — canceling here, AFTER building that result,
			// simulates the finalize deadline elapsing at the exact
			// instant between fastURL's turn ending and slowURL's turn
			// beginning, without any real sleep/timer race.
			cancel()
			return resp, nil
		},
	}

	store := NewStore(t.TempDir(), wrapper.NewOS(), wrapper.NewJSON(), zaptest.NewLogger(t))
	c := &capturer{httpClient: client, store: store, logger: zaptest.NewLogger(t)}

	tracker := newCaptureTracker()
	tracker.recordResource(fastURL, go_http.StatusOK, "application/octet-stream", "", nil, go_http.MethodGet)
	tracker.recordResource(slowURL, go_http.StatusOK, "application/octet-stream", "", nil, go_http.MethodGet)

	resources, coverage := c.resolveResources(ctx, tracker, newCaptureDiskBudget(0, true))

	require.Len(t, resources, 2)
	byURL := make(map[string]Resource, len(resources))
	for _, res := range resources {
		byURL[res.URL] = res
	}
	assert.NotEmpty(t, byURL[fastURL].SHA256, "a fetch that already succeeded before cancellation must still be saved")
	assert.Empty(t, byURL[slowURL].SHA256, "a resource whose turn had not started when ctx was canceled must be skipped")
	assert.False(t, coverage.Complete)
	assert.Contains(t, coverage.Reason, "finalization_deadline_exceeded:"+slowURL)
	assert.NotContains(t, coverage.Reason, "finalization_deadline_exceeded:"+fastURL)
	assert.Equal(t, 1, client.calls, "only fastURL should ever be fetched; slowURL's turn starts strictly after cancellation")
}

// fakeCDPSession is a minimal hand-rolled CDPSession fake (package
// offlinecache; importing mocks here would be a cycle — same reason
// countingHTTPClient above is hand-rolled rather than a gomock mock).
// Send records every Storage.clearDataForOrigin call's origin, in call
// order, and runs onSend (if set) before returning — letting a test
// react to (e.g. cancel a context after) a specific call, the same
// pattern countingHTTPClient.doFunc uses for fetchAndStoreBody above.
type fakeCDPSession struct {
	sentOrigins []string
	onSend      func(ctx context.Context, method string, params map[string]interface{})
}

func (f *fakeCDPSession) Send(ctx context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	if method == "Storage.clearDataForOrigin" {
		if origin, ok := params["origin"].(string); ok {
			f.sentOrigins = append(f.sentOrigins, origin)
		}
	}
	if f.onSend != nil {
		f.onSend(ctx, method, params)
	}
	return json.RawMessage(`{}`), nil
}
func (f *fakeCDPSession) On(string, func(json.RawMessage)) {}
func (f *fakeCDPSession) ForSession(string) CDPSession     { return f }
func (f *fakeCDPSession) Close() error                     { return nil }

// TestClearObservedOriginsStorage_ClearsEveryDistinctOrigin is the
// baseline companion to the deadline test below: with no deadline
// pressure at all, every distinct origin among resources must still get
// its own Storage.clearDataForOrigin call — pinned here so a future
// change to the aggregate-bound logic cannot accidentally skip origins
// unconditionally instead of only once the deadline actually elapses.
func TestClearObservedOriginsStorage_ClearsEveryDistinctOrigin(t *testing.T) {
	c := &capturer{logger: zaptest.NewLogger(t)}
	session := &fakeCDPSession{}

	resources := []Resource{
		{URL: "https://a.example.com/1.bin"},
		{URL: "https://a.example.com/2.bin"}, // same origin as above — must be deduped
		{URL: "https://b.example.com/1.bin"},
		{URL: "not a url"}, // unparsable; must be skipped, not fail the whole call
	}

	c.clearObservedOriginsStorage(context.Background(), session, resources)

	assert.ElementsMatch(t, []string{"https://a.example.com", "https://b.example.com"}, session.sentOrigins)
}

// TestClearObservedOriginsStorage_AggregateDeadlineSkipsRemainingOrigins
// is the regression test for the P1 finding that clearObservedOriginsStorage's
// per-origin cleanup loop had no aggregate deadline: each
// Storage.clearDataForOrigin call is individually bounded by CDPSession's
// own per-send timeout, but an artwork whose navigation/redirects/
// subresources touched MANY distinct origins could still cost
// (origin count) * that per-call timeout in the worst case, monopolizing
// the single capture worker slot for far longer than intended — see
// clearObservedOriginsStorageWindow's doc.
//
// Rather than waiting out the real 30s constant, the fake session's own
// first Send call cancels the ctx passed into clearObservedOriginsStorage
// — exactly like
// TestResolveResources_DeadlineExpiringMidFinalizationPreservesEarlierFetches
// above simulates "the deadline already elapsed" deterministically,
// without any real sleep/timer race. context.WithTimeout's child
// deadline can only be EARLIER than (or equal to) its parent's, so
// canceling this test's parent ctx exercises the exact same ctx.Err()
// short-circuit a real elapsed clearObservedOriginsStorageWindow would
// — proving the bound is enforced via ctx propagation, not merely
// documented.
func TestClearObservedOriginsStorage_AggregateDeadlineSkipsRemainingOrigins(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	session := &fakeCDPSession{
		onSend: func(context.Context, string, map[string]interface{}) {
			// Cancel after the first call is recorded, so every
			// remaining origin's turn observes an already-done
			// context — proving the loop's own ctx.Err() check, not
			// luck, is what stops it early.
			cancel()
		},
	}

	c := &capturer{logger: zaptest.NewLogger(t)}

	const originCount = 5
	resources := make([]Resource, 0, originCount)
	for i := 0; i < originCount; i++ {
		resources = append(resources, Resource{URL: fmt.Sprintf("https://origin-%d.example.com/asset.bin", i)})
	}

	c.clearObservedOriginsStorage(ctx, session, resources)

	assert.Len(t, session.sentOrigins, 1,
		"only the first origin's clear should run once the aggregate deadline (bounded here by the canceled parent ctx) has elapsed")
}
