package commandrouter

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/control"
)

// stubHandler is a controllable inner Handler for gate tests.
type stubHandler struct {
	calls atomic.Int64
	// block, when non-nil, is received from inside Process so tests can hold a
	// command "in flight" to exercise concurrency/dedupe behavior.
	block chan struct{}
	// entered is signaled once per Process entry (after the call is counted).
	entered chan struct{}
	result  interface{}
	err     error
}

func (s *stubHandler) Process(ctx context.Context, command commands.Command) (interface{}, error) {
	s.calls.Add(1)
	if s.entered != nil {
		s.entered <- struct{}{}
	}
	if s.block != nil {
		<-s.block
	}
	return s.result, s.err
}

const testCmd = commands.Type("test")

func frozenClock() func() time.Time {
	now := time.Unix(0, 0)
	return func() time.Time { return now }
}

func TestNewGate_DisabledReturnsInner(t *testing.T) {
	inner := &stubHandler{}
	g := NewGate(inner, GateConfig{Enabled: false}, zap.NewNop())
	assert.Same(t, inner, g, "disabled gate must return the inner handler unchanged")
}

func TestGate_EmptyTypePassesThrough(t *testing.T) {
	inner := &stubHandler{result: "ok"}
	cfg := GateConfig{Enabled: true, MaxConcurrent: 1, now: frozenClock()}
	g := NewGate(inner, cfg, zap.NewNop())

	_, err := g.Process(context.Background(), commands.Command{Type: ""})
	require.NoError(t, err)
	assert.Equal(t, int64(1), inner.calls.Load())
}

func TestGate_RateLimitRejectsBeyondBurst(t *testing.T) {
	inner := &stubHandler{result: "ok"}
	cfg := GateConfig{
		Enabled:       true,
		MaxConcurrent: 16,
		Policies:      map[commands.Type]Policy{testCmd: {Rate: 1, Burst: 2, Weight: 1}},
		now:           frozenClock(), // no refill during the test
	}
	g := NewGate(inner, cfg, zap.NewNop())

	cmd := commands.Command{Type: testCmd, Arguments: map[string]any{"i": 1}}

	_, err1 := g.Process(context.Background(), cmd)
	_, err2 := g.Process(context.Background(), cmd)
	_, err3 := g.Process(context.Background(), cmd)

	require.NoError(t, err1)
	require.NoError(t, err2)
	require.Error(t, err3)
	assert.True(t, IsRateLimited(err3), "third call rejected by rate limit")
	assert.Equal(t, int64(2), inner.calls.Load(), "only burst-many commands reach inner")
}

func TestGate_DedupeCollapsesConcurrentIdentical(t *testing.T) {
	inner := &stubHandler{
		result:  "shared",
		block:   make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	cfg := GateConfig{
		Enabled:       true,
		MaxConcurrent: 16,
		// Rate 0 => unlimited, so dedupe (not rate) is what's under test.
		Policies: map[commands.Type]Policy{testCmd: {Rate: 0, Weight: 1, Dedupe: true}},
		now:      frozenClock(),
	}
	g := NewGate(inner, cfg, zap.NewNop())
	cmd := commands.Command{Type: testCmd, Arguments: map[string]any{"url": "feed"}}

	const n = 8
	var wg sync.WaitGroup
	results := make([]interface{}, n)
	errs := make([]error, n)

	// Launch the leader first and wait until it is inside inner.Process so the
	// followers join the same in-flight singleflight call.
	wg.Add(1)
	go func() {
		defer wg.Done()
		results[0], errs[0] = g.Process(context.Background(), cmd)
	}()
	<-inner.entered

	for i := 1; i < n; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			results[idx], errs[idx] = g.Process(context.Background(), cmd)
		}(i)
	}

	// Give followers a moment to coalesce onto the leader, then release.
	time.Sleep(50 * time.Millisecond)
	close(inner.block)
	wg.Wait()

	assert.Equal(t, int64(1), inner.calls.Load(), "identical in-flight commands collapse to one execution")
	for i := 0; i < n; i++ {
		require.NoError(t, errs[i])
		assert.Equal(t, "shared", results[i])
	}
}

