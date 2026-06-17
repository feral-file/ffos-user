package state_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/state"

	"github.com/stretchr/testify/assert"
	"go.uber.org/mock/gomock"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
)

type testSetup struct {
	ctrl     *gomock.Controller
	ctx      context.Context
	mockOS   *mocks.MockOS
	mockJSON *mocks.MockJSON
	sm       state.StateManager
	logger   *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockOS := mocks.NewMockOS(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	sm := state.NewStateManagerWithDeps(mockOS, mockJSON)

	return &testSetup{
		ctrl:     ctrl,
		ctx:      ctx,
		mockOS:   mockOS,
		mockJSON: mockJSON,
		sm:       sm,
		logger:   logger,
	}
}

func (ts *testSetup) teardown() {
	state.ResetForTesting()
	ts.ctrl.Finish()
}

// Test StateManager interface

func TestStateManager_Load_Success_ExistingFile(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	stateDir := filepath.Dir(constants.STATE_FILE)
	stateData := `{
		"connectedDevice": {
			"device_id": "test-device-123",
			"device_name": "Test Device",
			"platform": 1
		},
		"relayer": {
			"topicId": "test-topic-456"
		}
	}`

	// Setup expectations
	ts.mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0750)).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		ReadFile(constants.STATE_FILE).
		Return([]byte(stateData), nil).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(1)

	ts.mockJSON.EXPECT().
		Unmarshal([]byte(stateData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			st := v.(*state.State)
			st.ConnectedDevice = &state.Device{
				ID:       "test-device-123",
				Name:     "Test Device",
				Platform: 1,
			}
			st.Relayer = &state.RelayerState{
				TopicID: "test-topic-456",
			}
			return nil
		}).
		Times(1)

	// Execute
	result, err := ts.sm.Load(ts.logger)

	// Verify
	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Equal(t, "test-device-123", result.ConnectedDevice.ID)
	assert.Equal(t, "Test Device", result.ConnectedDevice.Name)
	assert.Equal(t, 1, result.ConnectedDevice.Platform)
	assert.Equal(t, "test-topic-456", result.Relayer.TopicID)
	assert.True(t, result.Relayer.IsReady())
}

func TestStateManager_Load_Success_FileNotExists(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	notFoundErr := &os.PathError{Op: "open", Path: constants.STATE_FILE, Err: os.ErrNotExist}

	ts.mockOS.EXPECT().
		MkdirAll(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		ReadFile(gomock.Any()).
		Return(nil, notFoundErr).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(notFoundErr).
		Return(true).
		Times(1)

	result, err := ts.sm.Load(ts.logger)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Empty(t, result.ConnectedDevice.ID)
	assert.Empty(t, result.Relayer.TopicID)
	assert.False(t, result.Relayer.IsReady())
}

func TestStateManager_Load_Success_EmptyFile(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.mockOS.EXPECT().
		MkdirAll(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		ReadFile(gomock.Any()).
		Return([]byte{}, nil).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(1)

	result, err := ts.sm.Load(ts.logger)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Empty(t, result.ConnectedDevice.ID)
	assert.Empty(t, result.Relayer.TopicID)
}

func TestStateManager_Load_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup)
		wantErr   string
	}{
		{
			name: "mkdir error",
			setupFunc: func(ts *testSetup) {
				ts.mockOS.EXPECT().
					MkdirAll(gomock.Any(), gomock.Any()).
					Return(fmt.Errorf("permission denied")).
					Times(1)
			},
			wantErr: "failed to create state directory",
		},
		{
			name: "read file error",
			setupFunc: func(ts *testSetup) {
				readErr := fmt.Errorf("permission denied")

				ts.mockOS.EXPECT().
					MkdirAll(gomock.Any(), gomock.Any()).
					Return(nil).
					Times(1)

				ts.mockOS.EXPECT().
					ReadFile(gomock.Any()).
					Return(nil, readErr).
					Times(1)

				ts.mockOS.EXPECT().
					IsNotExist(readErr).
					Return(false).
					Times(1)
			},
			wantErr: "failed to read state file",
		},
		{
			name: "JSON unmarshal error",
			setupFunc: func(ts *testSetup) {
				invalidJSON := `{"invalid": json}`

				ts.mockOS.EXPECT().
					MkdirAll(gomock.Any(), gomock.Any()).
					Return(nil).
					Times(1)

				ts.mockOS.EXPECT().
					ReadFile(gomock.Any()).
					Return([]byte(invalidJSON), nil).
					Times(1)

				ts.mockOS.EXPECT().
					IsNotExist(nil).
					Return(false).
					Times(1)

				ts.mockJSON.EXPECT().
					Unmarshal([]byte(invalidJSON), gomock.Any()).
					Return(fmt.Errorf("invalid character 'j' looking for beginning of value")).
					Times(1)
			},
			wantErr: "failed to unmarshal state file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			tt.setupFunc(ts)

			result, err := ts.sm.Load(ts.logger)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
			assert.Nil(t, result)
		})
	}
}

