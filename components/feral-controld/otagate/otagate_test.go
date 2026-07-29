package otagate

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"
)

// localDeps builds Deps for a locally-driven gate whose version check always
// returns the given manifest and whose runner follows the script.
func localDeps(current string, manifest string, runner UpdateRunner, clock *fakeClock) Deps {
	return Deps{
		HTTP: &fakeHTTP{do: func(*http.Request) (*http.Response, error) {
			return jsonResponse(200, manifest), nil
		}},
		Clock:  clock,
		Runner: runner,
		Config: fakeConfig{branch: "b", version: current, endpoint: "https://x"},
	}
}

// TestSingleFlightCoalesces mirrors the feral-setupd ownership-guard intent
// (device_ownership_is_exclusive_across_entry_points): concurrent update requests
// must not each launch an updater. Here they coalesce onto one flight, so the
// runner and the version check each run exactly once for N parallel callers.
func TestSingleFlightCoalesces(t *testing.T) {
	const callers = 8
	runner := &fakeRunner{
		results: []error{nil},
		gate:    make(chan struct{}),
		entered: make(chan struct{}),
	}
	http := &fakeHTTP{do: func(*http.Request) (*http.Response, error) {
		return jsonResponse(200, okManifest("1.0.0", "0.9.0", "2.0.0")), nil
	}}
	gate := New(Deps{
		HTTP:   http,
		Clock:  newFakeClock(),
		Runner: runner,
		Config: fakeConfig{branch: "b", version: "1.0.0", endpoint: "https://x"},
	})

	var start sync.WaitGroup
	start.Add(callers)
	var done sync.WaitGroup
	done.Add(callers)
	for i := 0; i < callers; i++ {
		go func() {
			defer done.Done()
			start.Done()
			_, _ = gate.RequestUpdate(context.Background())
		}()
	}

	// Wait for the leader to reach the (blocked) runner, then give the followers a
	// moment to park inside singleflight before releasing the one flight.
	<-runner.entered
	start.Wait()
	time.Sleep(50 * time.Millisecond)
	close(runner.gate)
	done.Wait()

	if got := runner.calls(); got != 1 {
		t.Errorf("runner called %d times, want 1 (callers coalesced)", got)
	}
	if got := http.calls(); got != 1 {
		t.Errorf("version check ran %d times, want 1 (callers coalesced)", got)
	}
}

// TestRetryLadderTiming mirrors update_coordinator.rs::update: a persistently
// transient updater failure is retried up to MaxUpdateRetries with 2^attempt
// second backoff (2s, 4s), then latches a permanent failure.
func TestRetryLadderTiming(t *testing.T) {
	clock := newFakeClock()
	runner := &fakeRunner{results: []error{transientRunErr()}} // always transient
	gate := New(localDeps("1.0.0", okManifest("1.0.0", "0.9.0", "2.0.0"), runner, clock))

	res, err := gate.RequestUpdate(context.Background())
	if err == nil {
		t.Fatal("expected permanent failure after exhausting the ladder")
	}
	if res != ResultUpdateStarted {
		t.Errorf("result = %v, want ResultUpdateStarted", res)
	}
	if got := runner.calls(); got != MaxUpdateRetries {
		t.Errorf("runner attempts = %d, want %d", got, MaxUpdateRetries)
	}
	// MaxUpdateRetries attempts => MaxUpdateRetries-1 backoffs: 2s then 4s.
	wantSleeps := []time.Duration{2 * time.Second, 4 * time.Second}
	got := clock.recordedSleeps()
	if len(got) != len(wantSleeps) {
		t.Fatalf("backoffs = %v, want %v", got, wantSleeps)
	}
	for i := range wantSleeps {
		if got[i] != wantSleeps[i] {
			t.Errorf("backoff[%d] = %v, want %v", i, got[i], wantSleeps[i])
		}
	}
	// A transient exhaustion latches a permanent failure.
	if fs := gate.Failure(); !fs.Failed {
		t.Error("expected latched failure after ladder exhaustion")
	}
}

