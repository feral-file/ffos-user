package devicectl

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/feral-file/godbus"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/cdp"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dbus"
	"github.com/feral-file/ffos-user/components/feral-controld/helper"
	"github.com/feral-file/ffos-user/components/feral-controld/logger"
	"github.com/feral-file/ffos-user/components/feral-controld/state"
	"github.com/feral-file/ffos-user/components/feral-controld/status"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

var CmdOK = struct {
	OK bool `json:"ok"`
}{
	OK: true,
}

const (
	// AnalyticsToggleOffFile is the sentinel file that disables proactive metrics collection.
	AnalyticsToggleOffFile = "/home/feralfile/.state/analytics-toggle-off"
	// BetaFeaturesToggleOnFile is the sentinel file that enables beta features (default is off).
	BetaFeaturesToggleOnFile = "/home/feralfile/.state/beta-features-toggle-on"
	// TimezoneManualOverrideFile is the sentinel file that indicates timezone has been manually set,
	// preventing automatic timezone synchronization from other sources.
	TimezoneManualOverrideFile = "/etc/timezone-manual-override"
)

type Device struct {
	ID       string `json:"device_id"`
	Name     string `json:"device_name"`
	Platform int    `json:"platform"`
}

//go:generate mockgen -source=executor.go -destination=../mocks/executor.go -package=mocks -mock_names=Executor=MockExecutor
type Executor interface {
	SaveLastSysMetrics(metrics []byte)
	Execute(ctx context.Context, cmd commands.Command) (interface{}, error)
}

type executor struct {
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
	statusPoller status.Poller,
	json wrapper.JSON,
	os wrapper.OS,
	exec wrapper.Exec,
	math wrapper.Math,
	l *zap.Logger,
) Executor {
	return &executor{
		cdp:          cdp,
		dbus:         dbus,
		deviceStatus: deviceStatus,
		statusPoller: statusPoller,
		logger:       l,
		json:         json,
		os:           os,
		exec:         exec,
		math:         math,
	}
}

func (e *executor) SaveLastSysMetrics(metrics []byte) {
	e.Lock()
	defer e.Unlock()
	e.lastSysMetrics = metrics
}

func (e *executor) Execute(ctx context.Context, cmd commands.Command) (interface{}, error) {
	cmdJSON, _ := cmd.JSON()
	e.logger.Info("Executing command", zap.ByteString("command", helper.TruncateBytes(cmdJSON, logger.MAX_FIELD_LENGTH)))

	var err error
	var bytes []byte

	bytes, err = e.json.Marshal(cmd.Arguments)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	var result interface{}
	switch cmd.Type {
	case commands.CMD_CONNECT:
		result, err = e.connect(bytes)
	case commands.CMD_SHOW_PAIRING_QR_CODE:
		result, err = e.showPairingQRCode(ctx, bytes)
	case commands.CMD_KEYBOARD_EVENT:
		result, err = e.handleKeyboardEvent(bytes)
	case commands.CMD_MOUSE_DRAG_EVENT:
		result, err = e.handleMouseMoveEvent(bytes)
	case commands.CMD_MOUSE_TAP_EVENT:
		result, err = e.handleMouseTapEvent()
	case commands.CMD_PROFILE:
		result, err = e.getSysMetrics()
	case commands.CMD_SCREEN_ROTATION:
		result, err = e.handleScreenRotation(ctx, bytes)
	case commands.CMD_SHUTDOWN:
		result, err = e.shutdown(ctx)
	case commands.CMD_REBOOT:
		result, err = e.reboot(ctx)
	case commands.CMD_ANALYTICS_TOGGLE:
		result, err = e.setAnalyticsToggle(ctx, bytes)
	case commands.CMD_BETA_FEATURES_TOGGLE:
		result, err = e.setBetaFeaturesToggle(ctx, bytes)
	case commands.CMD_DEVICE_STATUS:
		result, err = e.getDeviceStatus(ctx)
	case commands.CMD_UPDATE_TO_LATEST:
		result, err = e.updateToLatest(ctx)
	case commands.CMD_FACTORY_RESET:
		result, err = e.factoryReset(ctx)
	case commands.CMD_UPLOAD_LOGS:
		result, err = e.uploadLogs(ctx, bytes)
	case commands.CMD_SET_TIMEZONE:
		result, err = e.setTimeZone(ctx, bytes)
	case commands.CMD_SET_VOLUME:
		result, err = e.setVolume(ctx, bytes)
	case commands.CMD_TOGGLE_MUTE:
		result, err = e.toggleMute(ctx)
	default:
		return nil, fmt.Errorf("invalid command: %s", cmd)
	}

	return result, err
}