func TestStateManager_Save_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	stateDir := "/home/feralfile/.state"
	stateFile := stateDir + "/controld.state"
	tempFile := stateFile + ".tmp"

	testState := &state.State{
		ConnectedDevice: &state.Device{
			ID:       "test-device-123",
			Name:     "Test Device",
			Platform: 1,
		},
		Relayer: &state.RelayerState{
			TopicID: "test-topic-456",
		},
	}

	stateData := []byte(`{"connectedDevice":{"device_id":"test-device-123","device_name":"Test Device","platform":1},"relayer":{"topicId":"test-topic-456"}}`)

	ts.mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0750)).
		Return(nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Marshal(testState).
		Return(stateData, nil).
		Times(1)

	ts.mockOS.EXPECT().
		WriteFile(tempFile, stateData, os.FileMode(0600)).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		Rename(tempFile, stateFile).
		Return(nil).
		Times(1)

	err := ts.sm.Save(testState)

	assert.NoError(t, err)
	// Verify that internal state was updated
	assert.Equal(t, testState, ts.sm.GetState())
}

func TestStateManager_Save_Errors(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup, *state.State)
		wantErr   string
	}{
		{
			name: "mkdir error",
			setupFunc: func(ts *testSetup, testState *state.State) {
				ts.mockOS.EXPECT().
					MkdirAll(gomock.Any(), gomock.Any()).
					Return(fmt.Errorf("permission denied")).
					Times(1)
			},
			wantErr: "failed to create state directory",
		},
		{
			name: "JSON marshal error",
			setupFunc: func(ts *testSetup, testState *state.State) {
				ts.mockOS.EXPECT().
					MkdirAll(gomock.Any(), gomock.Any()).
					Return(nil).
					Times(1)

				ts.mockJSON.EXPECT().
					Marshal(testState).
					Return(nil, fmt.Errorf("marshal error")).
					Times(1)
			},
			wantErr: "failed to marshal state",
		},
		{
			name: "write file error",
			setupFunc: func(ts *testSetup, testState *state.State) {
				stateData := []byte(`{"test":"data"}`)

				ts.mockOS.EXPECT().
					MkdirAll(gomock.Any(), gomock.Any()).
					Return(nil).
					Times(1)

				ts.mockJSON.EXPECT().
					Marshal(testState).
					Return(stateData, nil).
					Times(1)

				ts.mockOS.EXPECT().
					WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(fmt.Errorf("write error")).
					Times(1)
			},
			wantErr: "failed to write state file",
		},
		{
			name: "rename error",
			setupFunc: func(ts *testSetup, testState *state.State) {
				stateData := []byte(`{"test":"data"}`)

				ts.mockOS.EXPECT().
					MkdirAll(gomock.Any(), gomock.Any()).
					Return(nil).
					Times(1)

				ts.mockJSON.EXPECT().
					Marshal(testState).
					Return(stateData, nil).
					Times(1)

				ts.mockOS.EXPECT().
					WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).
					Return(nil).
					Times(1)

				ts.mockOS.EXPECT().
					Rename(gomock.Any(), gomock.Any()).
					Return(fmt.Errorf("rename error")).
					Times(1)
			},
			wantErr: "failed to finalize state file",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			testState := &state.State{
				ConnectedDevice: &state.Device{
					ID:       "test-device-123",
					Name:     "Test Device",
					Platform: 1,
				},
				Relayer: &state.RelayerState{
					TopicID: "test-topic-456",
				},
			}

			tt.setupFunc(ts, testState)

			err := ts.sm.Save(testState)

			assert.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

func TestStateManager_GetState_InitialCall(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	sm := state.NewStateManager()
	result := sm.GetState()

	assert.NotNil(t, result)
	assert.NotNil(t, result.ConnectedDevice)
	assert.NotNil(t, result.Relayer)
	assert.Empty(t, result.ConnectedDevice.ID)
	assert.Empty(t, result.Relayer.TopicID)
}

// Test concurrent access patterns with StateManager

func TestStateManager_ConcurrentGetState(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test concurrent GetState calls
	const numGoroutines = 20
	results := make(chan *state.State, numGoroutines)

	for range numGoroutines {
		go func() {
			result := ts.sm.GetState()
			results <- result
		}()
	}

	// Collect results
	var states []*state.State
	for range numGoroutines {
		result := <-results
		assert.NotNil(t, result)
		assert.NotNil(t, result.ConnectedDevice)
		assert.NotNil(t, result.Relayer)
		assert.Empty(t, result.ConnectedDevice.ID)
		assert.Empty(t, result.Relayer.TopicID)
		states = append(states, result)
	}

	// Verify all results are identical (due to mutex protection)
	firstState := states[0]
	for i, s := range states {
		assert.Equal(t, firstState.ConnectedDevice.ID, s.ConnectedDevice.ID,
			"concurrent GetState %d: device ID mismatch", i)
		assert.Equal(t, firstState.Relayer.TopicID, s.Relayer.TopicID,
			"concurrent GetState %d: relayer topic ID mismatch", i)
	}
}

func TestStateManager_ConcurrentSave(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	stateDir := "/home/feralfile/.state"
	stateFile := stateDir + "/controld.state"
	tempFile := stateFile + ".tmp"

	// Create test states with different data
	testStates := []*state.State{
		{
			ConnectedDevice: &state.Device{
				ID:       "device-1",
				Name:     "Device One",
				Platform: 1,
			},
			Relayer: &state.RelayerState{
				TopicID: "topic-1",
			},
		},
		{
			ConnectedDevice: &state.Device{
				ID:       "device-2",
				Name:     "Device Two",
				Platform: 2,
			},
			Relayer: &state.RelayerState{
				TopicID: "topic-2",
			},
		},
		{
			ConnectedDevice: &state.Device{
				ID:       "device-3",
				Name:     "Device Three",
				Platform: 3,
			},
			Relayer: &state.RelayerState{
				TopicID: "topic-3",
			},
		},
	}

	// Expect MkdirAll to succeed (called once per goroutine)
	ts.mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0750)).
		Return(nil).
		Times(len(testStates))

	// Expect JSON marshal to succeed for each state
	for _, testState := range testStates {
		stateData := fmt.Sprintf(`{"connectedDevice":{"device_id":"%s","device_name":"%s","platform":%d},"relayer":{"topicId":"%s"}}`,
			testState.ConnectedDevice.ID, testState.ConnectedDevice.Name, testState.ConnectedDevice.Platform, testState.Relayer.TopicID)

		ts.mockJSON.EXPECT().
			Marshal(testState).
			Return([]byte(stateData), nil).
			Times(1)
	}

	// Expect WriteFile to succeed (called once per goroutine)
	ts.mockOS.EXPECT().
		WriteFile(tempFile, gomock.Any(), os.FileMode(0600)).
		Return(nil).
		Times(len(testStates))

	// Expect Rename to succeed (called once per goroutine)
	ts.mockOS.EXPECT().
		Rename(tempFile, stateFile).
		Return(nil).
		Times(len(testStates))

	// Execute concurrent saves
	errors := make(chan error, len(testStates))

	for _, testState := range testStates {
		go func(s *state.State) {
			err := ts.sm.Save(s)
			errors <- err
		}(testState)
	}

	// Collect results
	for i := range testStates {
		err := <-errors
		assert.NoError(t, err, "expected no error from concurrent save %d", i)
	}

	// Verify that the final state is one of the saved states
	finalState := ts.sm.GetState()
	assert.NotNil(t, finalState)

	// The final state should be one of the states we saved
	foundMatch := false
	for _, testState := range testStates {
		if finalState.ConnectedDevice.ID == testState.ConnectedDevice.ID &&
			finalState.Relayer.TopicID == testState.Relayer.TopicID {
			foundMatch = true
			break
		}
	}
	assert.True(t, foundMatch, "final state should match one of the saved states")
}

