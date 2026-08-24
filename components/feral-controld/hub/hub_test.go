package hub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/feral-file/ffos-user/components/feral-controld/commandrouter"
	"github.com/feral-file/ffos-user/components/feral-controld/commands"
	"github.com/feral-file/ffos-user/components/feral-controld/mdns"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
	"github.com/feral-file/ffos-user/components/feral-controld/screenshot"
	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

type fakeScreenshotCapturer struct {
	image  *screenshot.Image
	err    error
	bounds screenshot.Bounds
	calls  int
}

type blockingScreenshotResponseWriter struct {
	header       http.Header
	status       int
	writeStarted chan struct{}
	unblock      chan struct{}
}

func (w *blockingScreenshotResponseWriter) Header() http.Header {
	return w.header
}

func (w *blockingScreenshotResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *blockingScreenshotResponseWriter) Write(data []byte) (int, error) {
	close(w.writeStarted)
	<-w.unblock
	return len(data), nil
}

func (f *fakeScreenshotCapturer) Capture(_ context.Context, bounds screenshot.Bounds) (*screenshot.Image, error) {
	f.calls++
	f.bounds = bounds
	return f.image, f.err
}

type testSetup struct {
	ctrl        *gomock.Controller
	ctx         context.Context
	mockWS      *mocks.MockWS
	mockCmd     *mocks.MockCommandHandler
	mockServer  *mocks.MockHTTPServer
	mockJSON    *mocks.MockJSON
	mockJSONDec *mocks.MockJSONDecoder
	mockJSONEnc *mocks.MockJSONEncoder
	capturer    *fakeScreenshotCapturer
	mux         *http.ServeMux
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
	capturer := &fakeScreenshotCapturer{}

	// Mock HTTPServer Handler to return a ServeMux (needed for routes() in constructor)
	// Create a fresh ServeMux for each test to avoid route conflicts
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()

	h := New(ctx, mockWS, mockCmd, nil, capturer, mockServer, mockJSON, logger)

	return &testSetup{
		ctrl:        ctrl,
		ctx:         ctx,
		mockWS:      mockWS,
		mockCmd:     mockCmd,
		mockServer:  mockServer,
		mockJSON:    mockJSON,
		mockJSONDec: mockJSONDec,
		mockJSONEnc: mockJSONEnc,
		capturer:    capturer,
		mux:         mux,
		hub:         h,
		logger:      logger,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
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

	h := New(ctx, mockWS, mockCmd, nil, nil, mockServer, mockJSON, logger)
	assert.NotNil(t, h)
}

// TestMetricsRouteServesAllRegistries pins that /metrics merges every
// producer-owned registry: the playback counters (status) and the stage-0
// network gauges (netmetrics, docs/wan-outage-observability.md). A regression
// that drops one gatherer would silently blind the vmagent timeline.
func TestMetricsRouteServesAllRegistries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	mux := ts.hub.(*hub).server.Handler()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	assert.Contains(t, body, "playback_start_total", "status playback registry missing from /metrics")
	assert.Contains(t, body, "relayer_connected", "netmetrics registry missing from /metrics")
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
		New(ctx, mockWS, mockCmd, nil, nil, mockServer, mockJSON, logger)
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

func TestStart_ListenAndServeError_Retries(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Compresses the package-level backoff seam; do not add t.Parallel() to
	// hub tests while this mutation pattern is in use.
	oldBase := listenRetryBase
	listenRetryBase = time.Millisecond
	defer func() { listenRetryBase = oldBase }()

	// Mock WS Close - may be called due to context cancellation
	ts.mockWS.EXPECT().
		Close().
		MinTimes(1)

	// A transient bind failure must be retried, not abandoned: the hub is the
	// LAN recovery channel and a one-shot listener would stay dead until an
	// unrelated daemon restart. Two failures, then the server reports closed.
	served := make(chan struct{})
	bindErr := errors.New("listen tcp 0.0.0.0:1111: bind: address already in use")
	gomock.InOrder(
		ts.mockServer.EXPECT().
			ListenAndServe().
			Return(bindErr).
			Times(2),
		ts.mockServer.EXPECT().
			ListenAndServe().
			DoAndReturn(func() error {
				close(served)
				return http.ErrServerClosed
			}).
			Times(1),
	)

	ts.mockServer.EXPECT().
		Shutdown(gomock.Any()).
		Return(nil).
		MinTimes(1)

	// Start the hub
	ts.hub.Start()

	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("listener was not retried after bind failures")
	}

	// Stop the hub
	err := ts.hub.Stop()
	assert.NoError(t, err)
}

