// Package otagate is controld's owner of the mandatory OTA update gate, ported
// from feral-setupd (update_coordinator.rs / updater.rs). It guards, retries, and
// latches system updates so a device is brought to a supported build before it is
// claimed, and so a user-triggered update runs at most once at a time.
//
// Delete-don't-translate note: the feral-setupd source this ports from carried a
// large amount of BLE-7-phase and Chromium-UI machinery (SetupPhase transitions,
// PRE_FAILURE_PHASE persistence, reflashing/version-check TV screens, the
// Blocking/NonBlocking BLE response contract, the D-Bus startup/QR follow-up
// decisions). None of that is ported here — it belonged to the setup wizard, not
// the update gate. Only the guard, retry ladder, version check, updater
// spawn/monitor, and permanent-failure latch are reproduced.
//
// This package must not import relayer/.
package otagate

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// MaxUpdateRetries caps updater spawn attempts in the retry ladder. Ported from
// feral-setupd update_coordinator.rs::MAX_UPDATE_RETRIES.
const MaxUpdateRetries = 3

// singleFlightKey coalesces every update flow onto one in-flight token.
//
// Guard invariant: at most one OTA owns the device at a time. Both entry points —
// the user-triggered RequestUpdate and the mandatory EnsureLatestBeforeClaim —
// share this key, so concurrent callers join the one in-flight update and receive
// its shared outcome instead of launching a second updater. This ports the intent
// of feral-setupd's single UpdateGuard ownership token, but coalesces (callers
// wait and share the result) rather than rejecting, as the port brief requires.
const singleFlightKey = "ota"

// Mode controls when an update is triggered. Ported from
// update_coordinator.rs::UpdateMode.
type Mode int

const (
	// ModeRequired updates only when the running build is below the distributor's
	// minimum supported version (mandatory pre-claim gate).
	ModeRequired Mode = iota
	// ModeAvailable updates when any newer version is available (user-triggered).
	ModeAvailable
)

// Result reports what the gate decided/did. Ported from
// update_coordinator.rs::UpdateCheckResult (UI-only variants dropped).
type Result int

const (
	// ResultNoUpdateNeeded: the running build already satisfies the mode.
	ResultNoUpdateNeeded Result = iota
	// ResultUpdateStarted: an update ran (the device reboots on success). A
	// non-nil error alongside this result means the update failed permanently and
	// the failure latch is now set.
	ResultUpdateStarted
	// ResultTooOldToUpgrade: the build is below the minimum upgradeable version;
	// the device must be reflashed (no update is attempted).
	ResultTooOldToUpgrade
	// ResultVersionCheckFailed: the distributor version check failed. This does
	// NOT latch a permanent failure (matches feral-setupd: only a failed update
	// process latches update_failed, not a failed version check).
	ResultVersionCheckFailed
)

// FailureState is the latched permanent-failure snapshot, surfaced to a future
// setupui package via Failure() or the OnPermanentFailure callback. The zero
// value (Failed == false) means no latched failure.
type FailureState struct {
	Failed bool
	Err    error
	At     time.Time
}

// Deps are the injectable seams the gate is built from. Clock, HTTP, Runner, and
// Config are all replaceable so the table tests run fast and deterministic.
type Deps struct {
	HTTP   wrapper.HTTPClient
	Clock  wrapper.Clock
	Runner UpdateRunner
	Config ConfigProvider
	Logger *zap.Logger

	// OnProgress, when non-nil, receives each update progress percent parsed from
	// the updater log (pct == -1 for a progress line that carried no percent
	// field). nil keeps the debug-log-only behavior. devicectl wires this to the
	// setupui updating narration so the on-screen overlay tracks the update.
	OnProgress func(pct int)
}

// Gate is the OTA update gate. Construct it once (long-lived) so the single-flight
// guard and the failure latch persist across relayer commands.
type Gate struct {
	deps  Deps
	group singleflight.Group

	mu      sync.Mutex
	failure FailureState
	onLatch func(FailureState)
}

// New builds a Gate from the given deps.
func New(deps Deps) *Gate {
	return &Gate{deps: deps}
}

// RequestUpdate is entry point (a): the user-triggered updateToLatestVersion
// relayer command (ModeAvailable). It drives the updater locally and is
// single-flighted so a burst of relayer commands coalesces.
func (g *Gate) RequestUpdate(ctx context.Context) (Result, error) {
	res, err, _ := g.do(ctx, func() (Result, error) {
		return g.runLocal(ctx, ModeAvailable)
	})
	return res, err
}

// EnsureLatestBeforeClaim is entry point (b): the mandatory pre-claim gate
// (ModeRequired). A future claim-flow story calls this before the device is
// claimed; it always drives the updater locally (this is the new controld-owned
// path the port exists to create) and coalesces with any in-flight update.
func (g *Gate) EnsureLatestBeforeClaim(ctx context.Context) (Result, error) {
	res, err, _ := g.do(ctx, func() (Result, error) {
		return g.runLocal(ctx, ModeRequired)
	})
	return res, err
}

// do runs fn under the single-flight guard and unpacks the shared result.
func (g *Gate) do(_ context.Context, fn func() (Result, error)) (Result, error, bool) {
	v, err, shared := g.group.Do(singleFlightKey, func() (interface{}, error) {
		return fn()
	})
	// The closure always returns a Result, even on error, so the type assertion is
	// safe. singleflight surfaces the closure's error as err and its value as v.
	res, _ := v.(Result)
	return res, err, shared
}

