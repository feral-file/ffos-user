package command

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/feral-file/ffos-user/components/feral-connectd/cdp"
	"github.com/feral-file/ffos-user/components/feral-connectd/dbus"
	"github.com/feral-file/ffos-user/components/feral-connectd/relayer"
	"github.com/feral-file/ffos-user/components/feral-connectd/state"
	"github.com/feral-file/ffos-user/components/feral-connectd/status"
	"github.com/feral-file/ffos-user/components/feral-connectd/wrapper"
	"github.com/feral-file/godbus"
	"go.uber.org/zap"
)

var CmdOK = struct {
	OK bool `json:"ok"`
}{
	OK: true,
}

type Command struct {
	Command   relayer.RelayerCmd
	Arguments map[string]interface{}
}

type Device struct {
	ID       string `json:"device_id"`
	Name     string `json:"device_name"`
	Platform int    `json:"platform"`
}

//go:generate mockgen -source=command.go -destination=../mocks/command.go -package=mocks -mock_names=CommandHandler=MockCommandHandler
type CommandHandler interface {
	SaveLastSysMetrics(metrics []byte)
	Execute(ctx context.Context, cmd Command) (interface{}, error)
	SetStatusPoller(statusPoller status.Poller)
}

type handler struct {
	sync.Mutex
	cdp          cdp.CDP
	dbus         dbus.DBus
	deviceStatus status.DeviceStatus
	logger       *zap.Logger

	// State
	lastSysMetrics []byte

	// Add reference to StatusPoller to get metrics
	statusPoller status.Poller

	// Mouse position tracking
	cursorPositionX   float64
	cursorPositionY   float64
	screenWidth       float64
	screenHeight      float64
	screenInitialized bool
	movingScaleFactor float64

	// Deps
	json wrapper.JSON
	os   wrapper.OS
	exec wrapper.Exec
	math wrapper.Math
}

func New(
	cdp cdp.CDP,
	dbus dbus.DBus,
	deviceStatus status.DeviceStatus,
	json wrapper.JSON,
	os wrapper.OS,
	exec wrapper.Exec,
	math wrapper.Math,
	l *zap.Logger,
) CommandHandler {
	return &handler{
		cdp:          cdp,
		dbus:         dbus,
		deviceStatus: deviceStatus,
		logger:       l,
		json:         json,
		os:           os,
		exec:         exec,
		math:         math,
	}
}

func (c *handler) SaveLastSysMetrics(metrics []byte) {
	c.Lock()
	defer c.Unlock()
	c.lastSysMetrics = metrics
}

// SetStatusPoller sets the StatusPoller reference after initialization
func (c *handler) SetStatusPoller(statusPoller status.Poller) {
	c.statusPoller = statusPoller
}

func (c *handler) Execute(ctx context.Context, cmd Command) (interface{}, error) {
	c.logger.Info("Executing command", zap.String("command", string(cmd.Command)))

	var err error
	var bytes []byte

	bytes, err = c.json.Marshal(cmd.Arguments)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	var result interface{}
	switch cmd.Command {
	case relayer.CMD_CONNECT:
		result, err = c.connect(bytes)
	case relayer.CMD_SHOW_PAIRING_QR_CODE:
		result, err = c.showPairingQRCode(ctx, bytes)
	case relayer.CMD_KEYBOARD_EVENT:
		result, err = c.handleKeyboardEvent(bytes)
	case relayer.CMD_MOUSE_DRAG_EVENT:
		result, err = c.handleMouseMoveEvent(bytes)
	case relayer.CMD_MOUSE_TAP_EVENT:
		result, err = c.handleMouseTapEvent()
	case relayer.RELAYER_CMD_SYS_METRICS:
		result, err = c.getSysMetrics()
	case relayer.CMD_SCREEN_ROTATION:
		result, err = c.handleScreenRotation(ctx, bytes)
	case relayer.CMD_SHUTDOWN:
		result, err = c.shutdown(ctx)
	case relayer.CMD_REBOOT:
		result, err = c.reboot(ctx)
	case relayer.CMD_DEVICE_STATUS:
		result, err = c.getDeviceStatus(ctx)
	case relayer.CMD_UPDATE_TO_LATEST:
		result, err = c.updateToLatest(ctx)
	default:
		return nil, fmt.Errorf("invalid command: %s", cmd)
	}

	return result, err
}

func (c *handler) connect(args []byte) (interface{}, error) {
	var cmdArgs struct {
		Device         Device `json:"clientDevice"`
		PrimaryAddress string `json:"primaryAddress"`
	}
	err := c.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	s := state.GetState()
	s.ConnectedDevice = &state.Device{
		ID:       cmdArgs.Device.ID,
		Name:     cmdArgs.Device.Name,
		Platform: cmdArgs.Device.Platform,
	}
	err = s.Save()
	if err != nil {
		return nil, fmt.Errorf("failed to save state: %w", err)
	}

	return CmdOK, nil
}

