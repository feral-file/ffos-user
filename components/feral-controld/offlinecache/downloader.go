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
	Release()
	// Close tears Chromium down immediately and makes all pending/future
	// Acquire calls fail. Called once, on daemon shutdown.
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

	mu             sync.Mutex
	cmd            wrapper.ExecCmd
	procCancel     context.CancelFunc
	procDone       chan struct{} // closed once the reaper goroutine's cmd.Wait() returns
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

func (d *downloader) Acquire(ctx context.Context) (string, error) {
	select {
	case d.sem <- struct{}{}:
	case <-ctx.Done():
		return "", ctx.Err()
	}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		<-d.sem
		return "", ErrDownloaderClosed
	}
	// A pending idle-teardown from the previous job's Release is now moot:
	// this Acquire reuses (or restarts) the process instead.
	if d.teardownCancel != nil {
		d.teardownCancel()
		d.teardownCancel = nil
	}
	alreadyRunning := d.cmd != nil
	d.mu.Unlock()

	if !alreadyRunning {
		if err := d.start(ctx); err != nil {
			<-d.sem
			return "", err
		}
	}
	return d.endpoint(), nil
}

func (d *downloader) Release() {
	select {
	case <-d.sem:
	default:
		// Release without a matching Acquire should not happen in normal
		// operation; tolerate it rather than panicking so a caller bug
		// cannot wedge the semaphore for every subsequent job.
	}

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
		// GPU acceleration is intentionally left enabled (no --disable-gpu).
		// DP-1 software artworks are canvas/WebGL/WASM content (see this
		// package's doc comment), and the kiosk Chromium a capture must
		// behave like (users/feralfile/scripts/start-kiosk.sh) runs with
		// GPU rasterization on. --disable-gpu forces WebGL context
		// creation to fail outright rather than falling back to software
		// rendering, so an artwork that feature-detects WebGL and only
		// fetches its shader/texture assets when a context is available
		// would silently skip those requests during capture — producing
		// a Coverage.Complete=true record that is actually missing
		// resources the real kiosk needs. --disable-gpu is a legacy
		// workaround for Chromium's old headless implementation and is
		// not needed for "--headless=new", which supports real GPU
		// rendering; --ignore-gpu-blocklist mirrors the kiosk's own flag
		// to stop an offscreen/headless surface being treated as
		// unsupported hardware. This narrows, but does not close, the
		// fidelity gap between the two Chromium instances' GPU code
		// paths (headless capture still renders off-screen rather than
		// through the kiosk's Wayland surface) — see
		// docs/offline-artwork-capture.md's known limitations.
		"--ignore-gpu-blocklist",
		"--enable-gpu-rasterization",
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
	// a zombie, and clears state once it exits by any means (teardown,
	// crash, or the daemon's own shutdown canceling ctx upstream). done is
	// closed last so Close/scheduleTeardown can synchronously wait for the
	// process to actually be gone instead of returning while it is still
	// exiting in the background.
	go func() {
		waitErr := cmd.Wait()
		d.mu.Lock()
		d.cmd = nil
		d.procCancel = nil
		d.mu.Unlock()
		if waitErr != nil {
			d.logger.Debug("offline cache: headless chromium process exited", zap.Error(waitErr))
		}
		close(done)
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
// CommandContext's contract), clears process state, and returns the
// channel the caller should wait on (after unlocking d.mu) to know the
// reaper goroutine has actually finished cmd.Wait(). Callers must hold
// d.mu; it is safe to call when no process is running (returns nil).
func (d *downloader) stopLocked() <-chan struct{} {
	if d.procCancel != nil {
		d.procCancel()
	}
	done := d.procDone
	d.cmd = nil
	d.procCancel = nil
	d.procDone = nil
	return done
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
