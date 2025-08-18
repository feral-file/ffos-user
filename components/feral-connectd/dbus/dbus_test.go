package dbus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Feral-File/ffos-user/components/feral-connectd/dbus"
	"github.com/Feral-File/ffos-user/components/feral-connectd/mocks"
	"github.com/Feral-File/ffos-user/components/feral-connectd/relayer"
	"github.com/Feral-File/ffos-user/components/feral-connectd/state"

	godbus "github.com/godbus/dbus/v5"
	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"
)

type testSetup struct {
	ctrl        *gomock.Controller
	ctx         context.Context
	mockRelayer *mocks.MockRelayer
	mockState   *mocks.MockStateManager
	client      dbus.DBusHandler
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockRelayer := mocks.NewMockRelayer(ctrl)
	mockState := mocks.NewMockStateManager(ctrl)

	// Inject the mock state manager for testing
	state.InjectStateManagerForTesting(mockState)

	client := dbus.NewHandler(ctx, mockRelayer, logger)

	return &testSetup{
		ctrl:        ctrl,
		ctx:         ctx,
		mockRelayer: mockRelayer,
		mockState:   mockState,
		client:      client,
	}
}

func (ts *testSetup) teardown() {
	// Reset state manager after test
	state.ResetForTesting()
	ts.ctrl.Finish()
}

func TestClient_GetRelayerTopicID_Success(t *testing.T) {
	tests := []struct {
		name      string
		setupFunc func(*testSetup) (string, chan struct{})
	}{
		{
			name: "returns existing topic ID immediately",
			setupFunc: func(ts *testSetup) (string, chan struct{}) {
				existingTopicID := "existing-topic-id"

				// Mock state to return existing topic ID
				ts.mockState.EXPECT().
					GetState().
					Return(&state.State{
						Relayer: &state.RelayerState{
							TopicID: existingTopicID,
						},
					}).
					Times(1)

				return existingTopicID, nil
			},
		},
		{
			name: "retrieves new topic ID from relayer",
			setupFunc: func(ts *testSetup) (string, chan struct{}) {
				expectedTopicID := "new-topic-id"

				// Mock state to return empty topic ID initially
				initialState := &state.State{
					Relayer: &state.RelayerState{TopicID: ""},
				}

				// Mock state to return updated topic ID after save
				finalState := &state.State{
					Relayer: &state.RelayerState{TopicID: expectedTopicID},
				}

				// First call returns empty topic ID (initial check)
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				// Channel to coordinate the handler and test
				handlerCalled := make(chan struct{})
				var capturedHandler relayer.Handler

				// Expect OnRelayerMessage to be called
				ts.mockRelayer.EXPECT().
					OnRelayerMessage(gomock.Any()).
					DoAndReturn(func(handler relayer.Handler) {
						capturedHandler = handler
					}).
					Times(1)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					DoAndReturn(func(ctx context.Context) error {
						// Simulate receiving the system message with topicID
						go func() {
							time.Sleep(50 * time.Millisecond)

							payload := relayer.Payload{
								MessageID: relayer.MESSAGE_ID_SYSTEM,
								Message: struct {
									Command *relayer.RelayerCmd    `json:"command,omitempty"`
									Args    map[string]interface{} `json:"request,omitempty"`
									TopicID *string                `json:"topicID,omitempty"`
								}{
									TopicID: &expectedTopicID,
								},
							}

							if capturedHandler != nil {
								_ = capturedHandler(context.Background(), payload)
								close(handlerCalled)
							}
						}()
						return nil
					}).
					Times(1)

				// Second GetState call inside the handler
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				// Expect Save to be called
				ts.mockState.EXPECT().
					Save(gomock.Any()).
					DoAndReturn(func(s *state.State) error {
						assert.Equal(t, expectedTopicID, s.Relayer.TopicID)
						return nil
					}).
					Times(1)

				// Third GetState call for final return
				ts.mockState.EXPECT().
					GetState().
					Return(finalState).
					Times(1)

				// Expect RemoveRelayerMessage to be called twice
				ts.mockRelayer.EXPECT().
					RemoveRelayerMessage(gomock.Any()).
					Times(2)

				return expectedTopicID, handlerCalled
			},
		},
		{
			name: "ignores non-system messages and processes system message",
			setupFunc: func(ts *testSetup) (string, chan struct{}) {
				expectedTopicID := "new-topic-id"

				initialState := &state.State{
					Relayer: &state.RelayerState{TopicID: ""},
				}

				finalState := &state.State{
					Relayer: &state.RelayerState{TopicID: expectedTopicID},
				}

				// First call returns empty topic ID (initial check)
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				// Channel to coordinate the handler and test
				handlerCalled := make(chan struct{})
				var capturedHandler relayer.Handler

				// Expect OnRelayerMessage to be called
				ts.mockRelayer.EXPECT().
					OnRelayerMessage(gomock.Any()).
					DoAndReturn(func(handler relayer.Handler) {
						capturedHandler = handler
					}).
					Times(1)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					DoAndReturn(func(ctx context.Context) error {
						go func() {
							time.Sleep(50 * time.Millisecond)

							// First send a non-system message (should be ignored)
							nonSystemPayload := relayer.Payload{
								MessageID: "non-system-message",
								Message: struct {
									Command *relayer.RelayerCmd    `json:"command,omitempty"`
									Args    map[string]interface{} `json:"request,omitempty"`
									TopicID *string                `json:"topicID,omitempty"`
								}{
									TopicID: &expectedTopicID,
								},
							}

							if capturedHandler != nil {
								_ = capturedHandler(context.Background(), nonSystemPayload)
							}

							// Then send the system message (should be processed)
							time.Sleep(50 * time.Millisecond)
							systemPayload := relayer.Payload{
								MessageID: relayer.MESSAGE_ID_SYSTEM,
								Message: struct {
									Command *relayer.RelayerCmd    `json:"command,omitempty"`
									Args    map[string]interface{} `json:"request,omitempty"`
									TopicID *string                `json:"topicID,omitempty"`
								}{
									TopicID: &expectedTopicID,
								},
							}

							if capturedHandler != nil {
								_ = capturedHandler(context.Background(), systemPayload)
								close(handlerCalled)
							}
						}()
						return nil
					}).
					Times(1)

				// Second GetState call inside the handler (for the system message only)
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				// Expect Save to be called only once (for the system message)
				ts.mockState.EXPECT().
					Save(gomock.Any()).
					DoAndReturn(func(s *state.State) error {
						assert.Equal(t, expectedTopicID, s.Relayer.TopicID)
						return nil
					}).
					Times(1)

				// Third GetState call for final return
				ts.mockState.EXPECT().
					GetState().
					Return(finalState).
					Times(1)

				// Expect RemoveRelayerMessage to be called twice
				ts.mockRelayer.EXPECT().
					RemoveRelayerMessage(gomock.Any()).
					Times(2)

				return expectedTopicID, handlerCalled
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			expectedTopicID, handlerCalled := tt.setupFunc(ts)

			// Test
			result, dbusErr := ts.client.GetRelayerTopicID()

			// Wait for async handler if needed
			if handlerCalled != nil {
				select {
				case <-handlerCalled:
					// Good
				case <-time.After(1 * time.Second):
					t.Fatal("handler was not called within timeout")
				}
			}

			// Verify
			assert.Nil(t, dbusErr, "expected no dbus error, got %v", dbusErr)
			assert.Equal(t, expectedTopicID, result, "expected topic ID to be %s, got %s", expectedTopicID, result)
		})
	}
}

