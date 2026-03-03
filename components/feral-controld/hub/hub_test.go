package hub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type testSetup struct {
	ctrl        *gomock.Controller
	ctx         context.Context
	mockWS      *mocks.MockWS
	mockCmd     *mocks.MockCommandHandler
	mockServer  *mocks.MockHTTPServer
	mockJSON    *mocks.MockJSON
	mockJSONDec *mocks.MockJSONDecoder
	mockJSONEnc *mocks.MockJSONEncoder
	hub         Hub
	logger      *zap.Logger
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Dependencies
	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockJSONDec := mocks.NewMockJSONDecoder(ctrl)
	mockJSONEnc := mocks.NewMockJSONEncoder(ctrl)

	// Mock HTTPServer Handler to return a ServeMux (needed for routes() in constructor)
	// Create a fresh ServeMux for each test to avoid route conflicts
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()

	h := New(ctx, mockWS, mockCmd, mockServer, mockJSON, logger)

	return &testSetup{
		ctrl:        ctrl,
		ctx:         ctx,
		mockWS:      mockWS,
		mockCmd:     mockCmd,
		mockServer:  mockServer,
		mockJSON:    mockJSON,
		mockJSONDec: mockJSONDec,
		mockJSONEnc: mockJSONEnc,
		hub:         h,
		logger:      logger,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

func withFF1ConfigReadError(ts *testSetup, readErr error) {
	hubImpl := ts.hub.(*hub)
	hubImpl.readConfig = func() ([]byte, error) {
		return nil, readErr
	}
}

func withFF1Config(ts *testSetup, rawJSON string) {
	hubImpl := ts.hub.(*hub)
	hubImpl.readConfig = func() ([]byte, error) {
		return []byte(rawJSON), nil
	}
}

func TestNew(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Dependencies
	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)

	// Mock HTTPServer Handler to return a HandlerFunc (not ServeMux) to trigger panic
	mockServer.EXPECT().
		Handler().
		Return(http.NewServeMux()).
		Times(1)

	h := New(ctx, mockWS, mockCmd, mockServer, mockJSON, logger)
	assert.NotNil(t, h)
}

func TestNew_UnsupportedHandlerType(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	// Dependencies
	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)

	// Mock HTTPServer Handler to return a HandlerFunc (not ServeMux) to trigger panic
	mockServer.EXPECT().
		Handler().
		Return(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {})).
		Times(1)

	assert.Panics(t, func() {
		New(ctx, mockWS, mockCmd, mockServer, mockJSON, logger)
	})
}

func TestStart_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WS Close - may be called due to context cancellation
	ts.mockWS.EXPECT().
		Close().
		AnyTimes()

	// Mock HTTPServer ListenAndServe to return nil (success)
	ts.mockServer.EXPECT().
		ListenAndServe().
		Return(nil).
		Times(1)

	// Mock HTTPServer Shutdown for Stop() calls
	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Start the hub
	ts.hub.Start()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Stop the hub
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

func TestStart_ListenAndServeError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WS Close - may be called due to context cancellation
	ts.mockWS.EXPECT().
		Close().
		MinTimes(1)

	// Mock HTTPServer ListenAndServe to return an error
	expectedErr := errors.New("server error")
	ts.mockServer.EXPECT().
		ListenAndServe().
		Return(expectedErr).
		Times(1)

	// Mock Stop to be called when ListenAndServe fails
	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		MinTimes(1)

	// Start the hub
	ts.hub.Start()

	// Give it a moment to process
	time.Sleep(10 * time.Millisecond)

	// Stop the hub
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

