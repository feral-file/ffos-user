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
	go_http "net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// mediaHealthRenderer is the media-health L1 render probe over the offline
// cache's headless Chromium (feral-file/ffos-user#392, docs/media-health.md
// §6). It reuses the capturer wholesale — the same Downloader (one browser,
// one job at a time, idle teardown) and the SAME source guard: the page is
// untrusted artwork code, so every request it makes, child targets
// included, goes through the reserved-address policy exactly as a
// capture's does, plus the kiosk's gateway policy on each request.
//
// Unlike a capture, each render runs in its OWN DISPOSABLE BROWSER CONTEXT
// (Target.createBrowserContext, disposed when the render ends): storage a
// page writes — its own, a child frame's partitioned storage, a frame the
// page removed mid-render — dies with the context and can never reach a
// later render or an offline capture, which share the browser process but
// never a context. That is the whole reason the render dials the BROWSER
// endpoint rather than the capturer's single page target. Nothing is
// stored; the viewport is pinned so the pHash is comparable; the only
// output is the frame plus the main document's status.
type mediaHealthRenderer struct {
	c *capturer
	// policy is the kiosk's gateway policy, applied to EVERY request the
	// page makes (the navigation, its redirects, child frames and
	// subresources) through the guard's rewrite hook, as the kiosk's own
	// uarewrite interceptor does: a content-addressed path on a hostile
	// gateway goes to the replacement gateway, any other request to those
	// hosts gets the policy agent. Without it L1 would fetch a different
	// destination from the kiosk and judge a page the wall never shows.
	policy mediahealth.Rewriter
}

// NewMediaHealthRenderer adapts the capturer into mediahealth.Renderer
// under the kiosk's gateway policy (nil = render as written). nil when the
// capturer is not the package's own implementation (a test double), so the
// L1 runner is simply not wired.
func NewMediaHealthRenderer(cap Capturer, policy mediahealth.Rewriter) mediahealth.Renderer {
	c, ok := cap.(*capturer)
	if !ok || c == nil {
		return nil
	}
	return &mediaHealthRenderer{c: c, policy: policy}
}

// renderRewrite is the guard's per-request rewrite for policy: the kiosk
// interceptor's decision, verbatim — a matching host is either redirected
// (a content-addressed path) or continued with the policy agent.
func renderRewrite(policy mediahealth.Rewriter) func(string, map[string]string) (string, []map[string]string, bool) {
	if policy == nil {
		return nil
	}
	return func(rawURL string, headers map[string]string) (string, []map[string]string, bool) {
		if !policy.Matches(rawURL) {
			return "", nil, false
		}
		if target, ok := policy.RewriteURL(rawURL); ok {
			return target, nil, true
		}
		return "", userAgentHeaders(headers, policy.UserAgent()), true
	}
}

// userAgentHeaders mirrors uarewrite.Policy.RewriteHeaders: the request's
// headers with User-Agent replaced (or added), sorted by name.
func userAgentHeaders(headers map[string]string, agent string) []map[string]string {
	out := make([]map[string]string, 0, len(headers)+1)
	replaced := false
	for name, value := range headers {
		if strings.EqualFold(name, "user-agent") {
			if replaced {
				continue
			}
			out = append(out, map[string]string{"name": name, "value": agent})
			replaced = true
			continue
		}
		out = append(out, map[string]string{"name": name, "value": value})
	}
	if !replaced {
		out = append(out, map[string]string{"name": "User-Agent", "value": agent})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["name"] < out[j]["name"] })
	return out
}

const (
	// renderViewport is the indexer's DefaultViewportWidth/Height: the
	// pHash is only comparable at one geometry.
	renderViewport = 1024
	// renderNavigateWindow bounds Page.navigate's own round trip.
	renderNavigateWindow = 30 * time.Second
	// renderScreenshotWindow bounds the capture call.
	renderScreenshotWindow = 20 * time.Second
	// renderTeardownWindow bounds disposing the render's browser context
	// after the render (on its own context: the render's may be gone).
	renderTeardownWindow = 10 * time.Second
	// maxScreenshotBytes caps the PNG the browser hands back; a 1024x1024
	// capture is far below it.
	maxScreenshotBytes = 8 << 20
	// maxDecodedPixels rejects a declared geometry larger than the pinned
	// viewport could produce before the pixel buffer is allocated.
	maxDecodedPixels = renderViewport * renderViewport * 2
)