// TestUnmatchedRouteGoesThroughMiddleware: the mux's default 404 must NOT
// serve unmatched paths bare — they have to pass the same chokepoint as real
// routes (storm cap, logging, the future LAN-auth seam). Proven by showing an
// unmatched request is shed with 429 when every slot is held: a bypass would
// return 404 regardless of saturation.
func TestUnmatchedRouteGoesThroughMiddleware(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()

	h := New(context.Background(), mockWS, mockCmd, nil, nil, mockServer, mockJSON, logger)
	hh := h.(*hub)

	// Unsaturated: unmatched path 404s (served through the middleware).
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/definitely-not-a-route", nil))
	assert.Equal(t, http.StatusNotFound, rr.Code)

	// Saturated: every slot held -> the unmatched request must be shed by the
	// storm cap, not slip past it to a bare 404.
	for i := 0; i < MAX_INFLIGHT_REQUESTS; i++ {
		hh.reqSlots <- struct{}{}
	}
	defer func() {
		for i := 0; i < MAX_INFLIGHT_REQUESTS; i++ {
			<-hh.reqSlots
		}
	}()
	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/definitely-not-a-route", nil))
	assert.Equal(t, http.StatusTooManyRequests, rr.Code)
}

func TestStart_ListenRetryStopsOnContextCancel(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)
	mockServer.EXPECT().Handler().Return(http.NewServeMux()).AnyTimes()

	h := New(ctx, mockWS, mockCmd, nil, nil, mockServer, mockJSON, logger)

	// One failing attempt; the default 1s backoff leaves ample room to cancel
	// before a second attempt, and ctrl.Finish asserts exactly one call.
	attempted := make(chan struct{})
	mockServer.EXPECT().
		ListenAndServe().
		DoAndReturn(func() error {
			close(attempted)
			return errors.New("listen tcp 0.0.0.0:1111: bind: address already in use")
		}).
		Times(1)
	mockWS.EXPECT().Close().AnyTimes()
	mockServer.EXPECT().Shutdown(gomock.Any()).Return(nil).AnyTimes()

	h.Start()

	select {
	case <-attempted:
	case <-time.After(2 * time.Second):
		t.Fatal("listener was never attempted")
	}
	cancel()

	// Give the retry goroutine a moment to observe cancellation; ctrl.Finish
	// then asserts ListenAndServe was not called again.
	time.Sleep(50 * time.Millisecond)
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

	// Compresses the package-level backoff seam; do not add t.Parallel() to
	// hub tests while this mutation pattern is in use.
	oldBase := listenRetryBase
	listenRetryBase = time.Millisecond
	defer func() { listenRetryBase = oldBase }()

	// One transient error, then the retry loop observes the closed server and
	// exits. Bounding the sequence keeps the retry goroutine from outliving
	// the test (an unbounded Times(1) raced the retry's backoff before).
	expectedErr := errors.New("server error")
	gomock.InOrder(
		ts.mockServer.EXPECT().
			ListenAndServe().
			Return(expectedErr).
			Times(1),
		ts.mockServer.EXPECT().
			ListenAndServe().
			Return(http.ErrServerClosed).
			AnyTimes(),
	)

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
	h := New(ctx, ts.mockWS, ts.mockCmd, nil, nil, mockServer, wrapper.NewJSON(), logger)

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

func TestHandleScreenshot_SuccessWithCustomBounds(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.capturer.image = &screenshot.Image{
		Data:       []byte("png-data"),
		Width:      800,
		Height:     450,
		SHA256:     "abc123",
		CapturedAt: time.Date(2026, time.August, 18, 12, 0, 0, 0, time.UTC),
	}
	req := httptest.NewRequest(http.MethodGet, "/api/screenshot?width=800&height=800", nil)
	w := httptest.NewRecorder()

	ts.mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, screenshot.Bounds{Width: 800, Height: 800}, ts.capturer.bounds)
	assert.Equal(t, "image/png", w.Header().Get("Content-Type"))
	assert.Equal(t, "no-store", w.Header().Get("Cache-Control"))
	assert.Equal(t, "chromium-cdp", w.Header().Get("X-FF1-Capture-Source"))
	assert.Equal(t, "800", w.Header().Get("X-FF1-Screenshot-Width"))
	assert.Equal(t, "450", w.Header().Get("X-FF1-Screenshot-Height"))
	assert.Equal(t, "abc123", w.Header().Get("X-FF1-Capture-SHA256"))
	assert.Equal(t, "2026-08-18T12:00:00Z", w.Header().Get("X-FF1-Captured-At"))
	assert.Equal(t, "png-data", w.Body.String())
}