func (c *handler) showPairingQRCode(ctx context.Context, args []byte) (interface{}, error) {
	var cmdArgs struct {
		Show bool `json:"show"`
	}
	err := c.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	err = c.dbus.RetryableSend(ctx,
		godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_SHOW_PAIRING_QR_CODE,
			Body:      []interface{}{cmdArgs.Show},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to send show pairing QR code: %w", err)
	}

	return CmdOK, nil
}

func (c *handler) getDeviceStatus(ctx context.Context) (interface{}, error) {
	return c.deviceStatus.GetStatus(ctx)
}

func (c *handler) handleScreenRotation(ctx context.Context, args []byte) (interface{}, error) {
	var cmdArgs struct {
		Clockwise bool `json:"clockwise"`
	}

	err := c.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	clockwise := cmdArgs.Clockwise
	c.logger.Info("Screen rotation request",
		zap.Bool("clockwise", clockwise))

	// Execute wlr-randr command
	cmd := c.exec.CommandContext(ctx, "wlr-randr")

	// Get current outputs
	output, err := cmd.Output()
	if err != nil {
		c.logger.Error("Failed to execute wlr-randr", zap.Error(err))
		return nil, fmt.Errorf("failed to get display info: %w", err)
	}

	// Find the active output name
	outputName := ""
	lines := strings.Split(string(output), "\n")
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if strings.Contains(line, "Output") {
			parts := strings.Split(line, " ")
			if len(parts) > 1 {
				outputName = parts[1]
				break
			}
		} else if i == 0 && len(line) > 0 {
			// First line might directly contain the output name
			parts := strings.Split(line, " ")
			if len(parts) > 0 {
				outputName = parts[0]
				break
			}
		}
	}

	if outputName == "" {
		return nil, fmt.Errorf("could not find active output")
	}

	// Determine rotation
	// Assume normal is 0, then 90, 180, 270 degrees
	rotations := []string{"normal", "90", "180", "270"}

	// Read current orientation from config file (this is what user perceives)
	currentIndex := 0 // Default to normal
	configPath := "/home/feralfile/.config/screen-orientation"
	configData, err := c.os.ReadFile(configPath)
	if err == nil && len(configData) > 0 {
		savedRotation := strings.TrimSpace(string(configData))
		for i, rot := range rotations {
			if rot == savedRotation {
				currentIndex = i
				break
			}
		}
		c.logger.Info("Using perceived rotation from config", zap.String("rotation", savedRotation))
	} else {
		c.logger.Warn("No saved rotation found, assuming normal orientation")
	}

	// Calculate new orientation based on perceived current orientation
	var newIndex int
	if clockwise {
		newIndex = (currentIndex - 1 + 4) % 4
	} else {
		newIndex = (currentIndex + 1) % 4
	}

	newRotation := rotations[newIndex]

	// Apply with wlr-randr (force absolute orientation)
	// This makes wlr-randr and config file stay in sync
	//nolint:gosec
	rotateCmd := c.exec.CommandContext(ctx, "wlr-randr", "--output", outputName, "--transform", newRotation)
	err = rotateCmd.Run()
	if err != nil {
		c.logger.Error("Failed to rotate screen", zap.Error(err))
		return nil, fmt.Errorf("failed to rotate screen: %w", err)
	}

	// Write rotation value to file
	if err := c.os.WriteFile(configPath, []byte(newRotation), 0600); err != nil {
		c.logger.Warn("Failed to save screen orientation", zap.Error(err))
	}

	c.logger.Info("Screen rotated and saved",
		zap.String("output", outputName),
		zap.String("rotation", newRotation))

	c.screenInitialized = false

	// Force refresh status poller
	c.statusPoller.ForceRefresh()

	orientationReplyMsg := "landscape"
	switch newRotation {
	case "90":
		orientationReplyMsg = "portrait"
	case "180":
		orientationReplyMsg = "landscapeReverse"
	case "270":
		orientationReplyMsg = "portraitReverse"
	}
	return map[string]string{"orientation": orientationReplyMsg}, nil
}

