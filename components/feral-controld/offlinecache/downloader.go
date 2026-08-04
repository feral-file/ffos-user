package offlinecache

import (
	"context"
	"errors"
	"fmt"
	go_http "net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// chromiumStartupTimeout/chromiumPollInterval bound how long Acquire waits
// for a freshly spawned headless Chromium's DevTools endpoint to answer.
// Generous relative to normal cold-start (order of a second) because a
// heavily loaded device can be slower, but bounded so a wedged Chromium
// binary fails a download instead of hanging the job queue forever.
const (
	chromiumStartupTimeout = 20 * time.Second
	chromiumPollInterval   = 200 * time.Millisecond
)

// Headless-Chromium resource-limit defaults (HeadlessLimits, applied by
// start() via a transient systemd scope plus a taskset/env argv wrapper —
// see captureWrapperArgv for why the wrapper is load-bearing rather than
// belt-and-braces). These bound how much of the machine an IN-FLIGHT
// capture can consume — the one window the admission gate (admission.go)
// cannot cover, since it only defers jobs from starting and a running
// capture is never aborted. The capture Chromium renders WebGL on the CPU
// (SwiftShader), so without a cap a single heavy artwork capture can pull
// sustained all-core boost load: heat toward the firmware thermal envelope
// and scheduling pressure on the live kiosk. An FF1 also hard-reset in the
// field at the instant such a capture started, with every limit here
// inert; the evidence (empty pstore, no oops/OOM/panic, journal truncated
// mid-line) points to an abrupt power event and rules out the software
// causes, but does not by itself prove the capture's draw was the
// mechanism. Treat the power spike as the leading hypothesis, not settled
// fact -- see docs/offline-artwork-capture.md 2.1.
const (
	// DefaultHeadlessCPUShareOfPinned is what the default CPUQuota is
	// derived FROM rather than a fixed percentage: the quota is this
	// fraction of what the pinned CPU set (AllowedCPUs) can deliver. The
	// two properties must agree — a quota above 100% x len(AllowedCPUs)
	// is unreachable, so it silently stops being a limit (see
	// alignHeadlessLimits, which clamps a configured one) — and a fixed
	// default cannot stay reachable across machines with different core
	// counts. Deriving it keeps them aligned by construction: on the
	// 16-thread target, 4 pinned CPUs x 75% = 300%.
	//
	// 75% rather than 100% so the capture leaves slack inside its own
	// pinned set: SwiftShader is happy to saturate every core it can see,
	// and the residue is what keeps those CPUs from sitting at a
	// sustained 100% for the whole capture window. Too-tight a quota
	// degrades capture fidelity instead (pages that cannot finish loading
	// inside captureWindowMs produce partial coverage), which is why this
	// is a cap, not a minimum.
	DefaultHeadlessCPUShareOfPinned = 0.75
	// DefaultHeadlessMemoryMaxBytes is a cgroup memory ceiling for the
	// capture Chromium: exceeding it OOM-kills the CAPTURE process (the
	// job fails cleanly, retryable) instead of letting the kernel's
	// global OOM killer pick a victim on a device where the other big
	// process is the live kiosk.
	DefaultHeadlessMemoryMaxBytes = int64(2) << 30 // 2 GiB
	// scopeUnitPrefix names the transient scopes so stale ones from a
	// crashed daemon can be swept by glob on the next start (see
	// ensureCaptureSupport) and so operators can find them in systemctl.
	scopeUnitPrefix = "feral-offline-capture-"
	// scopeStopTimeoutSec is the scope's own SIGTERM->SIGKILL escalation
	// (systemd TimeoutStopSec). Short: nothing in a capture Chromium
	// deserves a graceful goodbye longer than this. The escalation runs
	// inside systemd once the stop job is issued, so it completes even if
	// this daemon has already exited — which is what lets Close() bound
	// its own wait (scopeCloseWait) instead of riding out the full stop.
	scopeStopTimeoutSec = 5
	// scopeCloseWait bounds how long Close() waits for a SCOPED
	// generation's teardown. main.go's shutdown path force-exits the
	// whole daemon SHUTDOWN_TIMEOUT (2s) after Stop begins — the same
	// budget that forced the notifier's ShutdownCloseBudget to 250ms —
	// so Close cannot block for a scope's worst-case teardown (~5s
	// SIGTERM escalation, more if systemctl wedges). Waiting the full
	// escalation is also unnecessary: the stop job is already enqueued in
	// systemd and the scope dies on its own, outside our cgroup, whether
	// or not this process is still around to observe it; the next daemon
	// run's ensureCaptureSupport sweep covers any residue. Idle/steady-state
	// teardowns (scheduleTeardown) still wait unbounded — only shutdown
	// is budget-constrained.
	scopeCloseWait = 1 * time.Second
	// scopeStopWait bounds how long stopScope waits for the reaper after
	// issuing `systemctl --user stop` before falling back to killing
	// systemd-run directly. Longer than scopeStopTimeoutSec so the
	// scope's own escalation gets to finish first.
	scopeStopWait = 15 * time.Second
	// scopeProbeTimeout bounds the one-time systemd-run capability probe
	// and each systemctl invocation.
	scopeProbeTimeout = 10 * time.Second
	// sessionBusEnvVar is unset for the capture Chromium (see
	// captureWrapperArgv): reaching the user session bus is precisely what
	// lets Chromium move itself OUT of the scope we just capped it with.
	sessionBusEnvVar = "DBUS_SESSION_BUS_ADDRESS"
)

// HeadlessLimits configures the resource cap applied to the headless
// capture Chromium via a transient systemd scope (`systemd-run --user
// --scope`) plus an argv wrapper (captureWrapperArgv). The zero value
// disables the cap entirely (plain spawn — pre-limits behavior);
// OptionsFromConfig enables it with the defaults above. Applying limits
// through a RUNTIME transient scope rather than unit-file properties is
// deliberate: unit files ship on the full-image rail (users/**), and this
// must stay a package-rail change. The two capabilities are probed once at
// first use and INDEPENDENTLY (see ensureCaptureSupport), so losing one
// never surrenders the other, and a broken environment degrades with a
// warning naming what is left rather than blocking captures outright.
type HeadlessLimits struct {
	Enabled bool
	// CPUQuotaPercent caps total CPU cycles (systemd CPUQuota=; 100 = one
	// full CPU). <=0 disables the quota property. Enforced ONLY by the
	// cgroup, so it depends on Chromium staying inside our scope — see
	// captureWrapperArgv.
	CPUQuotaPercent int
	// AllowedCPUs pins the capture Chromium to a CPU subset (e.g. "0-3"),
	// bounding worst-case package power draw from all-core boost — a quota
	// alone still lets short bursts light up every core. Empty disables the
	// pin. Passed BOTH as systemd AllowedCPUs= and to taskset in the argv
	// wrapper: the systemd property is inert unless the cpuset controller
	// is delegated to the user manager (on the FF1 target it is not — only
	// cpu, memory and pids are), so taskset is what actually enforces this.
	AllowedCPUs string
	// MemoryMaxBytes is the cgroup memory ceiling (systemd MemoryMax=).
	// <=0 disables the property. Cgroup-only, same caveat as
	// CPUQuotaPercent.
	MemoryMaxBytes int64
}

var ErrDownloaderClosed = errors.New("offline cache: downloader closed")

// ErrHeadlessNotReady means the capture Chromium was spawned but never
// answered its DevTools endpoint in time. It is distinct from any capture
// failure because the cause is environmental — no usable browser here —
// rather than anything about the item being captured: a missing or
// unlaunchable binary, a sandbox the container forbids, no /dev/shm.
// Callers that only make sense WITH a browser (the real-browser guard
// test skips on it rather than reporting a false failure) need to tell
// the two apart.
var ErrHeadlessNotReady = errors.New("offline cache: headless chromium did not become ready")

// Downloader owns the lifecycle of a separate headless Chromium process
// used only for offline-artwork capture (default :9223, its own
// user-data-dir). It is a distinct process from the kiosk Chromium the
// player uses (127.0.0.1:9222) by design: capture can run for the full
// captureWindowMs per item and must never contend with or destabilize the
// player surface. Jobs are serialized to one at a time — the device
// already runs under OOM pressure from the kiosk Chromium alone — and the
// process is torn down after an idle period rather than kept warm,
// trading a few seconds of cold-start latency per download for not
// holding a second Chromium's memory footprint indefinitely.
//
//go:generate mockgen -source=downloader.go -destination=../mocks/offlinecache_downloader.go -package=mocks -mock_names=Downloader=MockOfflineCacheDownloader
type Downloader interface {
	// Acquire reserves the single capture slot (blocking until any prior
	// job's Release has run) and ensures headless Chromium is running,
	// starting it on first use or after an idle teardown. It returns the
	// CDP HTTP endpoint (e.g. "http://127.0.0.1:9223") capture.go polls
	// /json against to discover the page target to dial.
	Acquire(ctx context.Context) (endpoint string, err error)
	// Release frees the capture slot for the next job. If no new Acquire
	// arrives within the configured idle timeout, Chromium is torn down.
	// The slot is deliberately freed only AFTER any idle-teardown for
	// THIS release has been fully recorded (see Release's doc): a caller
	// blocked in Acquire can therefore never observe the slot as free
	// while a pending teardown for the process it is about to reuse is
	// still unrecorded.
	Release()
	// Close tears Chromium down immediately and makes all pending/future
	// Acquire calls fail. Called once, on daemon shutdown, via
	// Capturer.Close -> Service.Stop (main.go has no direct handle to
	// Downloader itself — see bootstrap.go).
	Close() error
}

type downloader struct {
	binaryPath   string
	userDataDir  string
	debugPort    int
	idleTeardown time.Duration
	limits       HeadlessLimits

	exec       wrapper.Exec
	os         wrapper.OS
	clock      wrapper.Clock
	httpClient wrapper.HTTPClient
	logger     *zap.Logger

	sem chan struct{} // capacity 1: the single-job-at-a-time gate

	// scopeProbed/scopeOK/wrapperArgv cache the one-time capability probe
	// (ensureCaptureSupport). Written and read only from start(), which the
	// sem above already serializes, so they need no lock of their own.
	// wrapperArgv is the resolved argv prefix (nil = spawn Chromium bare).
	scopeProbed bool
	scopeOK     bool
	wrapperArgv []string
	// scopeSeq numbers transient scope units so a new generation can
	// never collide with a predecessor's still-deactivating scope name.
	// Same serialization argument as scopeProbed.
	scopeSeq uint64

	// needScopeSweep (under mu) is set when stopScope's backstop fired:
	// killing systemd-run directly may have orphaned a Chromium inside
	// its scope, still holding the debug port and profile lock, and the
	// next scoped spawn's readiness probe would otherwise succeed against
	// that stale orphan as if it were its own process. The next
	// ensureCaptureSupport call re-runs the glob sweep before spawning to
	// clear it.
	needScopeSweep bool

	mu         sync.Mutex
	cmd        wrapper.ExecCmd
	procCancel context.CancelFunc
	// scopeUnit is the transient systemd scope the CURRENT generation's
	// Chromium runs in ("" = plain spawn). stopLocked needs it to tear
	// the process down via `systemctl --user stop`: with a scope, cmd is
	// systemd-run — Chromium's PARENT — and killing it via procCancel
	// would orphan Chromium inside the scope, still holding the debug
	// port and profile lock (exactly the race Acquire's reap-wait exists
	// to prevent).
	scopeUnit string
	// procDone identifies the CURRENT process generation and is closed
	// once that generation's reaper goroutine (start()'s go func) has
	// observed cmd.Wait() return. Unlike cmd/procCancel, stopLocked does
	// NOT clear this: it deliberately stays non-nil for the whole window
	// between "kill signal sent" and "reaped", so Acquire can tell a
	// process is still exiting (even though cmd==nil already) and wait
	// for it instead of racing a replacement onto the same fixed debug
	// port / user-data-dir lock (see Acquire's doc). Only the matching
	// reaper clears it, and only if no newer generation has since
	// replaced it — see reapCompleted's doc.
	procDone       chan struct{}
	teardownCancel context.CancelFunc
	closed         bool
}

// NewDownloader constructs a Downloader. It performs no I/O; Chromium is
// started lazily on first Acquire. limits' zero value disables the
// systemd-scope resource cap (see HeadlessLimits).
func NewDownloader(
	binaryPath, userDataDir string,
	debugPort int,
	idleTeardown time.Duration,
	limits HeadlessLimits,
	execWrapper wrapper.Exec,
	osWrapper wrapper.OS,
	clockWrapper wrapper.Clock,
	httpClient wrapper.HTTPClient,
	logger *zap.Logger,
) Downloader {
	return &downloader{
		binaryPath:   binaryPath,
		userDataDir:  userDataDir,
		debugPort:    debugPort,
		idleTeardown: idleTeardown,
		limits:       limits,
		exec:         execWrapper,
		os:           osWrapper,
		clock:        clockWrapper,
		httpClient:   httpClient,
		logger:       logger,
		sem:          make(chan struct{}, 1),
	}
}

func (d *downloader) endpoint() string {
	return fmt.Sprintf("http://127.0.0.1:%d", d.debugPort)
}

// Acquire reserves the capture slot and ensures Chromium is running. If a
// prior generation was killed (Close/idle-teardown) but has not finished
// being reaped yet, Acquire waits for that reap before starting a
// replacement: starting a new Chromium on the same fixed debug port and
// user-data-dir while the old one still holds them (socket/profile lock)
// would make the new process fail to bind or corrupt the profile lock —
// a race that is otherwise easy to hit because stopLocked's kill is
// asynchronous (cmd.Wait() completing is what actually frees them).
func (d *downloader) Acquire(ctx context.Context) (string, error) {
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	for {
		d.mu.Lock()
		if d.closed {
			d.mu.Unlock()
			<-d.sem
			return "", ErrDownloaderClosed
		}
		// A pending idle-teardown from the previous job's Release is now
		// moot: this Acquire reuses (or restarts) the process instead.
		if d.teardownCancel != nil {
			d.teardownCancel()
			d.teardownCancel = nil
		}
		if d.cmd != nil {
			// A live generation is already running; reuse it.
			d.mu.Unlock()
			return d.endpoint(), nil
		}
		waitDone := d.procDone
		d.mu.Unlock()

		if waitDone == nil {
			break // nothing running and nothing still exiting: start fresh
		}
		select {
		case <-waitDone:
			continue // prior generation fully reaped; re-check state
		case <-ctx.Done():
			<-d.sem
			return "", ctx.Err()
		}
	}

	if err := d.start(ctx); err != nil {
		<-d.sem
		return "", err
	}
	return d.endpoint(), nil
}

// Release's semaphore drain (freeing the slot for the NEXT Acquire) must
// happen strictly LAST, after the teardown decision below is fully
// recorded under d.mu — hence the defer, rather than draining first as
// an earlier version of this method did. That earlier ordering left a
// window between freeing the slot and recording d.teardownCancel: a new
// Acquire could take the now-free slot, see d.teardownCancel still nil,
// and reuse the running process — and THIS Release call, resuming a
// moment later, would then go on to set d.teardownCancel anyway (its own
// d.cmd != nil check still holds; nothing has told it a new job is now
// using that same process). scheduleTeardown's timer would later fire
// and kill Chromium out from under that active job. Freeing the slot
// only after teardownCancel is set (or the "nothing to tear down"
// decision is made) closes the window: Go's memory model guarantees a
// channel receive that unblocks because of this defer's send/close
// happens after everything this function did beforehand, so any Acquire
// that gets in is guaranteed to see this method's own teardownCancel
// already recorded and can correctly cancel it (see Acquire's own
// teardownCancel check) instead of racing past it.
func (d *downloader) Release() {
	defer func() {
		select {
		case <-d.sem:
		default:
			// Release without a matching Acquire should not happen in
			// normal operation; tolerate it rather than panicking so a
			// caller bug cannot wedge the semaphore for every subsequent
			// job.
		}
	}()

	d.mu.Lock()
	if d.closed || d.cmd == nil {
		d.mu.Unlock()
		return
	}
	teardownCtx, cancel := context.WithCancel(context.Background())
	d.teardownCancel = cancel
	d.mu.Unlock()

	go d.scheduleTeardown(teardownCtx)
}

func (d *downloader) scheduleTeardown(ctx context.Context) {
	if err := d.clock.SleepContext(ctx, d.idleTeardown); err != nil {
		return // canceled by a new Acquire or Close
	}
	d.mu.Lock()
	if d.teardownCancel == nil {
		d.mu.Unlock()
		return // already handled by a race with Close/Acquire
	}
	d.teardownCancel = nil
	d.logger.Info("offline cache: tearing down idle headless chromium",
		zap.Duration("idle_teardown", d.idleTeardown))
	waitDone := d.stopLocked()
	d.mu.Unlock()
	if waitDone != nil {
		<-waitDone
	}
}

func (d *downloader) start(ctx context.Context) error {
	if err := d.os.MkdirAll(d.userDataDir, dirPerm); err != nil {
		return fmt.Errorf("offline cache: create headless chromium user-data-dir: %w", err)
	}

	procCtx, cancel := context.WithCancel(context.Background())
	args := []string{
		fmt.Sprintf("--remote-debugging-port=%d", d.debugPort),
		"--remote-debugging-address=127.0.0.1",
		"--headless=new",
		// WebGL context creation must SUCCEED here (plain --disable-gpu
		// forces it to fail outright rather than fall back to software
		// rendering — see below), because an artwork that feature-detects
		// WebGL and only fetches its shader/texture assets when a context
		// is available would otherwise silently skip those requests
		// during capture, producing a Coverage.Complete=true record that
		// is actually missing resources the real kiosk needs.
		//
		// An earlier version of this code satisfied that by mirroring the
		// kiosk's REAL GPU hardware acceleration flags
		// (--ignore-gpu-blocklist --enable-gpu-rasterization). That was
		// wrong: capture never renders to a visible surface (see
		// capture.go's doc — CDP Network events are used only to learn
		// which URLs were requested, then bytes are re-fetched directly
		// over HTTP), so pixel-accurate or fast rendering is never
		// needed, only a non-null WebGL context. Contending for the
		// SAME physical GPU the kiosk Chromium is actively using for
		// live hardware-accelerated playback caused device-wide hard
		// freezes (no OOM-killer trace, no clean shutdown — consistent
		// with a GPU-driver-level lockup) when a download ran during
		// playback. Forcing Chromium's software WebGL backend (SwANGLE:
		// ANGLE translating GL ES calls to SwiftShader) instead gives
		// the SAME successful-context-creation behavior the artwork
		// feature-detection needs, entirely on the CPU, with zero
		// contention for the kiosk's GPU.
		//
		// --enable-unsafe-swiftshader is required from Chromium 130+:
		// automatic SwiftShader-as-WebGL-fallback was deprecated, so
		// software WebGL must be explicitly requested or context
		// creation fails the same way --disable-gpu does — see
		// https://chromium.googlesource.com/chromium/src/+/main/docs/gpu/swiftshader.md.
		// "unsafe" here refers to SwiftShader's weaker sandboxing
		// guarantees for untrusted content, an accepted trade-off this
		// process already makes by design (it exists specifically to
		// navigate to and execute untrusted third-party artwork code).
		"--use-gl=angle",
		"--use-angle=swiftshader-webgl",
		"--enable-unsafe-swiftshader",
		// Matches the kiosk's autoplay policy (start-kiosk.sh). An
		// artwork that only requests further assets after a video/audio
		// element's play() promise resolves must see the same
		// "autoplay always allowed" behavior here as it does live on the
		// kiosk, or those requests never fire during capture.
		"--autoplay-policy=no-user-gesture-required",
		"--user-data-dir=" + d.userDataDir,
		"about:blank",
	}

	// Wrap the spawn in a resource-capped transient systemd scope when
	// enabled and available, and prefix the binary with the argv wrapper
	// that carries the CPU pin (see captureWrapperArgv). The fallback to a
	// plain spawn is deliberate: a missing systemd-run or session bus must
	// degrade — never block capture. Note the wrapper applies on BOTH
	// paths, so the degraded path still gets the pin that bounds power
	// draw; only the cgroup quota and memory ceiling are lost with it.
	wrapperArgv, scopeOK := d.ensureCaptureSupport(ctx)

	var cmd wrapper.ExecCmd
	scopeUnit := ""
	if scopeOK {
		d.scopeSeq++
		scopeUnit = fmt.Sprintf("%s%d.scope", scopeUnitPrefix, d.scopeSeq)
		runArgs := append(d.scopeRunArgs(scopeUnit), wrapperArgv...)
		runArgs = append(runArgs, d.binaryPath)
		runArgs = append(runArgs, args...)
		cmd = d.exec.CommandContext(procCtx, "systemd-run", runArgs...)
	} else {
		name, spawnArgs := d.binaryPath, args
		if len(wrapperArgv) > 0 {
			name = wrapperArgv[0]
			spawnArgs = make([]string, 0, len(wrapperArgv)+len(args))
			spawnArgs = append(spawnArgs, wrapperArgv[1:]...)
			spawnArgs = append(spawnArgs, d.binaryPath)
			spawnArgs = append(spawnArgs, args...)
		}
		cmd = d.exec.CommandContext(procCtx, name, spawnArgs...)
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("offline cache: start headless chromium: %w", err)
	}

	done := make(chan struct{})
	d.mu.Lock()
	d.cmd = cmd
	d.procCancel = cancel
	d.procDone = done
	d.scopeUnit = scopeUnit
	d.mu.Unlock()

	// CommandContext ties the process to procCtx: canceling it (stopLocked)
	// sends the kill signal. Wait reaps the process so it does not remain
	// a zombie; reapCompleted clears state once it exits by any means
	// (teardown, crash, or the daemon's own shutdown canceling ctx
	// upstream) and closes done last, so Close/scheduleTeardown/Acquire
	// can synchronously wait for the process to actually be gone instead
	// of proceeding while it is still exiting in the background.
	go func() {
		waitErr := cmd.Wait()
		if waitErr != nil {
			d.logger.Debug("offline cache: headless chromium process exited", zap.Error(waitErr))
		}
		d.reapCompleted(done)
	}()

	if err := d.waitForDebugEndpoint(ctx); err != nil {
		d.mu.Lock()
		waitDone := d.stopLocked()
		d.mu.Unlock()
		if waitDone != nil {
			<-waitDone
		}
		return err
	}
	// Chromium is up and has finished its own startup re-parenting by now,
	// so this is the earliest point the escape check is meaningful.
	if scopeUnit != "" {
		d.warnOnScopeEscape(scopeUnit, cmd.Pid())
	}
	return nil
}

// scopeRunArgs builds the systemd-run argument list for one capture
// scope. `--scope` keeps systemd-run in the FOREGROUND — it creates the
// unit and then execs the command in place rather than daemonizing it, so
// the existing cmd.Wait reaper still observes process exit and the PID
// Start() returned BECOMES Chromium's browser process (this is what lets
// warnOnScopeEscape read that PID's cgroup directly; see its doc and
// wrapper.ExecCmd.Pid). It is emphatically not a surviving systemd-run
// parent — an earlier version of this comment said "Chromium's parent",
// which is wrong and is corrected in stopLocked's doc too, along with the
// reason the cgroup kill is still required despite that.
//
// `--collect` garbage-collects the unit even if it fails, so a failed
// generation can never leave a stuck unit behind. TimeoutStopSec bounds
// the SIGTERM->SIGKILL escalation `systemctl stop` performs at teardown.
func (d *downloader) scopeRunArgs(unit string) []string {
	runArgs := []string{
		"--user", "--scope", "--collect", "--quiet",
		"--unit=" + unit,
		fmt.Sprintf("--property=TimeoutStopSec=%d", scopeStopTimeoutSec),
	}
	if d.limits.CPUQuotaPercent > 0 {
		runArgs = append(runArgs, fmt.Sprintf("--property=CPUQuota=%d%%", d.limits.CPUQuotaPercent))
	}
	if d.limits.AllowedCPUs != "" {
		// Kept even though taskset is what enforces the pin today (see
		// captureWrapperArgv): the property costs nothing, documents intent
		// where an operator looks first (`systemctl show`), and becomes the
		// real enforcement the day the cpuset controller is delegated to the
		// user manager. It must NOT be read as evidence the pin applied —
		// systemd reports it verbatim whether or not it landed anywhere.
		runArgs = append(runArgs, "--property=AllowedCPUs="+d.limits.AllowedCPUs)
	}
	if d.limits.MemoryMaxBytes > 0 {
		runArgs = append(runArgs, fmt.Sprintf("--property=MemoryMax=%d", d.limits.MemoryMaxBytes))
	}
	return append(runArgs, "--")
}

// captureWrapperArgv builds the argv prefix that must sit between the
// scope (if any) and the Chromium binary. Both elements exist because the
// systemd scope ALONE does not actually constrain Chromium:
//
//   - `env -u DBUS_SESSION_BUS_ADDRESS` stops the escape. Given a reachable
//     user session bus, Chromium moves its own browser process into a
//     transient "app-org.chromium.Chromium-<pid>.scope" under app.slice at
//     startup — a sibling of ours with every property at its default
//     (CPUQuota=infinity, MemoryMax=infinity). Everything we set on our
//     scope is silently voided the moment that happens. Headless capture
//     has no use for the session bus, so denying it is free.
//
//   - `taskset -c` is what actually applies the CPU pin. systemd accepts
//     and reports AllowedCPUs=, but writing it needs the cpuset controller
//     delegated to the user manager, and on the FF1 target only cpu, memory
//     and pids are — so the property lands nowhere and no cpuset.cpus file
//     is ever created. CPU affinity, by contrast, is a plain process
//     attribute: children inherit it and a cgroup migration does not reset
//     it, so the pin survives even an escape that voids everything else.
//
// The pin is the limit that matters most here: it is what bounds package
// power draw, and an unpinned SwiftShader capture lighting up every core
// was the load in flight when an FF1 hard-reset in the field (leading
// hypothesis, not proven -- see the const block above). Returns nil when
// limits are disabled, in which case Chromium is spawned bare.
//
// withPin=false drops the taskset element, keeping only the escape
// prevention — the fallback when taskset itself is unusable (see
// resolveWrapper).
func (d *downloader) captureWrapperArgv(withPin bool) []string {
	if !d.limits.Enabled {
		return nil
	}
	argv := []string{"env", "-u", sessionBusEnvVar}
	// countAllowedCPUs, not a bare != "" check: systemd's AllowedCPUs=
	// accepts whitespace-separated lists ("0 1 2 3") that taskset -c
	// rejects outright, and alignHeadlessLimits already treats exactly
	// those specs as "no pin". Gating on the same parser keeps the two
	// helpers agreeing about what a CPU list is, so a config spelling that
	// is legal upstream cannot turn into a spawn that fails to exec.
	if withPin && countAllowedCPUs(d.limits.AllowedCPUs) > 0 {
		argv = append(argv, "taskset", "-c", d.limits.AllowedCPUs)
	}
	return argv
}

// ensureCaptureSupport runs once per process: it sweeps any capture scopes
// a previous daemon run may have leaked (a scope survives its spawner —
// that is the point of it being outside our cgroup — so a SIGKILLed
// daemon leaves its capture Chromium running, still holding the debug
// port and profile lock, and the next spawn would probe THAT stale
// process's DevTools endpoint as if it were its own), then probes the
// EXACT spawn shape a real capture will use.
//
// Probing the real shape matters: the previous version probed a bare
// `/bin/true` and, on success, logged that every configured limit was
// "active". /bin/true neither re-parents itself out of the scope nor needs
// a cpuset, so it passed on a device where the pin and the ceiling were
// both inert — a false all-clear over exactly the defect that crashed the
// device.
//
// The two capabilities are probed INDEPENDENTLY and then composed, rather
// than as one ladder of combined shapes. A single combined probe cannot
// say which half failed, so a broken wrapper would surrender the scope too
// (dropping straight to a bare, fully uncapped spawn) and would blame
// systemd in the log while systemd was fine. Resolving the wrapper first
// and then probing the scope WITH it yields the honest matrix:
//
//	scope + pinned wrapper -> quota, memory ceiling and pin all in force
//	scope + env-only       -> quota and ceiling in force; no pin
//	pinned wrapper only    -> pin in force; quota and ceiling lost
//	neither                -> uncapped, the pre-limits behavior
//
// Serialized by the Acquire semaphore; see the scopeProbed field comment.
func (d *downloader) ensureCaptureSupport(ctx context.Context) ([]string, bool) {
	if !d.limits.Enabled {
		return nil, false
	}
	if d.scopeProbed {
		if d.scopeOK && d.takeNeedScopeSweep() {
			// A prior backstop kill may have orphaned a Chromium in its
			// scope (see needScopeSweep) — clear it before spawning a
			// generation that would otherwise probe the orphan's stale
			// DevTools endpoint.
			d.sweepStaleScopes(ctx)
		}
		return d.wrapperArgv, d.scopeOK
	}

	d.sweepStaleScopes(ctx)

	wrapperArgv, ok := d.resolveWrapper(ctx)
	if !ok {
		return nil, false // canceled mid-probe: leave the question open
	}
	scopeOK, ok := d.probeScope(ctx, wrapperArgv)
	if !ok {
		return nil, false
	}

	d.scopeProbed, d.scopeOK, d.wrapperArgv = true, scopeOK, wrapperArgv
	switch {
	case scopeOK:
		d.logger.Info("offline cache: capture chromium resource limits active",
			zap.Int("cpu_quota_percent", d.limits.CPUQuotaPercent),
			zap.String("allowed_cpus", d.limits.AllowedCPUs),
			zap.Int64("memory_max_bytes", d.limits.MemoryMaxBytes),
			zap.Strings("wrapper", wrapperArgv),
			zap.Bool("cpu_pin_active", slices.Contains(wrapperArgv, "taskset")))
	case len(wrapperArgv) > 0:
		d.logger.Warn("offline cache: transient systemd scope unavailable, capture chromium runs WITHOUT the cpu quota and memory ceiling",
			zap.Strings("wrapper", wrapperArgv),
			zap.Bool("cpu_pin_active", slices.Contains(wrapperArgv, "taskset")))
	default:
		d.logger.Warn("offline cache: neither transient systemd scope nor argv wrapper available, capture chromium will run WITHOUT resource limits")
	}
	return wrapperArgv, scopeOK
}

// resolveWrapper picks the strongest usable argv wrapper: pinned, then
// env-only (escape prevention without the pin), then none. The bool is
// false only when the caller's context was canceled mid-probe, which says
// nothing about support and must not latch a verdict.
func (d *downloader) resolveWrapper(ctx context.Context) ([]string, bool) {
	pinned := d.captureWrapperArgv(true)
	if len(pinned) == 0 {
		return nil, true // limits disabled
	}
	if _, err := d.probeCaptureShape(ctx, nil, pinned); err == nil {
		return pinned, true
	} else if ctx.Err() != nil {
		return nil, false
	}

	// taskset is what just failed (env alone is coreutils and the spec was
	// already validated by countAllowedCPUs), so retry without the pin
	// rather than lose the escape prevention with it. When the spec carried
	// no pin to begin with the two wrappers are identical, and re-probing
	// the same argv would only burn another probe timeout on an image that
	// is already broken.
	envOnly := d.captureWrapperArgv(false)
	if slices.Equal(pinned, envOnly) {
		d.logger.Warn("offline cache: argv wrapper unusable for capture chromium; it may re-parent itself out of any resource scope",
			zap.Strings("wrapper", envOnly))
		return nil, true
	}
	out, err := d.probeCaptureShape(ctx, nil, envOnly)
	if err == nil {
		d.logger.Warn("offline cache: cpu pin unavailable for capture chromium; the cgroup quota and memory ceiling still apply, but package power draw is no longer bounded",
			zap.String("allowed_cpus", d.limits.AllowedCPUs))
		return envOnly, true
	}
	if ctx.Err() != nil {
		return nil, false
	}
	d.logger.Warn("offline cache: argv wrapper unusable for capture chromium; it may re-parent itself out of any resource scope",
		zap.Error(err), zap.ByteString("output", out))
	return nil, true
}

// probeScope checks that a transient scope carrying our exact properties
// can be created around the resolved wrapper. Same cancellation contract
// as resolveWrapper.
func (d *downloader) probeScope(ctx context.Context, wrapperArgv []string) (bool, bool) {
	scopeArgs := d.scopeRunArgs(fmt.Sprintf("%sprobe.scope", scopeUnitPrefix))
	out, err := d.probeCaptureShape(ctx, scopeArgs, wrapperArgv)
	if err == nil {
		return true, true
	}
	if ctx.Err() != nil {
		return false, false
	}
	d.logger.Warn("offline cache: transient systemd scope probe failed",
		zap.Error(err), zap.ByteString("output", out))
	return false, true
}

// probeCaptureShape runs /bin/true through the given scope args (nil for
// no scope) and argv wrapper — the same composition a real spawn uses, so
// what is validated is what will actually run.
func (d *downloader) probeCaptureShape(ctx context.Context, scopeArgs, wrapperArgv []string) ([]byte, error) {
	probeCtx, cancel := context.WithTimeout(ctx, scopeProbeTimeout)
	defer cancel()

	var name string
	var args []string
	switch {
	case len(scopeArgs) > 0:
		name = "systemd-run"
		args = append(append([]string{}, scopeArgs...), wrapperArgv...)
		args = append(args, "/bin/true")
	case len(wrapperArgv) > 0:
		name, args = wrapperArgv[0], append(append([]string{}, wrapperArgv[1:]...), "/bin/true")
	default:
		name = "/bin/true"
	}
	return d.exec.CommandContext(probeCtx, name, args...).CombinedOutput()
}

// warnOnScopeEscape detects the failure mode captureWrapperArgv exists to
// prevent: a Chromium build that re-parents itself out of our capped scope
// anyway, leaving the cpu quota and memory ceiling enforcing nothing.
//
// It asks the process directly — /proc/<pid>/cgroup must name the unit we
// spawned it into — which is exact, and cheap enough to run on every
// scoped spawn. `systemd-run --scope` creates the unit and then execs in
// place, so the PID Start() returned IS the capture Chromium's browser
// process (see wrapper.ExecCmd.Pid); an escape moves that process to
// another cgroup but never changes its PID. An earlier version instead
// diffed `app-org.chromium.Chromium-*.scope` units before and after the
// spawn, which was both false-positive prone (a kiosk Chromium restart
// inside the window — exactly what the watchdog does under the OOM
// pressure a capture creates — looked like an escape) and false-negative
// prone (a future Chromium whose unit name missed the glob would slip
// through, in precisely the case the check exists for).
//
// The exec-in-place claim above is the one thing here a reader is likely
// to "correct" back to the wrong model, because the intuitive reading of
// `systemd-run cmd` is that systemd-run stays as cmd's parent. It does
// not: it registers the transient scope with its OWN pid, waits for the
// job, then execvp()s. Reviewers have now proposed resolving "the real
// Chromium child PID" more than once; doing that would make this check
// permanently inert, since there is no surviving systemd-run process to
// descend from — a silent regression, because an inert check logs
// nothing. Measured on FF1, systemd 261, running the daemon's exact argv
// chain and inspecting the pid Start() returned:
//
//	held pid=834442 comm=chromium
//	cgroup=0::/user.slice/.../app.slice/probe-chrome.scope
//	cmdline=/usr/lib/chromium/chromium --headless=new ...
//	$ pgrep systemd-run   ->   (no processes)
//
// env(1) and taskset(1) exec in place for the same reason, so the pid
// survives the whole wrapper chain. Re-measure before changing this.
//
// Never fails a spawn: capture with a pin but no ceiling still beats no
// capture. The loud log is the point — this went unnoticed in the field
// precisely because nothing ever said the limits had stopped applying.
func (d *downloader) warnOnScopeEscape(scopeUnit string, pid int) {
	if pid <= 0 {
		return
	}
	raw, err := d.os.ReadFile(fmt.Sprintf("/proc/%d/cgroup", pid))
	if err != nil {
		// The process may simply have exited already (a capture Chromium
		// that died on startup); that is the readiness probe's problem to
		// report, not evidence about cgroup containment either way.
		d.logger.Debug("offline cache: could not read capture chromium cgroup",
			zap.Int("pid", pid), zap.Error(err))
		return
	}
	cgroup := strings.TrimSpace(string(raw))
	if strings.Contains(cgroup, scopeUnit) {
		return
	}
	d.logger.Warn("offline cache: capture chromium moved itself out of its resource scope; the cpu quota and memory ceiling are NOT in force (an argv-wrapper cpu pin, if any, still is)",
		zap.String("expected_unit", scopeUnit),
		zap.String("actual_cgroup", cgroup),
		zap.String("hint", "chromium reached a session bus despite env -u "+sessionBusEnvVar))
}

// sweepStaleScopes glob-stops every capture scope. Best-effort: with
// --collect, dead scopes are already gone, and "no units matched" exits
// 0 — a real error here only foreshadows scope operations failing too.
func (d *downloader) sweepStaleScopes(ctx context.Context) {
	sweepCtx, cancel := context.WithTimeout(ctx, scopeProbeTimeout)
	defer cancel()
	if out, err := d.exec.CommandContext(sweepCtx, "systemctl", "--user", "stop", scopeUnitPrefix+"*.scope").CombinedOutput(); err != nil {
		d.logger.Debug("offline cache: sweeping stale capture scopes failed",
			zap.Error(err), zap.ByteString("output", out))
	}
}

// takeNeedScopeSweep reads-and-clears the backstop-orphan flag.
func (d *downloader) takeNeedScopeSweep() bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	need := d.needScopeSweep
	d.needScopeSweep = false
	return need
}

