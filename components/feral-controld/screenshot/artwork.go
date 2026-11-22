package screenshot

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"math"
	"time"
)

// ArtworkFrame is a capture of the artwork on the wall, for the media-health
// render check (feral-file/ffos-user#392): the frame clipped to the active
// artwork element and downscaled, and the rectangles inside it that the
// player's own UI covers. The frame never leaves the device; the caller
// keeps only what it classifies (a variance and a perceptual hash).
type ArtworkFrame struct {
	Image image.Image
	// Masks are the player's UI over the artwork (the title label), in
	// Image's pixel coordinates.
	Masks []image.Rectangle
	// Clipped is false when no artwork element was found and the whole
	// viewport was captured instead.
	Clipped    bool
	CapturedAt time.Time
}

// ErrCovered means something other than the artwork is drawn over its
// center — the player's loading overlay or error modal, a setup or pairing
// overlay — so there is nothing to judge.
var ErrCovered = errors.New("artwork is covered by a player overlay")

const methodEvaluate = "Runtime.evaluate"

// artworkLayoutScript locates the artwork and the player UI over it, in CSS
// pixels of the viewport. It is coupled to ff-player's DOM (verified on its
// main at d748416), and degrades rather than fails when that DOM changes:
//
//   - the artwork is the largest img/video/iframe/object/model-viewer with
//     no ancestor whose inline opacity is below 1 — ff-player double-buffers
//     two slot wrappers and fades the hidden one to opacity 0, and marks
//     neither (no data attribute); not found → the whole viewport;
//   - covered: the element at the artwork's center is neither the artwork
//     nor inside it. The title label and the cursor layer are
//     pointer-events:none and so are invisible to elementFromPoint; the
//     loading overlay, the error modal and the setup/pairing overlays are
//     not;
//   - masks: the title label ([data-testid="tombstone-overlay"]), pinned to
//     the viewport's bottom-left over the artwork, while visible
//     (aria-hidden false and not faded out). Never hidden by changing the
//     owner's tombstone setting: that is their saved preference.
const artworkLayoutScript = `(() => {
  const rect = (r) => ({x: r.left + scrollX, y: r.top + scrollY, w: r.width, h: r.height});
  const shown = (el) => {
    for (let n = el; n && n !== document.documentElement; n = n.parentElement) {
      const o = n.style && n.style.opacity;
      if (o !== undefined && o !== '' && parseFloat(o) < 0.99) return false;
    }
    return true;
  };
  let art = null, area = 0;
  for (const el of document.querySelectorAll('img,video,iframe,object,model-viewer')) {
    const r = el.getBoundingClientRect();
    if (r.width < 32 || r.height < 32 || !shown(el)) continue;
    if (r.width * r.height > area) { art = el; area = r.width * r.height; }
  }
  let covered = false;
  if (art) {
    const r = art.getBoundingClientRect();
    const hit = document.elementFromPoint(r.left + r.width / 2, r.top + r.height / 2);
    covered = !(hit && (hit === art || art.contains(hit)));
  }
  const masks = [];
  const label = document.querySelector('[data-testid="tombstone-overlay"]');
  if (label && label.getAttribute('aria-hidden') !== 'true' && parseFloat(getComputedStyle(label).opacity) > 0.05) {
    const r = label.getBoundingClientRect();
    if (r.width > 0 && r.height > 0) masks.push(rect(r));
  }
  return JSON.stringify({
    artwork: art ? rect(art.getBoundingClientRect()) : null,
    covered, masks,
    viewport: {x: scrollX, y: scrollY, w: innerWidth, h: innerHeight},
  });
})()`

type cssRect struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	W float64 `json:"w"`
	H float64 `json:"h"`
}

type artworkLayout struct {
	Artwork  *cssRect  `json:"artwork"`
	Covered  bool      `json:"covered"`
	Masks    []cssRect `json:"masks"`
	Viewport cssRect   `json:"viewport"`
}

