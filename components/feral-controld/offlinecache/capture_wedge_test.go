package offlinecache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	go_io "io"
	go_http "net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	c := &capturer{fetchClient: client, logger: zaptest.NewLogger(t)}

	tracker := newCaptureTracker()
	const resourceCount = 5
	for i := 0; i < resourceCount; i++ {
		tracker.recordResource(fmt.Sprintf("https://example.com/r%d.bin", i), go_http.StatusOK, "application/octet-stream", "", nil, go_http.MethodGet)
	}

	phaseCtx, cancel := context.WithCancel(context.Background())
	cancel() // stands in for captureFinalizeWindowDefault having already elapsed

	// The capture's own ctx stays live: the phase deadline is what gates
	// starting fetches now, and it must short-circuit on its own rather
	// than needing the whole capture to be canceled.
	resources, coverage := c.resolveResources(context.Background(), phaseCtx, tracker, newCaptureDiskBudget(0, true))

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

	phaseCtx, cancel := context.WithCancel(context.Background())
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
	c := &capturer{fetchClient: client, store: store, logger: zaptest.NewLogger(t)}

	tracker := newCaptureTracker()
	tracker.recordResource(fastURL, go_http.StatusOK, "application/octet-stream", "", nil, go_http.MethodGet)
	tracker.recordResource(slowURL, go_http.StatusOK, "application/octet-stream", "", nil, go_http.MethodGet)

	resources, coverage := c.resolveResources(context.Background(), phaseCtx, tracker, newCaptureDiskBudget(0, true))

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

// TestCapturer_CDPDiscoveryClientIsNotTheGuardedOne pins the split that a
// regression in this PR made necessary. The source guard exists to keep
// untrusted playlist sources away from loopback — and the capture
// Chromium's own DevTools endpoint IS on loopback (127.0.0.1:9223/json).
// Handing the guarded client to that discovery step therefore refused
// every ClassSoftware item at DialPageSession, before navigation, which
// is exactly what happened when one client was used for both roles.
//
// Asserted as behavior, not wiring: the guarded client must reject a
// loopback DevTools URL (proving the hazard is real) while the client the
// capturer actually uses for discovery must reach it.
func TestCapturer_CDPDiscoveryClientIsNotTheGuardedOne(t *testing.T) {
	devtools := httptest.NewServer(go_http.HandlerFunc(func(w go_http.ResponseWriter, r *go_http.Request) {
		require.Equal(t, "/json", r.URL.Path)
		_, _ = w.Write([]byte(`[{"type":"page","webSocketDebuggerUrl":"ws://127.0.0.1:9223/devtools/page/X"}]`))
	}))
	defer devtools.Close()

	// The guarded client — correctly — refuses our own browser, because
	// it cannot tell a trusted loopback destination from a hostile one.
	// That is why it must never be the discovery client.
	guarded := newGuardedHTTPClient(staticResolver{ip: "93.184.216.34"}, 0)
	req, err := guarded.NewRequest(go_http.MethodGet, devtools.URL+"/json", nil)
	require.NoError(t, err)
	resp, err := guarded.Do(req)
	if resp != nil {
		_ = resp.Body.Close()
	}
	require.Error(t, err, "the guarded client must reject loopback — that is its job")
	assert.ErrorIs(t, err, ErrUnsafeSource)

	// The discovery client production wires in reaches the same endpoint.
	discovery := wrapper.NewHTTPClient()
	req, err = discovery.NewRequest(go_http.MethodGet, devtools.URL+"/json", nil)
	require.NoError(t, err)
	resp, err = discovery.Do(req)
	require.NoError(t, err, "software capture cannot start if discovery is blocked")
	defer func() { _ = resp.Body.Close() }()
	assert.Equal(t, go_http.StatusOK, resp.StatusCode)

	// The two roles live in separate fields on the capturer, so the
	// compiler is what stops one quietly becoming the other again; this
	// only pins that both are actually populated.
	c := &capturer{cdpClient: discovery, fetchClient: guarded}
	require.NotNil(t, c.cdpClient)
	require.NotNil(t, c.fetchClient)
}