func TestHandleScreenshot_UsesNativeSizeWhenBoundsAreOmitted(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.capturer.image = &screenshot.Image{Data: []byte("png-data"), Width: 1920, Height: 1080}
	req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
	w := httptest.NewRecorder()

	ts.hub.(*hub).handleScreenshot(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, screenshot.Bounds{}, ts.capturer.bounds)
}

func TestHandleScreenshot_HoldsSlotUntilResponseWriteCompletes(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.capturer.image = &screenshot.Image{Data: []byte("png-data"), Width: 1920, Height: 1080}
	writeStarted := make(chan struct{})
	unblock := make(chan struct{})
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	firstWriter := &blockingScreenshotResponseWriter{
		header:       make(http.Header),
		writeStarted: writeStarted,
		unblock:      unblock,
	}
	firstDone := make(chan struct{})
	go func() {
		ts.hub.(*hub).handleScreenshot(
			firstWriter,
			httptest.NewRequest(http.MethodGet, "/api/screenshot", nil),
		)
		close(firstDone)
	}()

	select {
	case <-writeStarted:
	case <-time.After(time.Second):
		t.Fatal("first screenshot did not reach the response write")
	}

	secondWriter := httptest.NewRecorder()
	ts.hub.(*hub).handleScreenshot(
		secondWriter,
		httptest.NewRequest(http.MethodGet, "/api/screenshot", nil),
	)

	assert.Equal(t, http.StatusTooManyRequests, secondWriter.Code)
	assert.Equal(t, "1", secondWriter.Header().Get("Retry-After"))
	close(unblock)
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("first screenshot did not finish after the response write unblocked")
	}
	assert.Equal(t, http.StatusOK, firstWriter.status)
	assert.Equal(t, 1, ts.capturer.calls)
}

func TestHandleScreenshot_RejectsInvalidRequest(t *testing.T) {
	tests := []struct {
		name   string
		method string
		target string
		status int
	}{
		{name: "wrong method", method: http.MethodPost, target: "/api/screenshot", status: http.StatusMethodNotAllowed},
		{name: "empty width", method: http.MethodGet, target: "/api/screenshot?width=", status: http.StatusBadRequest},
		{name: "empty height", method: http.MethodGet, target: "/api/screenshot?height=", status: http.StatusBadRequest},
		{name: "non numeric width", method: http.MethodGet, target: "/api/screenshot?width=wide", status: http.StatusBadRequest},
		{name: "zero width", method: http.MethodGet, target: "/api/screenshot?width=0", status: http.StatusBadRequest},
		{name: "negative height", method: http.MethodGet, target: "/api/screenshot?height=-1", status: http.StatusBadRequest},
		{name: "dimension too large", method: http.MethodGet, target: "/api/screenshot?width=4097", status: http.StatusBadRequest},
		{name: "pixel budget too large", method: http.MethodGet, target: "/api/screenshot?width=4096&height=4096", status: http.StatusBadRequest},
		{name: "duplicate dimension", method: http.MethodGet, target: "/api/screenshot?width=800&width=900", status: http.StatusBadRequest},
		{name: "unknown query field", method: http.MethodGet, target: "/api/screenshot?size=800", status: http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			req := httptest.NewRequest(tt.method, tt.target, nil)
			w := httptest.NewRecorder()

			ts.hub.(*hub).handleScreenshot(w, req)

			assert.Equal(t, tt.status, w.Code)
			assert.Zero(t, ts.capturer.calls)
		})
	}
}

func TestHandleScreenshot_RejectsBrowserRequest(t *testing.T) {
	tests := []struct {
		name   string
		header string
		value  string
	}{
		{name: "origin", header: "Origin", value: "https://attacker.example"},
		{name: "fetch metadata", header: "Sec-Fetch-Site", value: "cross-site"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
			req.Header.Set(tt.header, tt.value)
			w := httptest.NewRecorder()

			ts.hub.(*hub).handleScreenshot(w, req)

			assert.Equal(t, http.StatusForbidden, w.Code)
			assert.Zero(t, ts.capturer.calls)
		})
	}
}

