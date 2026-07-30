package state

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"

	"go.uber.org/zap"
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

// ClaimInfo is a value snapshot of the claim-relevant fields — connected-device
// identity and the relayer topic — taken atomically under stateLock (see
// ClaimSnapshot). Consumers that decide something from more than one of these
// fields (claimed-ness, topic, topic-readiness) must take a single snapshot
// rather than separate GetState() field reads: two such reads can otherwise
// straddle a concurrent writer (connect(), clearPersistedClaim(), the
// mediator's topic assignment) and observe a state that never existed.
type ClaimInfo struct {
	DeviceID   string
	DeviceName string
	// Claimed mirrors the derivation every other claim consumer uses: a
	// non-empty (trimmed) connected-device ID.
	Claimed bool
	TopicID string
	// TopicReady mirrors RelayerState.IsReady()'s exact (untrimmed) check.
	TopicReady bool
}

//go:generate mockgen -source=state.go -destination=../mocks/state.go -package=mocks -mock_names=StateManager=MockStateManager
type StateManager interface {
	Load(*zap.Logger) (*State, error)
	Save(*State) error
	GetState() *State

	// ClaimSnapshot returns a locked, internally-consistent read of the
	// claim-relevant fields. See ClaimInfo.
	ClaimSnapshot() ClaimInfo
	// SetConnectedDevice persists a new claim record, holding the state lock
	// across the mutate-then-save so no reader can observe the mutated
	// in-memory field before (or the pre-mutation field after) the file
	// write lands.
	SetConnectedDevice(d Device) error
	// ClearClaim zeroes the persisted relayer TopicID and resets
	// ConnectedDevice to a fresh Device{}, atomically. changed reports
	// whether anything was actually cleared; a no-op (nothing persisted)
	// skips the save entirely, mirroring the prior clearPersistedClaim
	// discipline.
	ClearClaim() (changed bool, err error)
	// SetRelayerTopicID persists a new relayer topic ID, atomically.
	// hadTopicBefore reports whether a non-empty topic was already
	// persisted BEFORE this write — the edge callers notify observers on.
	SetRelayerTopicID(topicID string) (hadTopicBefore bool, err error)
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
	logger.Info("Loading state", zap.String("file", constants.STATE_FILE))

	// Lock during the entire load operation to prevent concurrent access
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	// Ensure directory exists
	stateDir := filepath.Dir(constants.STATE_FILE)
	if err := m.os.MkdirAll(stateDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create state directory: %w", err)
	}

	// Try to read the file
	data, err := m.os.ReadFile(constants.STATE_FILE)
	if m.os.IsNotExist(err) {
		// File doesn't exist, return empty state
		logger.Info("State file does not exist, returning empty state object")
		m.state = emptyState()
		return m.state, nil
	} else if err != nil {
		return nil, fmt.Errorf("failed to read state file: %w", err)
	} else if len(data) == 0 {
		// File is empty, return empty state
		logger.Info("State file is empty, returning empty state object")
		m.state = emptyState()
		return m.state, nil
	}

	var s State
	if err := m.json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("failed to unmarshal state file: %w", err)
	}

	m.state = &s
	return m.state, nil
}

// emptyState builds the fresh-boot default. Load assigns its result to
// m.state on EVERY branch (not-exist, empty file, and the successful
// unmarshal path) so a later GetState() call returns the SAME pointer Load
// handed its caller — not a second, independent fresh state. Before this,
// only the unmarshal branch assigned m.state: a fresh-boot Load (the
// overwhelmingly common not-exist case) left m.state nil, so main's `s :=
// state.Load(...)` and, e.g., the mediator's `state.GetState()` diverged onto
// two different objects whose mutations were invisible to each other.
func emptyState() *State {
	return &State{
		Relayer:         &RelayerState{},
		ConnectedDevice: &Device{},
	}
}

func (m *defaultStateManager) Save(s *State) error {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	return m.saveLocked(s)
}