func TestGate_ConcurrencyBudgetExhausted(t *testing.T) {
	inner := &stubHandler{
		result:  "ok",
		block:   make(chan struct{}),
		entered: make(chan struct{}, 2),
	}
	cfg := GateConfig{
		Enabled:       true,
		MaxConcurrent: 2,
		// Rate 0 => unlimited; distinct args avoid dedupe so each call competes
		// for a concurrency slot.
		Policies: map[commands.Type]Policy{testCmd: {Rate: 0, Weight: 1}},
		now:      frozenClock(),
	}
	g := NewGate(inner, cfg, zap.NewNop())

	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cmd := commands.Command{Type: testCmd, Arguments: map[string]any{"i": idx}}
			_, _ = g.Process(context.Background(), cmd)
		}(i)
	}
	// Wait until both slots are held inside inner.Process.
	<-inner.entered
	<-inner.entered

	// A third command cannot acquire a slot and is rejected immediately.
	_, err := g.Process(context.Background(), commands.Command{Type: testCmd, Arguments: map[string]any{"i": 99}})
	require.Error(t, err)
	assert.True(t, IsRateLimited(err), "concurrency budget exhausted is reported as rate limited")

	close(inner.block)
	wg.Wait()
	assert.Equal(t, int64(2), inner.calls.Load(), "rejected command never reached inner")
}

// A configured MaxConcurrent smaller than a command's policy weight must not
// permanently disable that command. Before the clamp, TryAcquire(weight) on a
// too-small semaphore failed forever — even on an idle device — silently
// bricking heavy commands (e.g. the default Weight 4 against a hand-tuned
// maxConcurrent of 1-3). Regression for the command-storm review finding.
func TestGate_WeightAboveBudgetStillAdmitsWhenIdle(t *testing.T) {
	inner := &stubHandler{result: "ok"}
	cfg := GateConfig{
		Enabled:       true,
		MaxConcurrent: 2, // below the policy weight of 4
		Policies:      map[commands.Type]Policy{testCmd: {Rate: 0, Weight: 4}},
		now:           frozenClock(),
	}
	g := NewGate(inner, cfg, zap.NewNop())

	res, err := g.Process(context.Background(), commands.Command{Type: testCmd, Arguments: map[string]any{"i": 1}})
	require.NoError(t, err, "a weight-above-budget command must still be admitted when idle")
	assert.Equal(t, "ok", res)
	assert.Equal(t, int64(1), inner.calls.Load())
}

// With the weight clamped to the budget, a heavy command reserves the whole
// budget while in flight, so a concurrent heavy command is rejected (legibly
// rate-limited) rather than silently never admitted.
func TestGate_WeightAboveBudgetClampsToFullBudget(t *testing.T) {
	inner := &stubHandler{
		result:  "ok",
		block:   make(chan struct{}),
		entered: make(chan struct{}, 1),
	}
	cfg := GateConfig{
		Enabled:       true,
		MaxConcurrent: 2, // below the policy weight of 4
		Policies:      map[commands.Type]Policy{testCmd: {Rate: 0, Weight: 4}},
		now:           frozenClock(),
	}
	g := NewGate(inner, cfg, zap.NewNop())

	// Hold one heavy command in flight; clamped to weight 2 it occupies the
	// entire budget.
	go func() {
		_, _ = g.Process(context.Background(), commands.Command{Type: testCmd, Arguments: map[string]any{"i": 1}})
	}()
	<-inner.entered

	_, err := g.Process(context.Background(), commands.Command{Type: testCmd, Arguments: map[string]any{"i": 2}})
	require.Error(t, err)
	assert.True(t, IsRateLimited(err), "second heavy command is rejected while the budget is reserved")

	close(inner.block)
	assert.Equal(t, int64(1), inner.calls.Load(), "rejected command never reached inner")
}

