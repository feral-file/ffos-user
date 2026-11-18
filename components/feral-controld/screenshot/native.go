package screenshot

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
)

// Native captures the compositor output, including device UI and libmpv. The
// bounded subprocess and separate single-flight lane never occupy player IPC.
// It is evidence of the compositor buffer, not a photograph of panel output.
type Native struct{ slot chan struct{} }

func NewNative() *Native { return &Native{slot: make(chan struct{}, 1)} }

type boundedOutput struct{ bytes.Buffer }

func (b *boundedOutput) Write(p []byte) (int, error) {
	if len(p) > maxScreenshotBytes-b.Len() {
		return 0, fmt.Errorf("capture exceeds byte limit")
	}
	return b.Buffer.Write(p)
}

func (n *Native) Capture(ctx context.Context, bounds Bounds) (*Image, error) {
	if err := validateBounds(bounds); err != nil {
		return nil, err
	}
	select {
	case n.slot <- struct{}{}:
		defer func() { <-n.slot }()
	default:
		return nil, ErrBusy
	}
	ctx, cancel := context.WithTimeout(ctx, CaptureTimeout)
	defer cancel()
	var outputs boundedOutput
	query := exec.CommandContext(ctx, "wlr-randr", "--json")
	query.Stdout = &outputs
	if err := query.Run(); err != nil {
		return nil, fmt.Errorf("%w: output query: %w", ErrUnavailable, err)
	}
	var screens []struct {
		Name      string `json:"name"`
		Enabled   bool   `json:"enabled"`
		Transform string `json:"transform"`
		Modes     []struct {
			Width   int  `json:"width"`
			Height  int  `json:"height"`
			Current bool `json:"current"`
		} `json:"modes"`
	}
	if err := json.Unmarshal(outputs.Bytes(), &screens); err != nil {
		return nil, err
	}
	for _, screen := range screens {
		if !screen.Enabled {
			continue
		}
		for _, mode := range screen.Modes {
			if !mode.Current {
				continue
			}
			w, h := mode.Width, mode.Height
			if screen.Transform == "90" || screen.Transform == "270" || screen.Transform == "flipped-90" || screen.Transform == "flipped-270" {
				w, h = h, w
			}
			_, _, scale, err := fitDimensions(w, h, bounds)
			if err != nil {
				return nil, err
			}
			var png boundedOutput
			// No shell: the compositor-owned output name is one argv element, and scale is a bounded computed number.
			capture := exec.CommandContext(ctx, "grim", "-o", screen.Name, "-s", strconv.FormatFloat(scale, 'f', 8, 64), "-t", "png", "-") //nolint:gosec // G204: compositor output name, no shell or controller-provided command.
			capture.Stdout = &png
			if err = capture.Run(); err != nil {
				return nil, fmt.Errorf("%w: compositor capture: %w", ErrUnavailable, err)
			}
			return decodeScreenshot(base64.StdEncoding.EncodeToString(png.Bytes()), bounds)
		}
	}
	return nil, ErrUnavailable
}

// The old DOM-based artwork classifier cannot classify a native composition.
// Refuse it explicitly until host composition bounds and masks are consumed.
func (n *Native) CaptureArtwork(context.Context, int) (*ArtworkFrame, error) {
	return nil, ErrUnavailable
}
