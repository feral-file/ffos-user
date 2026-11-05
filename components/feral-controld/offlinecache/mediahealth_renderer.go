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
	"sort"
	"strings"
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
	obs := newRenderObserver(ctx, session)
	// Everything the page touched is cleared before the shared browser
	// goes back to the capturer: every observed origin (as a capture
	// clears its own) AND every frame's storage key — third-party frames
	// keep partitioned storage that an origin-scoped clear does not reach,
	// and a nested frame's marker surviving into the next offline capture
	// changes what that capture fetches (PR #443 review, F20). On every
	// exit path, on its own bounded window.
	defer obs.cleanup(c)
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
	status := obs.mainStatus()
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

// renderObserver watches one render across the root session and every
// child target the browser attaches (flatten mode: all sessions hang off
// the root, and a child's own attachments are announced on the child): the
// main document's status, the origins of every response, and the sessions
// themselves, which cleanup walks for frame storage keys.
type renderObserver struct {
	ctx  context.Context
	root CDPSession

	mu         sync.Mutex
	status     int
	statusSeen bool
	observed   []Resource
	sessions   []CDPSession
}

func newRenderObserver(ctx context.Context, root CDPSession) *renderObserver {
	o := &renderObserver{ctx: ctx, root: root, sessions: []CDPSession{root}}
	o.watch(root, true)
	return o
}

// watch registers the observer's handlers on session: responses, and the
// attach event that announces the session's own child targets. Handlers
// are appended to the session's list, so the capturer's guard handlers on
// the same methods keep running.
func (o *renderObserver) watch(session CDPSession, isRoot bool) {
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
		o.mu.Lock()
		defer o.mu.Unlock()
		if ev.Response.URL != "" && len(o.observed) < maxObservedRenderResponses {
			o.observed = append(o.observed, Resource{URL: ev.Response.URL})
		}
		if isRoot && ev.Type == "Document" && !o.statusSeen {
			o.statusSeen = true
			o.status = ev.Response.Status
		}
	})
	session.On("Target.attachedToTarget", func(params json.RawMessage) {
		var evt targetAttachedEvent
		if err := json.Unmarshal(params, &evt); err != nil || evt.SessionID == "" {
			return
		}
		child := o.root.ForSession(evt.SessionID)
		o.mu.Lock()
		if len(o.sessions) >= maxRenderSessions {
			o.mu.Unlock()
			return
		}
		o.sessions = append(o.sessions, child)
		o.mu.Unlock()
		o.watch(child, false)
		// Off the dispatch goroutine: a Send from inside a handler would
		// wait on the very read pump that is delivering this event.
		go func() {
			_, _ = child.Send(o.ctx, "Network.enable", map[string]interface{}{})
		}()
	})
}

func (o *renderObserver) mainStatus() int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.status
}

// maxRenderSessions bounds the child targets remembered for cleanup; a
// page opening more frames than this still has its origins cleared.
const maxRenderSessions = 64

// renderCleanupWindow bounds the whole post-render clear.
const renderCleanupWindow = 10 * time.Second

// cleanup clears what the render touched: every observed origin through
// the capturer's own routine, then the storage key of every frame in every
// session (the partitioned storage an origin clear cannot reach).
func (o *renderObserver) cleanup(c *capturer) {
	o.mu.Lock()
	resources := append([]Resource(nil), o.observed...)
	sessions := append([]CDPSession(nil), o.sessions...)
	o.mu.Unlock()
	c.clearObservedOriginsStorage(context.Background(), o.root, resources)

	ctx, cancel := context.WithTimeout(context.Background(), renderCleanupWindow)
	defer cancel()
	keys := map[string]struct{}{}
	for _, session := range sessions {
		for _, key := range frameStorageKeys(ctx, session) {
			keys[key] = struct{}{}
		}
	}
	for key := range keys {
		if ctx.Err() != nil {
			c.logger.Warn("media health render: storage-key clear window elapsed, skipping remaining keys")
			return
		}
		if _, err := o.root.Send(ctx, "Storage.clearDataForStorageKey", map[string]interface{}{
			"storageKey": key, "storageTypes": "all",
		}); err != nil {
			c.logger.Warn("media health render: Storage.clearDataForStorageKey failed", zap.Error(err))
		}
	}
}

// frameStorageKeys returns the storage key of every frame the session's
// frame tree shows, best effort (a frame gone by the time it is asked is
// skipped).
func frameStorageKeys(ctx context.Context, session CDPSession) []string {
	raw, err := session.Send(ctx, "Page.getFrameTree", map[string]interface{}{})
	if err != nil {
		return nil
	}
	var tree struct {
		FrameTree frameTreeNode `json:"frameTree"`
	}
	if err := json.Unmarshal(raw, &tree); err != nil {
		return nil
	}
	var keys []string
	var walk func(n frameTreeNode)
	walk = func(n frameTreeNode) {
		if n.Frame.ID != "" {
			raw, err := session.Send(ctx, "Storage.getStorageKeyForFrame", map[string]interface{}{"frameId": n.Frame.ID})
			if err == nil {
				var reply struct {
					StorageKey string `json:"storageKey"`
				}
				if json.Unmarshal(raw, &reply) == nil && reply.StorageKey != "" {
					keys = append(keys, reply.StorageKey)
				}
			}
		}
		for _, child := range n.ChildFrames {
			walk(child)
		}
	}
	walk(tree.FrameTree)
	return keys
}

type frameTreeNode struct {
	Frame struct {
		ID string `json:"id"`
	} `json:"frame"`
	ChildFrames []frameTreeNode `json:"childFrames"`
}