func TestStateManager_ConcurrentLoad(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	stateDir := filepath.Dir(constants.STATE_FILE)
	stateData := `{
		"connectedDevice": {
			"device_id": "concurrent-device-123",
			"device_name": "Concurrent Device",
			"platform": 2
		},
		"relayer": {
			"topicId": "concurrent-topic-789"
		}
	}`

	const numGoroutines = 5

	// Expect MkdirAll to succeed (called once per goroutine)
	ts.mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0750)).
		Return(nil).
		Times(numGoroutines)

	// Expect ReadFile to return state data (called once per goroutine)
	ts.mockOS.EXPECT().
		ReadFile(constants.STATE_FILE).
		Return([]byte(stateData), nil).
		Times(numGoroutines)

	// Expect IsNotExist check with nil error (called once per goroutine)
	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(numGoroutines)

	// Expect JSON unmarshal to succeed (called once per goroutine)
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(stateData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			st := v.(*state.State)
			st.ConnectedDevice = &state.Device{
				ID:       "concurrent-device-123",
				Name:     "Concurrent Device",
				Platform: 2,
			}
			st.Relayer = &state.RelayerState{
				TopicID: "concurrent-topic-789",
			}
			return nil
		}).
		Times(numGoroutines)

	// Execute concurrent loads
	results := make(chan *state.State, numGoroutines)
	errors := make(chan error, numGoroutines)

	for range numGoroutines {
		go func() {
			result, err := ts.sm.Load(ts.logger)
			results <- result
			errors <- err
		}()
	}

	// Collect results
	var loadedStates []*state.State
	for range numGoroutines {
		result := <-results
		err := <-errors
		assert.NoError(t, err, "expected no error from concurrent load")
		assert.NotNil(t, result, "expected non-nil state from concurrent load")
		loadedStates = append(loadedStates, result)
	}

	// Verify all results are identical (due to mutex protection)
	firstState := loadedStates[0]
	for i, loadedState := range loadedStates {
		assert.Equal(t, firstState.ConnectedDevice.ID, loadedState.ConnectedDevice.ID,
			"concurrent load %d: device ID mismatch", i)
		assert.Equal(t, firstState.ConnectedDevice.Name, loadedState.ConnectedDevice.Name,
			"concurrent load %d: device name mismatch", i)
		assert.Equal(t, firstState.ConnectedDevice.Platform, loadedState.ConnectedDevice.Platform,
			"concurrent load %d: device platform mismatch", i)
		assert.Equal(t, firstState.Relayer.TopicID, loadedState.Relayer.TopicID,
			"concurrent load %d: relayer topic ID mismatch", i)
	}
}