// fakePausedSession records Fetch verdicts so the capture-side guard can
// be exercised without a browser. Send is what decidePausedRequest calls
// to answer a paused request.
type fakePausedSession struct {
	mu       sync.Mutex
	sent     []string
	handlers map[string]func(json.RawMessage)
	children map[string]*fakePausedSession
	// gate, when non-nil, holds every Send until it is closed — so a test
	// can observe how many setups are genuinely IN FLIGHT rather than how
	// many ran in total. Inherited by child views.
	gate     chan struct{}
	inFlight *atomic.Int32
	peak     *atomic.Int32
	// failMethod, when set, makes Send return an error for that CDP
	// method — so a test can drive the arming failure paths. Inherited by
	// child views.
	failMethod string
}

func newFakePausedSession() *fakePausedSession {
	return &fakePausedSession{handlers: map[string]func(json.RawMessage){}}
}

func (f *fakePausedSession) Send(_ context.Context, method string, _ map[string]interface{}) (json.RawMessage, error) {
	f.mu.Lock()
	f.sent = append(f.sent, method)
	gate, in, peak, failMethod := f.gate, f.inFlight, f.peak, f.failMethod
	f.mu.Unlock()
	if failMethod != "" && method == failMethod {
		return nil, errors.New("simulated CDP failure for " + method)
	}
	if gate != nil {
		if in != nil {
			cur := in.Add(1)
			for {
				old := peak.Load()
				if cur <= old || peak.CompareAndSwap(old, cur) {
					break
				}
			}
			defer in.Add(-1)
		}
		<-gate
	}
	return json.RawMessage(`{}`), nil
}
func (f *fakePausedSession) On(method string, h func(json.RawMessage)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlers[method] = h
}
func (f *fakePausedSession) Close() error { return nil }

// ForSession returns a DISTINCT child view, so a test can tell whether the
// guard was armed on the child target or only on the root.
func (f *fakePausedSession) ForSession(id string) CDPSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.children == nil {
		f.children = map[string]*fakePausedSession{}
	}
	if c, ok := f.children[id]; ok {
		return c
	}
	c := newFakePausedSession()
	c.gate, c.inFlight, c.peak, c.failMethod = f.gate, f.inFlight, f.peak, f.failMethod
	f.children[id] = c
	return c
}

func (f *fakePausedSession) child(id string) *fakePausedSession {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.children[id]
}

func (f *fakePausedSession) handler(method string) func(json.RawMessage) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.handlers[method]
}

func (f *fakePausedSession) verdicts() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.sent...)
}

// pause drives one Fetch.requestPaused event through the registered
// handler and waits for the asynchronous decision to land. The handler
// hands work to a goroutine by design (CDPSession.On forbids Send on the
// read pump), so the wait is what makes the assertion deterministic.
func (f *fakePausedSession) pause(t *testing.T, url string) string {
	t.Helper()
	h := f.handler("Fetch.requestPaused")
	require.NotNil(t, h, "the guard must register a Fetch.requestPaused handler on this session")
	before := len(f.verdicts())
	h(json.RawMessage(`{"requestId":"r1","request":{"url":"` + url + `"}}`))
	require.Eventually(t, func() bool { return len(f.verdicts()) > before },
		2*time.Second, 5*time.Millisecond, "the paused request was never answered")
	v := f.verdicts()
	return v[len(v)-1]
}

// TestCapturer_SourceGuard_BlocksPageRequestsToReservedAddresses is the
// point of the capture-side guard. fetchClient only covers bytes THIS
// daemon pulls; the page is untrusted code issuing its own requests,
// which never touch a Go client. Without interception an artwork can
// simply fetch the unauthenticated hub on :1111 or the kiosk's DevTools
// on :9222 from inside the capture browser.
func TestCapturer_SourceGuard_BlocksPageRequestsToReservedAddresses(t *testing.T) {
	for _, tc := range []struct {
		name, url, want string
	}{
		{"the unauthenticated hub", "http://127.0.0.1:1111/api/cast", "Fetch.failRequest"},
		{"the kiosk devtools", "http://127.0.0.1:9222/json/new", "Fetch.failRequest"},
		{"our own capture devtools", "http://127.0.0.1:9223/json", "Fetch.failRequest"},
		{"monitord", "http://127.0.0.1:9001/metrics", "Fetch.failRequest"},
		{"a LAN host", "http://192.168.1.5/admin", "Fetch.failRequest"},
		{"link-local metadata", "http://169.254.169.254/latest/meta-data", "Fetch.failRequest"},
		{"a public origin", "https://cdn.example.com/art.js", "Fetch.continueRequest"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &capturer{
				json:   wrapper.NewJSON(),
				logger: zaptest.NewLogger(t),
				guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
			}
			session := newFakePausedSession()
			c.attachSourceGuard(context.Background(), session)
			assert.Equal(t, tc.want, session.pause(t, tc.url))
		})
	}
}

