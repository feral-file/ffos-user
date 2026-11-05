package offlinecache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
)

// mediaHealthRenderer is the media-health L1 render probe over the offline
// cache's headless Chromium (feral-file/ffos-user#392, docs/media-health.md
// §6). It reuses the capturer wholesale — the same Downloader (one browser,
// one job at a time, idle teardown), the same page dial, and the SAME
// source guard: the page is untrusted artwork code, so every request it
// makes, child targets included, goes through the reserved-address policy
// exactly as a capture's does. What differs from a capture: nothing is
// stored, the viewport is pinned to a fixed size so the pHash is
// comparable across devices and with the indexer, and the only output is
// the frame plus the main document's status.
type mediaHealthRenderer struct {
	c *capturer
}

// NewMediaHealthRenderer adapts the capturer into mediahealth.Renderer. nil
// when the capturer is not the package's own implementation (a test
// double), so the L1 runner is simply not wired.
func NewMediaHealthRenderer(cap Capturer) mediahealth.Renderer {
	c, ok := cap.(*capturer)
	if !ok || c == nil {
		return nil
	}
	return &mediaHealthRenderer{c: c}
}

const (
	// renderViewport is the indexer's DefaultViewportWidth/Height: the
	// pHash is only comparable at one geometry.
	renderViewport = 1024
	// renderNavigateWindow bounds Page.navigate's own round trip.
	renderNavigateWindow = 30 * time.Second
	// renderScreenshotWindow bounds the capture call.
	renderScreenshotWindow = 20 * time.Second
	// maxScreenshotBytes caps the PNG the browser hands back; a 1024x1024
	// capture is far below it.
	maxScreenshotBytes = 8 << 20
	// maxDecodedPixels rejects a declared geometry larger than the pinned
	// viewport could produce before the pixel buffer is allocated.
	maxDecodedPixels = renderViewport * renderViewport * 2
	// maxObservedRenderResponses bounds the origins remembered for the
	// post-render storage clear: a hostile page issuing thousands of
	// requests must not size an allocation; the clear is best effort and
	// window-bounded anyway.
	maxObservedRenderResponses = 4096
)

// Render implements mediahealth.Renderer.
func (r *mediaHealthRenderer) Render(ctx context.Context, wireURL string, settle time.Duration) (*mediahealth.Frame, error) {
	c := r.c
	endpoint, err := c.downloader.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: acquire headless chromium: %w", mediahealth.ErrRendererUnavailable, err)
	}
	defer c.downloader.Release()

	session, err := DialPageSession(ctx, endpoint, c.cdpClient, c.dialer, c.json, c.io, c.logger)
	if err != nil {
		return nil, fmt.Errorf("%w: dial render session: %w", mediahealth.ErrRendererUnavailable, err)
	}
	// Stop the page BEFORE dropping the session, for the same reason the
	// capturer does: Release only schedules an idle teardown, and the
	// untrusted page would otherwise keep running unguarded until then.
	defer func() {
		c.stopPageBeforeDetach(session)
		_ = session.Close()
	}()
	return r.renderOnSession(ctx, session, wireURL, settle)
}

