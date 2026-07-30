package playersession

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
)

// fakeCDP is a minimal in-memory model of the real cdp.CDP client's
// post-decoded contract (evaluate results arrive already unmarshaled), tuned
// to the handful of expression shapes this package sends. It mirrors
// setupui_test.go's fakeCDP idiom (including modeling the document-replacement
// transition window a Page.navigate opens) without pulling in that package's
// setupDisplay-specific parsing.
type fakeCDP struct {
	mu sync.Mutex

	initialized bool

	// handlerInstalled / docStamp / navNonce / statusExists / statusRoute
	// model the CURRENT document's observable state. docStamp and navNonce
	// are SEPARATE globals [M4] — window.__ffosDocStamp (the session's
	// barrier-resolution stamp, source-3 carrier) and window.__ffosNavNonce
	// (stampNavNonce's pre-navigate marker) — exactly like production.
	handlerInstalled bool
	docStamp         string
	navNonce         string
	statusExists     bool
	statusRoute      string

	// pendingNavigate / evalsSinceNavigate / hydrateAfterEvals model the
	// transition window opened by Page.navigate: handlerInstalled flips back
	// to true only after `hydrateAfterEvals` further evaluate calls, and both
	// docStamp and navNonce are cleared immediately (a real navigation wipes
	// window state), so a StageHandler probe carrying the pre-navigate nonce
	// reads ready=false until the (simulated) new document's handler installs.
	pendingNavigate    bool
	evalsSinceNavigate int
	hydrateAfterEvals  int
	// statusSurvivesNavigate keeps statusExists/statusRoute untouched across a
	// Page.navigate, for tests that want deterministic (non-racy) control over
	// what route the "new" document reports without a background goroutine
	// racing the verification wait.
	statusSurvivesNavigate bool

	// routeSettleAfterPolls / routeSettleTo model AppWrapper's post-hydration
	// auto-route: statusRoute switches to routeSettleTo once the route probe
	// (the __ffosPlayerStatus() eval) has been read routeSettleAfterPolls
	// times. 0 (default) means "never settles" — statusRoute stays whatever
	// it was set to.
	routeSettleAfterPolls int
	routeProbeReads       int
	routeSettleTo         string

	// statusProtocol overrides the "protocol" field the status probe
	// answers with; 0 (the zero value) means "use 1" (the version this
	// package understands by default). Tests set a different value to model
	// a player advertising a protocol version this package cannot decode.
	statusProtocol float64

	navigateCount int
	purgeCount    int
	evalCount     int

	// methodErrOnce forces the NEXT send matching method to fail; consumed once.
	methodErrOnce map[string]error
}

func newFakeCDP() *fakeCDP {
	return &fakeCDP{initialized: true, methodErrOnce: map[string]error{}}
}

func (f *fakeCDP) Initialized() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.initialized
}

func (f *fakeCDP) setInitialized(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.initialized = v
}

func (f *fakeCDP) setHandlerInstalled(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handlerInstalled = v
}

func (f *fakeCDP) setStatus(exists bool, route string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.statusExists = exists
	f.statusRoute = route
}

func (f *fakeCDP) failNextOnce(method string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.methodErrOnce[method] = err
}

func (f *fakeCDP) getNavigateCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.navigateCount
}

func (f *fakeCDP) getPurgeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.purgeCount
}

func (f *fakeCDP) getEvalCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.evalCount
}

func (f *fakeCDP) NoLogSend(method string, params map[string]interface{}) (interface{}, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err, ok := f.methodErrOnce[method]; ok {
		delete(f.methodErrOnce, method)
		return nil, err
	}

	switch method {
	case "Page.navigate":
		f.navigateCount++
		f.pendingNavigate = true
		f.evalsSinceNavigate = 0
		f.handlerInstalled = false
		f.docStamp = ""
		f.navNonce = ""
		if !f.statusSurvivesNavigate {
			f.statusExists = false
		}
		return map[string]any{}, nil
	case "Network.clearBrowserCache":
		f.purgeCount++
		return map[string]any{}, nil
	case cdp.METHOD_EVALUATE:
		f.evalCount++
		if f.pendingNavigate {
			f.evalsSinceNavigate++
			if f.evalsSinceNavigate > f.hydrateAfterEvals {
				f.handlerInstalled = true
				f.pendingNavigate = false
			}
		}
		expr, _ := params["expression"].(string)
		return f.evalLocked(expr)
	default:
		return map[string]any{}, nil
	}
}

