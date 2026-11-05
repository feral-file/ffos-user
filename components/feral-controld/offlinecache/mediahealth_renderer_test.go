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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/mediahealth"
)

// renderSession is a CDPSession fake for the render conversation: it
// records the methods sent in order, answers Page.navigate and
// Page.captureScreenshot from a script, and lets a test fire the
// Network.responseReceived events the renderer listens for.
type renderSession struct {
	mu       sync.Mutex
	sent     []string
	params   map[string]map[string]interface{}
	handlers map[string]func(json.RawMessage)
	navError string
	clears   []string
	shot     []byte
	fail     map[string]error
	// onNavigate runs when Page.navigate is sent, after the handlers are
	// registered, so a test can emit the main-document response.
	onNavigate func(s *renderSession)
}

func newRenderSession() *renderSession {
	return &renderSession{params: map[string]map[string]interface{}{}, handlers: map[string]func(json.RawMessage){}, fail: map[string]error{}}
}

func (s *renderSession) Send(_ context.Context, method string, params map[string]interface{}) (json.RawMessage, error) {
	s.mu.Lock()
	s.sent = append(s.sent, method)
	s.params[method] = params
	if method == "Storage.clearDataForOrigin" {
		if origin, ok := params["origin"].(string); ok {
			s.clears = append(s.clears, origin)
		}
	}
	navErr, shot, failErr, onNav := s.navError, s.shot, s.fail[method], s.onNavigate
	s.mu.Unlock()
	if failErr != nil {
		return nil, failErr
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

func (s *renderSession) On(method string, h func(json.RawMessage)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.handlers[method] = h
}
func (s *renderSession) ForSession(string) CDPSession { return s }
func (s *renderSession) Close() error                 { return nil }

func (s *renderSession) emit(method string, payload string) {
	s.mu.Lock()
	h := s.handlers[method]
	s.mu.Unlock()
	if h != nil {
		h(json.RawMessage(payload))
	}
}

func (s *renderSession) clearedOrigins() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.clears...)
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
	return &mediaHealthRenderer{c: &capturer{logger: zaptest.NewLogger(t)}}
}

func TestMediaHealthRenderer_ConversationAndFrame(t *testing.T) {
	s := newRenderSession()
	s.shot = pngOf(color.RGBA{R: 10, G: 20, B: 30, A: 255}, 64, 48)
	s.onNavigate = func(s *renderSession) {
		// A subresource first (ignored), then the main document, then an
		// iframe document (ignored: only the first Document counts).
		s.emit("Network.responseReceived", `{"type":"Script","response":{"status":500}}`)
		s.emit("Network.responseReceived", `{"type":"Document","response":{"status":203}}`)
		s.emit("Network.responseReceived", `{"type":"Document","response":{"status":404}}`)
	}
	r := newTestRenderer(t)
	frame, err := r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	require.NotNil(t, frame)
	assert.Equal(t, 203, frame.MainStatus, "the main document's status, not a subresource's or a child frame's")
	assert.Equal(t, image.Rect(0, 0, 64, 48), frame.Image.Bounds())
	r8, g8, b8, _ := frame.Image.At(1, 1).RGBA()
	assert.Equal(t, []uint32{10 << 8, 20 << 8, 30 << 8}, []uint32{r8 &^ 0xff, g8 &^ 0xff, b8 &^ 0xff})

	methods := s.methods()
	// The guard is armed around the navigate exactly as a capture arms it:
	// reset first (our clears are never paused), Fetch.enable before the
	// navigate, the child-target auto-attach, then the navigate, then the
	// viewport-bounded screenshot.
	idx := func(m string) int {
		for i, got := range methods {
			if got == m {
				return i
			}
		}
		return -1
	}
	for _, m := range []string{"Network.enable", "Page.enable", "Emulation.setDeviceMetricsOverride", "Network.clearBrowserCache", "Storage.clearDataForOrigin", "Fetch.enable", "Target.setAutoAttach", "Page.navigate", "Page.captureScreenshot"} {
		assert.NotEqual(t, -1, idx(m), m)
	}
	assert.Less(t, idx("Storage.clearDataForOrigin"), idx("Fetch.enable"))
	assert.Less(t, idx("Fetch.enable"), idx("Page.navigate"))
	assert.Less(t, idx("Target.setAutoAttach"), idx("Page.navigate"))
	assert.Less(t, idx("Page.navigate"), idx("Page.captureScreenshot"))
	metrics := s.params["Emulation.setDeviceMetricsOverride"]
	assert.Equal(t, renderViewport, metrics["width"])
	assert.Equal(t, renderViewport, metrics["height"])
	assert.Equal(t, false, s.params["Page.captureScreenshot"]["captureBeyondViewport"], "a page must not be able to grow the capture")
	assert.Equal(t, "https://art.example/p", s.params["Page.navigate"]["url"])
	assert.Equal(t, "https://art.example", s.params["Storage.clearDataForOrigin"]["origin"])
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
	assert.Nil(t, NewMediaHealthRenderer(nil))
	assert.NotNil(t, NewMediaHealthRenderer(&capturer{}))
	r := NewMediaHealthRenderer(&capturer{})
	assert.NotNil(t, r)
	assert.Implements(t, (*mediahealth.Renderer)(nil), r)
}

// F20 (PR #443 review): every origin the render touched is cleared before
// the browser goes back to the capturer, on the success and the failure
// path alike.
func TestMediaHealthRenderer_ClearsEveryObservedOrigin(t *testing.T) {
	s := newRenderSession()
	s.shot = pngOf(color.White, 8, 8)
	s.onNavigate = func(s *renderSession) {
		s.emit("Network.responseReceived", `{"type":"Document","response":{"status":200,"url":"https://art.example/p"}}`)
		s.emit("Network.responseReceived", `{"type":"Document","response":{"status":200,"url":"https://frames.example/inner"}}`)
		s.emit("Network.responseReceived", `{"type":"Script","response":{"status":200,"url":"https://cdn.example/lib.js"}}`)
		s.emit("Network.responseReceived", `{"type":"Image","response":{"status":200,"url":"data:image/png;base64,AAAA"}}`)
	}
	r := newTestRenderer(t)
	_, err := r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.NoError(t, err)
	cleared := s.clearedOrigins()
	assert.ElementsMatch(t, []string{"https://art.example", "https://art.example", "https://frames.example", "https://cdn.example"}, cleared,
		"the pre-navigation reset of the source origin, then every observed http(s) origin after the render")

	// The failure path clears too.
	s = newRenderSession()
	s.navError = "net::ERR_FAILED"
	s.onNavigate = func(s *renderSession) {
		s.emit("Network.responseReceived", `{"type":"Document","response":{"status":302,"url":"https://redirect.example/x"}}`)
	}
	_, err = r.renderOnSession(context.Background(), s, "https://art.example/p", time.Millisecond)
	require.Error(t, err)
	assert.Contains(t, s.clearedOrigins(), "https://redirect.example")
}