func TestHandleScreenshot_MapsCaptureErrors(t *testing.T) {
	tests := []struct {
		name       string
		captureErr error
		status     int
	}{
		{name: "busy", captureErr: screenshot.ErrBusy, status: http.StatusTooManyRequests},
		{name: "unavailable", captureErr: screenshot.ErrUnavailable, status: http.StatusServiceUnavailable},
		{name: "deadline", captureErr: context.DeadlineExceeded, status: http.StatusGatewayTimeout},
		{name: "capture failure", captureErr: errors.New("capture failed"), status: http.StatusBadGateway},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ts := setup(t)
			defer ts.teardown()

			ts.capturer.err = tt.captureErr
			req := httptest.NewRequest(http.MethodGet, "/api/screenshot", nil)
			w := httptest.NewRecorder()

			ts.hub.(*hub).handleScreenshot(w, req)

			assert.Equal(t, tt.status, w.Code)
		})
	}
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

// countingHandler is a real inner Handler that counts how many commands reach
// it, used to prove the storm gate sheds excess before they hit the device.
type countingHandler struct {
	calls atomic.Int64
}

func (c *countingHandler) Process(_ context.Context, _ commands.Command) (interface{}, error) {
	c.calls.Add(1)
	return nil, nil
}

// TestMiddleware_RejectsWhenAtCapacity verifies the shared hub middleware bounds
// concurrent in-flight requests across all routes: once the in-flight budget is
// exhausted, further requests are rejected with 429 before decoding or reaching
// the handler, so a storm cannot pile up unbounded HTTP goroutines
// (feral-file/ffos-user#208).
func TestMiddleware_RejectsWhenAtCapacity(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	hubImpl := ts.hub.(*hub)

	// Saturate the shared in-flight budget; nothing releases these in the test.
	for i := 0; i < MAX_INFLIGHT_REQUESTS; i++ {
		hubImpl.reqSlots <- struct{}{}
	}

	req := httptest.NewRequest(
		"POST",
		"/api/cast",
		strings.NewReader(`{"command":"displayPlaylist"}`),
	)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	// No mockCmd.Process / mockJSON.NewDecoder expectations: the capacity check
	// in the middleware runs before either, so a satisfied gomock controller
	// asserts they are never reached. The cast handler is reached only through
	// the shared middleware, so exercise the wrapped handler here.
	hubImpl.withMiddleware("cast", hubImpl.handleCast)(w, req)

	assert.Equal(t, http.StatusTooManyRequests, w.Code)
}

// stubStatusProvider is a fixed StatusProvider for status-endpoint tests.
type stubStatusProvider struct{ info StatusInfo }

func (s stubStatusProvider) Status(context.Context) StatusInfo { return s.info }

// TestHandleStatus_ReturnsContractAndFields verifies GET /api/status, served
// through the shared middleware, returns the provider's device fields plus the
// hub-owned LAN contract version.
func TestHandleStatus_ReturnsContractAndFields(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockServer.EXPECT().Handler().Return(http.NewServeMux()).AnyTimes()

	provider := stubStatusProvider{info: StatusInfo{
		DeviceID:     "ff1-abc",
		Version:      "1.2.3",
		Branch:       "develop",
		Claimed:      false,
		Internet:     true,
		SetupState:   "unclaimed",
		Connectivity: "connected",
		TopicID:      "topic-xyz",
	}}
	h := New(ctx, mockWS, mockCmd, provider, nil, mockServer, wrapper.NewJSON(), logger).(*hub)

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	h.withMiddleware("status", h.handleStatus)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "application/json", w.Header().Get("Content-Type"))

	var got map[string]any
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, "ff1-abc", got["device_id"])
	assert.Equal(t, "1.2.3", got["version"])
	assert.Equal(t, "develop", got["branch"],
		"branch is the claim-QR parity field; the LAN payload must carry it")
	assert.Equal(t, StatusContract, got["contract"])
	assert.Equal(t, false, got["claimed"])
	assert.Equal(t, true, got["internet"],
		"internet (reachability) is distinct from connectivity (LAN link)")
	assert.Equal(t, "unclaimed", got["setup_state"])
	assert.Equal(t, "connected", got["connectivity"])
	assert.Equal(t, "topic-xyz", got["topic_id"],
		"an UNCLAIMED device serves its topic_id — that is the LAN claim handover")
}