// TestLadderSucceedsAfterTransientRetry: a transient failure then success stops
// retrying, clears the latch, and reports ResultUpdateStarted.
func TestLadderSucceedsAfterTransientRetry(t *testing.T) {
	clock := newFakeClock()
	runner := &fakeRunner{results: []error{transientRunErr(), nil}}
	gate := New(localDeps("1.0.0", okManifest("1.0.0", "0.9.0", "2.0.0"), runner, clock))

	res, err := gate.RequestUpdate(context.Background())
	if err != nil {
		t.Fatalf("expected success after one retry, got %v", err)
	}
	if res != ResultUpdateStarted {
		t.Errorf("result = %v, want ResultUpdateStarted", res)
	}
	if got := runner.calls(); got != 2 {
		t.Errorf("runner attempts = %d, want 2", got)
	}
	if got := clock.recordedSleeps(); len(got) != 1 || got[0] != 2*time.Second {
		t.Errorf("backoffs = %v, want [2s]", got)
	}
	if fs := gate.Failure(); fs.Failed {
		t.Error("latch must be clear after a successful update")
	}
}

// TestPermanentFailureLatchesImmediately: a permanent updater error is not
// retried, latches, and fires the OnPermanentFailure callback.
func TestPermanentFailureLatchesImmediately(t *testing.T) {
	clock := newFakeClock()
	runner := &fakeRunner{results: []error{permanentRunErr()}}
	gate := New(localDeps("1.0.0", okManifest("1.0.0", "0.9.0", "2.0.0"), runner, clock))

	var cbFires int
	var cbState FailureState
	gate.OnPermanentFailure(func(fs FailureState) {
		cbFires++
		cbState = fs
	})

	_, err := gate.RequestUpdate(context.Background())
	if err == nil {
		t.Fatal("expected permanent failure")
	}
	if got := runner.calls(); got != 1 {
		t.Errorf("runner attempts = %d, want 1 (no retry for permanent)", got)
	}
	if len(clock.recordedSleeps()) != 0 {
		t.Errorf("permanent failure must not back off, got %v", clock.recordedSleeps())
	}
	if cbFires != 1 || !cbState.Failed {
		t.Errorf("callback fired=%d state.Failed=%v, want 1/true", cbFires, cbState.Failed)
	}
	if fs := gate.Failure(); !fs.Failed || fs.Err == nil {
		t.Errorf("Failure() = %+v, want latched with error", fs)
	}
}

// TestVersionCheckFailureDoesNotLatch: a failed version check is not an update
// failure; it returns ResultVersionCheckFailed and leaves the latch clear.
func TestVersionCheckFailureDoesNotLatch(t *testing.T) {
	runner := &fakeRunner{results: []error{nil}}
	gate := New(Deps{
		HTTP: &fakeHTTP{do: func(*http.Request) (*http.Response, error) {
			return jsonResponse(503, "unavailable"), nil
		}},
		Clock:  newFakeClock(),
		Runner: runner,
		Config: fakeConfig{branch: "b", version: "1.0.0", endpoint: "https://x"},
	})

	res, err := gate.EnsureLatestBeforeClaim(context.Background())
	if err == nil {
		t.Fatal("expected version-check error")
	}
	if res != ResultVersionCheckFailed {
		t.Errorf("result = %v, want ResultVersionCheckFailed", res)
	}
	if runner.calls() != 0 {
		t.Error("runner must not run when the version check fails")
	}
	if fs := gate.Failure(); fs.Failed {
		t.Error("a version-check failure must NOT latch a permanent update failure")
	}
}