func TestClient_GetRelayerTopicID_Failures(t *testing.T) {
	tests := []struct {
		name          string
		setupFunc     func(*testSetup)
		expectedError string
	}{
		{
			name: "payload missing topic ID",
			setupFunc: func(ts *testSetup) {
				initialState := &state.State{
					Relayer: &state.RelayerState{TopicID: ""},
				}

				// First call returns empty topic ID (initial check)
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				var capturedHandler relayer.Handler

				// Expect OnRelayerMessage to be called
				ts.mockRelayer.EXPECT().
					OnRelayerMessage(gomock.Any()).
					DoAndReturn(func(handler relayer.Handler) {
						capturedHandler = handler
					}).
					Times(1)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					DoAndReturn(func(ctx context.Context) error {
						go func() {
							time.Sleep(50 * time.Millisecond)

							payload := relayer.Payload{
								MessageID: relayer.MESSAGE_ID_SYSTEM,
								Message: struct {
									Command *relayer.RelayerCmd    `json:"command,omitempty"`
									Args    map[string]interface{} `json:"request,omitempty"`
									TopicID *string                `json:"topicID,omitempty"`
								}{
									TopicID: nil, // Missing topicID
								},
							}

							if capturedHandler != nil {
								_ = capturedHandler(context.Background(), payload)
							}
						}()
						return nil
					}).
					Times(1)

				// Expect RemoveRelayerMessage to be called
				ts.mockRelayer.EXPECT().
					RemoveRelayerMessage(gomock.Any()).
					Times(1)
			},
			expectedError: "payload doesn't contain topicID",
		},
		{
			name: "state save error",
			setupFunc: func(ts *testSetup) {
				expectedTopicID := "new-topic-id"
				saveError := errors.New("save failed")

				// First call returns empty topic ID (initial check)
				initialState := &state.State{
					Relayer: &state.RelayerState{TopicID: ""},
				}

				// First call returns empty topic ID (initial check)
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				// Expect OnRelayerMessage to be called
				var capturedHandler relayer.Handler
				ts.mockRelayer.EXPECT().
					OnRelayerMessage(gomock.Any()).
					DoAndReturn(func(handler relayer.Handler) {
						capturedHandler = handler
					}).
					Times(1)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					DoAndReturn(func(ctx context.Context) error {
						go func() {
							time.Sleep(50 * time.Millisecond)

							payload := relayer.Payload{
								MessageID: relayer.MESSAGE_ID_SYSTEM,
								Message: struct {
									Command *relayer.RelayerCmd    `json:"command,omitempty"`
									Args    map[string]interface{} `json:"request,omitempty"`
									TopicID *string                `json:"topicID,omitempty"`
								}{
									TopicID: &expectedTopicID,
								},
							}

							if capturedHandler != nil {
								_ = capturedHandler(context.Background(), payload)
							}
						}()
						return nil
					}).
					Times(1)

				// Second GetState call inside the handler
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				// Expect Save to be called
				ts.mockState.EXPECT().
					Save(gomock.Any()).
					Return(saveError).
					Times(1)

				// Expect RemoveRelayerMessage to be called
				ts.mockRelayer.EXPECT().
					RemoveRelayerMessage(gomock.Any()).
					Times(1)
			},
			expectedError: "save failed",
		},
		{
			name: "retryable connect error",
			setupFunc: func(ts *testSetup) {
				connectError := errors.New("connection failed")

				initialState := &state.State{
					Relayer: &state.RelayerState{TopicID: ""},
				}

				// First call returns empty topic ID (initial check)
				ts.mockState.EXPECT().
					GetState().
					Return(initialState).
					Times(1)

				// Expect OnRelayerMessage to be called
				ts.mockRelayer.EXPECT().
					OnRelayerMessage(gomock.Any()).
					Times(1)

				// Expect RetryableConnect to be called
				ts.mockRelayer.EXPECT().
					RetryableConnect(gomock.Any()).
					Return(connectError).
					Times(1)

				// Expect RemoveRelayerMessage to be called
				ts.mockRelayer.EXPECT().
					RemoveRelayerMessage(gomock.Any()).
					Times(1)
			},
			expectedError: "connection failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			tt.setupFunc(ts)

			// Test
			result, dbusErr := ts.client.GetRelayerTopicID()

			// Verify
			assert.NotNil(t, dbusErr, "expected dbus error")
			assert.Empty(t, result, "expected empty result")
			assert.Contains(t, dbusErr.Name, tt.expectedError, "expected error message to contain: %s", tt.expectedError)
		})
	}
}

