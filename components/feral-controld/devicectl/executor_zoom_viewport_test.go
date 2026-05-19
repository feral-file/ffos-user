package devicectl

import "testing"

func TestExecutor_ClampToViewport_ClampsInsideBounds(t *testing.T) {
	e := &executor{}

	x, y := e.clampToViewport(2000, -50, 100, 200, 800, 600)
	if x != 900 || y != 200 {
		t.Fatalf("expected clamped point (900,200), got (%v,%v)", x, y)
	}
}

func TestExecutor_ClampToViewport_KeepsPointInsideBounds(t *testing.T) {
	e := &executor{}

	x, y := e.clampToViewport(500, 450, 100, 200, 800, 600)
	if x != 500 || y != 450 {
		t.Fatalf("expected unchanged point (500,450), got (%v,%v)", x, y)
	}
}