// TestHandleCast_OversizedBodyRejected413: the storm cap bounds concurrency,
// not allocations — the middleware body cap must stop an oversized cast
// inside the decoder with 413 (not 400: the caller sent too much, not
// garbage), before anything reaches the command router. The command handler
// mock carries no expectations, so any call through to it fails the test.
func TestHandleCast_OversizedBodyRejected413(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))

	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()

	New(context.Background(), mockWS, mockCmd, nil, nil, mockServer, wrapper.NewJSON(), logger)

	body := strings.NewReader(`{"command":"` + strings.Repeat("a", MAX_REQUEST_BODY_BYTES+1024) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/api/cast", body)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, req)

	assert.Equal(t, http.StatusRequestEntityTooLarge, w.Code)
}

// TestStatusContractV2MatchesMDNSTXTVersion enforces the hub<->mdns version
// sync that production code keeps by convention (mdns must stay hub-free, so
// no import ties the constants together). The pairing app's gate is two-part —
// TXT api=<v> at discovery, contract <v> on /api/v2/status — and bumping one
// side without the other would silently desync it; this test makes that a
// build failure instead.
func TestStatusContractV2MatchesMDNSTXTVersion(t *testing.T) {
	assert.Equal(t, StatusContractV2, mdns.APITXTVersion,
		"hub.StatusContractV2 and mdns.APITXTVersion must be bumped together")
}

// TestHandleStatusV2_SamePayloadContract2 pins the v2 pairing surface: same
// fields as /api/status but contract "2". The versioned route is the firmware
// gate the app keys on — old firmware 404s here — so v2 must never silently
// diverge from the provider-backed payload, and v1 must keep reporting "1".
func TestHandleStatusV2_SamePayloadContract2(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mux := http.NewServeMux()
	mockServer.EXPECT().Handler().Return(mux).AnyTimes()

	provider := stubStatusProvider{info: StatusInfo{
		DeviceID:     "ff1-abc",
		Version:      "1.2.3",
		Branch:       "develop",
		Claimed:      false,
		Internet:     true,
		SetupState:   "unclaimed",
		Connectivity: "connected",
		TopicID:      "topic-xyz",
	}}
	New(ctx, mockWS, mockCmd, provider, nil, mockServer, wrapper.NewJSON(), logger)

	// Through the real mux, so the route registration itself is covered.
	get := func(path string) map[string]any {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		require.Equal(t, http.StatusOK, w.Code, path)
		var got map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got), path)
		return got
	}

	v2 := get("/api/v2/status")
	assert.Equal(t, StatusContractV2, v2["contract"])
	assert.Equal(t, "ff1-abc", v2["device_id"])
	assert.Equal(t, "develop", v2["branch"])
	assert.Equal(t, "topic-xyz", v2["topic_id"])
	assert.Equal(t, true, v2["internet"])

	v1 := get("/api/status")
	assert.Equal(t, StatusContract, v1["contract"], "legacy route keeps contract 1")
	delete(v1, "contract")
	delete(v2, "contract")
	assert.Equal(t, v1, v2, "v1 and v2 must serve the identical provider payload")
}

// TestHandleStatus_ClaimedDeviceStillServesTopicID pins the multi-controller
// contract: FF1 is controlled by several phones, and a frame whose original
// phone is lost/wiped must stay pairable from any LAN peer, so a CLAIMED
// device keeps serving its topic_id (LAN-presence = authorization, matching
// the BLE-era posture). The claimed flag stays visible so the app can
// suppress the unprompted app-open claim offer — manual pairing only.
func TestHandleStatus_ClaimedDeviceStillServesTopicID(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockWS := mocks.NewMockWS(ctrl)
	mockCmd := mocks.NewMockCommandHandler(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockServer.EXPECT().Handler().Return(http.NewServeMux()).AnyTimes()

	provider := stubStatusProvider{info: StatusInfo{
		DeviceID: "ff1-abc",
		Claimed:  true,
		TopicID:  "topic-xyz",
	}}
	h := New(ctx, mockWS, mockCmd, provider, nil, mockServer, wrapper.NewJSON(), logger).(*hub)

	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()
	h.withMiddleware("status", h.handleStatus)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var got map[string]any
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, true, got["claimed"])
	assert.Equal(t, "topic-xyz", got["topic_id"],
		"a claimed device keeps serving its topic — multi-phone pairing "+
			"and lost-phone recovery depend on it")
}

