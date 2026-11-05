package offlinecache

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// renderSession is a CDPSession fake for the render conversation: it
// records the methods sent in order, answers Page.navigate and
// Page.captureScreenshot from a script, and lets a test fire the
// Network.responseReceived events the renderer listens for.
type renderSession struct {
	mu       sync.Mutex
	sent     []string
	calls    []sessionCall
	params   map[string]map[string]interface{}
	handlers map[string][]func(json.RawMessage)
	navError string
	shot     []byte
	fail     map[string]error
	// replies answers a method with a scripted result; a reply keyed by
	// "method|<frameId>" answers Storage.getStorageKeyForFrame per frame.
	replies  map[string]json.RawMessage
	children map[string]*renderSession
	parent   *renderSession
	// failByID fails a method on one child only ("method@<id>"); shared
	// with children like replies.
	failByID map[string]error
	// id is the child session's id ("" for the root); a reply keyed by
	// "method@<id>" answers that child only. The replies map is SHARED
	// with children and must be complete before the render starts.
	id string
	// onNavigate runs when Page.navigate is sent, after the handlers are
	// registered, so a test can emit the main-document response.
	onNavigate func(s *renderSession)
}

type sessionCall struct {
	method string
	params map[string]interface{}
}

func newRenderSession() *renderSession {
	return &renderSession{params: map[string]map[string]interface{}{}, handlers: map[string][]func(json.RawMessage){}, fail: map[string]error{}, replies: map[string]json.RawMessage{}, children: map[string]*renderSession{}, failByID: map[string]error{}}
}

func (s *renderSession) Send(_ context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	s.mu.Lock()
	s.sent = append(s.sent, method)
	s.calls = append(s.calls, sessionCall{method: method, params: params})
	s.params[method] = params
	navErr, shot, failErr, onNav := s.navError, s.shot, s.fail[method], s.onNavigate
	if failErr == nil && s.id != "" {
		failErr = s.failByID[method+"@"+s.id]
	}
	reply, hasReply := s.replies[method]
	if s.id != "" {
		if r, ok := s.replies[method+"@"+s.id]; ok {
			reply, hasReply = r, true
		}
	}
	if method == "Storage.getStorageKeyForFrame" {
		if id, ok := params["frameId"].(string); ok {
			reply, hasReply = s.replies[method+"|"+id]
		}
	}
	s.mu.Unlock()
	if failErr != nil {
		return nil, failErr
	}
	if hasReply {
		return reply, nil
	}
	switch method {
	case "Page.navigate":
		if onNav != nil {
			onNav(s)
		}
		if navErr != "" {
			return json.RawMessage(`{"frameId":"F","errorText":"` + navErr + `"}`), nil
		}
		return json.RawMessage(`{"frameId":"F"}`), nil
	case "Page.captureScreenshot":
		return json.RawMessage(`{"data":"` + base64.StdEncoding.EncodeToString(shot) + `"}`), nil
	}
	return json.RawMessage(`{}`), nil
}