// evalLocked interprets the handful of expression shapes playersession sends.
// Caller holds f.mu.
func (f *fakeCDP) evalLocked(expr string) (interface{}, error) {
	switch {
	case strings.Contains(expr, "window.__ffosDocStamp = "):
		// The barrier-resolution stamp write (onGenerationReady), the
		// SESSION-exclusive source-3 carrier.
		val := extractQuoted(expr, "window.__ffosDocStamp = ")
		f.docStamp = val
		return map[string]any{"stamped": true}, nil

	case strings.Contains(expr, "window.__ffosNavNonce = "):
		// The pre-navigate nonce write (stampNavNonce) — a SEPARATE global
		// from __ffosDocStamp [M4], never visible to the stamp observer.
		val := extractQuoted(expr, "window.__ffosNavNonce = ")
		f.navNonce = val
		return map[string]any{"nonce": val}, nil

	case strings.Contains(expr, "typeof window.handleCDPRequest === 'function'"):
		ready := f.handlerInstalled
		if nonce := extractQuoted(expr, "window.__ffosNavNonce !== "); nonce != "" {
			ready = ready && f.navNonce != nonce
		}
		return map[string]any{"ready": ready}, nil

	case strings.Contains(expr, "typeof window.__ffosPlayerStatus === 'function'"):
		return map[string]any{"ready": f.statusExists}, nil

	case expr == `window.__ffosPlayerStatus ? window.__ffosPlayerStatus() : null`:
		if !f.statusExists {
			return nil, nil
		}
		f.routeProbeReads++
		if f.routeSettleAfterPolls > 0 && f.routeProbeReads > f.routeSettleAfterPolls {
			f.statusRoute = f.routeSettleTo
		}
		protocol := f.statusProtocol
		if protocol == 0 {
			protocol = 1
		}
		return map[string]any{
			"protocol": protocol,
			"route":    f.statusRoute,
		}, nil

	default:
		return map[string]any{"ok": true}, nil
	}
}

// extractQuoted returns the double-quoted literal immediately following
// prefix in expr, or "" if prefix is absent.
func extractQuoted(expr, prefix string) string {
	idx := strings.Index(expr, prefix)
	if idx < 0 {
		return ""
	}
	rest := expr[idx+len(prefix):]
	rest = strings.TrimPrefix(rest, `"`)
	end := strings.Index(rest, `"`)
	if end < 0 {
		return ""
	}
	return rest[:end]
}

func newTestSession(t *testing.T, sender CDPSender) *Session {
	t.Helper()
	s := New(context.Background(), sender, nil, nil, zap.NewNop())
	s.pollInterval = 5 * time.Millisecond
	s.inlineVerifyCap = 300 * time.Millisecond
	return s
}

// currentGenerationStampForTest reads the current generation's barrier-write
// stamp directly (same-package access), for tests that need a real baseline
// value rather than a synthetic one.
func (s *Session) currentGenerationStampForTest() string {
	s.mu.Lock()
	gen := s.current
	s.mu.Unlock()
	if gen == nil {
		return ""
	}
	gen.mu.Lock()
	defer gen.mu.Unlock()
	return gen.stamp
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	require.True(t, cond(), "condition not met within %s", timeout)
}

// --- Generation bump from all three sources ---------------------------------

func TestGeneration_ZeroBeforeAnyBump(t *testing.T) {
	s := newTestSession(t, newFakeCDP())
	assert.Equal(t, uint64(0), s.Generation())
}

func TestGeneration_BumpsOnConnect(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)

	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.Generation() == 1 })

	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.Generation() == 2 })
}

func TestGeneration_BumpsOnSessionExecutedNavigation(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)
	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.Generation() == 1 })

	err := s.NavigateHomeInline(NavOptions{})
	require.NoError(t, err)
	assert.Equal(t, uint64(2), s.Generation())
	assert.Equal(t, 1, f.getNavigateCount())
}

func TestGeneration_BumpsOnStatusStampMismatch(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)
	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.StageReady(StageHandler) })
	gen1 := s.Generation()

	// The baseline stamp itself, reported present, must NOT bump (this is the
	// session's own bump->stamp-write window observing its own value).
	baseline := s.currentGenerationStampForTest()
	require.NotEmpty(t, baseline)
	s.ObserveStatusStamp(baseline, true)
	assert.Equal(t, gen1, s.Generation())

	// A genuinely different stamp, reported present (someone else replaced
	// the document, e.g. feral-watchdog), must bump.
	s.ObserveStatusStamp("someone-elses-stamp", true)
	waitFor(t, time.Second, func() bool { return s.Generation() == gen1+1 })
}