// A storm of unique, attacker-controlled command names must share the single
// default budget, not each receive a fresh token bucket. Otherwise storm
// protection is trivially bypassed (every new name buys a fresh burst) and
// limiter state grows without bound — one bucket per attacker string. Both
// hazards are regressions from the command-storm review.
func TestGate_UnclassifiedCommandsShareDefaultBudget(t *testing.T) {
	inner := &stubHandler{result: "ok"}
	cfg := GateConfig{
		Enabled:       true,
		MaxConcurrent: 16,
		// No explicit policies: every command falls through to Default. Rate > 0
		// so the bucket actually limits; the frozen clock means no mid-test refill.
		Default: Policy{Rate: 1, Burst: 3, Weight: 1},
		now:     frozenClock(),
	}
	g := NewGate(inner, cfg, zap.NewNop())

	accepted := 0
	var lastErr error
	for i := 0; i < 20; i++ {
		cmd := commands.Command{Type: commands.Type(fmt.Sprintf("unknown-%d", i))}
		if _, err := g.Process(context.Background(), cmd); err == nil {
			accepted++
		} else {
			lastErr = err
		}
	}

	// Only burst-many commands pass, even though every name is unique: they
	// share one bucket. Per-name buckets would have accepted all 20.
	assert.Equal(t, 3, accepted, "unclassified commands share the default burst budget")
	require.Error(t, lastErr)
	assert.True(t, IsRateLimited(lastErr))

	// And the whole storm allocated exactly one token bucket.
	gate := g.(*gate)
	assert.Equal(t, 1, len(gate.limiters.buckets), "a storm of unique names must not allocate one bucket per name")
}

func TestArgsHash(t *testing.T) {
	// Order-independent: maps built in different orders hash identically.
	a := map[string]any{"a": 1.0, "b": "x", "c": true}
	b := map[string]any{"c": true, "b": "x", "a": 1.0}
	assert.Equal(t, argsHash(a), argsHash(b))

	// Different values hash differently.
	assert.NotEqual(t, argsHash(a), argsHash(map[string]any{"a": 2.0, "b": "x", "c": true}))

	// Empty/nil hash to the empty string (dedupe by type alone).
	assert.Equal(t, "", argsHash(nil))
	assert.Equal(t, "", argsHash(map[string]any{}))

	// Nested structures are handled deterministically.
	n1 := map[string]any{"outer": map[string]any{"x": 1.0, "y": 2.0}}
	n2 := map[string]any{"outer": map[string]any{"y": 2.0, "x": 1.0}}
	assert.Equal(t, argsHash(n1), argsHash(n2))
}

func TestDefaultGateConfig_ClassifiesCommands(t *testing.T) {
	cfg := DefaultGateConfig()
	require.True(t, cfg.Enabled)

	// Disruptive commands are deduped and tightly limited.
	reboot := cfg.Policies[commands.CMD_REBOOT]
	assert.True(t, reboot.Dedupe)
	assert.Less(t, reboot.Rate, 1.0)

	// The externally reachable display path is heavier-weight than a cheap query.
	cast := cfg.Policies[commands.CMD_DISPLAY_PLAYLIST]
	query := cfg.Policies[commands.CMD_DEVICE_STATUS]
	assert.Greater(t, cast.Weight, query.Weight)

	// Input gestures share one limiter group.
	assert.Equal(t, inputGroup, cfg.Policies[commands.CMD_MOUSE_DRAG_EVENT].Group)
	assert.Equal(t, inputGroup, cfg.Policies[commands.CMD_ZOOM_GESTURE].Group)

	// Slow, disruptive panel writes (incl. power) are not left at the generous
	// default: they carry a heavier weight than a cheap query.
	ddc := cfg.Policies[commands.CMD_DDC_PANEL_CONTROL]
	assert.Greater(t, ddc.Weight, query.Weight)
	assert.NotEqual(t, cfg.Default.Rate, ddc.Rate, "DDC control is explicitly classified, not Default")

	// User-initiated wake must not be throttled as hard as a reboot, so rapid
	// taps are not rejected (the executor coalesces bursts safely).
	wake := cfg.Policies[commands.CMD_WAKE_NOW]
	assert.Greater(t, wake.Rate, reboot.Rate)

	// Screen rotation is a relative step command: reaching a target
	// orientation takes up to 3 rapid taps, so it must not sit on the
	// 1-per-5s disruptive budget (the third tap used to be rejected with a
	// 429), and it must not dedupe — coalescing two overlapping identical
	// taps would silently drop a rotation.
	rotation := cfg.Policies[commands.CMD_SCREEN_ROTATION]
	assert.Greater(t, rotation.Rate, reboot.Rate)
	assert.GreaterOrEqual(t, rotation.Burst, 3)
	assert.False(t, rotation.Dedupe)

	// runNetworkDiagnostics is the security control for a probe command
	// reachable from the unauthenticated LAN hub: it holds a dispatch slot
	// for a live ladder pass, so it must never regress to the generous
	// Default budget, must stay deduped (identical in-flight requests share
	// one run), and must weigh more than a cheap query.
	netdiag, ok := cfg.Policies[commands.CMD_RUN_NETWORK_DIAGNOSTICS]
	require.True(t, ok, "runNetworkDiagnostics must be explicitly classified, not Default")
	assert.True(t, netdiag.Dedupe)
	assert.Less(t, netdiag.Rate, 1.0)
	assert.Greater(t, netdiag.Weight, query.Weight)

	// setDeviceName is a persisted eMMC write plus a full mDNS Stop+Start
	// re-registration per accepted change, reachable from the unauthenticated
	// LAN hub, and the mediator's unchanged-name early return is defeated by
	// alternating two names — so it must never regress to the Default budget.
	rename, ok := cfg.Policies[commands.CMD_SET_DEVICE_NAME]
	require.True(t, ok, "setDeviceName must be explicitly classified, not Default")
	assert.True(t, rename.Dedupe)
	assert.Less(t, rename.Rate, 1.0)

}

