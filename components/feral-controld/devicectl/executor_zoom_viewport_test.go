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
		t.Fatalf("expected clamped point (960,0), got (%v,%v) ", x, y)
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

func TestExecutor_InnerToVisualViewport_MapsDifferentInnerMousePositions(t *testing.T) {
	e := &executor{}

	tests := []struct {
		name      string
		innerX    float64
		innerY    float64
		viewportX float64
		viewportY float64
		viewportW float64
		viewportH float64
		wantX     float64
		wantY     float64
	}{
		{
			name:      "top left visible",
			innerX:    120,
			innerY:    80,
			viewportX: 120,
			viewportY: 80,
			viewportW: 960,
			viewportH: 540,
			wantX:     0,
			wantY:     0,
		},
		{
			name:      "first quadrant",
			innerX:    480,
			innerY:    270,
			viewportX: 120,
			viewportY: 80,
			viewportW: 960,
			viewportH: 540,
			wantX:     360,
			wantY:     190,
		},
		{
			name:      "center",
			innerX:    960,
			innerY:    540,
			viewportX: 120,
			viewportY: 80,
			viewportW: 960,
			viewportH: 540,
			wantX:     840,
			wantY:     460,
		},
		{
			name:      "right edge clamps",
			innerX:    1400,
			innerY:    540,
			viewportX: 120,
			viewportY: 80,
			viewportW: 960,
			viewportH: 540,
			wantX:     960,
			wantY:     460,
		},
		{
			name:      "above visible clamps",
			innerX:    480,
			innerY:    20,
			viewportX: 120,
			viewportY: 80,
			viewportW: 960,
			viewportH: 540,
			wantX:     360,
			wantY:     0,
		},
		{
			name:      "bottom visible edge",
			innerX:    1080,
			innerY:    620,
			viewportX: 120,
			viewportY: 80,
			viewportW: 960,
			viewportH: 540,
			wantX:     960,
			wantY:     540,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotX, gotY := e.innerToVisualViewport(tt.innerX, tt.innerY, tt.viewportX, tt.viewportY, tt.viewportW, tt.viewportH)
			if gotX != tt.wantX || gotY != tt.wantY {
				t.Fatalf("expected (%v,%v), got (%v,%v)", tt.wantX, tt.wantY, gotX, gotY)
			}
		})
	}
}