// TestGeneration_StatusStampAbsent_NeverBumps pins the real B1 semantics:
// present==false (an old player without the stamp carrier at all) must NEVER
// bump, regardless of the string value — including a non-empty string, which
// a caller could never legitimately observe from an absent field but the
// contract must not depend on that.
func TestGeneration_StatusStampAbsent_NeverBumps(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)
	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.StageReady(StageHandler) })
	gen1 := s.Generation()

	s.ObserveStatusStamp("", false)
	s.ObserveStatusStamp("some-value", false)
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, gen1, s.Generation(), "absent stamp must never bump")
}

// TestGeneration_StatusStampPresentEmpty_BumpsAfterNonEmptyBaseline pins the
// core B1 fix: a PRESENT-but-empty stamp (a fresh/foreign document with no
// __ffosDocStamp of its own — the shape a feral-watchdog navigate or a
// player-initiated reload produces) reported against a generation that
// already has a non-empty baseline IS the foreign-fresh-document mismatch and
// must bump. Before the fix, the flattened "" was indistinguishable from
// "absent" and this case was silently swallowed.
func TestGeneration_StatusStampPresentEmpty_BumpsAfterNonEmptyBaseline(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)
	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.StageReady(StageHandler) })
	gen1 := s.Generation()
	require.NotEmpty(t, s.currentGenerationStampForTest())

	s.ObserveStatusStamp("", true)
	waitFor(t, time.Second, func() bool { return s.Generation() == gen1+1 })
}

func TestGeneration_StatusStampMismatch_NoBaselineYet_NoBump(t *testing.T) {
	f := newFakeCDP()
	// handler never installs, so the generation never reaches barrier
	// resolution and never gets a baseline stamp.
	s := newTestSession(t, f)
	s.OnConnect()
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, uint64(1), s.Generation())

	s.ObserveStatusStamp("anything", true)
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, uint64(1), s.Generation(), "no baseline stamp yet: must not bump")

	// present-empty during the same no-baseline window must also not bump —
	// this is the session's own pre-bump window, not a foreign document.
	s.ObserveStatusStamp("", true)
	time.Sleep(30 * time.Millisecond)
	assert.Equal(t, uint64(1), s.Generation(), "present-empty with no baseline yet: must not bump")
}

// --- AwaitStage: positive-only caching + re-resolve after negative ----------

func TestAwaitStage_FailsFastWhenNotConnected(t *testing.T) {
	f := newFakeCDP()
	f.setInitialized(false)
	s := newTestSession(t, f)

	start := time.Now()
	err := s.AwaitStage(context.Background(), StageHandler)
	assert.ErrorIs(t, err, ErrNotConnected)
	assert.Less(t, time.Since(start), 50*time.Millisecond, "must fail fast, not poll")
}

func TestAwaitStage_NoGenerationYet(t *testing.T) {
	f := newFakeCDP()
	s := newTestSession(t, f)
	err := s.AwaitStage(context.Background(), StageHandler)
	assert.ErrorIs(t, err, ErrNoGeneration)
}

func TestAwaitStage_NegativeNotCached_ThenPositiveCached(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = false
	s := newTestSession(t, f)
	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.Generation() == 1 })

	// First wait: handler never installs within the short ctx -> negative,
	// not cached.
	ctx1, cancel1 := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel1()
	err := s.AwaitStage(ctx1, StageHandler)
	require.Error(t, err)
	assert.False(t, s.StageReady(StageHandler))

	// Now the handler installs; a fresh AwaitStage call re-polls from
	// scratch (the negative result was not latched) and succeeds.
	f.setHandlerInstalled(true)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	require.NoError(t, s.AwaitStage(ctx2, StageHandler))
	assert.True(t, s.StageReady(StageHandler))

	callsAfterReady := f.getEvalCount()
	// A cached positive must short-circuit: no further evaluate traffic.
	require.NoError(t, s.AwaitStage(context.Background(), StageHandler))
	assert.Equal(t, callsAfterReady, f.getEvalCount(), "cached positive must not re-probe")
}

