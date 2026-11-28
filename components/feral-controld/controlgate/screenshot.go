package controlgate

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
)

// SetScreenshotCapturer shares the native backend with the LAN PNG handler.
func (g *Gate) SetScreenshotCapturer(c screenshot.Capturer) { g.screen = c }

// Cap the image before capture. PNG, base64, the signed payload and its
// encrypted envelope must all fit the control protocol's 4 MiB compact limit.
const maxPreviewEdge = 512

func (g *Gate) captureScreenshot(ctx context.Context, adm *control.Admission, origin control.Origin) (string, error) {
	if !origin.Known || origin.Route != control.RouteLAN || adm.Principal.Role != control.RoleOwner {
		return adm.Reply(false, control.CodePermissionDenied, nil)
	}
	var params map[string]int
	if err := json.Unmarshal(adm.Request, &params); err != nil || params == nil {
		return adm.Reply(false, control.CodeInvalidRequest, nil)
	}
	bounds := screenshot.Bounds{Width: maxPreviewEdge, Height: maxPreviewEdge}
	for key, value := range params {
		if value < 1 || value > maxPreviewEdge {
			return adm.Reply(false, control.CodeInvalidRequest, nil)
		}
		switch key {
		case "width":
			bounds.Width = value
		case "height":
			bounds.Height = value
		default:
			return adm.Reply(false, control.CodeInvalidRequest, nil)
		}
	}
	if g.screen == nil {
		return adm.Reply(false, control.CodeCommandFailed, nil)
	}
	select {
	case g.screenSlot <- struct{}{}:
		defer func() { <-g.screenSlot }()
	default:
		return adm.Reply(false, control.CodeCommandFailed, map[string]string{"reason": "busy"})
	}
	generation := g.InvitationGeneration()
	if g.ScreenProtected() {
		return adm.Reply(false, control.CodeScreenProtected, nil)
	}
	image, err := g.screen.Capture(ctx, bounds)
	if generation != g.InvitationGeneration() || g.ScreenProtected() || !g.auth.GrantActive(adm.Principal.ControllerID) {
		return adm.Reply(false, control.CodeScreenProtected, nil)
	}
	if err != nil {
		reason := "capture_unavailable"
		if errors.Is(err, screenshot.ErrBusy) {
			reason = "busy"
		}
		return adm.Reply(false, control.CodeCommandFailed, map[string]string{"reason": reason})
	}
	return adm.Reply(true, "", map[string]any{"source": "wayland-compositor", "mimeType": "image/png", "width": image.Width, "height": image.Height, "sha256": image.SHA256, "data": image.Data})
}
