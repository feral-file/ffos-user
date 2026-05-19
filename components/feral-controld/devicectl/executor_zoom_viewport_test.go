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

func TestExecutor_InnerToVisualViewport_KeepsSameInnerAnchorAcrossViewportChanges(t *testing.T) {
	e := &executor{}

	const innerMouseX = 1920.0 / 4
	const innerMouseY = 1080.0 / 4

	firstX, firstY := e.innerToVisualViewport(innerMouseX, innerMouseY, 0, 0, 1920, 1080)
	if firstX != 480 || firstY != 270 {
		t.Fatalf("expected initial visual point (480,270), got (%v,%v)", firstX, firstY)
	}

	nextX, nextY := e.innerToVisualViewport(innerMouseX, innerMouseY, 120, 80, 1280, 720)
	if nextX != 360 || nextY != 190 {
		t.Fatalf("expected translated visual point (360,190), got (%v,%v)", nextX, nextY)
	}
}