// On appends, like the real session: the capturer's guard and the render
// observer both listen on the same methods.
func (s *renderSession) On(method string, h func(json.RawMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = append(s.handlers[method], h)
}

// ForSession returns a DISTINCT child view that shares the parent's
// scripted replies, as flatten-mode child targets do.
func (s *renderSession) ForSession(id string) CDPSession {
	if id == "" {
		return s
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c, ok := s.children[id]; ok {
		return c
	}
	c := newRenderSession()
	c.parent = s
	c.id = id
	c.replies = s.replies
	c.failByID = s.failByID
	c.shot = s.shot
	s.children[id] = c
	return c
}
func (s *renderSession) Close() error { return nil }

func (s *renderSession) emit(method string, payload string) {
	s.mu.Lock()
	var hs []func(json.RawMessage)
	hs = append(hs, s.handlers[method]...)
	s.mu.Unlock()
	for _, h := range hs {
		h(json.RawMessage(payload))
	}
}

// armedChild waits for the child session id to exist with the guard's
// Fetch.requestPaused handler registered.
func (s *renderSession) armedChild(t *testing.T, id string) *renderSession {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if c := s.child(id); c != nil {
			c.mu.Lock()
			armed := len(c.handlers["Fetch.requestPaused"]) > 0
			c.mu.Unlock()
			if armed {
				return c
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("child %s never armed", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func (s *renderSession) child(id string) *renderSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.children[id]
}

// callsOf returns every call of method with its params.
func (s *renderSession) callsOf(method string) []map[string]interface{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []map[string]interface{}
	for _, c := range s.calls {
		if c.method == method {
			out = append(out, c.params)
		}
	}
	return out
}

// waitCalls polls until method has been sent n times (the guard decides
// on its own goroutine).
func (s *renderSession) waitCalls(t *testing.T, method string, n int) []map[string]interface{} {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if got := s.callsOf(method); len(got) >= n {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: expected %d calls, got %d", method, n, len(s.callsOf(method)))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func (s *renderSession) methods() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.sent...)
}

func pngOf(c color.Color, w, h int) []byte {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, c)
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		panic(err)
	}
	return buf.Bytes()
}

func newTestRenderer(t *testing.T) *mediaHealthRenderer {
	t.Helper()
	return newTestRendererWithPolicy(t, nil)
}

// The guard resolves every name to a public address, so only a literal
// reserved address is refused.
func newTestRendererWithPolicy(t *testing.T, policy mediahealth.Rewriter) *mediaHealthRenderer {
	t.Helper()
	c := &capturer{
		logger: zaptest.NewLogger(t),
		json:   wrapper.NewJSON(),
		guard:  sourceGuard{resolver: staticResolver{ip: "93.184.216.34"}},
	}
	return &mediaHealthRenderer{c: c, policy: policy}
}

func TestMediaHealthRenderer_ConversationAndFrame(t *testing.T) {
	s := newRenderSession()
	s.shot = pngOf(color.RGBA{R: 10, G: 20, B: 30, A: 255}, 64, 48)
	s.onNavigate = func(s *renderSession) {
		// A subresource (ignored), the main frame's document, then an
		// iframe's document (ignored: another frame).
		s.emit("Network.responseReceived", `{"type":"Script","frameId":"F","response":{"status":500}}`)
		s.emit("Network.responseReceived", `{"type":"Document","frameId":"F","response":{"status":203}}`)
		s.emit("Network.responseReceived", `{"type":"Document","frameId":"I","response":{"status":404}}`)
	}
	r := newTestRenderer(t)
	frame, err := r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, frame)
	assert.Equal(t, 203, frame.MainStatus, "the main frame's document status, not a subresource's or a child frame's")
	assert.Equal(t, image.Rect(0, 0, 64, 48), frame.Image.Bounds())
	r8, g8, b8, _ := frame.Image.At(1, 1).RGBA()
	assert.Equal(t, []uint32{10 << 8, 20 << 8, 30 << 8}, []uint32{r8 &^ 0xff, g8 &^ 0xff, b8 &^ 0xff})

	methods := s.methods()
	// The guard is armed around the navigate exactly as a capture arms it:
	// Fetch.enable before the navigate, the child-target auto-attach, then
	// the navigate, then the viewport-bounded screenshot. No cache or
	// storage reset: the context is disposable.
	idx := func(m string) int {
		for i, got := range methods {
			if got == m {
				return i
			}
		}
		return -1
	}
	for _, m := range []string{"Network.enable", "Page.enable", "Emulation.setDeviceMetricsOverride", "Fetch.enable", "Target.setAutoAttach", "Page.navigate", "Page.captureScreenshot"} {
		assert.NotEqual(t, -1, idx(m), m)
	}
	assert.NotContains(t, methods, "Storage.clearDataForOrigin")
	assert.Less(t, idx("Fetch.enable"), idx("Page.navigate"))
	assert.Less(t, idx("Target.setAutoAttach"), idx("Page.navigate"))
	assert.Less(t, idx("Page.navigate"), idx("Page.captureScreenshot"))
	metrics := s.params["Emulation.setDeviceMetricsOverride"]
	assert.Equal(t, renderViewport, metrics["width"])
	assert.Equal(t, renderViewport, metrics["height"])
	assert.Equal(t, false, s.params["Page.captureScreenshot"]["captureBeyondViewport"], "a page must not be able to grow the capture")
	assert.Equal(t, "https://art.example/p", s.params["Page.navigate"]["url"])
}

func TestMediaHealthRenderer_FailuresAreErrorsNotFrames(t *testing.T) {
	r := newTestRenderer(t)

	s := newRenderSession()
	s.navError = "net::ERR_NAME_NOT_RESOLVED"
	_, err := r.renderOnSession(context.Background(), s, "https://gone.example/p", time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ERR_NAME_NOT_RESOLVED")
	assert.NotContains(t, s.methods(), "Page.captureScreenshot", "no screenshot after a failed navigation")

	s = newRenderSession()
	s.fail["Fetch.enable"] = errors.New("simulated")
	_, err = r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.Error(t, err)
	assert.NotContains(t, s.methods(), "Page.navigate", "the guard must be armed before anything navigates")

	s = newRenderSession()
	s.shot = []byte("not a png")
	_, err = r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.Error(t, err)

	// The settle honors cancellation.
	s = newRenderSession()
	s.shot = pngOf(color.Black, 8, 8)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.renderOnSession(ctx, s, "https://art.example/p", time.Hour)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.NotContains(t, s.methods(), "Page.captureScreenshot")
}

func TestDecodeScreenshotPNG_Caps(t *testing.T) {
	_, err := decodeScreenshotPNG(nil)
	assert.Error(t, err)
	_, err = decodeScreenshotPNG(make([]byte, maxScreenshotBytes+1))
	assert.Error(t, err)
	// A declared geometry past the cap is refused on the header alone.
	var buf bytes.Buffer
	require.NoError(t, png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))))
	raw := buf.Bytes()
	// Patch the IHDR width (bytes 16..20) to 100000.
	raw[16], raw[17], raw[18], raw[19] = 0x00, 0x01, 0x86, 0xa0
	_, err = decodeScreenshotPNG(raw)
	assert.Error(t, err)
	img, err := decodeScreenshotPNG(pngOf(color.White, 4, 4))
	require.NoError(t, err)
	assert.Equal(t, image.Rect(0, 0, 4, 4), img.Bounds())
}

func TestNewMediaHealthRenderer_NilForForeignCapturer(t *testing.T) {
	assert.Nil(t, NewMediaHealthRenderer(nil, nil))
	assert.NotNil(t, NewMediaHealthRenderer(&capturer{}, nil))
	r := NewMediaHealthRenderer(&capturer{}, nil)
	assert.NotNil(t, r)
	assert.Implements(t, (*mediahealth.Renderer)(nil), r)
}

// F22 (PR #443 review, round 2): the kiosk's gateway policy applies to
// EVERY request of the render — the navigation, redirects, child frames,
// subresources — through the guard, and the guard judges the rewritten
// destination.
func TestMediaHealthRenderer_PolicyAppliesToEveryRequest(t *testing.T) {
	policy := hopRewriter{retired: "https://retired.example", replacement: "https://replacement.example"}
	r := newTestRendererWithPolicy(t, policy)
	s := newRenderSession()
	s.shot = pngOf(color.White, 8, 8)
	s.onNavigate = func(s *renderSession) {
		// A subresource on the retired gateway, a plain request to it, an
		// unrelated origin, and the same from a child frame's session.
		s.emit("Fetch.requestPaused", `{"requestId":"r1","request":{"url":"https://retired.example/ipfs/bafy/a.js","headers":{"User-Agent":"Chromium","Accept":"*/*"}}}`)
		s.emit("Fetch.requestPaused", `{"requestId":"r2","request":{"url":"https://elsewhere.example/b.js","headers":{"User-Agent":"Chromium"}}}`)
		s.emit("Target.attachedToTarget", `{"sessionId":"child-1","targetInfo":{"targetId":"t1","type":"iframe"}}`)
		// A real child target stays paused (waitForDebuggerOnStart) until
		// the capturer's setup armed it; here that setup runs on its own
		// goroutine, so wait for the arming before the child's first request.
		s.armedChild(t, "child-1").emit("Fetch.requestPaused", `{"requestId":"r3","request":{"url":"https://retired.example/ipfs/bafy/frame.html","headers":{"User-Agent":"Chromium"}}}`)
	}
	_, err := r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)

	cont := s.waitCalls(t, "Fetch.continueRequest", 2)
	byID := map[string]map[string]interface{}{}
	for _, c := range cont {
		byID[c["requestId"].(string)] = c
	}
	assert.Equal(t, "https://replacement.example/ipfs/bafy/a.js", byID["r1"]["url"], "a retired-gateway request is redirected, as the kiosk does")
	assert.Nil(t, byID["r1"]["headers"])
	assert.Nil(t, byID["r2"]["url"], "an unmatched request continues as is")
	assert.Nil(t, byID["r2"]["headers"])
	child := s.child("child-1")
	require.NotNil(t, child)
	childCont := child.waitCalls(t, "Fetch.continueRequest", 1)
	assert.Equal(t, "https://replacement.example/ipfs/bafy/frame.html", childCont[0]["url"], "the policy rides on the guard the capturer arms on child targets too")
	assert.Empty(t, s.callsOf("Fetch.failRequest"))

	// A matched host without a content-addressed path gets the policy
	// agent; a rewrite that lands on a reserved address is refused by
	// the guard — the rewritten destination is what it judges.
	r = newTestRendererWithPolicy(t, agentOnlyRewriter{prefix: "https://hostile.example"})
	s = newRenderSession()
	s.shot = pngOf(color.White, 8, 8)
	s.onNavigate = func(s *renderSession) {
		s.emit("Fetch.requestPaused", `{"requestId":"ua","request":{"url":"https://hostile.example/ipns/name","headers":{"Accept":"*/*","user-agent":"Chromium"}}}`)
	}
	_, err = r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	cont = s.waitCalls(t, "Fetch.continueRequest", 1)
	assert.Nil(t, cont[0]["url"])
	assert.Equal(t, []map[string]string{{"name": "Accept", "value": "*/*"}, {"name": "user-agent", "value": "feral-player/2.0"}}, cont[0]["headers"])

	toReserved := hopRewriter{retired: "https://retired.example", replacement: "http://169.254.169.254"}
	r = newTestRendererWithPolicy(t, toReserved)
	s = newRenderSession()
	s.shot = pngOf(color.White, 8, 8)
	s.onNavigate = func(s *renderSession) {
		s.emit("Fetch.requestPaused", `{"requestId":"bad","request":{"url":"https://retired.example/ipfs/bafy/x","headers":{}}}`)
	}
	_, err = r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	failed := s.waitCalls(t, "Fetch.failRequest", 1)
	assert.Equal(t, "bad", failed[0]["requestId"], "the guard judges the rewritten destination")
	assert.Empty(t, s.callsOf("Fetch.continueRequest"))
}