func (e *executor) connect(args []byte) (interface{}, error) {
	var cmdArgs struct {
		Device         Device `json:"clientDevice"`
		PrimaryAddress string `json:"primaryAddress"`
	}
	err := e.json.Unmarshal(args, &cmdArgs)
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

func (e *executor) showPairingQRCode(ctx context.Context, args []byte) (interface{}, error) {
	var cmdArgs struct {
		Show bool `json:"show"`
	}
	err := e.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	err = e.dbus.RetryableSend(ctx,
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

func (e *executor) getDeviceStatus(ctx context.Context) (interface{}, error) {
	return e.deviceStatus.GetStatus(ctx)
}

func (e *executor) handleScreenRotation(ctx context.Context, args []byte) (interface{}, error) {
	var cmdArgs struct {
		Clockwise bool `json:"clockwise"`
	}

	err := e.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	clockwise := cmdArgs.Clockwise
	e.logger.Info("Screen rotation request",
		zap.Bool("clockwise", clockwise))

	// Execute wlr-randr command
	cmd := e.exec.CommandContext(ctx, "wlr-randr")

	// Get current outputs
	output, err := cmd.Output()
	if err != nil {
		e.logger.Error("Failed to execute wlr-randr", zap.Error(err))
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
		e.logger.Error("Screen rotation: Could not find active output")
		return nil, fmt.Errorf("could not find active output")
	}

	e.logger.Info("Screen rotation: Found active output",
		zap.String("output_name", outputName))

	// Determine rotation
	// Assume normal is 0, then 90, 180, 270 degrees
	rotations := []string{"normal", "90", "180", "270"}

	// Read current orientation from config file (this is what user perceives)
	currentIndex := 0 // Default to normal
	configData, err := e.os.ReadFile(constants.SCREEN_ORIENTATION_FILE)
	if err == nil && len(configData) > 0 {
		savedRotation := strings.TrimSpace(string(configData))
		for i, rot := range rotations {
			if rot == savedRotation {
				currentIndex = i
				break
			}
		}
		e.logger.Info("Using perceived rotation from config", zap.String("rotation", savedRotation))
	} else {
		e.logger.Warn("No saved rotation found, assuming normal orientation")
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
	rotateCmd := e.exec.CommandContext(ctx, "wlr-randr", "--output", outputName, "--transform", newRotation)
	e.logger.Info("Screen rotation: Applying rotation command",
		zap.String("output", outputName),
		zap.String("transform", newRotation))
	err = rotateCmd.Run()
	if err != nil {
		e.logger.Error("Failed to rotate screen", zap.Error(err))
		return nil, fmt.Errorf("failed to rotate screen: %w", err)
	}

	e.logger.Info("Screen rotation: Successfully applied rotation",
		zap.String("output", outputName),
		zap.String("transform", newRotation))

	// Write rotation value to file
	if err := e.os.WriteFile(constants.SCREEN_ORIENTATION_FILE, []byte(newRotation), 0600); err != nil {
		e.logger.Warn("Failed to save screen orientation", zap.Error(err))
	} else {
		e.logger.Info("Screen rotation: Saved rotation to config file",
			zap.String("rotation", newRotation))
	}

	e.logger.Info("Screen rotated and saved",
		zap.String("output", outputName),
		zap.String("rotation", newRotation))

	e.screenInitialized = false

	// Force refresh status poller
	e.statusPoller.ForceRefresh()

	orientationReplyMsg := "landscape"
	switch newRotation {
	case "90":
		orientationReplyMsg = "portrait"
	case "180":
		orientationReplyMsg = "landscapeReverse"
	case "270":
		orientationReplyMsg = "portraitReverse"
	}

	e.logger.Info("Screen rotation: Completed successfully",
		zap.String("output", outputName),
		zap.String("rotation", newRotation),
		zap.String("orientation_reply", orientationReplyMsg))

	return map[string]string{"orientation": orientationReplyMsg}, nil
}

func (e *executor) handleKeyboardEvent(args []byte) (interface{}, error) {
	var cmdArgs struct {
		Code int `json:"code"`
	}

	err := e.json.Unmarshal(args, &cmdArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Always map special keys first
	keyName := e.mapToYdoKey(cmdArgs.Code)
	isPrintable := false
	if keyName == "" && cmdArgs.Code >= 32 && cmdArgs.Code <= 126 {
		keyName = string(rune(cmdArgs.Code))
		isPrintable = true
	}

	e.logger.Info("Keyboard event", zap.Int("code", cmdArgs.Code), zap.String("key", keyName))

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
	_, err = e.cdp.Send("Input.dispatchKeyEvent", keyEventParams)
	if err != nil {
		e.logger.Error("Failed to send key via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to send keyboard event: %w", err)
	}

	// Only send keyUp for printable ASCII (not for special keys)
	if isPrintable {
		keyEventParams["type"] = "keyUp"
		_, err := e.cdp.Send("Input.dispatchKeyEvent", keyEventParams)
		if err != nil {
			e.logger.Error("Failed to send keyUp via CDP", zap.Error(err))
		}
	}

	return CmdOK, nil
}

func (e *executor) initializeScreenDimensions() {
	if e.screenInitialized {
		return
	}

	// Get screen dimensions using CDP's Runtime.evaluate
	evalParams := map[string]interface{}{
		"expression":    "({width: window.innerWidth, height: window.innerHeight})",
		"returnByValue": true,
	}

	result, err := e.cdp.Send("Runtime.evaluate", evalParams)
	if err != nil {
		e.logger.Error("Failed to get screen dimensions", zap.Error(err))
		// Use default values
		e.screenWidth = 1920
		e.screenHeight = 1080
	} else if result != nil {
		if dimensions, ok := result.(map[string]interface{}); ok {
			if width, ok := dimensions["width"].(float64); ok {
				e.screenWidth = width
			} else {
				e.screenWidth = 1920
			}
			if height, ok := dimensions["height"].(float64); ok {
				e.screenHeight = height
			} else {
				e.screenHeight = 1080
			}
		}
	}

	// Initialize cursor at the center of the screen
	e.cursorPositionX = e.screenWidth / 2
	e.cursorPositionY = e.screenHeight / 2
	e.screenInitialized = true
	e.movingScaleFactor = e.screenWidth / 1920

	e.logger.Info("Screen dimensions initialized",
		zap.Float64("width", e.screenWidth),
		zap.Float64("height", e.screenHeight),
		zap.Float64("cursorX", e.cursorPositionX),
		zap.Float64("cursorY", e.cursorPositionY))
}

func (e *executor) handleMouseMoveEvent(args []byte) (interface{}, error) {
	// Initialize screen dimensions if not done already
	e.initializeScreenDimensions()

	// Parse cursor offsets
	var cursorArgs struct {
		MessageID     string `json:"messageID"`
		CursorOffsets []struct {
			DX float64 `json:"dx"`
			DY float64 `json:"dy"`
		} `json:"cursorOffsets"`
	}

	err := e.json.Unmarshal(args, &cursorArgs)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Convert relative positions to absolute positions
	absolutePositions := make([]map[string]float64, 0, len(cursorArgs.CursorOffsets))

	for i, offset := range cursorArgs.CursorOffsets {
		// Calculate the magnitude of this offset
		magnitude := e.math.Sqrt(offset.DX*offset.DX + offset.DY*offset.DY)

		var clampedDX, clampedDY float64

		// Only clamp obvious outliers (very large jumps)
		if magnitude > 150 {
			// This is likely a catch-up jump, clamp aggressively
			maxOffset := 25.0
			clampedDX = e.math.Max(-maxOffset, e.math.Min(maxOffset, offset.DX))
			clampedDY = e.math.Max(-maxOffset, e.math.Min(maxOffset, offset.DY))

			e.logger.Debug("Clamping outlier offset",
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
		e.cursorPositionX += (clampedDX * e.movingScaleFactor)
		e.cursorPositionY += (clampedDY * e.movingScaleFactor)

		// Ensure position stays within screen bounds
		e.cursorPositionX = e.math.Max(0, e.math.Min(e.cursorPositionX, e.screenWidth))
		e.cursorPositionY = e.math.Max(0, e.math.Min(e.cursorPositionY, e.screenHeight))

		// Add to absolute positions array
		absolutePositions = append(absolutePositions, map[string]float64{
			"x": e.cursorPositionX,
			"y": e.cursorPositionY,
		})
	}

	// Skip if there are no positions
	if len(absolutePositions) == 0 {
		return CmdOK, nil
	}

	// 1. Pass the entire array of absolute positions to JavaScript via CDP
	positionsJSON, err := e.json.Marshal(map[string]interface{}{
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
	_, err = e.cdp.Send(cdp.METHOD_EVALUATE, map[string]interface{}{
		"expression": fmt.Sprintf("window.handleCDPRequest(%s)", string(positionsJSON)),
	})
	if err != nil {
		e.logger.Error("Failed to execute JavaScript cursor positions", zap.Error(err))
		return nil, fmt.Errorf("failed to process cursor positions: %w", err)
	}

	// 2. Send the final mouse event to actually move the cursor
	if len(absolutePositions) > 0 {
		// Get the last position for the final mouseMoved event
		moveParams := map[string]interface{}{
			"type":       "mouseMoved",
			"x":          e.cursorPositionX,
			"y":          e.cursorPositionY,
			"button":     "none",
			"buttons":    0,
			"clickCount": 0,
		}

		_, err = e.cdp.Send("Input.dispatchMouseEvent", moveParams)
		if err != nil {
			e.logger.Error("Failed to move mouse via CDP", zap.Error(err))
			return nil, fmt.Errorf("failed to move mouse: %w", err)
		}

		e.logger.Info("Mouse moved to final position",
			zap.Float64("x", e.cursorPositionX),
			zap.Float64("y", e.cursorPositionY))
	}

	return CmdOK, nil
}

func (e *executor) handleMouseTapEvent() (interface{}, error) {
	// Initialize screen dimensions if not done already
	e.initializeScreenDimensions()

	e.logger.Info("Mouse tap event at current position",
		zap.Float64("x", e.cursorPositionX),
		zap.Float64("y", e.cursorPositionY))

	// 1. Press mouse button at current position
	downParams := map[string]interface{}{
		"type":       "mousePressed",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     "left",
		"buttons":    1,
		"clickCount": 1,
	}

	_, err := e.cdp.Send("Input.dispatchMouseEvent", downParams)
	if err != nil {
		e.logger.Error("Failed to press mouse button via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to press mouse button: %w", err)
	}

	// 2. Release mouse button
	upParams := map[string]interface{}{
		"type":       "mouseReleased",
		"x":          e.cursorPositionX,
		"y":          e.cursorPositionY,
		"button":     "left",
		"buttons":    0,
		"clickCount": 1,
	}

	_, err = e.cdp.Send("Input.dispatchMouseEvent", upParams)
	if err != nil {
		e.logger.Error("Failed to release mouse button via CDP", zap.Error(err))
		return nil, fmt.Errorf("failed to release mouse button: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) mapToYdoKey(keyCode int) string {
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
		e.logger.Warn("Unhandled key code", zap.Int("code", keyCode))
		return ""
	}
}

func (e *executor) shutdown(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing shutdown command")

	cmd := e.exec.CommandContext(ctx, "sudo", "shutdown", "-h", "now")

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute shutdown command: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) reboot(ctx context.Context) (interface{}, error) {

	cmd := e.exec.CommandContext(ctx, "sudo", "reboot", "-h", "now")

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("failed to execute reboot command: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) setAnalyticsToggle(_ context.Context, args []byte) (interface{}, error) {
	var toggleArgs struct {
		Enabled bool `json:"enabled"`
	}
	if err := e.json.Unmarshal(args, &toggleArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	configDir := filepath.Dir(AnalyticsToggleOffFile)

	if err := e.os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}

	if toggleArgs.Enabled {
		if err := e.removeFileIfExists(AnalyticsToggleOffFile); err != nil {
			return nil, fmt.Errorf("failed to enable analytics collection: %w", err)
		}
		e.logger.Info("Analytics collection enabled (toggle file removed)", zap.String("path", AnalyticsToggleOffFile))
		return CmdOK, nil
	}

	content := []byte("analytics collection disabled via controld\n")
	if err := e.os.WriteFile(AnalyticsToggleOffFile, content, 0o644); err != nil {
		return nil, fmt.Errorf("failed to write analytics toggle file: %w", err)
	}

	e.logger.Info("Analytics collection disabled (toggle file created)", zap.String("path", AnalyticsToggleOffFile))

	return CmdOK, nil
}

func (e *executor) setBetaFeaturesToggle(_ context.Context, args []byte) (interface{}, error) {
	var toggleArgs struct {
		Enabled bool `json:"enabled"`
	}
	if err := e.json.Unmarshal(args, &toggleArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	configDir := filepath.Dir(BetaFeaturesToggleOnFile)

	if err := e.os.MkdirAll(configDir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create config directory: %w", err)
	}

	if toggleArgs.Enabled {
		content := []byte("beta features enabled via controld\n")
		if err := e.os.WriteFile(BetaFeaturesToggleOnFile, content, 0o644); err != nil {
			return nil, fmt.Errorf("failed to write beta features toggle file: %w", err)
		}
		e.logger.Info("Beta features enabled (toggle file created)", zap.String("path", BetaFeaturesToggleOnFile))
		return CmdOK, nil
	}

	if err := e.removeFileIfExists(BetaFeaturesToggleOnFile); err != nil {
		return nil, fmt.Errorf("failed to disable beta features: %w", err)
	}

	e.logger.Info("Beta features disabled (toggle file removed)", zap.String("path", BetaFeaturesToggleOnFile))

	return CmdOK, nil
}

func (e *executor) removeFileIfExists(path string) error {
	if err := os.Remove(path); err != nil && !e.os.IsNotExist(err) {
		return err
	}
	return nil
}

func (e *executor) getSysMetrics() (interface{}, error) {
	e.Lock()
	defer e.Unlock()

	var sysMetrics map[string]interface{}
	if e.lastSysMetrics != nil {
		err := e.json.Unmarshal(e.lastSysMetrics, &sysMetrics)
		if err != nil {
			return nil, fmt.Errorf("failed to unmarshal last sys metrics: %w", err)
		}
	}

	return sysMetrics, nil
}

func (e *executor) updateToLatest(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing system update command via DBus")

	// Send DBus signal to setupd to handle system update (show page + execute update)
	err := e.dbus.RetryableSend(ctx,
		godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_SYSTEM_UPDATE,
			Body:      []interface{}{},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to send system update signal: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) factoryReset(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing factory reset command via DBus")

	// Send DBus signal to setupd to handle factory reset (show page + execute reset)
	err := e.dbus.RetryableSend(ctx,
		godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_FACTORY_RESET,
			Body:      []interface{}{},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to send factory reset signal: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) uploadLogs(ctx context.Context, args []byte) (interface{}, error) {
	e.logger.Info("Executing upload logs command via DBus")

	var cmdArgs struct {
		UserID string `json:"userId"`
		APIKey string `json:"apiKey"`
		Title  string `json:"title"`
	}

	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if cmdArgs.UserID == "" || cmdArgs.APIKey == "" || cmdArgs.Title == "" {
		return nil, fmt.Errorf("missing required arguments: userId, apiKey, and title are required")
	}

	// Send DBus signal to setupd to handle log upload
	err := e.dbus.RetryableSend(ctx,
		godbus.DBusPayload{
			Interface: dbus.INTERFACE,
			Path:      dbus.PATH,
			Member:    dbus.SETUPD_EVENT_UPLOAD_LOGS,
			Body:      []interface{}{cmdArgs.UserID, cmdArgs.APIKey, cmdArgs.Title},
		})
	if err != nil {
		return nil, fmt.Errorf("failed to send upload logs signal: %w", err)
	}

	return CmdOK, nil
}

func (e *executor) setTimeZone(ctx context.Context, args []byte) (interface{}, error) {
	e.logger.Info("Executing set-time command")

	var cmdArgs struct {
		Timezone string `json:"timezone"`
	}

	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	if cmdArgs.Timezone == "" {
		return nil, fmt.Errorf("missing required arguments: timezone is required")
	}

	// Step 1: Validate timezone by listing all valid timezones
	e.logger.Info("Validating timezone", zap.String("timezone", cmdArgs.Timezone))
	listCmd := e.exec.CommandContext(ctx, "timedatectl", "list-timezones")
	listOutput, err := listCmd.Output()
	if err != nil {
		e.logger.Error("Failed to list timezones", zap.Error(err))
		return nil, fmt.Errorf("failed to list timezones: %w", err)
	}

	// Check if the provided timezone is in the list of valid timezones
	validTimezones := strings.Split(string(listOutput), "\n")
	isValid := false
	for _, tz := range validTimezones {
		if strings.TrimSpace(tz) == cmdArgs.Timezone {
			isValid = true
			break
		}
	}

	if !isValid {
		e.logger.Error("Invalid timezone provided",
			zap.String("timezone", cmdArgs.Timezone))
		return nil, fmt.Errorf("invalid timezone: %s. Use 'timedatectl list-timezones' to see valid timezones", cmdArgs.Timezone)
	}

	e.logger.Info("Timezone validation passed", zap.String("timezone", cmdArgs.Timezone))

	// Step 2: Set the timezone
	setCmd := e.exec.CommandContext(ctx, "timedatectl", "set-timezone", cmdArgs.Timezone)
	output, err := setCmd.CombinedOutput()
	if err != nil {
		e.logger.Error("Failed to set timezone",
			zap.Error(err),
			zap.String("output", string(output)))
		return nil, fmt.Errorf("failed to set timezone: %w", err)
	}

	e.logger.Info("Timezone set successfully",
		zap.String("timezone", cmdArgs.Timezone),
		zap.String("output", string(output)))

	// Step 3: Create flag file to indicate manual timezone override
	touchCmd := e.exec.CommandContext(ctx, "sudo", "touch", TimezoneManualOverrideFile)
	if err := touchCmd.Run(); err != nil {
		e.logger.Warn("Failed to create timezone manual override flag",
			zap.Error(err),
			zap.String("path", TimezoneManualOverrideFile))
		// Don't fail the command if flag creation fails, just log a warning
	} else {
		e.logger.Info("Timezone manual override flag created",
			zap.String("path", TimezoneManualOverrideFile))
	}

	// Step 4: Run sync to ensure changes are persisted
	syncCmd := e.exec.CommandContext(ctx, "sync")
	if err := syncCmd.Run(); err != nil {
		e.logger.Warn("Failed to run sync after setting timezone", zap.Error(err))
		// Don't fail the command if sync fails, just log a warning
	} else {
		e.logger.Info("Sync completed after timezone change")
	}

	return CmdOK, nil
}

func (e *executor) setVolume(ctx context.Context, args []byte) (interface{}, error) {
	e.logger.Info("Executing set-volume command")

	var cmdArgs struct {
		Percent int `json:"percent"`
	}

	if err := e.json.Unmarshal(args, &cmdArgs); err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}

	// Validate input range
	if cmdArgs.Percent < 0 || cmdArgs.Percent > 100 {
		return nil, fmt.Errorf("percent must be between 0 and 100, got: %d", cmdArgs.Percent)
	}

	// User input 0% maps to 25%, user input 100% maps to 100%
	// Formula: pactl_percent = 25 + (user_percent * 0.75)
	pactlPercent := 0
	if cmdArgs.Percent > 0 {
		pactlPercent = 25 + (cmdArgs.Percent * 75 / 100)
	}

	e.logger.Info("Setting volume", zap.Int("user_percent", cmdArgs.Percent), zap.Int("pactl_percent", pactlPercent))

	// Execute pamixer command
	cmd := e.exec.CommandContext(ctx, "pamixer", "--set-volume", fmt.Sprintf("%d", pactlPercent))
	output, err := cmd.CombinedOutput()
	if err != nil {
		e.logger.Error("Failed to set volume",
			zap.Error(err),
			zap.String("output", string(output)))
		return nil, fmt.Errorf("failed to set volume: %w", err)
	}

	e.logger.Info("Volume set successfully", zap.Int("percent", pactlPercent))

	return CmdOK, nil
}

func (e *executor) toggleMute(ctx context.Context) (interface{}, error) {
	e.logger.Info("Executing toggle-mute command")

	// Execute pamixer command to toggle mute
	cmd := e.exec.CommandContext(ctx, "pamixer", "--toggle-mute")
	output, err := cmd.CombinedOutput()
	if err != nil {
		e.logger.Error("Failed to toggle mute",
			zap.Error(err),
			zap.String("output", string(output)))
		return nil, fmt.Errorf("failed to toggle mute: %w", err)
	}

	e.logger.Info("Mute toggled successfully")

	return CmdOK, nil
}