// TestDefaultGateConfig_ClassifiesOfflineCacheCommands is the regression
// test pinning that the five offline-cache commands (see commands/types.go's
// "Offline artwork caching commands" block) are classified in Policies
// rather than silently inheriting the generous Default policy — undersized
// for downloadPlaylist/downloadPlaylistItem, which do DP1 resolution plus
// enqueue into offlinecache.Service's jobQueue (bounded at
// defaultMaxQueueLen, but not a limit a realistic burst is meant to hit).
func TestDefaultGateConfig_ClassifiesOfflineCacheCommands(t *testing.T) {
	cfg := DefaultGateConfig()
	query := cfg.Policies[commands.CMD_DEVICE_STATUS]

	// Downloads are explicitly classified, not left at Default, and carry
	// the same heavier weight as the externally reachable display path.
	cast := cfg.Policies[commands.CMD_DISPLAY_PLAYLIST]
	for _, cmd := range []commands.Type{commands.CMD_DOWNLOAD_PLAYLIST_ITEM, commands.CMD_DOWNLOAD_PLAYLIST} {
		p := cfg.Policies[cmd]
		assert.NotEqual(t, cfg.Default.Rate, p.Rate, "%s must not silently fall through to Default", cmd)
		assert.Equal(t, cast.Weight, p.Weight, "%s should be classified as heavy as displayPlaylist", cmd)
	}

	// Clears are explicitly classified, not left at Default, and carry a
	// heavier weight than a cheap query (disk I/O + a store-wide GC sweep).
	for _, cmd := range []commands.Type{commands.CMD_CLEAR_PLAYLIST_ITEM_CACHE, commands.CMD_CLEAR_PLAYLIST_CACHE} {
		p := cfg.Policies[cmd]
		assert.NotEqual(t, cfg.Default.Rate, p.Rate, "%s must not silently fall through to Default", cmd)
		assert.Greater(t, p.Weight, query.Weight, "%s should be heavier than a cheap query", cmd)
	}

	// Status is a cheap, dedupable query, same class as getDeviceStatus.
	status := cfg.Policies[commands.CMD_GET_OFFLINE_CACHE_STATUS]
	assert.NotEqual(t, cfg.Default.Rate, status.Rate, "getOfflineCacheStatus must not silently fall through to Default")
	assert.Equal(t, query.Weight, status.Weight)
}