// agentOnlyRewriter matches a host without rewriting any URL: the policy's
// User-Agent branch.
type agentOnlyRewriter struct{ prefix string }

func (agentOnlyRewriter) RewriteURL(string) (string, bool) { return "", false }
func (r agentOnlyRewriter) Matches(raw string) bool        { return strings.HasPrefix(raw, r.prefix) }
func (agentOnlyRewriter) UserAgent() string                { return "feral-player/2.0" }

// F24 (round 3): a child target the guard could not contain (a worker has
// no Fetch domain) is left paused by the capturer; the page then paints
// without it and the frame must not be evidence.
func TestMediaHealthRenderer_ContainedChildIsNotEvidence(t *testing.T) {
	s := newRenderSession()
	s.shot = pngOf(color.Black, 8, 8)
	s.failByID["Fetch.enable@worker-1"] = errors.New("'Fetch.enable' wasn't found")
	s.onNavigate = func(s *renderSession) {
		s.emit("Target.attachedToTarget", `{"sessionId":"worker-1","targetInfo":{"targetId":"w","type":"worker"}}`)
	}
	r := newTestRenderer(t)
	// The capturer's child setup runs on its own goroutine; the settle
	// outlasts it.
	_, err := r.renderOnSession(context.Background(), s, "https://a.test/p", 100*time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "left contained")
	assert.NotContains(t, s.methods(), "Page.captureScreenshot", "no frame is taken from a page missing its worker")

	// A child that arms fine is no objection.
	s = newRenderSession()
	s.shot = pngOf(color.White, 8, 8)
	s.onNavigate = func(s *renderSession) {
		s.emit("Target.attachedToTarget", `{"sessionId":"frame-1","targetInfo":{"targetId":"f","type":"iframe"}}`)
	}
	_, err = r.renderOnSession(context.Background(), s, "https://a.test/p", 100*time.Millisecond)
	require.NoError(t, err)
}

