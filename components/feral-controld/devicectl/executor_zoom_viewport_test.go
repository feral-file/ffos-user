package devicectl

import "testing"

func TestExecutor_InnerToVisualViewport_MapsProportionally(t *testing.T) {
	e := &executor{
		screenWidth:  1920,
		screenHeight: 1080,
	}

	x, y := e.innerToVisualViewport(960, 540, 100, 200, 960, 540)
	if x != 580 || y != 470 {
		t.Fatalf("expected mapped point (580,470), got (%v,%v)", x, y)
	}
}

func TestExecutor_InnerToVisualViewport_ClampsToVisualBounds(t *testing.T) {
	e := &executor{
		screenWidth:  1920,
		screenHeight: 1080,
	}

	x, y := e.innerToVisualViewport(3000, -10, 100, 200, 960, 540)
	if x != 1060 || y != 200 {
		t.Fatalf("expected clamped point (1060,200), got (%v,%v)", x, y)
	}
}