// A non-http scheme carries its own bytes and opens no socket, so
// blocking it would break ordinary artwork (data: images, blob: workers)
// for no security gain. The schemes that actually dial are the ones the
// guard covers.
func TestCapturer_SourceGuard_AllowsNonDialingSchemes(t *testing.T) {
	c := &capturer{
		json:   wrapper.NewJSON(),
		logger: zaptest.NewLogger(t),
		guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
	}
	session := newFakePausedSession()
	c.attachSourceGuard(context.Background(), session)
	for _, url := range []string{"data:text/plain;base64,aGk=", "blob:https://cdn.example.com/abc"} {
		assert.Equal(t, "Fetch.continueRequest", session.pause(t, url), url)
	}
}

// A hostname that resolves into reserved space is blocked too — the check
// is on the resolved address, not on the literal text, so a name pointed
// at loopback does not get through by not looking like an IP.
func TestCapturer_SourceGuard_BlocksHostnameResolvingToReserved(t *testing.T) {
	c := &capturer{
		json:   wrapper.NewJSON(),
		logger: zaptest.NewLogger(t),
		guard:  sourceGuard{resolver: staticResolver{ip: "127.0.0.1"}},
	}
	session := newFakePausedSession()
	c.attachSourceGuard(context.Background(), session)
	assert.Equal(t, "Fetch.failRequest", session.pause(t, "http://art.evil.example/x.js"))
}

// TestCapturer_SourceGuard_CoversChildTargets is the regression for the
// bypass that made root-only interception useless: a cross-origin iframe
// (OOPIF) and a worker each run in their OWN CDP target, and their
// requests are never delivered to the root page session's handler. An
// artwork embedding a cross-origin iframe could therefore reach loopback
// with the root guard fully armed.
//
// The ordering matters as much as the coverage: the child must be armed
// and Fetch-enabled BEFORE it is resumed, or its opening request races
// ahead of interception.
func TestCapturer_SourceGuard_CoversChildTargets(t *testing.T) {
	c := &capturer{
		json:   wrapper.NewJSON(),
		logger: zaptest.NewLogger(t),
		guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
	}
	root := newFakePausedSession()
	guard := c.attachSourceGuard(context.Background(), root)
	require.NoError(t, c.enableGuardedAutoAttach(context.Background(), root, guard))

	attach := root.handler("Target.attachedToTarget")
	require.NotNil(t, attach, "auto-attach must be armed on the root session")

	for _, targetType := range []string{"iframe", "worker", "service_worker"} {
		t.Run(targetType, func(t *testing.T) {
			sessionID := "child-" + targetType
			attach(json.RawMessage(`{"sessionId":"` + sessionID +
				`","targetInfo":{"targetId":"t1","type":"` + targetType + `"}}`))

			var child *fakePausedSession
			require.Eventually(t, func() bool {
				child = root.child(sessionID)
				return child != nil && child.handler("Fetch.requestPaused") != nil
			}, 2*time.Second, 5*time.Millisecond,
				"the guard must be armed on the child target, not only the root")

			// Armed and enabled before resume — never the other way round.
			sent := child.verdicts()
			require.Contains(t, sent, "Fetch.enable")
			require.Contains(t, sent, "Runtime.runIfWaitingForDebugger")
			assert.Less(t, indexOf(sent, "Fetch.enable"), indexOf(sent, "Runtime.runIfWaitingForDebugger"),
				"a child resumed before interception is armed can race its first request out")

			// And the child's own requests are actually policed.
			assert.Equal(t, "Fetch.failRequest", child.pause(t, "http://127.0.0.1:1111/api/cast"))
		})
	}
}

