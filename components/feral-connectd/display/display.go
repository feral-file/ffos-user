package display

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"go.uber.org/zap"
)

// Manager handles display-related operations
type Manager interface {
	ReapplySavedRotation(ctx context.Context) error
}

type manager struct {
	logger *zap.Logger
}

// NewManager creates a new display manager
func NewManager(logger *zap.Logger) Manager {
	return &manager{
		logger: logger,
	}
}

// ReapplySavedRotation reads the saved screen orientation and applies it
func (m *manager) ReapplySavedRotation(ctx context.Context) error {
	m.logger.Info("Attempting to reapply saved screen rotation")

	// Read saved rotation from config file
	rotation, err := m.readSavedRotation()
	if err != nil {
		return fmt.Errorf("failed to read saved rotation: %w", err)
	}

	if rotation == "" {
		m.logger.Info("No saved rotation found, skipping reapplication")
		return nil
	}

	// Validate rotation value
	if !m.isValidRotation(rotation) {
		return fmt.Errorf("invalid rotation value: %s, must be one of: normal, 90, 180, 270", rotation)
	}

	m.logger.Info("Found saved rotation", zap.String("rotation", rotation))

	// Get the current output name
	output, err := m.getCurrentOutput()
	if err != nil {
		return fmt.Errorf("failed to get current output: %w", err)
	}

	// Apply the rotation using wlr-randr
	err = m.applyRotation(ctx, output, rotation)
	if err != nil {
		return fmt.Errorf("failed to apply rotation: %w", err)
	}

	m.logger.Info("Successfully reapplied screen rotation")
	return nil
}

// readSavedRotation reads the saved rotation from the config file
func (m *manager) readSavedRotation() (string, error) {
	configPath := "/home/feralfile/.config/screen-orientation"

	data, err := os.ReadFile(configPath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil // No saved rotation
		}
		return "", err
	}

	rotation := strings.TrimSpace(string(data))
	return rotation, nil
}

// getCurrentOutput gets the current display output name
func (m *manager) getCurrentOutput() (string, error) {
	// For now, we'll use HDMI-A-1 as the default output
	// In a more robust implementation, we could query wlr-randr to get available outputs
	return "HDMI-A-1", nil
}

// applyRotation applies the rotation using wlr-randr
func (m *manager) applyRotation(ctx context.Context, output, rotation string) error {
	m.logger.Info("Applying rotation to output", zap.String("output", output), zap.String("rotation", rotation))

	cmd := exec.CommandContext(ctx, "wlr-randr", "--output", output, "--transform", rotation)

	outputBytes, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("wlr-randr failed: %w, output: %s", err, string(outputBytes))
	}

	m.logger.Debug("wlr-randr output", zap.String("output", string(outputBytes)))
	return nil
}

// isValidRotation checks if the rotation value is valid
func (m *manager) isValidRotation(rotation string) bool {
	validRotations := []string{"normal", "90", "180", "270"}

	for _, validRotation := range validRotations {
		if rotation == validRotation {
			return true
		}
	}

	return false
}
