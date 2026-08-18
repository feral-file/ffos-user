package screenshot

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image/png"
	"io"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	// MaxDimension caps caller-controlled output dimensions before Chromium allocates
	// the screenshot surface. MaxPixels is the companion aggregate cap.
	MaxDimension = 4096
	// MaxPixels permits a full 3840x2160 (4K UHD) surface while preventing a
	// caller from turning two individually valid 4096-pixel edges into a 4096-square
	// allocation. One-axis bounds are scaled down to this budget when necessary.
	MaxPixels = 3840 * 2160

	// CaptureTimeout bounds target discovery, CDP connection, and PNG encoding.
	CaptureTimeout = 10 * time.Second

	methodGetLayoutMetrics  = "Page.getLayoutMetrics"
	methodCaptureScreenshot = "Page.captureScreenshot"

	maxTargetsResponseBytes = 1 << 20
	// One response is single-flight through HTTP delivery. At MaxPixels, an
	// 8-bit RGBA scanline surface is about 32 MiB; 40 MiB leaves bounded PNG
	// framing/DEFLATE headroom, and 64 MiB carries its base64 JSON envelope.
	maxCDPMessageBytes = 64 << 20
	maxScreenshotBytes = 40 << 20
	// Full PNG decoding is required to validate IDAT data and chunk checksums.
	// Bound the decoded surface first so malformed metadata cannot force an
	// unbounded allocation. Native and custom captures share the same 4K-UHD
	// pixel budget even when their edge lengths differ.
	maxDecodedPixels = MaxPixels
)

var (
	// ErrBusy means another screenshot is already consuming the capture surface.
	ErrBusy = errors.New("screenshot capture is busy")
	// ErrUnavailable means Chromium does not currently expose exactly one page target.
	ErrUnavailable = errors.New("chromium screenshot target is unavailable")
	// ErrInvalidImage means Chromium returned malformed or out-of-bounds PNG data.
	ErrInvalidImage = errors.New("chromium returned an invalid screenshot")
)

// Bounds defines an optional output bounding box. A zero edge is derived from
// the other edge and the viewport ratio; two zero edges request native size.
type Bounds struct {
	Width  int
	Height int
}

// Image is a validated PNG capture and its evidence metadata.
type Image struct {
	Data       []byte
	Width      int
	Height     int
	SHA256     string
	CapturedAt time.Time
}

// Capturer owns the observation-only CDP path used by the Hub screenshot route.
// It intentionally does not share feral-controld's persistent command connection:
// PNG encoding or a stuck capture must not block playback commands and status polls.
type Capturer interface {
	Capture(ctx context.Context, bounds Bounds) (*Image, error)
}

type capturer struct {
	endpoint    string
	httpClient  wrapper.HTTPClient
	dialer      wrapper.WebSocketDialer
	captureSlot chan struct{}
}

// New creates a single-flight screenshot capturer for one Chromium endpoint.
func New(endpoint string, httpClient wrapper.HTTPClient, dialer wrapper.WebSocketDialer) Capturer {
	return &capturer{
		endpoint:    strings.TrimRight(endpoint, "/"),
		httpClient:  httpClient,
		dialer:      dialer,
		captureSlot: make(chan struct{}, 1),
	}
}

func (c *capturer) Capture(ctx context.Context, bounds Bounds) (*Image, error) {
	if err := validateBounds(bounds); err != nil {
		return nil, err
	}

	select {
	case c.captureSlot <- struct{}{}:
		defer func() { <-c.captureSlot }()
	default:
		return nil, ErrBusy
	}

	ctx, cancel := context.WithTimeout(ctx, CaptureTimeout)
	defer cancel()

	targetURL, err := c.pageTargetURL(ctx)
	if err != nil {
		return nil, err
	}

	conn, _, err := c.dialer.DialContext(ctx, targetURL, nil)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("%w: dial CDP page target: %w", ErrUnavailable, err)
	}
	defer func() { _ = conn.Close() }()
	// Gorilla applies the limit while assembling a message. The later length
	// check remains defense in depth, but cannot prevent allocation by itself.
	conn.SetReadLimit(maxCDPMessageBytes)

	deadline, ok := ctx.Deadline()
	if ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set CDP screenshot deadline: %w", err)
		}
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set CDP screenshot write deadline: %w", err)
		}
	}

	requestID := 1
	params := map[string]interface{}{
		"format":                "png",
		"fromSurface":           true,
		"captureBeyondViewport": false,
	}

	if bounds.Width != 0 || bounds.Height != 0 {
		viewport, err := layoutViewport(conn, requestID)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			return nil, err
		}
		requestID++

		_, _, scale, err := fitDimensions(viewport.Width, viewport.Height, bounds)
		if err != nil {
			return nil, err
		}
		params["clip"] = map[string]interface{}{
			"x":      viewport.X,
			"y":      viewport.Y,
			"width":  viewport.Width,
			"height": viewport.Height,
			"scale":  scale,
		}
	}

	var captureResult struct {
		Data string `json:"data"`
	}
	if err := sendCommand(conn, requestID, methodCaptureScreenshot, params, &captureResult); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("capture Chromium screenshot: %w", err)
	}

	imageData, err := decodeScreenshot(captureResult.Data, bounds)
	if err != nil {
		return nil, err
	}
	return imageData, nil
}

