package offlinecache

import (
	"context"
	"errors"
	"fmt"
	go_http "net/http"
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

var ErrDownloaderClosed = errors.New("offline cache: downloader closed")

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

	exec       wrapper.Exec
	os         wrapper.OS
	clock      wrapper.Clock
	httpClient wrapper.HTTPClient
	logger     *zap.Logger

	sem chan struct{} // capacity 1: the single-job-at-a-time gate

	mu         sync.Mutex
	cmd        wrapper.ExecCmd
	procCancel context.CancelFunc
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
// started lazily on first Acquire.
func NewDownloader(
	binaryPath, userDataDir string,
	debugPort int,
	idleTeardown time.Duration,
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
	cmd := d.exec.CommandContext(procCtx, d.binaryPath, args...)
	if err := cmd.Start(); err != nil {
		cancel()
		return fmt.Errorf("offline cache: start headless chromium: %w", err)
	}

	done := make(chan struct{})
	d.mu.Lock()
	d.cmd = cmd
	d.procCancel = cancel
	d.procDone = done
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
	return nil
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
			return fmt.Errorf("offline cache: headless chromium did not become ready within %s", chromiumStartupTimeout)
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

// stopLocked cancels the process context (killing Chromium via
// CommandContext's contract) and returns the channel the caller should
// wait on (after unlocking d.mu) to know the reaper goroutine has
// actually finished cmd.Wait(). Callers must hold d.mu; it is safe to
// call when no process is running (returns nil).
//
// It clears cmd/procCancel immediately (nothing needs them once the
// kill signal is sent) but deliberately leaves procDone set until
// reapCompleted runs: Acquire uses "procDone != nil" to detect that a
// process is still in the process of exiting (even though cmd is
// already nil) and waits for it rather than starting a replacement that
// would race the old process for the same debug port / user-data-dir
// lock.
func (d *downloader) stopLocked() <-chan struct{} {
	if d.procCancel != nil {
		d.procCancel()
	}
	done := d.procDone
	d.cmd = nil
	d.procCancel = nil
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
	waitDone := d.stopLocked()
	d.mu.Unlock()
	if waitDone != nil {
		<-waitDone
	}
	return nil
}