// F20 (round 4): every render runs in its own disposable browser context,
// so whatever storage the page or any of its frames wrote — including a
// frame removed mid-render — dies with the context. The browser
// conversation: create the context, a page in it, attach flat, render on
// that session, dispose the context on every exit path.
func TestMediaHealthRenderer_DisposableBrowserContext(t *testing.T) {
	browser := newRenderSession()
	browser.replies["Target.createBrowserContext"] = json.RawMessage(`{"browserContextId":"BC1"}`)
	browser.replies["Target.createTarget"] = json.RawMessage(`{"targetId":"T1"}`)
	browser.replies["Target.attachToTarget"] = json.RawMessage(`{"sessionId":"S1"}`)
	browser.shot = pngOf(color.White, 8, 8)
	r := newTestRenderer(t)
	frame, err := r.renderInDisposableContext(context.Background(), browser, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, frame)

	assert.Equal(t, true, browser.params["Target.createBrowserContext"]["disposeOnDetach"])
	assert.Equal(t, "BC1", browser.params["Target.createTarget"]["browserContextId"])
	assert.Equal(t, "about:blank", browser.params["Target.createTarget"]["url"])
	assert.Equal(t, "T1", browser.params["Target.attachToTarget"]["targetId"])
	assert.Equal(t, true, browser.params["Target.attachToTarget"]["flatten"])
	assert.Equal(t, "BC1", browser.params["Target.disposeBrowserContext"]["browserContextId"])
	bm := browser.methods()
	assert.Equal(t, "Target.disposeBrowserContext", bm[len(bm)-1], "disposed last")
	page := browser.child("S1")
	require.NotNil(t, page, "the render ran on the attached page session")
	assert.Contains(t, page.methods(), "Page.navigate")
	assert.Contains(t, page.methods(), "Page.captureScreenshot")
	assert.NotContains(t, browser.methods(), "Page.navigate", "never on the browser session")

	// A failed render still disposes the context.
	browser = newRenderSession()
	browser.replies["Target.createBrowserContext"] = json.RawMessage(`{"browserContextId":"BC2"}`)
	browser.replies["Target.createTarget"] = json.RawMessage(`{"targetId":"T2"}`)
	browser.replies["Target.attachToTarget"] = json.RawMessage(`{"sessionId":"S2"}`)
	browser.replies["Page.navigate@S2"] = json.RawMessage(`{"frameId":"F","errorText":"net::ERR_FAILED"}`)
	_, err = r.renderInDisposableContext(context.Background(), browser, "https://art.example/p", time.Millisecond)
	require.Error(t, err)
	assert.Equal(t, "BC2", browser.params["Target.disposeBrowserContext"]["browserContextId"])

	// No context: unavailable, nothing else attempted.
	browser = newRenderSession()
	browser.fail["Target.createBrowserContext"] = errors.New("simulated")
	_, err = r.renderInDisposableContext(context.Background(), browser, "https://art.example/p", time.Millisecond)
	require.Error(t, err)
	assert.ErrorIs(t, err, mediahealth.ErrRendererUnavailable)
	assert.NotContains(t, browser.methods(), "Target.createTarget")
}

// F26 (round 4): the status is the main frame's LATEST document — a page
// that replaces itself onto an error page is judged by that page — and a
// child frame's document never counts.
func TestMediaHealthRenderer_MainStatusFollowsTheMainFrame(t *testing.T) {
	s := newRenderSession()
	s.shot = pngOf(color.Black, 8, 8)
	s.onNavigate = func(s *renderSession) {
		s.emit("Network.responseReceived", `{"type":"Document","frameId":"F","response":{"status":200}}`)
		s.emit("Network.responseReceived", `{"type":"Document","frameId":"I","response":{"status":200}}`)
		s.emit("Network.responseReceived", `{"type":"Document","frameId":"F","response":{"status":503}}`)
	}
	r := newTestRenderer(t)
	frame, err := r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, 503, frame.MainStatus, "the document on screen at capture time")

	// No frameId on the navigate reply: the first document response.
	s = newRenderSession()
	s.shot = pngOf(color.Black, 8, 8)
	s.replies["Page.navigate"] = json.RawMessage(`{}`)
	s.onNavigate = func(s *renderSession) {}
	s.replies["Page.navigate"] = json.RawMessage(`{}`)
	frame, err = r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	assert.Equal(t, 0, frame.MainStatus, "unknown when nothing was observed")
}