// A nested target (an iframe inside the artwork's own iframe) must be
// covered too — a boundary that stops at depth one is a boundary with a
// documented way around it.
func TestCapturer_SourceGuard_RecursesIntoNestedTargets(t *testing.T) {
	c := &capturer{
		json:   wrapper.NewJSON(),
		logger: zaptest.NewLogger(t),
		guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
	}
	root := newFakePausedSession()
	guard := c.attachSourceGuard(context.Background(), root)
	require.NoError(t, c.enableGuardedAutoAttach(context.Background(), root, guard))

	root.handler("Target.attachedToTarget")(json.RawMessage(
		`{"sessionId":"child-1","targetInfo":{"targetId":"t1","type":"iframe"}}`))

	var child *fakePausedSession
	require.Eventually(t, func() bool {
		child = root.child("child-1")
		return child != nil && child.handler("Target.attachedToTarget") != nil
	}, 2*time.Second, 5*time.Millisecond, "auto-attach must be extended into the child")
	assert.Contains(t, child.verdicts(), "Target.setAutoAttach")

	// Grandchild events arrive on the child's session but route through
	// root, so the view must still come from root.
	child.handler("Target.attachedToTarget")(json.RawMessage(
		`{"sessionId":"grandchild-1","targetInfo":{"targetId":"t2","type":"iframe"}}`))
	var grand *fakePausedSession
	require.Eventually(t, func() bool {
		grand = root.child("grandchild-1")
		return grand != nil && grand.handler("Fetch.requestPaused") != nil
	}, 2*time.Second, 5*time.Millisecond, "a nested target must be guarded too")
	assert.Equal(t, "Fetch.failRequest", grand.pause(t, "http://127.0.0.1:9222/json/new"))
}

// Non-http(s) schemes other than data:/blob: must FAIL CLOSED. An earlier
// version continued everything that was not http(s), which is a denylist
// wearing an allowlist's clothes.
func TestCapturer_SourceGuard_UnsafeSchemesFailClosed(t *testing.T) {
	c := &capturer{
		json:   wrapper.NewJSON(),
		logger: zaptest.NewLogger(t),
		guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
	}
	session := newFakePausedSession()
	c.attachSourceGuard(context.Background(), session)
	for _, url := range []string{
		"file:///etc/shadow",
		"ftp://example.com/x",
		"chrome://settings",
		"devtools://devtools/bundled/inspector.html",
		"ws://127.0.0.1:9222/devtools/page/X",
	} {
		assert.Equal(t, "Fetch.failRequest", session.pause(t, url), url)
	}
}

func indexOf(haystack []string, needle string) int {
	for i, v := range haystack {
		if v == needle {
			return i
		}
	}
	return -1
}

// TestCapturer_SourceGuard_TargetFloodIsBounded pins the second bound.
// Deciding a paused request and setting up a child target are different
// work: each attach performs several sequential CDP calls, so the
// request semaphore does not constrain it at all. A hostile page can
// create targets faster than those calls complete, and one unbounded
// goroutine per attach event would exhaust the daemon.
//
// Fail-closed is what makes dropping safe: children are created with
// waitForDebuggerOnStart, so a target that is never set up stays PAUSED
// and never runs. Declining work can only cost fidelity, never admit an
// unguarded target.
func TestCapturer_SourceGuard_TargetFloodIsBounded(t *testing.T) {
	c := &capturer{
		json:   wrapper.NewJSON(),
		logger: zaptest.NewLogger(t),
		guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
	}
	root := newFakePausedSession()
	// Every CDP Send stalls until released, so setups pile up in flight
	// and the peak is what the bound has to hold.
	root.gate = make(chan struct{})
	root.inFlight = &atomic.Int32{}
	root.peak = &atomic.Int32{}

	guard := c.attachSourceGuard(context.Background(), root)
	// setAutoAttach itself Sends, so arm the handler without it.
	c.armAutoAttachHandler(context.Background(), root, root, guard)
	attach := root.handler("Target.attachedToTarget")
	require.NotNil(t, attach)

	const flood = captureTargetSetupConcurrency * 20
	for i := range flood {
		attach(json.RawMessage(fmt.Sprintf(
			`{"sessionId":"flood-%d","targetInfo":{"targetId":"t%d","type":"iframe"}}`, i, i)))
	}

	// Give the goroutines that DID get a slot time to reach their Send.
	require.Eventually(t, func() bool { return root.peak.Load() > 0 },
		2*time.Second, 5*time.Millisecond, "no setup ever started")
	time.Sleep(100 * time.Millisecond)

	assert.LessOrEqual(t, int(root.peak.Load()), captureTargetSetupConcurrency,
		"child-target setup must be bounded; an unbounded goroutine per attach event would let all %d run at once", flood)

	close(root.gate)
}

