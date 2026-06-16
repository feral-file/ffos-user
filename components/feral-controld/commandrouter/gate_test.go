package commandrouter

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
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

	// The externally reachable cast path is heavier-weight than a cheap query.
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
}