// TestHandleStatus_NilProviderReturnsContract verifies a nil provider still
// yields a valid response carrying the contract (forward-compat detection must
// work even before a provider is wired).
func TestHandleStatus_NilProviderReturnsContract(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	hubImpl := ts.hub.(*hub) // setup() wires a nil status provider
	req := httptest.NewRequest("GET", "/api/status", nil)
	w := httptest.NewRecorder()

	// setup()'s hub uses a mocked JSON encoder, so drive a real one here.
	hubImpl.json = wrapper.NewJSON()
	hubImpl.withMiddleware("status", hubImpl.handleStatus)(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var got map[string]any
	assert.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	assert.Equal(t, StatusContract, got["contract"])
}

// TestHandleStatus_InvalidMethod verifies non-GET is rejected.
func TestHandleStatus_InvalidMethod(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	hubImpl := ts.hub.(*hub)
	req := httptest.NewRequest("POST", "/api/status", nil)
	w := httptest.NewRecorder()
	hubImpl.handleStatus(w, req)

	assert.Equal(t, http.StatusMethodNotAllowed, w.Code)
}

// TestMiddleware_EnvelopeRoundTrip proves a normal cast envelope flows through
// the shared middleware (in-flight limiter + logging active) and the storm gate
// to the inner handler and back, unharmed.
func TestMiddleware_EnvelopeRoundTrip(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockWS := mocks.NewMockWS(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockServer.EXPECT().Handler().Return(http.NewServeMux()).AnyTimes()

	stub := &countingHandler{}
	gated := commandrouter.NewGate(stub, commandrouter.GateConfig{
		Enabled:       true,
		MaxConcurrent: 16,
		Default:       commandrouter.Policy{Rate: 5, Burst: 5, Weight: 1},
	}, logger)
	h := New(ctx, mockWS, gated, nil, nil, mockServer, wrapper.NewJSON(), logger).(*hub)

	body := `{"command":"roundtrip","request":{"k":"v"}}`
	req := httptest.NewRequest("POST", "/api/cast", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.withMiddleware("cast", h.handleCast)(w, req)

	// countingHandler returns (nil, nil) -> 204 No Content, proving the envelope
	// decoded and reached the inner handler through the wrapped path.
	assert.Equal(t, http.StatusNoContent, w.Code)
	assert.Equal(t, int64(1), stub.calls.Load())
}

// TestHandleCast_StormProtection drives a burst of cast requests through the LAN
// hub ingress wrapped with a real command-storm gate. Beyond the configured
// burst the hub must answer 429 and the inner handler must never see the
// excess (feral-file/ffos-user#208).
func TestHandleCast_StormProtection(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockWS := mocks.NewMockWS(ctrl)
	mockServer := mocks.NewMockHTTPServer(ctrl)
	mockServer.EXPECT().Handler().Return(http.NewServeMux()).AnyTimes()

	stub := &countingHandler{}
	gateCfg := commandrouter.GateConfig{
		Enabled:       true,
		MaxConcurrent: 16,
		// Unlisted command type falls to Default: a tight burst of 2.
		Default: commandrouter.Policy{Rate: 1, Burst: 2, Weight: 1},
	}
	gated := commandrouter.NewGate(stub, gateCfg, logger)

	h := New(ctx, mockWS, gated, nil, nil, mockServer, wrapper.NewJSON(), logger).(*hub)

	const total = 5
	var accepted, limited int
	for i := 0; i < total; i++ {
		body := fmt.Sprintf(`{"command":"stormtest","request":{"i":%d}}`, i)
		req := httptest.NewRequest("POST", "/api/cast", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		h.handleCast(w, req)

		if w.Code == http.StatusTooManyRequests {
			limited++
		} else {
			accepted++
		}
	}

	assert.Equal(t, 2, accepted, "only the burst of 2 is accepted")
	assert.Equal(t, total-2, limited, "the rest are rejected with 429")
	assert.Equal(t, int64(2), stub.calls.Load(), "shed commands never reach the inner handler")
}