func TestAwaitStage_StageStatus_NeverResolvesOnOldPlayer(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	f.statusExists = false // old player: no __ffosPlayerStatus
	s := newTestSession(t, f)
	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.Generation() == 1 })

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	err := s.AwaitStage(ctx, StageStatus)
	assert.Error(t, err, "StageStatus must never resolve on a player without the carrier")
}

// --- NavigationPending lifecycle across every outcome ------------------------

func TestNavigationPending_ClearsOnEveryTerminalOutcome(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*fakeCDP, *Session)
	}{
		{"asleep", func(f *fakeCDP, s *Session) { s.asleep = func() bool { return true } }},
		{"error-page", func(f *fakeCDP, s *Session) { f.setStatus(true, routeError) }},
		{"overlay", func(f *fakeCDP, s *Session) {
			s.RegisterOverlayOwner("test-owner", func() bool { return true })
		}},
		{"executed", func(f *fakeCDP, s *Session) { f.handlerInstalled = true }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeCDP()
			s := newTestSession(t, f)
			tc.setup(f, s)

			assert.False(t, s.NavigationPending())
			_ = s.NavigateHomeInline(NavOptions{})
			assert.False(t, s.NavigationPending(), "must clear on every terminal outcome")
		})
	}
}

func TestNavigationPending_SetBeforeOverlayProbe(t *testing.T) {
	f := newFakeCDP()
	s := newTestSession(t, f)

	var observedPending bool
	s.RegisterOverlayOwner("probe-observer", func() bool {
		observedPending = s.NavigationPending()
		return true // skip the navigation
	})

	res := s.navigateAndVerify(context.Background(), NavOptions{})
	assert.Equal(t, NavSkippedOverlay, res.Outcome)
	assert.True(t, observedPending, "NavigationPending must already be true when the overlay probe runs")
	assert.False(t, s.NavigationPending())
}

// --- Overlay / error / sleep gates -------------------------------------------

func TestNavigateHomeInline_SleepGate(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)
	s.asleep = func() bool { return true }

	err := s.NavigateHomeInline(NavOptions{})
	assert.ErrorIs(t, err, ErrNavSkippedAsleep)
	assert.Equal(t, 0, f.getNavigateCount())
}

func TestNavigateHomeInline_ErrorPageGate(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	f.setStatus(true, routeError)
	s := newTestSession(t, f)

	err := s.NavigateHomeInline(NavOptions{})
	assert.ErrorIs(t, err, ErrNavSkippedOverlay)
	assert.Equal(t, 0, f.getNavigateCount())
}

func TestNavigateHomeInline_ErrorPageGate_UnavailableStatusTreatedAsNotError(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	f.statusExists = false // old player / probe unavailable
	s := newTestSession(t, f)

	err := s.NavigateHomeInline(NavOptions{})
	assert.NoError(t, err, "unavailable status must not gate as an error page")
	assert.Equal(t, 1, f.getNavigateCount())
}

// TestNavigateHomeInline_ErrorPageGate_ProtocolMismatchStillGates pins
// finding 3: the error-page safety gate reads route independent of
// protocol version. A player advertising a protocol version this package
// cannot decode must still gate — [NV2] "never navigate over /error" must
// not silently disable the moment a player starts advertising a newer
// protocol number.
func TestNavigateHomeInline_ErrorPageGate_ProtocolMismatchStillGates(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	f.setStatus(true, routeError)
	f.statusProtocol = 2 // a future protocol version this package can't decode
	s := newTestSession(t, f)

	err := s.NavigateHomeInline(NavOptions{})
	assert.ErrorIs(t, err, ErrNavSkippedOverlay)
	assert.Equal(t, 0, f.getNavigateCount())
}

func TestNavigateHomeInline_OverlayGate(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)
	s.RegisterOverlayOwner("narration", func() bool { return true })

	err := s.NavigateHomeInline(NavOptions{})
	assert.ErrorIs(t, err, ErrNavSkippedOverlay)
	assert.Equal(t, 0, f.getNavigateCount())
}

// --- Inline's synchronous contract -------------------------------------------

func TestNavigateHomeInline_SucceedsSynchronously(t *testing.T) {
	f := newFakeCDP()
	f.hydrateAfterEvals = 0 // handler installs immediately after navigate
	s := newTestSession(t, f)

	err := s.NavigateHomeInline(NavOptions{PurgeCache: true})
	require.NoError(t, err)
	assert.Equal(t, 1, f.getNavigateCount())
	assert.Equal(t, 1, f.getPurgeCount())
	assert.Equal(t, uint64(1), s.Generation())
}