// Render implements mediahealth.Renderer.
func (r *mediaHealthRenderer) Render(ctx context.Context, wireURL string, settle time.Duration) (*mediahealth.Frame, error) {
	c := r.c
	endpoint, err := c.downloader.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("%w: acquire headless chromium: %w", mediahealth.ErrRendererUnavailable, err)
	}
	defer c.downloader.Release()

	browser, err := dialBrowserSession(ctx, endpoint, c.cdpClient, c.dialer, c.json, c.io, c.logger)
	if err != nil {
		return nil, fmt.Errorf("%w: dial browser: %w", mediahealth.ErrRendererUnavailable, err)
	}
	defer func() { _ = browser.Close() }()
	return r.renderInDisposableContext(ctx, browser, wireURL, settle)
}

// dialBrowserSession dials the browser target itself (/json/version's
// webSocketDebuggerUrl), the only session that can create and dispose
// browser contexts. Same plain loopback client and dialer as the page
// dial: our own browser is a trusted destination.
func dialBrowserSession(ctx context.Context, endpoint string, httpClient wrapper.HTTPClient, dialer wrapper.WebSocketDialer, jsonWrapper wrapper.JSON, ioWrapper wrapper.IO, logger *zap.Logger) (CDPSession, error) {
	ctx, cancel := context.WithTimeout(ctx, defaultDialTimeout)
	defer cancel()
	req, err := httpClient.NewRequest(go_http.MethodGet, endpoint+"/json/version", nil)
	if err != nil {
		return nil, fmt.Errorf("build version request: %w", err)
	}
	resp, err := httpClient.Do(req.WithContext(ctx))
	if err != nil {
		return nil, fmt.Errorf("fetch version: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := ioWrapper.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read version: %w", err)
	}
	var version struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := jsonWrapper.Unmarshal(body, &version); err != nil {
		return nil, fmt.Errorf("parse version: %w", err)
	}
	if version.WebSocketDebuggerURL == "" {
		return nil, fmt.Errorf("no browser target at %s", endpoint)
	}
	conn, _, err := dialer.DialContext(ctx, version.WebSocketDebuggerURL, nil)
	if err != nil {
		return nil, fmt.Errorf("dial browser target: %w", err)
	}
	return NewCDPSession(conn, jsonWrapper, logger), nil
}

// renderInDisposableContext creates a browser context for this render,
// a page in it, attaches (flat) and renders on that page session, then
// disposes the context — which closes the page and deletes every byte of
// storage the render produced, whichever frame wrote it and whether or not
// that frame still existed at the end. On an injectable browser session so
// the conversation can be pinned without a browser.
func (r *mediaHealthRenderer) renderInDisposableContext(ctx context.Context, browser CDPSession, wireURL string, settle time.Duration) (*mediahealth.Frame, error) {
	c := r.c
	raw, err := browser.Send(ctx, "Target.createBrowserContext", map[string]interface{}{"disposeOnDetach": true})
	if err != nil {
		return nil, fmt.Errorf("%w: Target.createBrowserContext: %w", mediahealth.ErrRendererUnavailable, err)
	}
	var created struct {
		BrowserContextID string `json:"browserContextId"`
	}
	if err := json.Unmarshal(raw, &created); err != nil || created.BrowserContextID == "" {
		return nil, fmt.Errorf("%w: Target.createBrowserContext: no browserContextId", mediahealth.ErrRendererUnavailable)
	}
	// Disposed on every exit path, on its own bounded context: the
	// render's context may be the thing that expired. disposeOnDetach
	// above is the backstop for a session that dies before this runs.
	defer func() {
		disposeCtx, cancel := context.WithTimeout(context.Background(), renderTeardownWindow)
		defer cancel()
		if _, err := browser.Send(disposeCtx, "Target.disposeBrowserContext", map[string]interface{}{"browserContextId": created.BrowserContextID}); err != nil {
			c.logger.Warn("media health render: could not dispose the render's browser context; it is disposed when the browser connection detaches", zap.Error(err))
		}
	}()

	raw, err = browser.Send(ctx, "Target.createTarget", map[string]interface{}{
		"url": "about:blank", "browserContextId": created.BrowserContextID,
	})
	if err != nil {
		return nil, fmt.Errorf("%w: Target.createTarget: %w", mediahealth.ErrRendererUnavailable, err)
	}
	var target struct {
		TargetID string `json:"targetId"`
	}
	if err := json.Unmarshal(raw, &target); err != nil || target.TargetID == "" {
		return nil, fmt.Errorf("%w: Target.createTarget: no targetId", mediahealth.ErrRendererUnavailable)
	}
	raw, err = browser.Send(ctx, "Target.attachToTarget", map[string]interface{}{"targetId": target.TargetID, "flatten": true})
	if err != nil {
		return nil, fmt.Errorf("%w: Target.attachToTarget: %w", mediahealth.ErrRendererUnavailable, err)
	}
	var attached struct {
		SessionID string `json:"sessionId"`
	}
	if err := json.Unmarshal(raw, &attached); err != nil || attached.SessionID == "" {
		return nil, fmt.Errorf("%w: Target.attachToTarget: no sessionId", mediahealth.ErrRendererUnavailable)
	}
	page := browser.ForSession(attached.SessionID)
	defer func() { _ = page.Close() }()
	return r.renderOnSession(ctx, page, wireURL, settle)
}