// runLocal executes the controld-owned gate: version check, upgrade decision, and
// the retry ladder. Ported from update_coordinator.rs::check_and_update_system
// with all phase/UI side effects removed.
func (g *Gate) runLocal(ctx context.Context, mode Mode) (Result, error) {
	branch, currentStr, _, err := g.deps.Config.LocalBuild(ctx)
	if err != nil {
		g.logf("OTA gate: cannot read local build", zap.Error(err))
		return ResultVersionCheckFailed, err
	}
	current, err := parseSemver(currentStr)
	if err != nil {
		g.logf("OTA gate: unparseable local version", zap.String("version", currentStr), zap.Error(err))
		return ResultVersionCheckFailed, err
	}

	// Forced live version check. A failure surfaces as ResultVersionCheckFailed
	// and does NOT latch (a version-check failure is not an update failure).
	versions, err := fetchRemoteVersion(ctx, g.deps)
	if err != nil {
		g.logf("OTA gate: version check failed", zap.String("branch", branch), zap.Error(err))
		return ResultVersionCheckFailed, err
	}

	// Reflash gate: a device below the minimum upgradeable version cannot be
	// auto-upgraded. No update is attempted and nothing is latched.
	if isTooOldToUpgrade(current, versions) {
		g.logf("OTA gate: device too old to auto-upgrade; reflash required")
		return ResultTooOldToUpgrade, nil
	}

	needsUpdate := false
	switch mode {
	case ModeRequired:
		needsUpdate = isUpdateRequired(current, versions)
	case ModeAvailable:
		needsUpdate = isUpdateAvailable(current, versions)
	}
	if !needsUpdate {
		return ResultNoUpdateNeeded, nil
	}

	if err := g.runUpdateLadder(ctx); err != nil {
		g.latch(err)
		return ResultUpdateStarted, err
	}
	g.clearLatch()
	return ResultUpdateStarted, nil
}

// runUpdateLadder spawns the updater and retries on transient failures with
// exponential backoff, latching nothing here (the caller latches on the returned
// error). Ported from update_coordinator.rs::update.
//
// Ladder semantics (exactly as feral-setupd): up to MaxUpdateRetries spawn
// attempts; a transient failure before the final attempt sleeps 2^attempt seconds
// (2s, then 4s) and retries; a permanent failure, or a transient failure on the
// final attempt, returns the error. Reaching this ladder is itself the "explicit
// retry" that clears any prior latch, so we clear at the top.
func (g *Gate) runUpdateLadder(ctx context.Context) error {
	g.clearLatch()

	for attempt := 1; attempt <= MaxUpdateRetries; attempt++ {
		err := g.deps.Runner.Run(ctx, g.forwardProgress)
		if err == nil {
			return nil
		}

		kind := classifyUpdaterMessage(err.Error())
		if kind == errTransient && attempt < MaxUpdateRetries {
			backoff := time.Duration(1<<attempt) * time.Second // 2^attempt: 2s, 4s
			g.logf("OTA gate: transient update failure, retrying",
				zap.Int("attempt", attempt), zap.Int("max", MaxUpdateRetries),
				zap.Duration("backoff", backoff), zap.Error(err))
			if serr := g.deps.Clock.SleepContext(ctx, backoff); serr != nil {
				return serr
			}
			continue
		}

		g.logf("OTA gate: permanent update failure",
			zap.Int("attempt", attempt), zap.Error(err))
		return err
	}
	return nil
}

func (g *Gate) forwardProgress(pct int, message string) {
	// Progress copy drove the setupd TV screen; controld now narrates it through
	// Deps.OnProgress (devicectl points that at setupui's updating overlay). pct is
	// forwarded as-is, including -1 for a percent-less line, so the policy of what
	// to do with -1 lives with the consumer, not here. The debug log is retained.
	if g.deps.Logger != nil {
		g.deps.Logger.Debug("OTA update progress", zap.Int("pct", pct), zap.String("progress", message))
	}
	if g.deps.OnProgress != nil {
		g.deps.OnProgress(pct)
	}
}

// latch records the permanent-failure state and notifies any registered callback.
// Ported from the update_failed latch in handle_permanent_update_failure (phase
// persistence dropped; the state lives in memory and is surfaced by getter).
func (g *Gate) latch(err error) {
	g.mu.Lock()
	g.failure = FailureState{Failed: true, Err: err, At: g.deps.Clock.Now()}
	fs := g.failure
	cb := g.onLatch
	g.mu.Unlock()

	if cb != nil {
		cb(fs)
	}
}

func (g *Gate) clearLatch() {
	g.mu.Lock()
	g.failure = FailureState{}
	g.mu.Unlock()
}

// Failure returns the current latched failure snapshot (Failed == false when none).
func (g *Gate) Failure() FailureState {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.failure
}

// OnPermanentFailure registers a callback invoked whenever the gate latches a
// permanent failure. A setupui surface uses this to paint the failure screen.
func (g *Gate) OnPermanentFailure(cb func(FailureState)) {
	g.mu.Lock()
	g.onLatch = cb
	g.mu.Unlock()
}

func (g *Gate) logf(msg string, fields ...zap.Field) {
	if g.deps.Logger != nil {
		g.deps.Logger.Info(msg, fields...)
	}
}