func TestNavigateHomeInline_NavigateSendFailure(t *testing.T) {
	f := newFakeCDP()
	f.failNextOnce("Page.navigate", fmt.Errorf("boom"))
	s := newTestSession(t, f)

	err := s.NavigateHomeInline(NavOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "navigate")
	assert.Equal(t, uint64(0), s.Generation(), "a failed send must not bump the generation")
}

func TestNavigateHomeInline_VerifyTimeout(t *testing.T) {
	f := newFakeCDP()
	f.hydrateAfterEvals = 1_000_000 // handler never installs within the cap
	s := newTestSession(t, f)
	s.inlineVerifyCap = 60 * time.Millisecond

	err := s.NavigateHomeInline(NavOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification")
}

func TestNavigateHomeInline_RouteMismatchAfterHandlerReady(t *testing.T) {
	f := newFakeCDP()
	f.hydrateAfterEvals = 0
	f.statusSurvivesNavigate = true
	// The "new" document's __ffosPlayerStatus answers with a route that is
	// neither /playlist nor /sleep — an unexpected route the verification
	// step must reject even though StageHandler resolved.
	f.setStatus(true, "/settings")
	s := newTestSession(t, f)

	err := s.NavigateHomeInline(NavOptions{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "route")
}

// TestNavigateHomeInline_RootRoute_PersistsAsVerifiedSuccess pins the B2 fix:
// Page.navigate always lands on "/" first, and AppWrapper's auto-route to
// /playlist only fires after hydration finishes — well after StageHandler
// resolves. A navigation that lands on "/" and STAYS there through the whole
// verification window (nothing cast; a legitimate idle steady state) must be
// reported as a verified success, not a route-mismatch error — a repaired
// wall must not burn a boot-recovery attempt or report failure to the
// relayer.
func TestNavigateHomeInline_RootRoute_PersistsAsVerifiedSuccess(t *testing.T) {
	f := newFakeCDP()
	f.hydrateAfterEvals = 0
	f.statusSurvivesNavigate = true
	f.setStatus(true, "/")
	s := newTestSession(t, f)
	s.inlineVerifyCap = 80 * time.Millisecond

	err := s.NavigateHomeInline(NavOptions{})
	assert.NoError(t, err, "a persisted \"/\" must verify as success, not time out as a mismatch")
}

// TestNavigateHomeInline_RootRoute_SettlesToPlaylist_SucceedsWithoutWaitingOutTheCap
// pins the other half of B2: when "/" resolves to /playlist mid-poll (the
// real post-hydration auto-route), verification must succeed as soon as that
// happens, not only after the full cap elapses.
func TestNavigateHomeInline_RootRoute_SettlesToPlaylist_SucceedsWithoutWaitingOutTheCap(t *testing.T) {
	f := newFakeCDP()
	f.hydrateAfterEvals = 0
	f.statusSurvivesNavigate = true
	f.setStatus(true, "/")
	f.routeSettleAfterPolls = 1
	f.routeSettleTo = routePlaylist
	s := newTestSession(t, f)
	s.pollInterval = 5 * time.Millisecond
	s.inlineVerifyCap = 2 * time.Second

	start := time.Now()
	err := s.NavigateHomeInline(NavOptions{})
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, time.Second, "must succeed as soon as the route settles, not wait out the full cap")
}

// TestNavigateHomeInline_LandsOnErrorPage_ClassifiedAsSkippedOverlay pins
// finding 4: a navigation that EXECUTED (Page.navigate sent, generation
// bumped, StageHandler resolved) but settles on /error is not a broken
// navigation to retry — the [NV2] error-page gate would have refused it as a
// pre-nav check too had the daemon already been showing it, so a post-nav
// landing there is classified identically: NavSkippedOverlay, not
// NavExecuted with an "unexpected route" error. This matters downstream —
// boot recovery maps NavSkippedOverlay to Expired and does NOT count it as
// an executed attempt, unlike NavExecuted+Err.
func TestNavigateHomeInline_LandsOnErrorPage_ClassifiedAsSkippedOverlay(t *testing.T) {
	f := newFakeCDP()
	f.hydrateAfterEvals = 0
	f.statusSurvivesNavigate = true
	f.setStatus(true, "/") // pre-nav gate sees "/", not /error, so the navigation proceeds
	f.routeSettleAfterPolls = 1
	f.routeSettleTo = routeError // the "new" document settles on /error
	s := newTestSession(t, f)
	s.pollInterval = 5 * time.Millisecond
	s.inlineVerifyCap = 2 * time.Second

	err := s.NavigateHomeInline(NavOptions{})
	assert.ErrorIs(t, err, ErrNavSkippedOverlay)
	assert.Equal(t, 1, f.getNavigateCount(), "the navigation must have executed, not been pre-nav-skipped")
}

func TestNavigateHomeInline_TakesNoExternalLock(t *testing.T) {
	// Regression guard for [NV7]: Inline must be callable while the test
	// holds a lock the session never touches, proving it needs no
	// caller-held lock to make progress.
	var callerLock sync.Mutex
	f := newFakeCDP()
	f.hydrateAfterEvals = 0
	s := newTestSession(t, f)

	callerLock.Lock()
	defer callerLock.Unlock()
	err := s.NavigateHomeInline(NavOptions{})
	assert.NoError(t, err)
}

// TestOnGenerationReady_BacksOffPollingAfterThreshold pins minor #6: a
// generation whose page never installs its command handler must not be
// polled at the flat cadence forever — onGenerationReady's own indefinite
// background retry backs off to a slower interval after readyPollBackoffAfter.
// Bounded callers (AwaitStage) are unaffected by this pacing, which this test
// does not exercise.
func TestOnGenerationReady_BacksOffPollingAfterThreshold(t *testing.T) {
	f := newFakeCDP()
	// handler never installs.
	s := newTestSession(t, f)
	s.pollInterval = 5 * time.Millisecond
	s.readyPollBackoffAfter = 30 * time.Millisecond
	s.readyPollBackoffInterval = 300 * time.Millisecond

	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.Generation() == 1 })

	// Let the flat-cadence window fully elapse plus a bit of the backoff
	// window, then snapshot the eval count.
	time.Sleep(80 * time.Millisecond)
	countAfterBackoffStarts := f.getEvalCount()

	// Sleep well past one more backoff-interval tick: at the flat 5ms cadence
	// this would rack up dozens more evals; backed off to 300ms it should add
	// at most one or two.
	time.Sleep(150 * time.Millisecond)
	countAfterWaiting := f.getEvalCount()

	added := countAfterWaiting - countAfterBackoffStarts
	assert.LessOrEqual(t, added, 2, "expected the poll cadence to back off, got %d more evals in 150ms", added)
}