func TestStateManager_ConcurrentGetStateWithSaveInterference(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	stateDir := "/home/feralfile/.state"
	stateFile := stateDir + "/controld.state"
	tempFile := stateFile + ".tmp"

	// New state to save
	newState := &state.State{
		ConnectedDevice: &state.Device{
			ID:       "updated-device",
			Name:     "Updated Device",
			Platform: 2,
		},
		Relayer: &state.RelayerState{
			TopicID: "updated-topic",
		},
	}

	// Mock the save operation
	ts.mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0750)).
		Return(nil).
		Times(1)

	newStateData := []byte(`{"connectedDevice":{"device_id":"updated-device","device_name":"Updated Device","platform":2},"relayer":{"topicId":"updated-topic"}}`)
	ts.mockJSON.EXPECT().
		Marshal(newState).
		Return(newStateData, nil).
		Times(1)

	ts.mockOS.EXPECT().
		WriteFile(tempFile, newStateData, os.FileMode(0600)).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		Rename(tempFile, stateFile).
		Return(nil).
		Times(1)

	// Synchronization channels
	getState1Done := make(chan *state.State)
	saveCanStart := make(chan struct{})
	saveDone := make(chan struct{})
	getState2Done := make(chan *state.State)

	// Start first GetState - should see initial state
	go func() {
		state1 := ts.sm.GetState()
		getState1Done <- state1
		close(saveCanStart) // Signal save can start
	}()

	// Start Save operation - changes internal state
	go func() {
		<-saveCanStart // Wait for first GetState
		err := ts.sm.Save(newState)
		assert.NoError(t, err)
		close(saveDone) // Signal save completed
	}()

	// Start second GetState - should see updated state after save
	go func() {
		<-saveDone // Wait for save to complete
		state2 := ts.sm.GetState()
		getState2Done <- state2
	}()

	// Collect results
	state1 := <-getState1Done
	state2 := <-getState2Done

	// Verify state1 has initial empty state (before save)
	assert.NotNil(t, state1)
	assert.Empty(t, state1.ConnectedDevice.ID) // Empty initial state
	assert.Empty(t, state1.Relayer.TopicID)

	// Verify state2 has updated state (after save)
	assert.NotNil(t, state2)
	assert.Equal(t, "updated-device", state2.ConnectedDevice.ID)
	assert.Equal(t, "Updated Device", state2.ConnectedDevice.Name)
	assert.Equal(t, 2, state2.ConnectedDevice.Platform)
	assert.Equal(t, "updated-topic", state2.Relayer.TopicID)

	// Verify that save operation interfered between the two GetState calls
	assert.NotEqual(t, state1.ConnectedDevice.ID, state2.ConnectedDevice.ID,
		"Save should have interfered between GetState calls")
	assert.NotEqual(t, state1.Relayer.TopicID, state2.Relayer.TopicID,
		"Save should have interfered between GetState calls")
}