func (c *handler) handleKeyboardEvent(args []byte) (interface{}, error) {
	var cmdArgs struct {
		Code int `json:"code"`
	}

	err := c.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Always map special keys first
	keyName := c.mapToYdoKey(cmdArgs.Code)
	isPrintable := false
	if keyName == "" && cmdArgs.Code >= 32 && cmdArgs.Code <= 126 {
		keyName = string(rune(cmdArgs.Code))
		isPrintable = true
	}

	c.logger.Info("Keyboard event", zap.Int("code", cmdArgs.Code), zap.String("key", keyName))

	// Prepare CDP command to dispatch a key event
	keyEventParams := map[string]interface{}{
		"type":                  "keyDown",
		"windowsVirtualKeyCode": cmdArgs.Code,
		"key":                   keyName,
		"text":                  keyName,
		"unmodifiedText":        keyName,
		"nativeVirtualKeyCode":  cmdArgs.Code,
	}

	// Send key directly via CDP
	_, err = c.cdp.Send("Input.dispatchKeyEvent", keyEventParams)
	if err != nil {
		c.logger.Error("Failed to send key via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to send keyboard event: %w", err)
	}

	// Only send keyUp for printable ASCII (not for special keys)
	if isPrintable {
		keyEventParams["type"] = "keyUp"
		_, err := c.cdp.Send("Input.dispatchKeyEvent", keyEventParams)
		if err != nil {
			c.logger.Error("Failed to send keyUp via CDP", zap.Error(err))
		}
	}

	return CmdOK, nil
}

func (c *handler) initializeScreenDimensions() {
	if c.screenInitialized {
		return
	}

	// Get screen dimensions using CDP's Runtime.evaluate
	evalParams := map[string]interface{}{
		"expression":    "({width: window.innerWidth, height: window.innerHeight})",
		"returnByValue": true,
	}

	result, err := c.cdp.Send("Runtime.evaluate", evalParams)
	if err != nil {
		c.logger.Error("Failed to get screen dimensions", zap.Error(err))
		// Use default values
		c.screenWidth = 1920
		c.screenHeight = 1080
	} else if result != nil {
		if dimensions, ok := result.(map[string]interface{}); ok {
			if width, ok := dimensions["width"].(float64); ok {
				c.screenWidth = width
			} else {
				c.screenWidth = 1920
			}
			if height, ok := dimensions["height"].(float64); ok {
				c.screenHeight = height
			} else {
				c.screenHeight = 1080
			}
		}
	}

	// Initialize cursor at the center of the screen
	c.cursorPositionX = c.screenWidth / 2
	c.cursorPositionY = c.screenHeight / 2
	c.screenInitialized = true
	c.movingScaleFactor = c.screenWidth / 1920

	c.logger.Info("Screen dimensions initialized",
		zap.Float64("width", c.screenWidth),
		zap.Float64("height", c.screenHeight),
		zap.Float64("cursorX", c.cursorPositionX),
		zap.Float64("cursorY", c.cursorPositionY))
}

func (c *handler) handleMouseMoveEvent(args []byte) (interface{}, error) {
	// Initialize screen dimensions if not done already
	c.initializeScreenDimensions()

	// Parse cursor offsets
	var cursorArgs struct {
		MessageID     string `json:"messageID"`
		CursorOffsets []struct {
			DX float64 `json:"dx"`
			DY float64 `json:"dy"`
		} `json:"cursorOffsets"`
	}

	err := c.json.Unmarshal(args, &cursorArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Convert relative positions to absolute positions
	absolutePositions := make([]map[string]float64, 0, len(cursorArgs.CursorOffsets))

	for i, offset := range cursorArgs.CursorOffsets {
		// Calculate the magnitude of this offset
		magnitude := c.math.Sqrt(offset.DX*offset.DX + offset.DY*offset.DY)

		var clampedDX, clampedDY float64

		// Only clamp obvious outliers (very large jumps)
		if magnitude > 150 {
			// This is likely a catch-up jump, clamp aggressively
			maxOffset := 25.0
			clampedDX = c.math.Max(-maxOffset, c.math.Min(maxOffset, offset.DX))
			clampedDY = c.math.Max(-maxOffset, c.math.Min(maxOffset, offset.DY))

			c.logger.Debug("Clamping outlier offset",
				zap.Int("index", i),
				zap.Float64("magnitude", magnitude),
				zap.Float64("originalDX", offset.DX),
				zap.Float64("originalDY", offset.DY),
				zap.Float64("clampedDX", clampedDX),
				zap.Float64("clampedDY", clampedDY))
		} else {
			// Normal movement, use original values
			clampedDX = offset.DX
			clampedDY = offset.DY
		}

		// Update cursor position with the offset
		c.cursorPositionX += (clampedDX * c.movingScaleFactor)
		c.cursorPositionY += (clampedDY * c.movingScaleFactor)

		// Ensure position stays within screen bounds
		c.cursorPositionX = c.math.Max(0, c.math.Min(c.cursorPositionX, c.screenWidth))
		c.cursorPositionY = c.math.Max(0, c.math.Min(c.cursorPositionY, c.screenHeight))

		// Add to absolute positions array
		absolutePositions = append(absolutePositions, map[string]float64{
			"x": c.cursorPositionX,
			"y": c.cursorPositionY,
		})
	}

	// Skip if there are no positions
	if len(absolutePositions) == 0 {
		return CmdOK, nil
	}

	// 1. Pass the entire array of absolute positions to JavaScript via CDP
	positionsJSON, err := c.json.Marshal(map[string]interface{}{
		"messageID": cursorArgs.MessageID,
		"message": map[string]interface{}{
			"command": "cursorUpdate",
			"request": map[string]interface{}{
				"positions": absolutePositions,
			},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to marshal positions: %w", err)
	}

	// Call JavaScript function to process all positions
	_, err = c.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(positionsJSON)),
	})
	if err != nil {
		c.logger.Error("Failed to execute JavaScript cursor positions", zap.Error(err))
		return nil, fmt.Errorf("failed to process cursor positions: %w", err)
	}

	// 2. Send the final mouse event to actually move the cursor
	if len(absolutePositions) > 0 {
		// Get the last position for the final mouseMoved event
		moveParams := map[string]interface{}{
			"type":       "mouseMoved",
			"x":          c.cursorPositionX,
			"y":          c.cursorPositionY,
			"button":     "none",
			"buttons":    0,
			"clickCount": 0,
		}

		_, err = c.cdp.Send("Input.dispatchMouseEvent", moveParams)
		if err != nil {
			c.logger.Error("Failed to move mouse via CDP", zap.Error(err))
			return nil, fmt.Errorf("failed to move mouse: %w", err)
		}

		c.logger.Info("Mouse moved to final position",
			zap.Float64("x", c.cursorPositionX),
			zap.Float64("y", c.cursorPositionY))
	}

	return CmdOK, nil
}