// --- NavigateHome (async) coalescing -----------------------------------------

func TestNavigateHome_Async_Succeeds(t *testing.T) {
	f := newFakeCDP()
	f.hydrateAfterEvals = 0
	s := newTestSession(t, f)

	done := make(chan NavResult, 1)
	s.NavigateHome(NavOptions{}, func(res NavResult) { done <- res })

	select {
	case res := <-done:
		assert.Equal(t, NavExecuted, res.Outcome)
		assert.NoError(t, res.Err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for NavigateHome to complete")
	}
}

func TestNavigateHome_Async_SupersedesQueuedRequest(t *testing.T) {
	f := newFakeCDP()
	s := newTestSession(t, f)
	// Hold the overlay gate open for the FIRST call so it stays "in flight"
	// (well, gate-skipped, but still occupies navBusy) long enough for a
	// second call to queue behind it.
	release := make(chan struct{})
	var gateMu sync.Mutex
	first := true
	s.RegisterOverlayOwner("gate", func() bool {
		gateMu.Lock()
		isFirst := first
		first = false
		gateMu.Unlock()
		if isFirst {
			<-release
		}
		return true
	})

	firstDone := make(chan NavResult, 1)
	secondDone := make(chan NavResult, 1)
	s.NavigateHome(NavOptions{}, func(res NavResult) { firstDone <- res })
	// Give the first call time to enter navBusy before queuing the second.
	time.Sleep(20 * time.Millisecond)
	s.NavigateHome(NavOptions{}, func(res NavResult) { secondDone <- res })
	// A THIRD call must supersede the still-queued second one.
	thirdDone := make(chan NavResult, 1)
	s.NavigateHome(NavOptions{}, func(res NavResult) { thirdDone <- res })

	select {
	case res := <-secondDone:
		assert.Equal(t, NavSuperseded, res.Outcome)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the superseded callback")
	}

	close(release)
	select {
	case res := <-firstDone:
		assert.Equal(t, NavSkippedOverlay, res.Outcome)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the first call")
	}
	select {
	case res := <-thirdDone:
		assert.Equal(t, NavSkippedOverlay, res.Outcome)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the third call")
	}
}

// --- [M1] Inline vs async navigation concurrency -----------------------------

// TestNavigateHomeInline_SupersededByInFlightAsyncNavigation pins the M1 fix:
// Inline must never run navigateAndVerify concurrently with an already
// in-flight (or queued) async NavigateHome — that would let the two calls'
// terminal outcomes race NavigationPending's clear out from under each
// other. Inline detects the concurrent case and reports NavSuperseded
// immediately, rather than either blocking indefinitely or racing a second
// navigateAndVerify (which used to misclassify as NavExecuted with a
// ctx-canceled error).
func TestNavigateHomeInline_SupersededByInFlightAsyncNavigation(t *testing.T) {
	f := newFakeCDP()
	s := newTestSession(t, f)

	release := make(chan struct{})
	var mu sync.Mutex
	first := true
	s.RegisterOverlayOwner("gate", func() bool {
		mu.Lock()
		isFirst := first
		first = false
		mu.Unlock()
		if isFirst {
			<-release
		}
		return true
	})

	asyncDone := make(chan NavResult, 1)
	s.NavigateHome(NavOptions{}, func(res NavResult) { asyncDone <- res })
	// Give the async worker time to claim navBusy and block inside the
	// overlay probe.
	waitFor(t, time.Second, func() bool { return s.NavigationPending() })

	err := s.NavigateHomeInline(NavOptions{})
	assert.ErrorIs(t, err, ErrNavSuperseded)
	// The async navigation is still in flight: NavigationPending must still
	// read true (the refcount, not a bool two callers could clobber).
	assert.True(t, s.NavigationPending())

	close(release)
	select {
	case res := <-asyncDone:
		assert.Equal(t, NavSkippedOverlay, res.Outcome)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the async navigation to finish")
	}
	waitFor(t, time.Second, func() bool { return !s.NavigationPending() })
}

// TestNavigateHomeInline_HandsOffQueuedAsyncRequestOnFinish pins that a
// NavigateHome call arriving while Inline holds the slot is queued (exactly
// like two async calls would coalesce) and runs once Inline finishes,
// instead of being silently dropped.
func TestNavigateHomeInline_HandsOffQueuedAsyncRequestOnFinish(t *testing.T) {
	f := newFakeCDP()
	s := newTestSession(t, f)

	release := make(chan struct{})
	s.RegisterOverlayOwner("gate", func() bool {
		<-release
		return true
	})

	inlineDone := make(chan error, 1)
	go func() { inlineDone <- s.NavigateHomeInline(NavOptions{}) }()
	waitFor(t, time.Second, func() bool { return s.NavigationPending() })

	// A NavigateHome call arriving while Inline holds the slot must queue,
	// not be dropped.
	queuedDone := make(chan NavResult, 1)
	s.NavigateHome(NavOptions{}, func(res NavResult) { queuedDone <- res })

	close(release)

	select {
	case err := <-inlineDone:
		assert.ErrorIs(t, err, ErrNavSkippedOverlay)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for Inline to finish")
	}
	select {
	case res := <-queuedDone:
		assert.Equal(t, NavSkippedOverlay, res.Outcome, "the queued request must run after Inline releases the slot")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the queued request to run")
	}
}

// --- Reconciler ordering + re-run per generation -----------------------------

func TestReconcilers_RunInRegistrationOrderOnEveryGenerationReady(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)

	var mu sync.Mutex
	var order []string
	record := func(name string) func(context.Context) {
		return func(ctx context.Context) {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
		}
	}
	s.RegisterReconciler("first", record("first"))
	s.RegisterReconciler("second", record("second"))
	s.RegisterReconciler("third", record("third"))

	s.OnConnect()
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 3
	})
	mu.Lock()
	assert.Equal(t, []string{"first", "second", "third"}, order)
	mu.Unlock()

	// A second generation re-runs every reconciler again, in order.
	s.OnConnect()
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 6
	})
	mu.Lock()
	assert.Equal(t, []string{"first", "second", "third", "first", "second", "third"}, order)
	mu.Unlock()
}

