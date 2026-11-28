package screenshot

import (
	"context"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
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

func TestNativeCaptureReportsUnavailableOutput(t *testing.T) {
	for _, tc := range []struct {
		name, query, grim string
		want              error
	}{
		{"query failed", "exit 1", "", ErrUnavailable},
		{"unreadable outputs", "echo broken", "", nil},
		{"disabled output", `echo '[{"enabled":false}]'`, "", ErrUnavailable},
		{"no current mode", `echo '[{"enabled":true,"modes":[{"current":false}]}]'`, "", ErrUnavailable},
		{"cannot capture output", `echo '[{"name":"DP-1","enabled":true,"modes":[{"width":10,"height":10,"current":true}]}]'`, "exit 1", ErrUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Setenv("PATH", dir)
			for name, script := range map[string]string{"wlr-randr": tc.query, "grim": tc.grim} {
				require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script+"\n"), 0700)) //nolint:gosec // Private executable test fixtures.
			}
			image, err := NewNative().Capture(t.Context(), Bounds{Height: 480})
			require.Nil(t, image)
			require.Error(t, err)
			if tc.want != nil {
				require.ErrorIs(t, err, tc.want)
			}
		})
	}
	n := NewNative()
	n.slot <- struct{}{}
	_, err := n.Capture(t.Context(), Bounds{Height: 480})
	require.ErrorIs(t, err, ErrBusy)
	_, err = n.Capture(t.Context(), Bounds{Height: -1})
	require.Error(t, err)
	_, err = n.CaptureArtwork(t.Context(), 1)
	require.ErrorIs(t, err, ErrUnavailable)
}