// renderOnSession is the render on an attached page session: the CDP
// conversation itself, on an injectable session so it can be pinned
// without a browser.
func (r *mediaHealthRenderer) renderOnSession(ctx context.Context, session CDPSession, wireURL string, settle time.Duration) (*mediahealth.Frame, error) {
	c := r.c
	// The main document's status follows the main FRAME through every
	// navigation it makes — a page that location.replace()s itself onto an
	// error page is judged by the document that was captured, not by the
	// first response (PR #443 review, F26). Keyed by frameId: child frames'
	// documents never count. Registered before Network.enable so the
	// navigation's own response cannot be missed.
	docs := &mainDocTracker{status: map[string]int{}}
	session.On("Network.responseReceived", docs.observe)
	// Registered BEFORE Fetch.enable so no paused request can arrive
	// without a handler (an unanswered pause hangs the request). The
	// policy rides on the guard so it applies to every request, child
	// targets included (the capturer arms the same guard on each child).
	guard := c.attachSourceGuardWithRewrite(ctx, session, renderRewrite(r.policy))

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
	// Before the navigate, so its first request is covered. (No cache or
	// storage reset: the browser context is brand new.)
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
		FrameID   string `json:"frameId"`
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

	// A child target the guard could not contain was left paused — a
	// worker (no Fetch domain) most often. The page then painted without
	// it, and a frame of THAT is not evidence about the work: the
	// containment stays, the render does not count (stalled; retried
	// another night, never a verdict).
	if n := guard.childSetupFailures.Load(); n > 0 {
		return nil, fmt.Errorf("media health render: %d child target(s) left contained; the frame is not evidence", n)
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
	status := docs.latest(nav.FrameID)
	c.logger.Debug("media health render: frame captured",
		zap.String("url", redactSourceForLog(wireURL)), zap.Int("main_status", status), zap.Duration("settle", settle))
	return &mediahealth.Frame{Image: img, MainStatus: status}, nil
}

// mainDocTracker keeps the LATEST Document response per frame, so the
// status reported is the one of the document actually on screen when the
// capture is taken, whatever navigations the page made since the first.
type mainDocTracker struct {
	mu     sync.Mutex
	status map[string]int
	// first is the first Document response seen, for a navigate reply
	// that carried no frameId (older protocol shapes).
	first     int
	firstSeen bool
}

func (t *mainDocTracker) observe(params json.RawMessage) {
	var ev struct {
		Type     string `json:"type"`
		FrameID  string `json:"frameId"`
		Response struct {
			Status int `json:"status"`
		} `json:"response"`
	}
	if err := json.Unmarshal(params, &ev); err != nil || ev.Type != "Document" {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if ev.FrameID != "" {
		t.status[ev.FrameID] = ev.Response.Status
	}
	if !t.firstSeen {
		t.firstSeen = true
		t.first = ev.Response.Status
	}
}

// latest is the main frame's current document status, 0 when none was
// observed (the renderer treats 0 as unknown, not as a failure).
func (t *mainDocTracker) latest(frameID string) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	if frameID != "" {
		return t.status[frameID]
	}
	return t.first
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
