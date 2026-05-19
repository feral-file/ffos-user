package devicectl

import "testing"

func TestExecutor_InnerToVisualViewport_TranslatesByVisualOffset(t *testing.T) {
	e := &executor{}

	x, y := e.innerToVisualViewport(480, 270, 120, 80, 960, 540)
	if x != 360 || y != 190 {
		t.Fatalf("expected translated point (360,190), got (%v,%v)", x, y)
	}
}

func TestExecutor_InnerToVisualViewport_ClampsToVisualBounds(t *testing.T) {
	e := &executor{}

	x, y := e.innerToVisualViewport(3000, -10, 100, 200, 960, 540)
	if x != 960 || y != 0 {
		t.Fatalf("expected clamped point (960,0), got (%v,%v)", x, y)
	}
}