func TestStateManager_ConcurrentLoadAndSaveInterference(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	stateDir := filepath.Dir(constants.STATE_FILE)

	// Initial state data (what's in the file before save)
	initialStateData := `{
		"connectedDevice": {
			"device_id": "initial-device-123",
			"device_name": "Initial Device",
			"platform": 1
		},
		"relayer": {
			"topicId": "initial-topic-456"
		}
	}`

	// Updated state data (what the save operation will write)
	updatedStateData := `{
		"connectedDevice": {
			"device_id": "updated-device-789",
			"device_name": "Updated Device",
			"platform": 2
		},
		"relayer": {
			"topicId": "updated-topic-789"
		}
	}`

	sm := state.NewStateManagerWithDeps(ts.mockOS, ts.mockJSON)

	// State to save
	saveState := &state.State{
		ConnectedDevice: &state.Device{
			ID:       "updated-device-789",
			Name:     "Updated Device",
			Platform: 2,
		},
		Relayer: &state.RelayerState{
			TopicID: "updated-topic-789",
		},
	}

	// Synchronization channels
	load1CanStart := make(chan struct{})
	saveCanStart := make(chan struct{})
	load2CanStart := make(chan struct{})
	saveCompleted := make(chan struct{})

	// Track completion status
	saveHasCompleted := false

	// Setup expectations for Load operations
	ts.mockOS.EXPECT().
		MkdirAll(stateDir, os.FileMode(0750)).
		Return(nil).
		Times(3) // 2 loads + 1 save

	// Setup expectations for file reads with timing simulation
	ts.mockOS.EXPECT().
		ReadFile(constants.STATE_FILE).
		DoAndReturn(func(path string) ([]byte, error) {
			// Return data based on whether save has completed
			if saveHasCompleted {
				return []byte(updatedStateData), nil
			}
			return []byte(initialStateData), nil
		}).
		Times(2) // 2 loads

	// Setup expectations for IsNotExist checks
	ts.mockOS.EXPECT().
		IsNotExist(nil).
		Return(false).
		Times(2) // 2 loads

	// Setup expectations for JSON unmarshal - initial data
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(initialStateData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			st := v.(*state.State)
			st.ConnectedDevice = &state.Device{
				ID:       "initial-device-123",
				Name:     "Initial Device",
				Platform: 1,
			}
			st.Relayer = &state.RelayerState{
				TopicID: "initial-topic-456",
			}
			return nil
		}).
		Times(1) // Load before save

	// Setup expectations for JSON unmarshal - updated data
	ts.mockJSON.EXPECT().
		Unmarshal([]byte(updatedStateData), gomock.Any()).
		DoAndReturn(func(data []byte, v interface{}) error {
			st := v.(*state.State)
			st.ConnectedDevice = &state.Device{
				ID:       "updated-device-789",
				Name:     "Updated Device",
				Platform: 2,
			}
			st.Relayer = &state.RelayerState{
				TopicID: "updated-topic-789",
			}
			return nil
		}).
		Times(1) // Load after save

	// Setup expectations for save operation
	saveData := []byte(`{"connectedDevice":{"device_id":"updated-device-789","device_name":"Updated Device","platform":2},"relayer":{"topicId":"updated-topic-789"}}`)

	ts.mockJSON.EXPECT().
		Marshal(saveState).
		Return(saveData, nil).
		Times(1)

	ts.mockOS.EXPECT().
		WriteFile(gomock.Any(), saveData, os.FileMode(0600)).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		Rename(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	// Execute operations with controlled timing: Load1 → Save → Load2
	load1Result := make(chan *state.State, 1)
	load1Error := make(chan error, 1)
	load2Result := make(chan *state.State, 1)
	load2Error := make(chan error, 1)
	saveError := make(chan error, 1)

	// Start Load1 (should see initial data)
	go func() {
		<-load1CanStart // Wait for signal to start
		result, err := sm.Load(ts.logger)
		load1Result <- result
		load1Error <- err
		close(saveCanStart) // Signal that save can start
	}()

	// Start Save operation (will change the internal state)
	go func() {
		<-saveCanStart // Wait for Load1 to complete
		err := sm.Save(saveState)
		saveHasCompleted = true // Mark save as completed
		saveError <- err
		close(saveCompleted) // Signal that save has completed
		close(load2CanStart) // Signal that Load2 can start
	}()

	// Start Load2 (should see updated data after save)
	go func() {
		<-load2CanStart // Wait for save to complete
		<-saveCompleted // Ensure save is fully done
		result, err := sm.Load(ts.logger)
		load2Result <- result
		load2Error <- err
	}()

	// Start the sequence
	close(load1CanStart)

	// Collect results in order
	result1 := <-load1Result
	err1 := <-load1Error
	assert.NoError(t, err1, "expected no error from Load1")
	assert.NotNil(t, result1, "expected non-nil state from Load1")

	saveErr := <-saveError
	assert.NoError(t, saveErr, "expected no error from Save")

	result2 := <-load2Result
	err2 := <-load2Error
	assert.NoError(t, err2, "expected no error from Load2")
	assert.NotNil(t, result2, "expected non-nil state from Load2")

	// Verify Load1 sees initial data (before save)
	assert.Equal(t, "initial-device-123", result1.ConnectedDevice.ID,
		"Load1 should see initial device ID (before save)")
	assert.Equal(t, "Initial Device", result1.ConnectedDevice.Name,
		"Load1 should see initial device name (before save)")
	assert.Equal(t, 1, result1.ConnectedDevice.Platform,
		"Load1 should see initial platform (before save)")
	assert.Equal(t, "initial-topic-456", result1.Relayer.TopicID,
		"Load1 should see initial topic ID (before save)")

	// Verify Load2 sees updated data (after save)
	assert.Equal(t, "updated-device-789", result2.ConnectedDevice.ID,
		"Load2 should see updated device ID (after save)")
	assert.Equal(t, "Updated Device", result2.ConnectedDevice.Name,
		"Load2 should see updated device name (after save)")
	assert.Equal(t, 2, result2.ConnectedDevice.Platform,
		"Load2 should see updated platform (after save)")
	assert.Equal(t, "updated-topic-789", result2.Relayer.TopicID,
		"Load2 should see updated topic ID (after save)")

	// Verify that the data is different between loads (save operation interfered)
	assert.NotEqual(t, result1.ConnectedDevice.ID, result2.ConnectedDevice.ID,
		"Load1 and Load2 should return different data due to save operation")
	assert.NotEqual(t, result1.Relayer.TopicID, result2.Relayer.TopicID,
		"Load1 and Load2 should return different topic IDs due to save operation")
}

