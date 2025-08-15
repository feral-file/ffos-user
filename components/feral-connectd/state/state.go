package state

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/Feral-File/ffos-user/components/feral-connectd/wrapper"
	"go.uber.org/zap"
)

const (
	STATE_FILE = "/home/feralfile/.state/connectd.state"
)

type RelayerState struct {
	TopicID string `json:"topicId"`
}

type Device struct {
	ID       string `json:"device_id"`
	Name     string `json:"device_name"`
	Platform int    `json:"platform"`
}

func (r *RelayerState) IsReady() bool {
	return r.TopicID != ""
}

type State struct {
	ConnectedDevice *Device       `json:"connectedDevice"`
	Relayer         *RelayerState `json:"relayer"`
}

//go:generate mockgen -source=state.go -destination=../mocks/state.go -package=mocks -mock_names=StateManager=MockStateManager
type StateManager interface {
	Load(*zap.Logger) (*State, error)
	Save(*State) error
	GetState() *State
}

type defaultStateManager struct {
	stateLock sync.Mutex
	state     *State
	os        wrapper.OS
	json      wrapper.JSON
}

func NewStateManager() StateManager {
	return &defaultStateManager{
		os:   wrapper.NewOS(),
		json: wrapper.NewJSON(),
	}
}

// NewStateManagerWithDeps creates a StateManager with custom dependencies (for testing)
func NewStateManagerWithDeps(osWrapper wrapper.OS, jsonWrapper wrapper.JSON) StateManager {
	return &defaultStateManager{
		os:   osWrapper,
		json: jsonWrapper,
	}
}

func (m *defaultStateManager) Load(logger *zap.Logger) (*State, error) {
	logger.Info("Loading state", zap.String("file", STATE_FILE))

	// Lock during the entire load operation to prevent concurrent access
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	// Ensure directory exists
	stateDir := filepath.Dir(STATE_FILE)
	if err := m.os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	// Try to read the file
	data, err := m.os.ReadFile(STATE_FILE)
	if m.os.IsNotExist(err) {
		// File doesn't exist, return empty state
		logger.Info("State file does not exist, returning empty state object")
		return &State{
			Relayer:         &RelayerState{},
			ConnectedDevice: &Device{},
		}, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	} else if len(data) == 0 {
		// File is empty, return empty state
		logger.Info("State file is empty, returning empty state object")
		return &State{
			Relayer:         &RelayerState{},
			ConnectedDevice: &Device{},
		}, nil
	}

	var s State
	if err := m.json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state file: %w", err)
	}

	m.state = &s
	return m.state, nil
}

func (m *defaultStateManager) Save(s *State) error {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	// Ensure directory exists
	stateDir := filepath.Dir(STATE_FILE)
	if err := m.os.MkdirAll(stateDir, 0750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := m.json.Marshal(s)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Write to a temporary file first, then rename for atomic updates
	tempFile := STATE_FILE + ".tmp"
	if err := m.os.WriteFile(tempFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	if err := m.os.Rename(tempFile, STATE_FILE); err != nil {
		return fmt.Errorf("failed to finalize state file: %w", err)
	}

	// Update the internal state after successful save
	m.state = s
	return nil
}

func (m *defaultStateManager) GetState() *State {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	if m.state == nil {
		m.state = &State{
			Relayer:         &RelayerState{},
			ConnectedDevice: &Device{},
		}
	}
	return m.state
}

// Global instance for backward compatibility
var globalStateManager StateManager = NewStateManager()

// Backward compatible functions
func Load(logger *zap.Logger) (*State, error) {
	return globalStateManager.Load(logger)
}

func GetState() *State {
	return globalStateManager.GetState()
}

// New convenience function for saving - replaces s.Save()
func SaveState(s *State) error {
	return globalStateManager.Save(s)
}

// Save method on State for backward compatibility (deprecated)
func (s *State) Save() error {
	return SaveState(s)
}

// For testing - inject a mock state manager
func InjectStateManagerForTesting(sm StateManager) {
	globalStateManager = sm
}

// Reset for testing
func ResetForTesting() {
	globalStateManager = NewStateManager()
}