func TestReconciler_AbortsWhenSupersededMidRun(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)

	var mu sync.Mutex
	var ran []string
	supersededOnce := false
	s.RegisterReconciler("slow-first", func(ctx context.Context) {
		mu.Lock()
		ran = append(ran, "slow-first")
		first := !supersededOnce
		supersededOnce = true
		mu.Unlock()
		if first {
			// Supersede THIS generation exactly once, from inside its own
			// reconciler run, and block until the supersession cancels it.
			s.OnConnect()
			<-ctx.Done()
		}
	})
	s.RegisterReconciler("second", func(ctx context.Context) {
		mu.Lock()
		ran = append(ran, "second")
		mu.Unlock()
	})

	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.Generation() == 2 })
	// Give the first generation's worker a moment to observe supersession and
	// bail before running "second" for gen 1.
	time.Sleep(30 * time.Millisecond)
	waitFor(t, time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		// gen1 runs slow-first only; gen2 (triggered from inside slow-first)
		// runs both reconcilers once it becomes ready.
		return len(ran) >= 3
	})
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []string{"slow-first", "slow-first", "second"}, ran)
}

// --- Overlay owner registration / StageReady park-style polling -------------

func TestStageReady_ReflectsCurrentGenerationOnly(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	f.hydrateAfterEvals = 0
	s := newTestSession(t, f)

	assert.False(t, s.StageReady(StageHandler))
	s.OnConnect()
	waitFor(t, time.Second, func() bool { return s.StageReady(StageHandler) })

	// A navigation bumps the generation; StageReady must reflect the NEW
	// generation (false until it, too, becomes ready).
	require.NoError(t, s.NavigateHomeInline(NavOptions{}))
	assert.True(t, s.StageReady(StageHandler), "inline verification already waited for the new generation")
}

