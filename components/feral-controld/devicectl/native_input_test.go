package devicectl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

type inputProbe struct {
	events         []map[string]any
	fences         []*playerlink.Preconditions
	failAt, closed int
	afterCall      func()
}

func (p *inputProbe) Snapshot(context.Context) (playerlink.Snapshot, error) {
	return playerlink.Snapshot{Instance: "host", Generation: 3, Revision: 4, Host: "responsive", Presented: &playerlink.Occurrence{ID: "visible", Generation: 2, Index: 0, Stage: "presented", NavigationCommitted: true}, PresentedPlaylist: json.RawMessage(`{"items":[{"source":"https://example.org"}]}`)}, nil
}
func (p *inputProbe) Call(_ context.Context, method string, params, result any, fence *playerlink.Preconditions) error {
	if method != "input.dispatch" {
		return errors.New("wrong method")
	}
	p.events = append(p.events, params.(map[string]any))
	p.fences = append(p.fences, fence)
	if p.afterCall != nil {
		p.afterCall()
	}
	if len(p.events) == p.failAt {
		return errors.New("stale_occurrence")
	}
	return nil
}
func (p *inputProbe) Close() error { p.closed++; return nil }
func inputHarness(p *inputProbe) *nativeInput {
	return &nativeInput{player: p, lane: make(chan struct{}, 1), sleep: func(ctx context.Context, d time.Duration) error { return ctx.Err() }}
}
func TestNativeInputGestures(t *testing.T) {
	for _, tc := range []struct {
		name    string
		command commands.Type
		args    string
		kinds   []string
		buttons []int
	}{
		{"tap", commands.CMD_MOUSE_TAP_EVENT, `{}`, []string{"button", "button"}, []int{1, 1}},
		{"right", commands.CMD_MOUSE_TAP_EVENT, `{"button":"right"}`, []string{"button", "button"}, []int{3, 3}},
		{"double", commands.CMD_MOUSE_DOUBLE_TAP_EVENT, `{"button":"middle"}`, []string{"button", "button", "button", "button"}, []int{2, 2, 2, 2}},
		{"drag", commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT, `{"cursorOffsets":[{"dx":3,"dy":4},{"dx":1000,"dy":-1000}]}`, []string{"button", "move", "move", "button"}, nil},
		{"move", commands.CMD_MOUSE_DRAG_EVENT, `{"cursorOffsets":[{"dx":3,"dy":4}]}`, []string{"move"}, nil},
		{"wheel", commands.CMD_ZOOM_GESTURE, `{"scaleSteps":[1,2,0.5]}`, []string{"wheel", "wheel"}, nil},
		{"text", commands.CMD_KEYBOARD_EVENT, `{"code":97}`, []string{"key", "text", "key"}, nil},
		{"shift", commands.CMD_KEYBOARD_EVENT, `{"code":37}`, []string{"key", "key", "text", "key", "key"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := &inputProbe{}
			if _, err := inputHarness(p).execute(context.Background(), tc.command, []byte(tc.args)); err != nil {
				t.Fatal(err)
			}
			var kinds []string
			for i, e := range p.events {
				kinds = append(kinds, e["kind"].(string))
				if e["occurrence"] != "visible" || p.fences[i].Instance != "host" || p.fences[i].Generation != 3 {
					t.Fatal("lost snapshot fence", e)
				}
				if tc.buttons != nil && e["button"] != tc.buttons[i] {
					t.Fatal("button mapping", e)
				}
			}
			if !reflect.DeepEqual(kinds, tc.kinds) || p.closed != 0 {
				t.Fatal(kinds, p.closed)
			}
			switch tc.name {
			case "double":
				for i, e := range p.events {
					if e["clicks"] != i/2+1 || e["down"] != (i%2 == 0) {
						t.Fatal("double click ordering", p.events)
					}
				}
			case "drag":
				if p.events[2]["dx"] != float64(25) || p.events[2]["dy"] != float64(-25) || p.events[2]["referenceWidth"] != 1920 {
					t.Fatal("relative delta clamp", p.events)
				}
			case "wheel":
				if p.events[0]["dy"] != 960 || p.events[1]["dy"] != -960 {
					t.Fatal("wheel direction", p.events)
				}
			case "shift":
				if p.events[0]["usage"] != 225 || p.events[1]["usage"] != 34 || p.events[2]["text"] != "%" {
					t.Fatal("ASCII is not VK", p.events)
				}
			}
		})
	}
}
func TestNativeInputWholeBatchValidation(t *testing.T) {
	for _, tc := range []struct {
		command commands.Type
		args    string
	}{
		{commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT, `{"cursorOffsets":[{"dx":1},{"dx":"bad"}]}`}, {commands.CMD_ZOOM_GESTURE, `{"scaleSteps":[2,0]}`}, {commands.CMD_MOUSE_TAP_EVENT, `{"button":"invalid"}`}, {commands.CMD_KEYBOARD_EVENT, `{"code":1000}`}, {commands.CMD_KEYBOARD_EVENT, `{}`}, {commands.CMD_KEYBOARD_EVENT, `{"code":1.5}`},
	} {
		p := &inputProbe{}
		if _, err := inputHarness(p).execute(context.Background(), tc.command, []byte(tc.args)); err == nil || len(p.events) != 0 || p.closed != 0 {
			t.Fatal(tc, err, p)
		}
	}
	for _, tc := range []struct {
		command commands.Type
		args    any
	}{{commands.CMD_MOUSE_DRAG_EVENT, map[string]any{"cursorOffsets": make([]map[string]int, 129)}}, {commands.CMD_ZOOM_GESTURE, map[string]any{"scaleSteps": make([]int, maxZoomGestureSteps+1)}}} {
		raw, _ := json.Marshal(tc.args)
		if _, err := gestureSteps(tc.command, raw); err == nil {
			t.Fatal("oversized batch admitted")
		}
	}
}
func TestNativeInputFailureNeverRetriesAndDisconnects(t *testing.T) {
	for _, failAt := range []int{1, 2, 3, 4} {
		p := &inputProbe{failAt: failAt}
		result, err := inputHarness(p).execute(context.Background(), commands.CMD_MOUSE_DOUBLE_TAP_EVENT, []byte(`{}`))
		if err == nil || result != nil || len(p.events) != failAt || p.closed != 1 {
			t.Fatal("uncertain input must close, not replay", failAt, p, err)
		}
	}
}
func TestNativeInputCanceledHoldReleasesConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &inputProbe{}
	n := inputHarness(p)
	n.sleep = func(ctx context.Context, d time.Duration) error {
		if d != time.Second {
			t.Fatal("hold duration", d)
		}
		cancel()
		return ctx.Err()
	}
	if _, err := n.execute(ctx, commands.CMD_MOUSE_LONG_PRESS_EVENT, []byte(`{}`)); !errors.Is(err, context.Canceled) || len(p.events) != 1 || p.closed != 1 {
		t.Fatal(p, err)
	}
}
func TestNativeInputCanceledKeyReleasesConnection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p := &inputProbe{afterCall: cancel}
	if _, err := inputHarness(p).execute(ctx, commands.CMD_KEYBOARD_EVENT, []byte(`{"code":65}`)); !errors.Is(err, context.Canceled) || len(p.events) != 1 || p.closed != 1 {
		t.Fatal(p, err)
	}
}
func TestNativeInputGesturesCannotInterleave(t *testing.T) {
	p := &inputProbe{}
	n := inputHarness(p)
	entered := make(chan struct{})
	release := make(chan struct{})
	n.sleep = func(context.Context, time.Duration) error { close(entered); <-release; return nil }
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := n.execute(context.Background(), commands.CMD_MOUSE_LONG_PRESS_EVENT, []byte(`{}`)); err != nil {
			t.Error(err)
		}
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := n.execute(ctx, commands.CMD_MOUSE_TAP_EVENT, []byte(`{}`)); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	close(release)
	wg.Wait()
	if len(p.events) != 2 || p.events[0]["down"] != true || p.events[1]["down"] != false || p.closed != 0 {
		t.Fatal(p)
	}
}
func TestPhoneASCIIKeyboardCoverage(t *testing.T) {
	for code := 32; code <= 126; code++ {
		usage, _, text := phoneKey(code)
		if usage == 0 || text != string(rune(code)) {
			t.Fatal(code, usage, text)
		}
	}
	for _, code := range []int{8, 9, 27} {
		usage, shift, text := phoneKey(code)
		if usage == 0 || shift || text != "" {
			t.Fatal(code, usage, shift, text)
		}
	}
}

