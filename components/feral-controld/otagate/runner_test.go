package otagate

import (
	"context"
	"errors"
	"fmt"
	"io"
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
	err := r.tail(context.Background(), strings.NewReader(lines), id, "feral-updater-run@controld-7.service", func(pct int, msg string) {
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

// TestTailDetectsSilentUnitDeath is the G2 regression: an updater killed without
// writing an id-tagged terminal line (SIGKILL/OOM/systemctl stop) must terminate
// the tail with a transient-classified error instead of EOF-polling forever and
// wedging the gate's single-flight (which would permanently block claiming).
func TestTailDetectsSilentUnitDeath(t *testing.T) {
	clock := newFakeClock()
	var probes int
	r := &systemdRunner{
		clock: clock,
		// Alive on the first probe (exercises the deadSince reset path), dead from
		// the second probe on.
		unitActive: func(context.Context, string) bool {
			probes++
			return probes == 1
		},
	}

	err := r.tail(context.Background(), strings.NewReader(""), "controld-1", "feral-updater-run@controld-1.service", nil)
	if err == nil {
		t.Fatal("tail returned nil for a silently-dead unit; want error")
	}
	if !strings.Contains(err.Error(), "updater service exited without reporting completion") {
		t.Fatalf("tail error = %q, want the silent-death message", err)
	}
	if kind := classifyUpdaterMessage(err.Error()); kind != errTransient {
		t.Fatalf("silent unit death classified as %v, want errTransient", kind)
	}
	if probes < 2 {
		t.Fatalf("liveness probes = %d, want at least 2", probes)
	}
}

// TestTailKeepsPollingWhileUnitAlive guards against the liveness watch aborting
// a healthy-but-quiet update: an alive unit with an idle log must keep the tail
// polling (here until ctx cancellation ends the test deterministically).
func TestTailKeepsPollingWhileUnitAlive(t *testing.T) {
	clock := newFakeClock()
	ctx, cancel := context.WithCancel(context.Background())
	polls := 0
	clockCancelAfter := 200 // ~40s of fake time, several liveness intervals
	clock.onSleep = func() {
		polls++
		if polls >= clockCancelAfter {
			cancel()
		}
	}
	r := &systemdRunner{
		clock:      clock,
		unitActive: func(context.Context, string) bool { return true },
	}

	err := r.tail(ctx, strings.NewReader(""), "controld-2", "feral-updater-run@controld-2.service", nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("tail error = %v, want context.Canceled (the test's own stop), not a liveness abort", err)
	}
}

// TestRunRestartsWatchdogOnFailure is the G3 regression: Run stops
// feral-watchdog up front, and Restart=always does NOT resurrect an explicitly
// stopped unit — so every failure path must start it again or the kiosk
// watchdog stays dead until a manual reboot.
func TestRunRestartsWatchdogOnFailure(t *testing.T) {
	exec := &fakeExec{fail: map[string]error{
		"systemctl start feral-updater-run@": fmt.Errorf("boom"),
	}}
	r := &systemdRunner{exec: exec, clock: newFakeClock()}
	r.unitActive = func(context.Context, string) bool { return true }

	err := r.Run(context.Background(), nil)
	if err == nil || !strings.Contains(err.Error(), "Failed to start updater service") {
		t.Fatalf("Run error = %v, want updater start failure", err)
	}

	var stopped, restarted bool
	for _, cmd := range exec.recorded() {
		switch cmd {
		case "systemctl --user stop feral-watchdog.service":
			stopped = true
		case "systemctl --user start feral-watchdog.service":
			restarted = true
		}
	}
	if !stopped {
		t.Error("watchdog was never stopped")
	}
	if !restarted {
		t.Error("watchdog was not restarted on the failure path")
	}
}

// TestRunRestartsWatchdogOnSuccess is the F5 regression: even on a successful
// update the watchdog must be restarted, so a deferred/staged/failed reboot does
// not leave a dead watchdog on the still-running old build. (In production the
// imminent reboot usually kills it again, harmlessly.)
func TestRunRestartsWatchdogOnSuccess(t *testing.T) {
	exec := &fakeExec{}
	r := &systemdRunner{exec: exec, clock: newFakeClock()}
	r.unitActive = func(context.Context, string) bool { return true }
	r.openLog = func(string) (io.ReadCloser, error) {
		// The run id is random; recover it from the recorded systemctl start so the
		// canned log lines carry a matching id= tag.
		id := ""
		for _, cmd := range exec.recorded() {
			if strings.Contains(cmd, "feral-updater-run@") {
				id = strings.TrimSuffix(strings.SplitAfter(cmd, "feral-updater-run@")[1], ".service")
			}
		}
		return io.NopCloser(strings.NewReader(
			`[PROGRESS] id=` + id + ` progress=100 message="Done"` + "\n")), nil
	}

	if err := r.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run error = %v, want nil", err)
	}
	var restarted bool
	for _, cmd := range exec.recorded() {
		if cmd == "systemctl --user start feral-watchdog.service" {
			restarted = true
		}
	}
	if !restarted {
		t.Error("watchdog was not restarted on the success path (F5): a deferred reboot would strand a dead watchdog")
	}
}