// stopScope tears down a scoped generation. Ordering is the whole point:
// `systemctl --user stop` kills the CGROUP (Chromium first, SIGTERM
// escalating to SIGKILL after TimeoutStopSec), systemd-run then exits on
// its own, cmd.Wait returns, and the reaper closes done — so waiters get
// the same "process actually gone, port and profile lock free" guarantee
// the plain-spawn kill path provides. Only if systemctl itself fails, or
// the reaper still has not run after scopeStopWait, does the backstop
// kill systemd-run directly. That is the ONE path that breaks the
// done-channel guarantee: Chromium may survive orphaned inside the
// scope, still holding the debug port and profile lock. Recovery is NOT
// automatic within this process — the startup glob sweep only runs once
// — so the backstop also raises needScopeSweep, making the next scoped
// spawn re-sweep before it would otherwise probe the orphan's stale
// DevTools endpoint as its own. Accepted over waiting forever on a
// wedged systemctl.
func (d *downloader) stopScope(unit string, done <-chan struct{}, backstop context.CancelFunc) {
	stopCtx, cancel := context.WithTimeout(context.Background(), scopeProbeTimeout)
	defer cancel()
	if out, err := d.exec.CommandContext(stopCtx, "systemctl", "--user", "stop", unit).CombinedOutput(); err != nil {
		d.logger.Warn("offline cache: stopping capture scope failed, killing systemd-run directly",
			zap.String("unit", unit), zap.Error(err), zap.ByteString("output", out))
		d.raiseNeedScopeSweep()
		backstop()
		return
	}
	waitCtx, cancelWait := context.WithTimeout(context.Background(), scopeStopWait)
	defer cancelWait()
	select {
	case <-done:
	case <-waitCtx.Done():
		d.logger.Warn("offline cache: capture scope did not exit after stop, killing systemd-run directly",
			zap.String("unit", unit))
		d.raiseNeedScopeSweep()
		backstop()
	}
}