func TestGenerationCounter_MonotonicAndUnique(t *testing.T) {
	f := newFakeCDP()
	f.handlerInstalled = true
	s := newTestSession(t, f)
	seen := map[uint64]bool{}
	for i := 0; i < 5; i++ {
		s.OnConnect()
		g := s.Generation()
		require.False(t, seen[g], "generation id reused: %d", g)
		seen[g] = true
	}
}

func TestStageString(t *testing.T) {
	assert.Equal(t, "target", StageTarget.String())
	assert.Equal(t, "handler", StageHandler.String())
	assert.Equal(t, "status", StageStatus.String())
	assert.Equal(t, "stage(99)", Stage(99).String())
}

func TestNavOutcomeString(t *testing.T) {
	assert.Equal(t, "executed", NavExecuted.String())
	assert.Equal(t, "skipped_overlay", NavSkippedOverlay.String())
	assert.Equal(t, "skipped_asleep", NavSkippedAsleep.String())
	assert.Equal(t, "superseded", NavSuperseded.String())
	assert.Equal(t, "evicted", NavEvicted.String())
	assert.Equal(t, "outcome(99)", NavOutcome(99).String())
}

func TestNavigateHomeInline_EvictedWhenSessionShuttingDown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	f := newFakeCDP()
	f.handlerInstalled = true
	s := New(ctx, f, nil, nil, zap.NewNop())
	s.pollInterval = 5 * time.Millisecond
	cancel()

	err := s.NavigateHomeInline(NavOptions{})
	assert.ErrorIs(t, err, ErrNavEvicted)
}

// Sanity check the nonce-extraction helper used by the fake against the exact
// expression shapes the package sends, since a mismatch here would silently
// make every gated test above pass for the wrong reason.
func TestFakeCDP_ExtractQuotedMatchesRealExpressionShapes(t *testing.T) {
	nonce := strconv.FormatInt(1234, 36)

	navNonceExpr := fmt.Sprintf(`JSON.stringify({nonce: window.__ffosNavNonce = %q})`, nonce)
	assert.Equal(t, nonce, extractQuoted(navNonceExpr, "window.__ffosNavNonce = "))

	probe := fmt.Sprintf(
		`JSON.stringify({ready: typeof window.handleCDPRequest === 'function' && window.__ffosNavNonce !== %q})`,
		nonce)
	assert.Equal(t, nonce, extractQuoted(probe, "window.__ffosNavNonce !== "))

	stamp := "42-abc123"
	docStampExpr := fmt.Sprintf(`JSON.stringify({stamped: Boolean(window.__ffosDocStamp = %q)})`, stamp)
	assert.Equal(t, stamp, extractQuoted(docStampExpr, "window.__ffosDocStamp = "))
}