// CaptureArtwork captures the artwork on the wall, clipped to its element
// and downscaled so its longer edge is at most maxEdge pixels. It shares
// Capture's single flight (ErrBusy while another capture runs), page target
// discovery, connection and PNG validation.
func (c *capturer) CaptureArtwork(ctx context.Context, maxEdge int) (*ArtworkFrame, error) {
	if maxEdge <= 0 || maxEdge > MaxDimension {
		return nil, fmt.Errorf("artwork capture edge must be 1-%d pixels, got %d", MaxDimension, maxEdge)
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
	conn.SetReadLimit(maxCDPMessageBytes)
	if deadline, ok := ctx.Deadline(); ok {
		if err := conn.SetReadDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set CDP artwork capture deadline: %w", err)
		}
		if err := conn.SetWriteDeadline(deadline); err != nil {
			return nil, fmt.Errorf("set CDP artwork capture write deadline: %w", err)
		}
	}

	var evaluated struct {
		Result struct {
			Value string `json:"value"`
		} `json:"result"`
		ExceptionDetails *struct {
			Text string `json:"text"`
		} `json:"exceptionDetails"`
	}
	if err := sendCommand(conn, 1, methodEvaluate, map[string]interface{}{
		"expression": artworkLayoutScript, "returnByValue": true,
	}, &evaluated); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("locate artwork: %w", err)
	}
	if evaluated.ExceptionDetails != nil {
		return nil, fmt.Errorf("locate artwork: script threw: %s", evaluated.ExceptionDetails.Text)
	}
	var layout artworkLayout
	if err := json.Unmarshal([]byte(evaluated.Result.Value), &layout); err != nil {
		return nil, fmt.Errorf("locate artwork: %w", err)
	}
	if layout.Covered {
		return nil, ErrCovered
	}
	clip, clipped := layout.Viewport, false
	if layout.Artwork != nil {
		clip, clipped = *layout.Artwork, true
	}
	if clip.W < 1 || clip.H < 1 {
		return nil, fmt.Errorf("%w: artwork area %.0fx%.0f", ErrUnavailable, clip.W, clip.H)
	}
	scale := math.Min(1, float64(maxEdge)/math.Max(clip.W, clip.H))

	var shot struct {
		Data string `json:"data"`
	}
	if err := sendCommand(conn, 2, methodCaptureScreenshot, map[string]interface{}{
		"format": "png", "fromSurface": true, "captureBeyondViewport": false,
		"clip": map[string]interface{}{"x": clip.X, "y": clip.Y, "width": clip.W, "height": clip.H, "scale": scale},
	}, &shot); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("capture artwork: %w", err)
	}
	decoded, err := decodeScreenshot(shot.Data, Bounds{})
	if err != nil {
		return nil, err
	}
	img, err := png.Decode(bytes.NewReader(decoded.Data))
	if err != nil {
		return nil, fmt.Errorf("%w: decode PNG: %w", ErrInvalidImage, err)
	}
	return &ArtworkFrame{
		Image:      img,
		Masks:      masksInFrame(layout.Masks, clip, img.Bounds()),
		Clipped:    clipped,
		CapturedAt: decoded.CapturedAt,
	}, nil
}

// masksInFrame maps CSS-pixel rectangles into the captured image's pixels:
// offset by the clip, scaled by the image's own size over the clip's (so a
// device pixel ratio other than 1 cannot skew them), clipped to the image,
// rounded outward so a label's edge is never left in.
func masksInFrame(masks []cssRect, clip cssRect, bounds image.Rectangle) []image.Rectangle {
	if len(masks) == 0 {
		return nil
	}
	sx, sy := float64(bounds.Dx())/clip.W, float64(bounds.Dy())/clip.H
	out := make([]image.Rectangle, 0, len(masks))
	for _, m := range masks {
		r := image.Rect(
			bounds.Min.X+int(math.Floor((m.X-clip.X)*sx)),
			bounds.Min.Y+int(math.Floor((m.Y-clip.Y)*sy)),
			bounds.Min.X+int(math.Ceil((m.X+m.W-clip.X)*sx)),
			bounds.Min.Y+int(math.Ceil((m.Y+m.H-clip.Y)*sy)),
		).Intersect(bounds)
		if !r.Empty() {
			out = append(out, r)
		}
	}
	return out
}