func (d *downloader) raiseNeedScopeSweep() {
	d.mu.Lock()
	d.needScopeSweep = true
	d.mu.Unlock()
}

// waitForDebugEndpoint polls /json/version until headless Chromium's
// DevTools endpoint answers, bounded by chromiumStartupTimeout and ctx.
func (d *downloader) waitForDebugEndpoint(ctx context.Context) error {
	deadline := d.clock.Now().Add(chromiumStartupTimeout)
	for {
		if d.probeDebugEndpoint(ctx) {
			return nil
		}
		if d.clock.Now().After(deadline) {
			return fmt.Errorf("%w within %s", ErrHeadlessNotReady, chromiumStartupTimeout)
		}
		if err := d.clock.SleepContext(ctx, chromiumPollInterval); err != nil {
			return err
		}
	}
}

func (d *downloader) probeDebugEndpoint(ctx context.Context) bool {
	req, err := d.httpClient.NewRequest(go_http.MethodGet, d.endpoint()+"/json/version", nil)
	if err != nil {
		return false
	}
	resp, err := d.httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return false
	}
	defer func() { _ = resp.Body.Close() }()
	return resp.StatusCode == go_http.StatusOK
}

// stopLocked initiates the current generation's teardown and returns the
// channel the caller should wait on (after unlocking d.mu) to know the
// reaper goroutine has actually finished cmd.Wait(). Callers must hold
// d.mu; it is safe to call when no process is running (returns nil).
//
// Plain spawn: cancels the process context (killing Chromium via
// CommandContext's contract) synchronously.
//
// Scoped spawn: `systemd-run --scope` creates the unit and then execs in
// place, so cmd is NOT a surviving systemd-run parent — the PID it holds
// is Chromium's own browser process. (An earlier version of this comment
// claimed otherwise; the conclusion below is unchanged, but the mechanism
// matters, because "just kill cmd" looks safe if you believe the parent
// story is false without checking WHY the cgroup kill is needed.) A
// SIGKILL to that PID reaps the browser process only, leaving its forked
// children — zygote, renderers, the GPU process — alive inside the scope
// still holding the debug port and the profile lock, while cmd.Wait
// returns early and frees Acquire to start a replacement against them. So
// the scope path hands teardown to an async stopScope goroutine (systemctl
// stop, which kills the whole cgroup, with a direct kill only as a bounded
// backstop) and does NOT cancel here; the returned done channel keeps its
// meaning ("process actually gone") on both paths.
//
// It clears cmd/procCancel/scopeUnit immediately (nothing else needs
// them once teardown is initiated) but deliberately leaves procDone set
// until reapCompleted runs: Acquire uses "procDone != nil" to detect
// that a process is still in the process of exiting (even though cmd is
// already nil) and waits for it rather than starting a replacement that
// would race the old process for the same debug port / user-data-dir
// lock.
func (d *downloader) stopLocked() <-chan struct{} {
	done := d.procDone
	if d.scopeUnit != "" && d.cmd != nil {
		go d.stopScope(d.scopeUnit, done, d.procCancel)
	} else if d.procCancel != nil {
		d.procCancel()
	}
	d.cmd = nil
	d.procCancel = nil
	d.scopeUnit = ""
	return done
}

