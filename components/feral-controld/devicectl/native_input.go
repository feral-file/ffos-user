package devicectl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/playerlink"
)

// InputPlayer must use a dedicated connection: closing an interrupted gesture
// releases its held state without aborting unrelated playback commands.
type InputPlayer interface {
	Snapshot(context.Context) (playerlink.Snapshot, error)
	Call(context.Context, string, any, any, *playerlink.Preconditions) error
	Close() error
}

type nativeInput struct {
	player InputPlayer
	lane   chan struct{}
	sleep  func(context.Context, time.Duration) error
}

// SetInputPlayer is wired once before the executor starts serving commands.
// There is deliberately no fallback to CDP or a second pointer authority.
func SetInputPlayer(target Executor, player InputPlayer, logger *zap.Logger) {
	e, ok := target.(*executor)
	if !ok {
		logger.Warn("Executor does not support native input")
		return
	}
	e.input = &nativeInput{player: player, lane: make(chan struct{}, 1), sleep: e.clock.SleepContext}
}

type inputStep struct {
	event map[string]any
	hold  time.Duration
}

func (e *executor) handleNativeInput(ctx context.Context, command commands.Type, args []byte) (any, error) {
	if e.input == nil {
		return nil, errors.New("native player input is unavailable")
	}
	return e.input.execute(ctx, command, args)
}