func TestClient_GetRelayerTopicID_ContextTimeout(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock state to return empty topic ID
	initialState := &state.State{
		Relayer: &state.RelayerState{TopicID: ""},
	}

	// First call returns empty topic ID (initial check)
	ts.mockState.EXPECT().
		GetState().
		Return(initialState).
		Times(1)

	// Expect OnRelayerMessage to be called
	ts.mockRelayer.EXPECT().
		OnRelayerMessage(gomock.Any()).
		Times(1)

	// Expect RetryableConnect to succeed but no message received (timeout scenario)
	ts.mockRelayer.EXPECT().
		RetryableConnect(gomock.Any()).
		DoAndReturn(func(ctx context.Context) error {
			// Don't send any message, let it timeout
			return nil
		}).
		Times(1)

	// Expect RemoveRelayerMessage to be called
	ts.mockRelayer.EXPECT().
		RemoveRelayerMessage(gomock.Any()).
		Times(1)

	// Test with a very short timeout context to speed up the test
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	// Create a new client with the short timeout context
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	client := dbus.NewHandler(ctx, ts.mockRelayer, logger)

	result, dbusErr := client.GetRelayerTopicID()

	// Verify
	assert.NotNil(t, dbusErr, "expected dbus error for timeout")
	assert.Empty(t, result, "expected empty result")
	assert.Contains(t, dbusErr.Name, "context deadline exceeded", "expected timeout error message")
}

func TestClient_GetRelayerTopicID_ConcurrentCalls(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	existingTopicID := "existing-topic-id"

	// Mock state to return existing topic ID for all calls
	ts.mockState.EXPECT().
		GetState().
		Return(&state.State{
			Relayer: &state.RelayerState{
				TopicID: existingTopicID,
			},
		}).
		AnyTimes()

	// Test concurrent calls
	numGoroutines := 10
	resultChan := make(chan string, numGoroutines)
	errorChan := make(chan *godbus.Error, numGoroutines)
	startChan := make(chan struct{})

	for range numGoroutines {
		go func() {
			<-startChan
			result, dbusErr := ts.client.GetRelayerTopicID()
			if dbusErr != nil {
				errorChan <- dbusErr
			} else {
				resultChan <- result
			}
		}()
	}

	// Start all goroutines at the same time
	close(startChan)

	// Collect results
	var results []string
	var errors []*godbus.Error

	for range numGoroutines {
		select {
		case result := <-resultChan:
			results = append(results, result)
		case err := <-errorChan:
			errors = append(errors, err)
		case <-time.After(1 * time.Second):
			t.Fatal("timeout waiting for goroutine results")
		}
	}

	// Verify all calls succeeded and returned the same topic ID
	assert.Empty(t, errors, "expected no errors from concurrent calls")
	assert.Len(t, results, numGoroutines, "expected all calls to succeed")

	for _, result := range results {
		assert.Equal(t, existingTopicID, result, "all calls should return the same topic ID")
	}
}