// reapCompleted runs once a process generation's cmd.Wait() has actually
// returned (see start()'s go func). The d.procDone==done identity check
// matters because this can be called after a NEWER generation has
// already replaced d.cmd/d.procDone/d.procCancel — e.g. this process
// crashed on its own after stopLocked had already run for an unrelated
// reason and a subsequent Acquire started a replacement once THIS
// generation's own reap (a still-earlier one, in a longer chain) had
// already completed and cleared state. Without the check, a late-waking
// reaper would blindly nil out a NEWER generation's live cmd/procCancel,
// making the downloader think nothing is running while a Chromium
// process it still owns is actually alive — leaking it and
// desynchronizing Acquire's "is something running" decision from
// reality. done is closed last (after the possible clear), so a waiter
// blocked on it always observes the post-clear-or-not state once it
// wakes.
func (d *downloader) reapCompleted(done chan struct{}) {
	d.mu.Lock()
	if d.procDone == done {
		d.cmd = nil
		d.procCancel = nil
		d.procDone = nil
		// A scoped generation that exits on its own (crash, cgroup OOM
		// kill) never went through stopLocked, so clear the unit here
		// too; --collect garbage-collects the dead scope on the systemd
		// side.
		d.scopeUnit = ""
	}
	d.mu.Unlock()
	close(done)
}