// saveLocked marshals and persists s, assuming stateLock is already held. It
// is the shared tail of every locked accessor below, so a mutate followed by
// its save is one critical section: before this helper existed, a caller
// mutating a field on the GetState() pointer and then calling Save()
// separately took stateLock twice, leaving a window where a concurrent
// mutator could interleave between the two acquisitions and have its own
// change silently overwritten (or overwrite this one) on disk.
func (m *defaultStateManager) saveLocked(s *State) error {
	// Ensure directory exists
	stateDir := filepath.Dir(constants.STATE_FILE)
	if err := m.os.MkdirAll(stateDir, 0750); err != nil {
		return fmt.Errorf("failed to create state directory: %w", err)
	}

	data, err := m.json.Marshal(s)
	if err != nil {
		return fmt.Errorf("failed to marshal state: %w", err)
	}

	// Write to a temporary file first, then rename for atomic updates
	tempFile := constants.STATE_FILE + ".tmp"
	if err := m.os.WriteFile(tempFile, data, 0600); err != nil {
		return fmt.Errorf("failed to write state file: %w", err)
	}

	if err := m.os.Rename(tempFile, constants.STATE_FILE); err != nil {
		return fmt.Errorf("failed to finalize state file: %w", err)
	}

	// Update the internal state after successful save
	m.state = s
	return nil
}

func (m *defaultStateManager) GetState() *State {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	return m.currentLocked()
}

// currentLocked returns m.state, lazily initializing it to a fresh empty
// state on first use. Assumes stateLock is already held.
func (m *defaultStateManager) currentLocked() *State {
	if m.state == nil {
		m.state = emptyState()
	}
	return m.state
}

// ClaimSnapshot takes stateLock once and reads every ClaimInfo field from the
// same in-memory State, so the fields can never disagree about which write
// they reflect (see ClaimInfo).
func (m *defaultStateManager) ClaimSnapshot() ClaimInfo {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	s := m.currentLocked()
	var info ClaimInfo
	if s.ConnectedDevice != nil {
		info.DeviceID = s.ConnectedDevice.ID
		info.DeviceName = s.ConnectedDevice.Name
		info.Claimed = strings.TrimSpace(s.ConnectedDevice.ID) != ""
	}
	if s.Relayer != nil {
		info.TopicID = s.Relayer.TopicID
		info.TopicReady = s.Relayer.IsReady()
	}
	return info
}

// SetConnectedDevice persists a new claim record. See StateManager.
func (m *defaultStateManager) SetConnectedDevice(d Device) error {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	s := m.currentLocked()
	s.ConnectedDevice = &d
	return m.saveLocked(s)
}

// ClearClaim zeroes the persisted claim. See StateManager.
func (m *defaultStateManager) ClearClaim() (changed bool, err error) {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	s := m.currentLocked()
	if s.Relayer != nil && s.Relayer.TopicID != "" {
		s.Relayer.TopicID = ""
		changed = true
	}
	if s.ConnectedDevice != nil && strings.TrimSpace(s.ConnectedDevice.ID) != "" {
		s.ConnectedDevice = &Device{}
		changed = true
	}
	if !changed {
		return false, nil
	}
	return true, m.saveLocked(s)
}

// SetRelayerTopicID persists a new relayer topic ID. See StateManager.
func (m *defaultStateManager) SetRelayerTopicID(topicID string) (hadTopicBefore bool, err error) {
	m.stateLock.Lock()
	defer m.stateLock.Unlock()

	s := m.currentLocked()
	hadTopicBefore = s.Relayer != nil && strings.TrimSpace(s.Relayer.TopicID) != ""
	if s.Relayer == nil {
		s.Relayer = &RelayerState{}
	}
	s.Relayer.TopicID = topicID
	return hadTopicBefore, m.saveLocked(s)
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

// ClaimSnapshot returns a locked, internally-consistent read of the
// claim-relevant fields. See ClaimInfo.
func ClaimSnapshot() ClaimInfo {
	return globalStateManager.ClaimSnapshot()
}

// SetConnectedDevice persists a new claim record atomically.
func SetConnectedDevice(d Device) error {
	return globalStateManager.SetConnectedDevice(d)
}

// ClearClaim zeroes the persisted claim atomically.
func ClearClaim() (changed bool, err error) {
	return globalStateManager.ClearClaim()
}

// SetRelayerTopicID persists a new relayer topic ID atomically.
func SetRelayerTopicID(topicID string) (hadTopicBefore bool, err error) {
	return globalStateManager.SetRelayerTopicID(topicID)
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