func (n *nativeInput) execute(ctx context.Context, command commands.Type, args []byte) (result any, err error) {
	steps, err := gestureSteps(command, args)
	if err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if len(steps) == 0 {
		return CmdOK, nil
	}
	// Bound the entire gesture, including queueing; per-call deadlines alone
	// would allow a large batch to monopolize the input connection indefinitely.
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	select {
	case n.lane <- struct{}{}:
		defer func() { <-n.lane }()
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() {
		if err != nil {
			err = errors.Join(err, n.player.Close())
			result = nil
		}
	}()
	snapshot, err := n.player.Snapshot(ctx)
	if err != nil {
		return nil, err
	}
	if err = snapshot.Validate(); err != nil {
		return nil, err
	}
	if snapshot.Presented == nil {
		return nil, errors.New("no presented artwork accepts input")
	}
	fence := snapshot.Preconditions()
	for _, step := range steps {
		if err = ctx.Err(); err != nil {
			return nil, err
		}
		step.event["occurrence"] = snapshot.Presented.ID
		if err = n.player.Call(ctx, "input.dispatch", step.event, nil, fence); err != nil {
			return nil, err
		}
		if step.hold > 0 {
			if err = n.sleep(ctx, step.hold); err != nil {
				return nil, err
			}
		}
	}
	return CmdOK, nil
}

// gestureSteps validates the whole legacy controller request before any press.
// Only relative motion leaves this boundary; the player owns output geometry,
// accumulated position, clamping, visible cursor and button/key state.
func gestureSteps(command commands.Type, args []byte) ([]inputStep, error) {
	var in struct {
		Code    *int   `json:"code"`
		Button  string `json:"button"`
		Offsets []struct {
			DX float64 `json:"dx"`
			DY float64 `json:"dy"`
		} `json:"cursorOffsets"`
		Scales []float64 `json:"scaleSteps"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, fmt.Errorf("invalid input arguments: %w", err)
	}
	var steps []inputStep
	add := func(event map[string]any) { steps = append(steps, inputStep{event: event}) }
	button := func(value int, down bool, clicks int) {
		add(map[string]any{"kind": "button", "button": value, "down": down, "clicks": clicks})
	}
	key := func(usage int, down bool) { add(map[string]any{"kind": "key", "usage": usage, "down": down}) }
	switch command {
	case commands.CMD_KEYBOARD_EVENT:
		if in.Code == nil {
			return nil, errors.New("missing keyboard code")
		}
		usage, shift, text := phoneKey(*in.Code)
		if usage == 0 {
			return nil, fmt.Errorf("unsupported keyboard event code: %d", *in.Code)
		}
		if shift {
			key(225, true)
		}
		key(usage, true)
		if text != "" {
			add(map[string]any{"kind": "text", "text": text})
		}
		key(usage, false)
		if shift {
			key(225, false)
		}
	case commands.CMD_MOUSE_TAP_EVENT, commands.CMD_MOUSE_DOUBLE_TAP_EVENT, commands.CMD_MOUSE_LONG_PRESS_EVENT:
		value := 1
		switch in.Button {
		case "", "left":
		case "middle":
			value = 2
		case "right":
			value = 3
		default:
			return nil, errors.New("unknown mouse button")
		}
		count := 1
		if command == commands.CMD_MOUSE_DOUBLE_TAP_EVENT {
			count = 2
		}
		for clicks := 1; clicks <= count; clicks++ {
			button(value, true, clicks)
			if command == commands.CMD_MOUSE_LONG_PRESS_EVENT {
				steps[len(steps)-1].hold = time.Second
			}
			button(value, false, clicks)
		}
	case commands.CMD_MOUSE_DRAG_EVENT, commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT:
		limit := 128 // Bound previously unbounded ordinary movement without truncation.
		if command == commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT {
			limit = maxClickAndDragCursorOffsets
		}
		if len(in.Offsets) > limit {
			return nil, fmt.Errorf("cursorOffsets exceeds maximum of %d", limit)
		}
		for _, offset := range in.Offsets {
			if math.IsNaN(offset.DX) || math.IsInf(offset.DX, 0) || math.IsNaN(offset.DY) || math.IsInf(offset.DY, 0) {
				return nil, errors.New("non-finite cursor offset")
			}
		}
		if len(in.Offsets) == 0 {
			return nil, nil
		}
		held := command == commands.CMD_MOUSE_CLICK_AND_DRAG_EVENT
		if held {
			button(1, true, 1)
		}
		for _, offset := range in.Offsets {
			dx, dy := offset.DX, offset.DY
			if math.Hypot(dx, dy) > 150 {
				dx = max(-25, min(25, dx))
				dy = max(-25, min(25, dy))
			}
			add(map[string]any{"kind": "move", "dx": dx, "dy": dy, "referenceWidth": 1920})
		}
		if held {
			button(1, false, 1)
		}
	case commands.CMD_ZOOM_GESTURE:
		if len(in.Scales) > maxZoomGestureSteps {
			return nil, fmt.Errorf("scaleSteps exceeds maximum of %d", maxZoomGestureSteps)
		}
		for _, scale := range in.Scales {
			if scale <= 0 || math.IsNaN(scale) || math.IsInf(scale, 0) {
				return nil, errors.New("scaleSteps must be finite and positive")
			}
			if scale == 1 {
				continue
			}
			// CEF uses positive-up wheel deltas; CDP used positive-down. The
			// protocol bound is explicit instead of sending arbitrarily large scrolls.
			delta := min(32768, 120*max(1, math.Round(math.Abs(math.Log2(scale))*8)))
			if scale < 1 {
				delta = -delta
			}
			add(map[string]any{"kind": "wheel", "dx": 0, "dy": int(delta)})
		}
	default:
		return nil, fmt.Errorf("unsupported input command: %s", command)
	}
	return steps, nil
}

// Legacy phone codes are ASCII characters plus these four control keys, not
// Windows VK codes (37 means percent, not ArrowLeft). Text remains separate
// from physical HID position, with US-ASCII Shift semantics for key handlers.
func phoneKey(code int) (usage int, shift bool, text string) {
	switch code {
	case 8:
		return 42, false, ""
	case 9:
		return 43, false, ""
	case 13:
		return 40, false, "\r"
	case 27:
		return 41, false, ""
	case 32:
		return 44, false, " "
	}
	if code < 33 || code > 126 {
		return 0, false, ""
	}
	text = string(rune(code))
	if code >= 'a' && code <= 'z' {
		return code - 'a' + 4, false, text
	}
	if code >= 'A' && code <= 'Z' {
		return code - 'A' + 4, true, text
	}
	if i := strings.IndexRune("1234567890", rune(code)); i >= 0 {
		return 30 + i, false, text
	}
	if i := strings.IndexRune("!@#$%^&*()", rune(code)); i >= 0 {
		return 30 + i, true, text
	}
	for _, p := range []struct {
		plain, shifted rune
		usage          int
	}{{'-', '_', 45}, {'=', '+', 46}, {'[', '{', 47}, {']', '}', 48}, {'\\', '|', 49}, {';', ':', 51}, {'\'', '"', 52}, {'`', '~', 53}, {',', '<', 54}, {'.', '>', 55}, {'/', '?', 56}} {
		if rune(code) == p.plain {
			return p.usage, false, text
		}
		if rune(code) == p.shifted {
			return p.usage, true, text
		}
	}
	return 0, false, ""
}