// TestDefaultGateConfig_ClassifiesHistoryAndPolicyCommands pins that all four
// commands behind the app's History and Content screens are classified rather
// than inheriting the generous Default. Both content-policy commands hold the
// policy-store lock across a CDP round trip, and setContentPolicy also does a
// persisted .state write, so Default (10/s, burst 20, no dedupe) would let one
// unauthenticated LAN caller park the global concurrency budget on them.
func TestDefaultGateConfig_ClassifiesHistoryAndPolicyCommands(t *testing.T) {
	cfg := DefaultGateConfig()
	query := cfg.Policies[commands.CMD_DEVICE_STATUS]
	reboot := cfg.Policies[commands.CMD_REBOOT]

	for _, cmd := range []commands.Type{
		commands.CMD_GET_RECENTLY_PLAYED,
		commands.CMD_PLAY_RECENTLY_PLAYED,
		commands.CMD_GET_CONTENT_POLICY,
		commands.CMD_SET_CONTENT_POLICY,
	} {
		p, ok := cfg.Policies[cmd]
		require.True(t, ok, "%s must be explicitly classified, not Default", cmd)
		assert.NotEqual(t, cfg.Default.Rate, p.Rate, "%s must not silently fall through to Default", cmd)
	}

	// Reading the policy is a cheap, dedupable query, same class as
	// getDeviceStatus. Dedupe is what bounds it, which is why the handler
	// rejects a non-empty get request instead of ignoring it.
	get := cfg.Policies[commands.CMD_GET_CONTENT_POLICY]
	assert.True(t, get.Dedupe)
	assert.Equal(t, query.Weight, get.Weight)

	// Writing it is a persisted flash write: the same tier as the other
	// persisted-write commands, never looser than a reboot.
	set := cfg.Policies[commands.CMD_SET_CONTENT_POLICY]
	assert.True(t, set.Dedupe)
	assert.LessOrEqual(t, set.Rate, reboot.Rate)
}

// TestGate_DedupeIsPerPrincipal pins feral-file#3551 F4: identical in-flight
// commands from DIFFERENT controllers execute separately; the same
// controller's duplicates (and plaintext duplicates) still collapse.
func TestGate_DedupeIsPerPrincipal(t *testing.T) {
	inner := &stubHandler{result: "r", block: make(chan struct{}), entered: make(chan struct{}, 4)}
	cfg := GateConfig{
		Enabled: true, MaxConcurrent: 16,
		Policies: map[commands.Type]Policy{testCmd: {Rate: 0, Weight: 1, Dedupe: true}},
		now:      frozenClock(),
	}
	g := NewGate(inner, cfg, zap.NewNop())
	cmd := commands.Command{Type: testCmd, Arguments: map[string]any{"a": 1}}
	a := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "a", Role: control.RoleOwner})
	b := control.WithPrincipal(context.Background(), control.Principal{ControllerID: "b", Role: control.RoleOwner})

	var wg sync.WaitGroup
	run := func(ctx context.Context) {
		wg.Add(1)
		go func() { defer wg.Done(); _, _ = g.Process(ctx, cmd) }()
	}
	run(a)
	<-inner.entered
	run(b)
	<-inner.entered // b did not join a's flight
	run(context.Background())
	<-inner.entered // plaintext is its own (pre-existing) key
	run(a)          // joins a's flight
	// Give the follower a moment to coalesce (same idiom as
	// TestGate_DedupeCollapsesConcurrentIdentical), then release.
	time.Sleep(50 * time.Millisecond)
	close(inner.block)
	wg.Wait()
	if got := inner.calls.Load(); got != 3 {
		t.Fatalf("inner ran %d times, want 3 (a, b, plaintext)", got)
	}
}

