package controlgate_test

import (
	"context"
	"encoding/json"
	"testing"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/control"
	"github.com/feral-file/ffos-user/components/feral-controld/controlgate"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
)

type screenFunc func(context.Context, screenshot.Bounds) (*screenshot.Image, error)

func (f screenFunc) Capture(ctx context.Context, b screenshot.Bounds) (*screenshot.Image, error) {
	return f(ctx, b)
}

func TestSignedCompositorPreview(t *testing.T) {
	a, _ := newAuthority(t)
	gate := controlgate.New(a, &recorder{}, zap.NewNop())
	owner := enrollOwner(t, gate, a)
	calls := 0
	gate.SetScreenshotCapturer(screenFunc(func(_ context.Context, b screenshot.Bounds) (*screenshot.Image, error) {
		calls++
		if b.Width != 256 || b.Height != 512 {
			t.Fatalf("bounds %+v", b)
		}
		return &screenshot.Image{Data: []byte("frame"), Width: 256, Height: 144}, nil
	}))
	for _, tc := range []struct {
		req   string
		route control.Route
		code  string
	}{
		{`{"width":513}`, control.RouteLAN, control.CodeInvalidRequest},
		{`{"w":20}`, control.RouteLAN, control.CodeInvalidRequest},
		{`{}`, control.RouteRelayer, control.CodePermissionDenied},
		{`{"width":256}`, control.RouteLAN, ""},
	} {
		s := owner.build(control.OpCmd, control.CmdCaptureScreenshot, tc.req, "")
		wire, pe := gate.HandleEnvelope(context.Background(), s.body, control.Origin{Known: true, Route: tc.route})
		if pe != nil {
			t.Fatal(pe)
		}
		b, err := json.Marshal(wire)
		if err != nil {
			t.Fatal(err)
		}
		res := owner.open(s, b)
		if res.Code != tc.code || res.OK != (tc.code == "") {
			t.Fatalf("request %s: %+v", tc.req, res)
		}
		if res.OK {
			var frame struct {
				Data   []byte
				Source string
			}
			if json.Unmarshal(res.Res, &frame) != nil || string(frame.Data) != "frame" || frame.Source != "wayland-compositor" {
				t.Fatalf("unexpected preview result")
			}
		}
	}
	if calls != 1 {
		t.Fatalf("capture called %d times", calls)
	}
}
