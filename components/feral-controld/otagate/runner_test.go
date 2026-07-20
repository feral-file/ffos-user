package otagate

import (
	"context"
	"strings"
	"testing"
)

// TestParseUpdaterLine ports the line-interpretation logic from
// updater.rs::run_update_and_send (id filtering, [PROGRESS] percent/message
// extraction with 100% terminal, [ERROR] message extraction with the
// "Unknown error occurred" fallback).
func TestParseUpdaterLine(t *testing.T) {
	const id = "controld-42"

	cases := []struct {
		name     string
		line     string
		wantKind updaterLineKind
		wantPct  int
		wantMsg  string
	}{
		{
			name:     "progress with percent and message",
			line:     `2026-01-01T00:00:00+0000 [PROGRESS] id=controld-42 progress=30 message="Downloading image"`,
			wantKind: updaterProgress, wantPct: 30, wantMsg: "30% - Downloading image",
		},
		{
			name:     "progress terminal 100",
			line:     `2026-01-01T00:00:00+0000 [PROGRESS] id=controld-42 progress=100 message="Done"`,
			wantKind: updaterProgress, wantPct: 100, wantMsg: "100% - Done",
		},
		{
			name:     "progress message only (no percent)",
			line:     `[PROGRESS] id=controld-42 message="Preparing"`,
			wantKind: updaterProgress, wantPct: 0, wantMsg: "Preparing",
		},
		{
			name:     "error with message",
			line:     `2026-01-01T00:00:00+0000 [ERROR] id=controld-42 message="No network connection. Aborting update."`,
			wantKind: updaterError, wantMsg: "No network connection. Aborting update.",
		},
		{
			name:     "error without message falls back",
			line:     `[ERROR] id=controld-42 something happened`,
			wantKind: updaterError, wantMsg: "Unknown error occurred",
		},
		{
			name:     "line for a different run id is ignored",
			line:     `[PROGRESS] id=controld-99 progress=50 message="Other run"`,
			wantKind: updaterOther,
		},
		{
			name:     "info line for our run is not progress or error",
			line:     `[INFO] id=controld-42 message="Reading config"`,
			wantKind: updaterOther,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			evt := parseUpdaterLine(tc.line, id)
			if evt.kind != tc.wantKind {
				t.Fatalf("kind = %v, want %v", evt.kind, tc.wantKind)
			}
			if tc.wantKind == updaterProgress && evt.pct != tc.wantPct {
				t.Errorf("pct = %d, want %d", evt.pct, tc.wantPct)
			}
			if tc.wantKind != updaterOther && evt.message != tc.wantMsg {
				t.Errorf("message = %q, want %q", evt.message, tc.wantMsg)
			}
		})
	}
}

// TestTailForwardsParsedPercent drives systemdRunner.tail over canned log lines
// and asserts each progress line's parsed percent reaches onProgress. A line with
// no percent field surfaces as -1 (not a misleading 0), and the terminal 100 line
// is forwarded before tail returns nil.
func TestTailForwardsParsedPercent(t *testing.T) {
	const id = "controld-7"
	lines := strings.Join([]string{
		`2026-01-01T00:00:00+0000 [PROGRESS] id=controld-7 message="Preparing"`,
		`2026-01-01T00:00:00+0000 [PROGRESS] id=controld-99 progress=5 message="Other run"`,
		`2026-01-01T00:00:00+0000 [PROGRESS] id=controld-7 progress=30 message="Downloading"`,
		`2026-01-01T00:00:00+0000 [PROGRESS] id=controld-7 progress=100 message="Done"`,
		"",
	}, "\n")

	r := &systemdRunner{clock: newFakeClock()}

	type report struct {
		pct int
		msg string
	}
	var got []report
	err := r.tail(context.Background(), strings.NewReader(lines), id, func(pct int, msg string) {
		got = append(got, report{pct, msg})
	})
	if err != nil {
		t.Fatalf("tail error: %v", err)
	}

	want := []report{
		{-1, "Preparing"},
		{30, "30% - Downloading"},
		{100, "100% - Done"},
	}
	if len(got) != len(want) {
		t.Fatalf("onProgress calls = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("onProgress[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}