func validateBounds(bounds Bounds) error {
	if bounds.Width < 0 || bounds.Height < 0 {
		return fmt.Errorf("screenshot dimensions cannot be negative")
	}
	if bounds.Width > MaxDimension || bounds.Height > MaxDimension {
		return fmt.Errorf("screenshot dimensions cannot exceed %d pixels", MaxDimension)
	}
	if bounds.Width > 0 && bounds.Height > 0 && bounds.Width > MaxPixels/bounds.Height {
		return fmt.Errorf("screenshot bounds cannot exceed %d pixels", MaxPixels)
	}
	return nil
}

func (c *capturer) pageTargetURL(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+"/json", nil)
	if err != nil {
		return "", fmt.Errorf("build CDP targets request: %w", err)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("%w: fetch CDP targets: %w", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%w: CDP targets returned HTTP %d", ErrUnavailable, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxTargetsResponseBytes+1))
	if err != nil {
		return "", fmt.Errorf("%w: read CDP targets: %w", ErrUnavailable, err)
	}
	if len(body) > maxTargetsResponseBytes {
		return "", fmt.Errorf("%w: CDP targets response exceeds %d bytes", ErrUnavailable, maxTargetsResponseBytes)
	}

	var targets []struct {
		Type                 string `json:"type"`
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &targets); err != nil {
		return "", fmt.Errorf("%w: decode CDP targets: %w", ErrUnavailable, err)
	}

	pageTargets := make([]string, 0, 1)
	for _, target := range targets {
		if target.Type == "page" && target.WebSocketDebuggerURL != "" {
			pageTargets = append(pageTargets, target.WebSocketDebuggerURL)
		}
	}
	if len(pageTargets) != 1 {
		return "", fmt.Errorf("%w: expected one page target, found %d", ErrUnavailable, len(pageTargets))
	}
	return pageTargets[0], nil
}

type viewport struct {
	X      float64
	Y      float64
	Width  int
	Height int
}

func layoutViewport(conn wrapper.WebSocketConn, requestID int) (viewport, error) {
	var result struct {
		CSSVisualViewport struct {
			PageX        float64 `json:"pageX"`
			PageY        float64 `json:"pageY"`
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssVisualViewport"`
		CSSLayoutViewport struct {
			PageX        float64 `json:"pageX"`
			PageY        float64 `json:"pageY"`
			ClientWidth  float64 `json:"clientWidth"`
			ClientHeight float64 `json:"clientHeight"`
		} `json:"cssLayoutViewport"`
	}
	if err := sendCommand(conn, requestID, methodGetLayoutMetrics, map[string]interface{}{}, &result); err != nil {
		return viewport{}, fmt.Errorf("get Chromium viewport: %w", err)
	}

	x := result.CSSVisualViewport.PageX
	y := result.CSSVisualViewport.PageY
	width := int(math.Round(result.CSSVisualViewport.ClientWidth))
	height := int(math.Round(result.CSSVisualViewport.ClientHeight))
	if width <= 0 || height <= 0 {
		x = result.CSSLayoutViewport.PageX
		y = result.CSSLayoutViewport.PageY
		width = int(math.Round(result.CSSLayoutViewport.ClientWidth))
		height = int(math.Round(result.CSSLayoutViewport.ClientHeight))
	}
	if width <= 0 || height <= 0 {
		return viewport{}, fmt.Errorf("%w: Chromium returned viewport %dx%d", ErrInvalidImage, width, height)
	}
	return viewport{X: x, Y: y, Width: width, Height: height}, nil
}

func fitDimensions(nativeWidth, nativeHeight int, bounds Bounds) (int, int, float64, error) {
	if nativeWidth <= 0 || nativeHeight <= 0 {
		return 0, 0, 0, fmt.Errorf("%w: invalid native viewport %dx%d", ErrInvalidImage, nativeWidth, nativeHeight)
	}
	if err := validateBounds(bounds); err != nil {
		return 0, 0, 0, err
	}

	scale := 1.0
	switch {
	case bounds.Width > 0 && bounds.Height > 0:
		scale = math.Min(float64(bounds.Width)/float64(nativeWidth), float64(bounds.Height)/float64(nativeHeight))
	case bounds.Width > 0:
		scale = float64(bounds.Width) / float64(nativeWidth)
	case bounds.Height > 0:
		scale = float64(bounds.Height) / float64(nativeHeight)
	}
	if bounds.Width > 0 || bounds.Height > 0 {
		// A caller may provide only one dimension. Keep the proportional dimension
		// bounded as well so a rotated or unusually wide viewport cannot make
		// Chromium allocate an unexpectedly large capture surface.
		scale = min(
			scale,
			float64(MaxDimension)/float64(nativeWidth),
			float64(MaxDimension)/float64(nativeHeight),
			math.Sqrt(float64(MaxPixels)/(float64(nativeWidth)*float64(nativeHeight))),
		)
	}

	width := max(1, int(math.Round(float64(nativeWidth)*scale)))
	height := max(1, int(math.Round(float64(nativeHeight)*scale)))
	// Rounding both axes can put the result a few pixels over the aggregate
	// budget. Tighten scale from an integer-safe width so Chromium receives a
	// proportional scale whose rounded output remains inside MaxPixels.
	if width > MaxPixels/height {
		width = MaxPixels / height
		scale = float64(width) / float64(nativeWidth)
		height = max(1, int(math.Round(float64(nativeHeight)*scale)))
	}
	if bounds.Width > 0 {
		width = min(width, bounds.Width)
	}
	if bounds.Height > 0 {
		height = min(height, bounds.Height)
	}
	return width, height, scale, nil
}

type cdpRequest struct {
	ID     int                    `json:"id"`
	Method string                 `json:"method"`
	Params map[string]interface{} `json:"params"`
}

func sendCommand(conn wrapper.WebSocketConn, id int, method string, params map[string]interface{}, result interface{}) error {
	request, err := json.Marshal(cdpRequest{ID: id, Method: method, Params: params})
	if err != nil {
		return fmt.Errorf("marshal CDP %s request: %w", method, err)
	}
	if err := conn.WriteMessage(websocket.TextMessage, request); err != nil {
		return fmt.Errorf("write CDP %s request: %w", method, err)
	}

	for {
		_, response, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read CDP %s response: %w", method, err)
		}
		if len(response) > maxCDPMessageBytes {
			return fmt.Errorf("CDP %s response exceeds %d bytes", method, maxCDPMessageBytes)
		}

		var envelope struct {
			ID     int             `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal(response, &envelope); err != nil {
			return fmt.Errorf("decode CDP %s response: %w", method, err)
		}
		if envelope.ID != id {
			continue
		}
		if envelope.Error != nil {
			return fmt.Errorf("CDP %s failed (%d): %s", method, envelope.Error.Code, envelope.Error.Message)
		}
		if err := json.Unmarshal(envelope.Result, result); err != nil {
			return fmt.Errorf("decode CDP %s result: %w", method, err)
		}
		return nil
	}
}

func decodeScreenshot(encoded string, bounds Bounds) (*Image, error) {
	if encoded == "" {
		return nil, fmt.Errorf("%w: empty image data", ErrInvalidImage)
	}
	if base64.StdEncoding.DecodedLen(len(encoded)) > maxScreenshotBytes {
		return nil, fmt.Errorf("%w: decoded image exceeds %d bytes", ErrInvalidImage, maxScreenshotBytes)
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, fmt.Errorf("%w: decode base64: %w", ErrInvalidImage, err)
	}
	if len(data) > maxScreenshotBytes {
		return nil, fmt.Errorf("%w: image exceeds %d bytes", ErrInvalidImage, maxScreenshotBytes)
	}

	config, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("%w: decode PNG metadata: %w", ErrInvalidImage, err)
	}
	if config.Width <= 0 || config.Height <= 0 {
		return nil, fmt.Errorf("%w: image has invalid dimensions %dx%d", ErrInvalidImage, config.Width, config.Height)
	}
	if config.Width > maxDecodedPixels/config.Height {
		return nil, fmt.Errorf(
			"%w: image dimensions %dx%d exceed the %d-pixel decode limit",
			ErrInvalidImage,
			config.Width,
			config.Height,
			maxDecodedPixels,
		)
	}
	if bounds.Width > 0 && config.Width > bounds.Width {
		return nil, fmt.Errorf("%w: image width %d exceeds requested bound %d", ErrInvalidImage, config.Width, bounds.Width)
	}
	if bounds.Height > 0 && config.Height > bounds.Height {
		return nil, fmt.Errorf("%w: image height %d exceeds requested bound %d", ErrInvalidImage, config.Height, bounds.Height)
	}
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return nil, fmt.Errorf("%w: fully decode PNG: %w", ErrInvalidImage, err)
	}

	digest := sha256.Sum256(data)
	return &Image{
		Data:       data,
		Width:      config.Width,
		Height:     config.Height,
		SHA256:     hex.EncodeToString(digest[:]),
		CapturedAt: time.Now().UTC(),
	}, nil
}
