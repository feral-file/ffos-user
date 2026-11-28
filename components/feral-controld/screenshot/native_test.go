package screenshot

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"
)

// Model a rotated, 2x-scaled Cage output. grim's -s applies to its
// logical dimensions, while wlr-randr reports the physical mode.
func TestNativeCaptureUsesLogicalOutputScale(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))

	for name, script := range map[string]string{
		"wlr-randr": `#!/bin/sh
echo '[{"name":"DP-1","enabled":true,"transform":"90","scale":2,"modes":[{"width":1920,"height":1080,"current":true}]}]'
`,
		"grim": `#!/bin/sh
[ "$#" = 7 ] && [ "$1" = -o ] && [ "$2" = DP-1 ] && [ "$3" = -s ] && [ "$4" = 0.50000000 ] && [ "$5" = -t ] && [ "$6" = png ] && [ "$7" = - ] || exit 1
cat "$CAPTURE_FIXTURE"
`,
	} {
		if err := os.WriteFile(filepath.Join(bin, name), []byte(script), 0700); err != nil { //nolint:gosec // G306: these two private test fixtures must execute.
			t.Fatal(err)
		}
	}
	f, err := os.CreateTemp(bin, "frame-*.png")
	if err != nil {
		t.Fatal(err)
	}
	if err := png.Encode(f, image.NewRGBA(image.Rect(0, 0, 270, 480))); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CAPTURE_FIXTURE", f.Name())
	got, err := NewNative().Capture(context.Background(), Bounds{Height: 480})
	if err != nil || got.Width != 270 || got.Height != 480 {
		t.Fatalf("capture failed or wrong geometry: %v", err)
	}
}