func (c *handler) handleMouseTapEvent() (interface{}, error) {
	// Initialize screen dimensions if not done already
	c.initializeScreenDimensions()

	c.logger.Info("Mouse tap event at current position",
		zap.Float64("x", c.cursorPositionX),
		zap.Float64("y", c.cursorPositionY))

	// 1. Press mouse button at current position
	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          c.cursorPositionX,
		"y":          c.cursorPositionY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}

	_, err := c.cdp.Send("Input.dispatchMouseEvent", downParams)
	if err != nil {
		c.logger.Error("Failed to press mouse button via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to press mouse button: %w", err)
	}

	// 2. Release mouse button
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          c.cursorPositionX,
		"y":          c.cursorPositionY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 1,
	}

	_, err = c.cdp.Send("Input.dispatchMouseEvent", upParams)
	if err != nil {
		c.logger.Error("Failed to release mouse button via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to release mouse button: %w", err)
	}

	return CmdOK, nil
}

func (c *handler) mapToYdoKey(keyCode int) string {
	switch keyCode {
	case 32:
		return "space"
	case 9:
		return "tab"
	case 13:
		return "return"
	case 27:
		return "escape"
	case 8:
		return "backspace"
	case 37:
		return "left"
	case 38:
		return "up"
	case 39:
		return "right"
	case 40:
		return "down"
	default:
		c.logger.Warn("Unhandled key code", zap.Int("code", keyCode))
		return ""
	}
}

func (c *handler) shutdown(ctx context.Context) (interface{}, error) {
	c.logger.Info("Executing shutdown command")

	cmd := c.exec.CommandContext(ctx, "sudo", "shutdown", "-h", "now")

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute shutdown command: %w", err)
	}

	return CmdOK, nil
}

func (c *handler) reboot(ctx context.Context) (interface{}, error) {

	cmd := c.exec.CommandContext(ctx, "sudo", "reboot", "-h", "now")

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute reboot command: %w", err)
	}

	return CmdOK, nil
}

func (c *handler) getSysMetrics() (interface{}, error) {
	c.Lock()
	defer c.Unlock()

	var sysMetrics map[string]interface{}
	if c.lastSysMetrics != nil {
		err := c.json.Unmarshal(c.lastSysMetrics, &sysMetrics)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal last sys metrics: %w", err)
		}
	}

	return sysMetrics, nil
}

func (c *handler) updateToLatest(ctx context.Context) (interface{}, error) {
	c.logger.Info("Executing update to latest version command")

	// execute command systemctl start feral-updater@00:00.service
	cmd := c.exec.CommandContext(ctx, "systemctl", "start", "feral-updater@00:00.service")

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute update to latest command: %w", err)
	}

	return CmdOK, nil
}