func (d *downloader) Close() error {
	d.mu.Lock()
	d.closed = true
	if d.teardownCancel != nil {
		d.teardownCancel()
		d.teardownCancel = nil
	}
	scoped := d.scopeUnit != ""
	hadLiveCmd := d.cmd != nil
	waitDone := d.stopLocked()
	d.mu.Unlock()
	if waitDone == nil {
		return nil
	}
	if hadLiveCmd && !scoped {
		// THIS Close call sent a plain-spawn SIGKILL: the reap is
		// near-immediate, so waiting for it keeps the strongest
		// guarantee at zero real cost. hadLiveCmd matters: a nil cmd
		// with a live procDone means a teardown was ALREADY in flight
		// when Close ran (idle teardown or a readiness-failure stop) and
		// its kind is no longer knowable here — stopLocked cleared
		// scopeUnit when it started — so it may well be a scoped
		// systemctl stop still mid-flight. That case must take the
		// bounded branch below, or Close reacquires exactly the
		// unbounded scoped wait the budget bound exists to prevent.
		<-waitDone
		return nil
	}
	// Scoped spawn, or an in-flight teardown of unknowable kind: bound
	// the wait to fit main.go's 2s forced-exit shutdown budget — see
	// scopeCloseWait's doc for why abandoning the wait is safe (the stop
	// job completes inside systemd regardless).
	waitCtx, cancel := context.WithTimeout(context.Background(), scopeCloseWait)
	defer cancel()
	select {
	case <-waitDone:
	case <-waitCtx.Done():
		d.logger.Warn("offline cache: shutdown proceeding while capture teardown completes in the background")
	}
	return nil
}