// TestCapturer_SourceGuard_ChildStaysPausedWhenArmingFails pins the
// fail-closed contract on BOTH arming steps. A child is created with
// waitForDebuggerOnStart, so it is inert until resumed — which makes
// "return without resuming" the safe outcome whenever the guard could not
// be fully established on it.
//
// The setAutoAttach case is the subtle one, and an earlier version got it
// wrong: it logged the failure and resumed anyway. That child's own
// nested iframes and workers would then be neither paused nor
// intercepted, reopening the loopback bypass one level down — the exact
// hole the recursion exists to close.
func TestCapturer_SourceGuard_ChildStaysPausedWhenArmingFails(t *testing.T) {
	for _, tc := range []struct{ name, failing string }{
		{"Fetch.enable fails", "Fetch.enable"},
		{"recursive setAutoAttach fails", "Target.setAutoAttach"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &capturer{
				json:   wrapper.NewJSON(),
				logger: zaptest.NewLogger(t),
				guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
			}
			root := newFakePausedSession()
			guard := c.attachSourceGuard(context.Background(), root)
			// Arm the handler without the root's own setAutoAttach, which
			// would otherwise trip the injected failure.
			c.armAutoAttachHandler(context.Background(), root, root, guard)
			root.mu.Lock()
			root.failMethod = tc.failing
			root.mu.Unlock()

			root.handler("Target.attachedToTarget")(json.RawMessage(
				`{"sessionId":"child-1","targetInfo":{"targetId":"t1","type":"iframe"}}`))

			var child *fakePausedSession
			require.Eventually(t, func() bool {
				child = root.child("child-1")
				return child != nil && len(child.verdicts()) > 0
			}, 2*time.Second, 5*time.Millisecond, "the child was never touched")
			// Let any (incorrect) resume land before asserting its absence.
			time.Sleep(50 * time.Millisecond)

			assert.NotContains(t, child.verdicts(), "Runtime.runIfWaitingForDebugger",
				"a child whose guard could not be fully armed must stay PAUSED, never be resumed")
		})
	}
}