// TestTooOldToUpgrade: below the minimum upgradeable version returns
// ResultTooOldToUpgrade without running the updater or latching.
func TestTooOldToUpgrade(t *testing.T) {
	runner := &fakeRunner{results: []error{nil}}
	gate := New(localDeps("0.5.0", okManifest("1.0.0", "0.9.0", "2.0.0"), runner, newFakeClock()))

	res, err := gate.EnsureLatestBeforeClaim(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != ResultTooOldToUpgrade {
		t.Errorf("result = %v, want ResultTooOldToUpgrade", res)
	}
	if runner.calls() != 0 {
		t.Error("runner must not run for a too-old device")
	}
}

// TestModeRequiredVsAvailable: a build at/above min-runtime but below latest needs
// no update under ModeRequired (pre-claim gate) yet would under ModeAvailable
// (user-triggered).
func TestModeRequiredVsAvailable(t *testing.T) {
	manifest := okManifest("1.2.0", "0.9.0", "1.5.0")

	// ModeRequired via EnsureLatestBeforeClaim: current 1.3.0 >= min-runtime 1.2.0.
	reqRunner := &fakeRunner{results: []error{nil}}
	reqGate := New(localDeps("1.3.0", manifest, reqRunner, newFakeClock()))
	res, err := reqGate.EnsureLatestBeforeClaim(context.Background())
	if err != nil {
		t.Fatalf("required-mode error: %v", err)
	}
	if res != ResultNoUpdateNeeded {
		t.Errorf("required-mode result = %v, want ResultNoUpdateNeeded", res)
	}
	if reqRunner.calls() != 0 {
		t.Error("required mode must not update when at/above min-runtime")
	}

	// ModeAvailable via RequestUpdate: current 1.3.0 < latest 1.5.0.
	availRunner := &fakeRunner{results: []error{nil}}
	availGate := New(localDeps("1.3.0", manifest, availRunner, newFakeClock()))
	res, err = availGate.RequestUpdate(context.Background())
	if err != nil {
		t.Fatalf("available-mode error: %v", err)
	}
	if res != ResultUpdateStarted {
		t.Errorf("available-mode result = %v, want ResultUpdateStarted", res)
	}
	if availRunner.calls() != 1 {
		t.Errorf("available mode should run updater once, got %d", availRunner.calls())
	}
}

// TestEnsureLatestAtStartupIsModeRequired pins the boot-time entry point to
// Required-mode semantics (the setupd on_startup_with_internet port): a build
// below min_runtime_version updates, a build at/above it does NOT — even when a
// newer latest_version exists (optional updates stay with the daily timer and
// the user-triggered command, exactly as on v1.0.21).
func TestEnsureLatestAtStartupIsModeRequired(t *testing.T) {
	manifest := okManifest("1.2.0", "0.9.0", "1.5.0")

	// Below min-runtime (a force release): the updater must run.
	forcedRunner := &fakeRunner{results: []error{nil}}
	forcedGate := New(localDeps("1.0.0", manifest, forcedRunner, newFakeClock()))
	res, err := forcedGate.EnsureLatestAtStartup(context.Background())
	if err != nil {
		t.Fatalf("forced-startup error: %v", err)
	}
	if res != ResultUpdateStarted {
		t.Errorf("forced-startup result = %v, want ResultUpdateStarted", res)
	}
	if forcedRunner.calls() != 1 {
		t.Errorf("forced startup should run updater once, got %d", forcedRunner.calls())
	}

	// At min-runtime but below latest: startup must NOT update.
	satisfiedRunner := &fakeRunner{results: []error{nil}}
	satisfiedGate := New(localDeps("1.2.0", manifest, satisfiedRunner, newFakeClock()))
	res, err = satisfiedGate.EnsureLatestAtStartup(context.Background())
	if err != nil {
		t.Fatalf("satisfied-startup error: %v", err)
	}
	if res != ResultNoUpdateNeeded {
		t.Errorf("satisfied-startup result = %v, want ResultNoUpdateNeeded", res)
	}
	if satisfiedRunner.calls() != 0 {
		t.Error("startup must not update a build that satisfies min-runtime")
	}
}

// TestUpdateProgressReachesOnProgress asserts the runner's parsed percent is
// carried through the gate to Deps.OnProgress in order (the seam devicectl points
// at the setupui updating overlay). A progress line with no percent surfaces as
// -1; the gate forwards it as-is and leaves the skip policy to the consumer.
func TestUpdateProgressReachesOnProgress(t *testing.T) {
	cases := []struct {
		name string
		emit []int
		want []int
	}{
		{name: "percent lines forwarded in order", emit: []int{30, 60, 100}, want: []int{30, 60, 100}},
		{name: "percent-less line surfaces as -1", emit: []int{-1, 50, 100}, want: []int{-1, 50, 100}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			runner := &fakeRunner{results: []error{nil}, emit: tc.emit}
			deps := localDeps("1.0.0", okManifest("1.0.0", "0.9.0", "2.0.0"), runner, newFakeClock())
			var got []int
			deps.OnProgress = func(pct int) { got = append(got, pct) }
			gate := New(deps)

			res, err := gate.RequestUpdate(context.Background())
			if err != nil {
				t.Fatalf("RequestUpdate error: %v", err)
			}
			if res != ResultUpdateStarted {
				t.Fatalf("result = %v, want ResultUpdateStarted", res)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("OnProgress received %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Errorf("OnProgress[%d] = %d, want %d", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestCanceledContextDoesNotLatch is the ctx-capture regression: the shared
// single-flight runs under the FIRST caller's ctx, so a canceled/expired ctx is
// the caller going away — not evidence the device cannot update — and must not
// latch a permanent failure that would then poison the outcome for every joiner
// and persist until the next explicit retry.
func TestCanceledContextDoesNotLatch(t *testing.T) {
	clock := newFakeClock()
	// The runner reports the cancellation the real systemd runner would surface
	// ("context canceled" classifies as permanent — exactly the bogus-latch trap).
	runner := &fakeRunner{results: []error{context.Canceled}}
	gate := New(localDeps("1.0.0", okManifest("1.0.0", "0.9.0", "2.0.0"), runner, clock))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := gate.RequestUpdate(ctx)
	if err == nil {
		t.Fatal("expected the canceled run to surface an error")
	}
	if fs := gate.Failure(); fs.Failed {
		t.Errorf("a canceled ctx latched a permanent failure: %+v", fs)
	}
}

// TestNoUpdateNeededClearsStaleLatch: a latched permanent failure clears once a
// later check shows the device satisfies the gate (e.g. the distributor lowered
// min_runtime_version, or the device was updated out of band). Otherwise the
// long-lived Gate keeps reporting a now-healthy device as permanently failed.
func TestNoUpdateNeededClearsStaleLatch(t *testing.T) {
	var mu sync.Mutex
	manifest := okManifest("2.0.0", "0.9.0", "2.0.0") // 1.0.0 requires an update
	runner := &fakeRunner{results: []error{permanentRunErr()}}
	gate := New(Deps{
		HTTP: &fakeHTTP{do: func(*http.Request) (*http.Response, error) {
			mu.Lock()
			defer mu.Unlock()
			return jsonResponse(200, manifest), nil
		}},
		Clock:  newFakeClock(),
		Runner: runner,
		Config: fakeConfig{branch: "b", version: "1.0.0", endpoint: "https://x"},
	})

	if _, err := gate.EnsureLatestBeforeClaim(context.Background()); err == nil {
		t.Fatal("expected permanent update failure")
	}
	if fs := gate.Failure(); !fs.Failed {
		t.Fatal("permanent failure must latch")
	}

	// The distributor lowers the requirement; the device now satisfies the gate.
	mu.Lock()
	manifest = okManifest("1.0.0", "0.9.0", "2.0.0")
	mu.Unlock()

	res, err := gate.EnsureLatestBeforeClaim(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if res != ResultNoUpdateNeeded {
		t.Errorf("result = %v, want ResultNoUpdateNeeded", res)
	}
	if fs := gate.Failure(); fs.Failed {
		t.Error("stale latch must clear when no update is needed")
	}
}