// TestDefaultGateConfig_SeriesPlaybackSetIsNotDeduped pins feralfile-bot F8
// (PR #426): setSeriesPlayback must share displayPlaylist's rate and weight
// but NOT its dedupe, unlike every other heavy command.
func TestDefaultGateConfig_SeriesPlaybackSetIsNotDeduped(t *testing.T) {
	cfg := DefaultGateConfig()
	cast := cfg.Policies[commands.CMD_DISPLAY_PLAYLIST]
	set := cfg.Policies[commands.CMD_SET_SERIES_PLAYBACK]

	assert.Equal(t, cast.Rate, set.Rate, "setSeriesPlayback should carry the same rate as the heavy tier")
	assert.Equal(t, cast.Weight, set.Weight, "setSeriesPlayback should carry the same weight as the heavy tier")
	assert.False(t, set.Dedupe, "setSeriesPlayback must not dedupe: a later call repeating an earlier in-flight call's mode must still reach the handler")

	// getSeriesPlayback is a read-only query and keeps dedupe, like
	// getContentPolicy.
	get := cfg.Policies[commands.CMD_GET_SERIES_PLAYBACK]
	assert.True(t, get.Dedupe)
}

// seriesPlaybackModeProbe is a Handler that reports every call it receives
// and blocks only the very first call it is given, letting every later call
// (regardless of mode) return immediately — modeling the real
// recastForMode preflight, which only ever stalls the one call racing it.
type seriesPlaybackModeProbe struct {
	calls     atomic.Int64
	mu        sync.Mutex
	seen      []string
	entered   chan struct{}
	block     chan struct{}
	firstDone atomic.Bool
}

func (p *seriesPlaybackModeProbe) Process(_ context.Context, command commands.Command) (interface{}, error) {
	p.calls.Add(1)
	mode, _ := command.Arguments["seriesPlayback"].(string)
	p.mu.Lock()
	p.seen = append(p.seen, mode)
	p.mu.Unlock()
	// sync.Once.Do would block a second CONCURRENT caller until the first
	// invocation's function returns, which is the opposite of what this probe
	// needs (request B must run to completion while request A is still
	// stalled) — so the "am I first" check is a plain CAS instead.
	if p.firstDone.CompareAndSwap(false, true) {
		p.entered <- struct{}{}
		<-p.block
	}
	return map[string]interface{}{"ok": true, "seriesPlayback": mode}, nil
}

// TestGate_SetSeriesPlaybackDoesNotDropALaterIdenticalRequest is the
// regression test for feralfile-bot F8 (PR #426). With dedupe left on,
// request C ("first") below used to be coalesced into request A's still
// in-flight singleflight call (byte-identical arguments), so C never ran its
// own execution. Request B ("full"), with different arguments, ran and
// completed independently in between. The gate then returned A's own
// ("first") result to C, even though B's call was the last to actually
// persist and re-display — exactly the "controller is told its setting saved
// when it did not" bug the finding reported. With dedupe off, every accepted
// call reaches the handler, so C runs for real.
func TestGate_SetSeriesPlaybackDoesNotDropALaterIdenticalRequest(t *testing.T) {
	inner := &seriesPlaybackModeProbe{
		entered: make(chan struct{}, 1),
		block:   make(chan struct{}),
	}
	cfg := DefaultGateConfig()
	cfg.now = frozenClock()
	g := NewGate(inner, cfg, zap.NewNop())

	first := commands.Command{Type: commands.CMD_SET_SERIES_PLAYBACK, Arguments: map[string]any{"seriesPlayback": "first"}}
	full := commands.Command{Type: commands.CMD_SET_SERIES_PLAYBACK, Arguments: map[string]any{"seriesPlayback": "full"}}

	// Request A ("first") enters and stalls, mirroring a live recast's
	// blocking preflight.
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = g.Process(context.Background(), first)
	}()
	<-inner.entered

	// Request B ("full") has a different argsHash, so it runs to completion
	// on its own, independently of A.
	_, err := g.Process(context.Background(), full)
	require.NoError(t, err)

	// Request C, byte-identical to A, arrives while A is still in flight.
	wg.Add(1)
	go func() {
		defer wg.Done()
		_, _ = g.Process(context.Background(), first)
	}()

	time.Sleep(50 * time.Millisecond)
	close(inner.block)
	wg.Wait()

	assert.Equal(t, int64(3), inner.calls.Load(), "a later setSeriesPlayback call must reach the handler even when it repeats an earlier, still in-flight request's mode")
	inner.mu.Lock()
	defer inner.mu.Unlock()
	assert.Equal(t, []string{"first", "full", "first"}, inner.seen, "each accepted call must run independently, in arrival order")
}