// TestCaptureTracker_BoundsResourcesAndURLs pins the anti-abuse ceiling on
// what one capture may accumulate. Passing the source guard means the
// ORIGIN is public; it says nothing about whether the page behaves. The
// disk budget caps bytes FETCHED, which does not help here: the tracker
// also holds URLs for resources it never fetches (failures, and requests
// still pending at the deadline), so a page emitting a stream of distinct
// long URLs grew daemon memory during the window and then grew
// ItemRecord.Resources and the on-disk coverage without limit.
//
// The important half is the last assertion: exceeding the bound must make
// the capture INCOMPLETE. Dropping resources while still reporting
// Complete:true would replay as an artwork with missing pieces and no
// explanation — strictly worse than the unbounded growth, because it is
// silent.
func TestCaptureTracker_BoundsResourcesAndURLs(t *testing.T) {
	t.Run("distinct resources are capped", func(t *testing.T) {
		tr := newCaptureTracker()
		for i := 0; i < MaxCaptureResources+50; i++ {
			tr.recordResource(fmt.Sprintf("https://example.com/r%d.png", i), 200, "image/png", "", nil, "GET")
		}
		keys, resources, _, _, overflow := tr.snapshot()
		require.Len(t, resources, MaxCaptureResources)
		require.Len(t, keys, MaxCaptureResources)
		require.Equal(t, 50, overflow)
	})

	t.Run("an oversized URL is refused, not stored truncated", func(t *testing.T) {
		tr := newCaptureTracker()
		long := "https://example.com/" + strings.Repeat("a", MaxCaptureResourceURLBytes)
		tr.recordResource(long, 200, "image/png", "", nil, "GET")
		_, resources, _, _, overflow := tr.snapshot()
		require.Empty(t, resources,
			"storing a truncated URL would be worse than dropping it: replay matches on exact URL, "+
				"so a truncated entry is a permanent false cache miss")
		require.Equal(t, 1, overflow)
	})

	t.Run("updating an already-tracked resource does not consume budget", func(t *testing.T) {
		tr := newCaptureTracker()
		for i := 0; i < MaxCaptureResources; i++ {
			tr.recordResource(fmt.Sprintf("https://example.com/r%d.png", i), 200, "image/png", "", nil, "GET")
		}
		// Re-recording an existing key must still be allowed at the
		// ceiling, or the last observation of a resource would be lost
		// and a stale first-sight record kept instead.
		tr.recordResource("https://example.com/r0.png", 404, "text/plain", "", nil, "GET")
		_, resources, _, _, overflow := tr.snapshot()
		require.Equal(t, 0, overflow)
		require.Equal(t, 404, resources[resourceKey("GET", "https://example.com/r0.png")].Status)
	})

	t.Run("failures and pending requests are capped too", func(t *testing.T) {
		tr := newCaptureTracker()
		for i := 0; i < MaxCaptureResources+10; i++ {
			tr.recordFailure(fmt.Sprintf("https://example.com/f%d.png", i), "net::ERR")
			tr.trackRequest(fmt.Sprintf("req-%d", i), fmt.Sprintf("https://example.com/p%d.png", i), "GET")
		}
		_, _, failures, pendingURLs, overflow := tr.snapshot()
		require.Len(t, failures, MaxCaptureResources)
		require.LessOrEqual(t, len(pendingURLs), MaxCaptureResources)
		require.Positive(t, overflow)
	})
}

// TestCapturer_TrackerOverflowMakesCoverageIncomplete is the half that
// matters operationally: the bound must be visible in the record, not
// only in memory.
func TestCapturer_TrackerOverflowMakesCoverageIncomplete(t *testing.T) {
	c := &capturer{logger: zaptest.NewLogger(t)}
	tr := newCaptureTracker()
	// Oversized URLs, so every observation is REFUSED and the tracker
	// ends up empty-but-overflowed. That isolates the coverage assembly:
	// with no accepted resource there is nothing for resolveResources to
	// fetch, so the only thing that can mark this capture incomplete is
	// the overflow marker itself.
	long := "https://example.com/" + strings.Repeat("a", MaxCaptureResourceURLBytes)
	tr.recordResource(long, 200, "image/png", "", nil, "GET")
	tr.recordFailure(long, "net::ERR")

	_, coverage := c.resolveResources(context.Background(), context.Background(), tr, c.newDiskBudget())

	require.False(t, coverage.Complete,
		"a capture that dropped observations must never report Complete:true")
	require.Contains(t, coverage.Reason, "tracker_limit_exceeded")
	// Assert against the ACTUAL refused URL. An earlier version of this
	// checked for "example.com/r", a copy-paste from the sibling subtest's
	// r%d.png URLs — a substring the refused URLs here never contain — so
	// it passed vacuously and would have passed just as happily if
	// resolveResources appended every refused URL in full.
	require.NotContains(t, coverage.Reason, long,
		"the marker must be bounded: naming the refused URLs would reintroduce the growth the bound prevents")
	require.Less(t, len(coverage.Reason), 512,
		"the marker is a fixed-size summary; it must not scale with what was refused")
}