// Opt-in hardware test. The fixture ID prevents sending gestures into a user's
// arbitrary artwork. Device harness owns fixture setup and DOM observation.
func TestNativeInputDevice(t *testing.T) {
	socket := os.Getenv("FF_NATIVE_INPUT_TEST_SOCKET")
	if socket == "" {
		t.Skip("isolated native device fixture required")
	}
	client := playerlink.New(socket)
	defer func() {
		if err := client.Close(); err != nil {
			t.Error(err)
		}
	}()
	snapshot, err := client.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var playlist struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal(snapshot.Playlist, &playlist); err != nil || playlist.ID != "controld-input-test" {
		t.Fatal("refusing non-test playlist", err)
	}
	n := &nativeInput{player: client, lane: make(chan struct{}, 1), sleep: func(ctx context.Context, d time.Duration) error {
		timer := time.NewTimer(d)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	}}
	for _, step := range []struct {
		command commands.Type
		args    string
	}{
		{commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT, `{"cursorOffsets":[{"dx":10,"dy":5}]}`},
		{commands.CMD_MOUSE_DOUBLE_TAP_EVENT, `{}`},
		{commands.CMD_ZOOM_GESTURE, `{"scaleSteps":[2,0.5]}`},
		{commands.CMD_KEYBOARD_EVENT, `{"code":65}`},
		{commands.CMD_KEYBOARD_EVENT, `{"code":37}`},
		{commands.CMD_KEYBOARD_EVENT, `{"code":13}`},
		{commands.CMD_KEYBOARD_EVENT, `{"code":9}`},
		{commands.CMD_KEYBOARD_EVENT, `{"code":65}`},
		{commands.CMD_KEYBOARD_EVENT, `{"code":13}`},
	} {
		if _, err = n.execute(context.Background(), step.command, []byte(step.args)); err != nil {
			t.Fatal(step.command, err)
		}
	}
}