// renderOnSession is Render after the browser is acquired and the page
// dialed: the CDP conversation itself, on an injectable session so it can
// be pinned without a browser.
func (r *mediaHealthRenderer) renderOnSession(ctx context.Context, session CDPSession, wireURL string, settle time.Duration) (*mediahealth.Frame, error) {
	c := r.c
	// The main document's status: the first Document-type response after
	// our navigate. Registered before Network.enable so the navigation's
	// own response cannot be missed.
	var (
		statusMu   sync.Mutex
		mainStatus int
		statusSeen bool
		observed   []Resource
	)
	session.On("Network.responseReceived", func(params json.RawMessage) {
		var ev struct {
			Type     string `json:"type"`
			Response struct {
				Status int    `json:"status"`
				URL    string `json:"url"`
			} `json:"response"`
		}
		if err := json.Unmarshal(params, &ev); err != nil {
			return
		}
		statusMu.Lock()
		defer statusMu.Unlock()
		if ev.Response.URL != "" && len(observed) < maxObservedRenderResponses {
			observed = append(observed, Resource{URL: ev.Response.URL})
		}
		if ev.Type == "Document" && !statusSeen {
			statusSeen = true
			mainStatus = ev.Response.Status
		}
	})
	// Every origin the page touched — the navigation's, its redirects',
	// its iframes' and subresources' — has its storage cleared before the
	// shared browser goes back to the capturer, exactly as a capture
	// clears its own. The pre-navigation reset can only scope to the
	// source's origin; a child frame writing storage at another origin
	// would otherwise survive into the next offline capture and change
	// what it fetches (PR #443 review, F20). Runs on every exit path, on
	// its own bounded window, from the capturer's cleanup.
	defer func() {
		statusMu.Lock()
		resources := append([]Resource(nil), observed...)
		statusMu.Unlock()
		c.clearObservedOriginsStorage(context.Background(), session, resources)
	}()
	// Registered BEFORE Fetch.enable so no paused request can arrive
	// without a handler (an unanswered pause hangs the request).
	guard := c.attachSourceGuard(ctx, session)

	for _, step := range []struct {
		method string
		params map[string]interface{}
	}{
		{"Network.enable", map[string]interface{}{}},
		{"Page.enable", map[string]interface{}{}},
		{"Emulation.setDeviceMetricsOverride", map[string]interface{}{
			"width": renderViewport, "height": renderViewport, "deviceScaleFactor": 1, "mobile": false,
		}},
	} {
		if _, err := session.Send(ctx, step.method, step.params); err != nil {
			return nil, fmt.Errorf("media health render: %s: %w", step.method, err)
		}
	}
	if err := c.resetTargetState(ctx, session, wireURL); err != nil {
		return nil, fmt.Errorf("media health render: reset chromium state: %w", err)
	}
	// After the reset (so our own clears are never paused) and before the
	// navigate (so its first request is covered).
	if _, err := session.Send(ctx, "Fetch.enable", fetchEnablePatternAll()); err != nil {
		return nil, fmt.Errorf("media health render: Fetch.enable: %w", err)
	}
	if err := c.enableGuardedAutoAttach(ctx, session, guard); err != nil {
		return nil, fmt.Errorf("media health render: arm child-target source guard: %w", err)
	}

	navCtx, navCancel := context.WithTimeout(ctx, renderNavigateWindow)
	defer navCancel()
	raw, err := session.Send(navCtx, "Page.navigate", map[string]interface{}{"url": wireURL})
	if err != nil {
		return nil, fmt.Errorf("media health render: Page.navigate: %w", err)
	}
	var nav struct {
		ErrorText string `json:"errorText"`
	}
	if json.Unmarshal(raw, &nav) == nil && nav.ErrorText != "" {
		return nil, fmt.Errorf("media health render: navigation failed: %s", nav.ErrorText)
	}

	// The settle: a bounded wall-clock window, the indexer's rule — there
	// is no "artwork fully loaded" signal across arbitrary software.
	select {
	case <-time.After(settle):
	case <-ctx.Done():
		return nil, fmt.Errorf("media health render: settle: %w", ctx.Err())
	}

	shotCtx, shotCancel := context.WithTimeout(ctx, renderScreenshotWindow)
	defer shotCancel()
	// Viewport-bounded: captureBeyondViewport would let a page make the
	// capture arbitrarily tall, and the pHash must match the pinned
	// geometry.
	raw, err = session.Send(shotCtx, "Page.captureScreenshot", map[string]interface{}{
		"format": "png", "captureBeyondViewport": false,
	})
	if err != nil {
		return nil, fmt.Errorf("media health render: Page.captureScreenshot: %w", err)
	}
	var shot struct {
		Data string `json:"data"`
	}
	if err := json.Unmarshal(raw, &shot); err != nil {
		return nil, fmt.Errorf("media health render: screenshot reply: %w", err)
	}
	if len(shot.Data) > maxScreenshotBytes*4/3+4 {
		return nil, fmt.Errorf("media health render: screenshot of %d base64 bytes exceeds the cap", len(shot.Data))
	}
	data, err := base64.StdEncoding.DecodeString(shot.Data)
	if err != nil {
		return nil, fmt.Errorf("media health render: screenshot base64: %w", err)
	}
	img, err := decodeScreenshotPNG(data)
	if err != nil {
		return nil, fmt.Errorf("media health render: %w", err)
	}
	statusMu.Lock()
	status := mainStatus
	statusMu.Unlock()
	c.logger.Debug("media health render: frame captured",
		zap.String("url", redactSourceForLog(wireURL)), zap.Int("main_status", status), zap.Duration("settle", settle))
	return &mediahealth.Frame{Image: img, MainStatus: status}, nil
}

// decodeScreenshotPNG decodes a PNG with the header read first, so a
// decompression bomb is rejected on its declared geometry rather than
// after allocating the pixel buffer.
func decodeScreenshotPNG(data []byte) (image.Image, error) {
	if len(data) == 0 {
		return nil, errors.New("empty screenshot")
	}
	if len(data) > maxScreenshotBytes {
		return nil, fmt.Errorf("screenshot of %d bytes exceeds the %d-byte cap", len(data), maxScreenshotBytes)
	}
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("reading screenshot header: %w", err)
	}
	if int64(cfg.Width)*int64(cfg.Height) > maxDecodedPixels {
		return nil, fmt.Errorf("screenshot of %dx%d exceeds the %d-pixel cap", cfg.Width, cfg.Height, maxDecodedPixels)
	}
	return png.Decode(bytes.NewReader(data))
}