// TestCaptureTracker_UnknownMethodIsNotTreatedAsIdempotent pins the
// security property behind methodUnknown. An untracked requestId used to
// yield "", which means GET everywhere downstream — and GET is in
// safeIdempotentMethods, so resolveResources RE-ISSUES a resource whose
// body it does not have. A page that got a requestId dropped could
// therefore have the daemon re-send its POST as a GET, which is precisely
// the side effect the unsupported_method branch exists to prevent.
//
// The bound added alongside this made that reachable on purpose: saturate
// the in-flight map, and every later request is refused and so untracked.
func TestCaptureTracker_UnknownMethodIsNotTreatedAsIdempotent(t *testing.T) {
	tr := newCaptureTracker()

	require.Equal(t, methodUnknown, tr.methodForRequest("never-seen"),
		"an untracked id must not report the empty string, which downstream reads as GET")
	require.False(t, safeIdempotentMethods[methodUnknown],
		"methodUnknown must never be re-issued; this is the assertion that keeps the downgrade closed")

	// A tracked request still reports its real method, and a resolved one
	// goes back to unknown rather than lingering as a stale attribution.
	tr.trackRequest("req-1", "https://example.com/a", "POST")
	require.Equal(t, "POST", tr.methodForRequest("req-1"))
	tr.resolveRequest("req-1")
	require.Equal(t, methodUnknown, tr.methodForRequest("req-1"))
}

// TestCaptureTracker_ResolveRequestPrunesInFlightState is what makes the
// in-flight ceiling a bound on CONCURRENCY rather than on total requests
// ever seen. Without pruning, a page can walk requestURL to the ceiling
// with sequential, perfectly-terminated requests and force every later
// one to be refused — which is the lever for the method downgrade above,
// and also marks otherwise-healthy range/tile-heavy captures incomplete.
func TestCaptureTracker_ResolveRequestPrunesInFlightState(t *testing.T) {
	tr := newCaptureTracker()
	for i := 0; i < MaxCaptureResources*2; i++ {
		id := fmt.Sprintf("req-%d", i)
		tr.trackRequest(id, fmt.Sprintf("https://example.com/s%d.png", i), "GET")
		tr.resolveRequest(id)
	}
	_, _, _, pendingURLs, overflow := tr.snapshot()
	require.Empty(t, pendingURLs)
	require.Equal(t, 0, overflow,
		"sequential terminated requests must never reach the in-flight ceiling")
}

// TestCaptureTracker_BoundsRedirectTarget covers the parameter the first
// version of the bound missed: redirectTo is an attacker-controlled 3xx
// Location, persisted in Resource.RedirectTo and served back by replay,
// yet only url was being checked.
func TestCaptureTracker_BoundsRedirectTarget(t *testing.T) {
	tr := newCaptureTracker()
	long := "https://example.com/" + strings.Repeat("t", MaxCaptureResourceURLBytes)
	tr.recordResource("https://example.com/a", 302, "text/html", long, nil, "GET")

	_, resources, _, _, overflow := tr.snapshot()
	res := resources[resourceKey("GET", "https://example.com/a")]
	require.Equal(t, "", res.RedirectTo,
		"an oversized redirect target is dropped, not stored truncated: replay matches exact URLs, "+
			"so a truncated target would mis-resolve rather than simply miss")
	require.Equal(t, 1, overflow, "and the capture is marked incomplete rather than silently altered")
}

// TestCaptureTracker_BoundsResourceMetadata covers the two remaining
// attacker-supplied strings recordResource stores: the content type and
// the allowlisted header values. Both are persisted in Resource and
// served back by replay. Chromium bounds one response's header block, but
// MaxCaptureResources is 4096, so unbounded values still let one capture
// accumulate on the order of a gigabyte — the exact accumulation these
// bounds exist to stop.
func TestCaptureTracker_BoundsResourceMetadata(t *testing.T) {
	tr := newCaptureTracker()
	longMeta := strings.Repeat("m", MaxCaptureResourceMetaBytes+1)
	tr.recordResource("https://example.com/a", 200, longMeta, "",
		map[string]string{"content-type": longMeta, "cache-control": "max-age=60"}, "GET")

	_, resources, _, _, overflow := tr.snapshot()
	res := resources[resourceKey("GET", "https://example.com/a")]
	require.Equal(t, "", res.ContentType,
		"dropped, not truncated: a truncated MIME type would be SERVED on replay as though it were real")
	require.NotContains(t, res.Headers, "content-type", "an oversized header value is dropped")
	require.Equal(t, "max-age=60", res.Headers["cache-control"],
		"and its in-bounds siblings survive")
	require.Equal(t, 2, overflow)
}