func TestStop(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WS Close - may be called multiple times due to context cancellation
	ts.mockWS.EXPECT().
		Close().
		MinTimes(1)

	// Mock HTTPServer ListenAndServe and Shutdown
	ts.mockServer.EXPECT().
		ListenAndServe().
		Return(nil).
		Times(1)
	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		MinTimes(1)

	// Start the hub
	ts.hub.Start()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Stop the hub
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

func TestStop_WithoutStart(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WS Close
	ts.mockWS.EXPECT().
		Close().
		Times(1)

	// Mock HTTPServer Shutdown
	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		Times(1)

	// Stop without starting
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

func TestHub_StartStop_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WS Close - may be called due to context cancellation
	ts.mockWS.EXPECT().
		Close().
		AnyTimes()

	// Mock HTTPServer ListenAndServe to return nil (success)
	ts.mockServer.EXPECT().
		ListenAndServe().
		Return(nil).
		Times(1)

	// Mock HTTPServer Shutdown for Stop() calls
	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Start the hub
	ts.hub.Start()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Stop the hub
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

func TestHub_Start_ListenAndServeError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WS Close - may be called due to context cancellation
	ts.mockWS.EXPECT().
		Close().
		AnyTimes()

	// Mock HTTPServer ListenAndServe to return an error
	expectedErr := errors.New("server error")
	ts.mockServer.EXPECT().
		ListenAndServe().
		Return(expectedErr).
		Times(1)

	// Mock Stop to be called when ListenAndServe fails
	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		AnyTimes()

	// Start the hub
	ts.hub.Start()

	// Give it a moment to process
	time.Sleep(10 * time.Millisecond)

	// Stop the hub
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

func TestHub_Stop_WithoutStart(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WS Close
	ts.mockWS.EXPECT().
		Close().
		Times(1)

	// Mock HTTPServer Shutdown
	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		Times(1)

	// Stop without starting
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

func TestHub_ContextCancellation(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a cancellable context
	ctx, cancel := context.WithCancel(context.Background())
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	// Create a fresh mock server for this test to avoid route conflicts
	mockServer := mocks.NewMockHTTPServer(ts.ctrl)
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()
	mockServer.EXPECT().ListenAndServe().Return(nil).AnyTimes()
	mockServer.EXPECT().Shutdown(gomock.Any()).Return(nil).AnyTimes()

	// Create hub with cancellable context
	h := New(ctx, ts.mockWS, ts.mockCmd, mockServer, wrapper.NewJSON(), logger)

	// Mock WS Close - may be called multiple times due to context cancellation
	ts.mockWS.EXPECT().Close().AnyTimes()

	// Start the hub
	h.Start()

	// Give it a moment to start
	time.Sleep(10 * time.Millisecond)

	// Cancel the context
	cancel()

	// Give it a moment to process the cancellation
	time.Sleep(10 * time.Millisecond)

	// Stop should still work
	err := h.Stop()
	assert.NoError(t, err)
}

func TestHandleCast_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test payload
	cmd := string(commands.CMD_DEVICE_STATUS)
	payload := commands.Command{
		Type:      commands.Type(cmd),
		Arguments: map[string]interface{}{"test": "value"},
	}

	expectedResult := map[string]string{"status": "success"}

	// Mock JSON decoder to return the payload
	ts.mockJSONDec.EXPECT().
		Decode(gomock.Any()).
		DoAndReturn(func(p *commands.Command) error {
			*p = payload
			return nil
		}).
		Times(1)

	// Mock command handler to return success (use gomock.Any() for payload)
	ts.mockCmd.EXPECT().
		Process(ts.ctx, gomock.Any()).
		Return(expectedResult, nil).
		Times(1)

	// Mock JSON encoder to capture the response
	ts.mockJSONEnc.EXPECT().
		Encode(expectedResult).
		Return(nil).
		Times(1)

	// Mock JSON to return the mocked decoder and encoder
	ts.mockJSON.EXPECT().
		NewDecoder(gomock.Any()).
		Return(ts.mockJSONDec).
		Times(1)
	ts.mockJSON.EXPECT().
		NewEncoder(gomock.Any()).
		Return(ts.mockJSONEnc).
		Times(1)

	// Create a test request with actual JSON payload
	jsonPayload := `{"command":"getDeviceStatus","request":{"test":"value"}}`
	req, err := http.NewRequest("POST", "/api/cast", strings.NewReader(jsonPayload))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleCast(w, req)

	// Verify the response
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
}

func TestHandleCast_InvalidMethod(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test request with wrong method
	req, err := http.NewRequest("GET", "/api/cast", nil)
	assert.NoError(t, err)

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleCast(w, req)

	// Verify the response
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandleCast_InvalidJSON(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock JSON decoder to return an error
	ts.mockJSONDec.EXPECT().
		Decode(gomock.Any()).
		Return(errors.New("invalid JSON")).
		Times(1)

	// Mock JSON to return the mocked decoder
	ts.mockJSON.EXPECT().
		NewDecoder(gomock.Any()).
		Return(ts.mockJSONDec).
		Times(1)

	// Create a test request with invalid JSON
	req, err := http.NewRequest("POST", "/api/cast", strings.NewReader("invalid json"))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleCast(w, req)

	// Verify the response
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestHandleCast_ProcessError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test payload
	cmd := commands.CMD_DEVICE_STATUS
	payload := commands.Command{
		Type:      cmd,
		Arguments: map[string]interface{}{"test": "value"},
	}

	// Mock JSON decoder to return the payload
	ts.mockJSONDec.EXPECT().
		Decode(gomock.Any()).
		DoAndReturn(func(p *commands.Command) error {
			*p = payload
			return nil
		}).
		Times(1)

	// Mock command handler to return error
	processErr := errors.New("process error")
	ts.mockCmd.EXPECT().
		Process(ts.ctx, gomock.Any()).
		Return(nil, processErr).
		Times(1)

	// Mock JSON to return the mocked decoder (no encoder needed for error case)
	ts.mockJSON.EXPECT().
		NewDecoder(gomock.Any()).
		Return(ts.mockJSONDec).
		Times(1)

	// Create a test request with actual JSON payload
	jsonPayload := `{"messageID":"test-123","message":{"command":"DEVICE_STATUS","request":{"test":"value"}}}`
	req, err := http.NewRequest("POST", "/api/cast", strings.NewReader(jsonPayload))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleCast(w, req)

	// Verify the response - should return 500 Internal Server Error
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleCast_ProcessNilResult(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test payload
	cmd := commands.CMD_DEVICE_STATUS
	payload := commands.Command{
		Type:      cmd,
		Arguments: map[string]interface{}{"test": "value"},
	}

	// Mock JSON decoder to return the payload
	ts.mockJSONDec.EXPECT().
		Decode(gomock.Any()).
		DoAndReturn(func(p *commands.Command) error {
			*p = payload
			return nil
		}).
		Times(1)

	// Mock command handler to return nil result (no error, but no content)
	ts.mockCmd.EXPECT().
		Process(ts.ctx, gomock.Any()).
		Return(nil, nil).
		Times(1)

	// Mock JSON to return the mocked decoder (no encoder needed for nil result case)
	ts.mockJSON.EXPECT().
		NewDecoder(gomock.Any()).
		Return(ts.mockJSONDec).
		Times(1)

	// Create a test request with actual JSON payload
	jsonPayload := `{"messageID":"test-123","message":{"command":"DEVICE_STATUS","request":{"test":"value"}}}`
	req, err := http.NewRequest("POST", "/api/cast", strings.NewReader(jsonPayload))
	assert.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleCast(w, req)

	// Verify the response - should return 204 No Content
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestHandleVersion_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	withFF1Config(ts, `{"version":"1.2.3"}`)

	hubImpl := ts.hub.(*hub)
	hubImpl.json = wrapper.NewJSON()

	req, err := http.NewRequest("GET", "/api/version", nil)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	hubImpl.handleVersion(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), `"version":"1.2.3"`)
}

func TestHandleVersion_InvalidMethod(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	req, err := http.NewRequest("POST", "/api/version", strings.NewReader("{}"))
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	hubImpl := ts.hub.(*hub)
	hubImpl.handleVersion(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandleVersion_ConfigReadFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	withFF1ConfigReadError(ts, errors.New("config read failed"))

	hubImpl := ts.hub.(*hub)
	hubImpl.json = wrapper.NewJSON()

	req, err := http.NewRequest("GET", "/api/version", nil)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	hubImpl.handleVersion(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestHandleInfo_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	withFF1Config(ts, `{"version":"2.0.0"}`)

	hubImpl := ts.hub.(*hub)
	hubImpl.json = wrapper.NewJSON()

	req, err := http.NewRequest("GET", "/api/info", nil)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	hubImpl.handleInfo(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), `"ff1Version":"2.0.0"`)
}

func TestHandleStatus_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	withFF1Config(ts, `{"version":"3.0.0"}`)

	hubImpl := ts.hub.(*hub)
	hubImpl.json = wrapper.NewJSON()

	req, err := http.NewRequest("GET", "/api/status", nil)
	assert.NoError(t, err)

	w := httptest.NewRecorder()
	hubImpl.handleStatus(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
	assert.Contains(t, w.Body.String(), `"installedVersion":"3.0.0"`)
}

func TestHandleNotification_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WebSocket handler to return success
	ts.mockWS.EXPECT().
		NewConnection(gomock.Any(), gomock.Any()).
		Return("conn-123", nil).
		Times(1)

	// Create a test request
	req, err := http.NewRequest("GET", "/api/notification", nil)
	assert.NoError(t, err)

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleNotification(w, req)

	// Verify the response (WebSocket upgrade should succeed)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestHandleNotification_InvalidMethod(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Create a test request with wrong method
	req, err := http.NewRequest("POST", "/api/notification", nil)
	assert.NoError(t, err)

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleNotification(w, req)

	// Verify the response
	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

func TestHandleNotification_WebSocketError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Mock WebSocket handler to return an error
	wsErr := errors.New("websocket upgrade failed")
	ts.mockWS.EXPECT().
		NewConnection(gomock.Any(), gomock.Any()).
		Return("", wsErr).
		Times(1)

	// Create a test request
	req, err := http.NewRequest("GET", "/api/notification", nil)
	assert.NoError(t, err)

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the handler directly (white box testing)
	hubImpl := ts.hub.(*hub)
	hubImpl.handleNotification(w, req)

	// Verify the response - should return 500 Internal Server Error
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestRespondJSON_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Test data
	testData := map[string]string{"test": "value"}

	// Mock JSON encoder to return success
	ts.mockJSONEnc.EXPECT().
		Encode(testData).
		Return(nil).
		Times(1)

	// Mock JSON to return the mocked encoder
	ts.mockJSON.EXPECT().
		NewEncoder(gomock.Any()).
		Return(ts.mockJSONEnc).
		Times(1)

	// Create a test response writer
	w := httptest.NewRecorder()

	// Call the respondJSON method directly (white box testing)
	hubImpl := ts.hub.(*hub)
	err := hubImpl.respondJSON(w, http.StatusOK, testData)

	// Verify the response
	assert.NoError(t, err)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))
}