// Test state package-level functions
func TestState_Load_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Use StateManager with mocked dependencies for testing global Load function
	sm := state.NewStateManagerWithDeps(ts.mockOS, ts.mockJSON)
	state.InjectStateManagerForTesting(sm)

	notFoundErr := &os.PathError{Op: "open", Path: constants.STATE_FILE, Err: os.ErrNotExist}

	ts.mockOS.EXPECT().
		MkdirAll(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		ReadFile(gomock.Any()).
		Return(nil, notFoundErr).
		Times(1)

	ts.mockOS.EXPECT().
		IsNotExist(notFoundErr).
		Return(true).
		Times(1)

	result, err := state.Load(ts.logger)

	assert.NoError(t, err)
	assert.NotNil(t, result)
	assert.Empty(t, result.ConnectedDevice.ID)
}

func TestState_GetState_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	result := state.GetState()

	assert.NotNil(t, result)
	assert.NotNil(t, result.ConnectedDevice)
	assert.NotNil(t, result.Relayer)
	assert.Empty(t, result.ConnectedDevice.ID)
	assert.Empty(t, result.Relayer.TopicID)
}

func TestState_SaveState_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Use StateManager with mocked dependencies for testing global SaveState function
	sm := state.NewStateManagerWithDeps(ts.mockOS, ts.mockJSON)
	state.InjectStateManagerForTesting(sm)

	testState := &state.State{
		ConnectedDevice: &state.Device{
			ID:       "test-device-123",
			Name:     "Test Device",
			Platform: 1,
		},
		Relayer: &state.RelayerState{
			TopicID: "test-topic-456",
		},
	}

	ts.mockOS.EXPECT().
		MkdirAll(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Marshal(testState).
		Return([]byte(`{"test":"data"}`), nil).
		Times(1)

	ts.mockOS.EXPECT().
		WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		Rename(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	err := state.SaveState(testState)

	assert.NoError(t, err)
	assert.Equal(t, testState, state.GetState())
}

func TestState_Save_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Use StateManager with mocked dependencies for testing deprecated Save method
	sm := state.NewStateManagerWithDeps(ts.mockOS, ts.mockJSON)
	state.InjectStateManagerForTesting(sm)

	testState := &state.State{
		ConnectedDevice: &state.Device{
			ID:       "test-device-123",
			Name:     "Test Device",
			Platform: 1,
		},
		Relayer: &state.RelayerState{
			TopicID: "test-topic-456",
		},
	}

	ts.mockOS.EXPECT().
		MkdirAll(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockJSON.EXPECT().
		Marshal(testState).
		Return([]byte(`{"test":"data"}`), nil).
		Times(1)

	ts.mockOS.EXPECT().
		WriteFile(gomock.Any(), gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	ts.mockOS.EXPECT().
		Rename(gomock.Any(), gomock.Any()).
		Return(nil).
		Times(1)

	// Test that the old Save() method still works
	err := testState.Save()

	assert.NoError(t, err)
}

func TestRelayerState_IsReady(t *testing.T) {
	tests := []struct {
		name     string
		topicID  string
		expected bool
	}{
		{
			name:     "empty topic ID",
			topicID:  "",
			expected: false,
		},
		{
			name:     "non-empty topic ID",
			topicID:  "test-topic-123",
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			relayerState := &state.RelayerState{
				TopicID: tt.topicID,
			}

			result := relayerState.IsReady()
			assert.Equal(t, tt.expected, result)
		})
	}
}
