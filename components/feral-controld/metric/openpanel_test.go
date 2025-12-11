package metric_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	dp1playlist "github.com/display-protocol/dp1-validator/playlist"

	constants "github.com/feral-file/ffos-user/components/feral-controld/constant"
	"github.com/feral-file/ffos-user/components/feral-controld/dp1"
	"github.com/feral-file/ffos-user/components/feral-controld/metric"
	"github.com/feral-file/ffos-user/components/feral-controld/mocks"
)

type testSetup struct {
	ctrl           *gomock.Controller
	ctx            context.Context
	mockOS         *mocks.MockOS
	mockHTTPClient *mocks.MockHTTPClient
	mockJSON       *mocks.MockJSON
	logger         *zap.Logger
	config         *metric.OpenPanelConfig
	tracker        metric.Tracker
}

func setup(t *testing.T) *testSetup {
	ctrl := gomock.NewController(t)
	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockOS := mocks.NewMockOS(ctrl)
	mockHTTPClient := mocks.NewMockHTTPClient(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)

	config := &metric.OpenPanelConfig{
		ClientID:     "test-client-id",
		ClientSecret: "test-client-secret",
	}

	tracker := metric.NewOpenPanelTracker(config, mockOS, mockHTTPClient, mockJSON, logger)

	return &testSetup{
		ctrl:           ctrl,
		ctx:            ctx,
		mockOS:         mockOS,
		mockHTTPClient: mockHTTPClient,
		mockJSON:       mockJSON,
		logger:         logger,
		config:         config,
		tracker:        tracker,
	}
}

func (ts *testSetup) teardown() {
	ts.ctrl.Finish()
}

// Helper function to create a mock HTTP response
func createMockResponse(statusCode int, body string) *http.Response {
	return &http.Response{
		StatusCode: statusCode,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
	}
}

// Helper function
func stringPtr(s string) *string {
	return &s
}

// Test NewOpenPanelTracker
func TestNewOpenPanelTracker(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	assert.NotNil(t, ts.tracker)
}

// Test OpenPanelConfig.IsEnabled
func TestOpenPanelConfig_IsEnabled(t *testing.T) {
	tests := []struct {
		name     string
		config   *metric.OpenPanelConfig
		expected bool
	}{
		{
			name: "enabled with valid credentials",
			config: &metric.OpenPanelConfig{
				ClientID:     "test-id",
				ClientSecret: "test-secret",
			},
			expected: true,
		},
		{
			name: "disabled with empty client ID",
			config: &metric.OpenPanelConfig{
				ClientID:     "",
				ClientSecret: "test-secret",
			},
			expected: false,
		},
		{
			name: "disabled with empty client secret",
			config: &metric.OpenPanelConfig{
				ClientID:     "test-id",
				ClientSecret: "",
			},
			expected: false,
		},
		{
			name:     "disabled with nil config",
			config:   nil,
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := tt.config.IsEnabled()
			assert.Equal(t, tt.expected, result)
		})
	}
}

// Test Initialize
func TestOpenPanelTracker_Initialize_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	hostnameData := []byte("ff1-00023\n")
	configData := []byte(`{"version": "0.8.1"}`)

	ts.mockOS.EXPECT().
		ReadFile(constants.HOSTNAME_FILE).
		Return(hostnameData, nil)

	ts.mockOS.EXPECT().
		ReadFile(constants.FF1_CONFIG_FILE).
		Return(configData, nil)

	ts.mockJSON.EXPECT().
		Unmarshal(configData, gomock.Any()).
		Return(nil)

	err := ts.tracker.Initialize()
	assert.NoError(t, err)
}

func TestOpenPanelTracker_Initialize_HostnameReadFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	ts.mockOS.EXPECT().
		ReadFile("/etc/hostname").
		Return(nil, errors.New("file not found"))

	err := ts.tracker.Initialize()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read device ID")
}

func TestOpenPanelTracker_Initialize_ConfigReadFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	hostnameData := []byte("ff1-00023\n")

	ts.mockOS.EXPECT().
		ReadFile(constants.HOSTNAME_FILE).
		Return(hostnameData, nil)

	ts.mockOS.EXPECT().
		ReadFile(constants.FF1_CONFIG_FILE).
		Return(nil, errors.New("config not found"))

	err := ts.tracker.Initialize()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to read ff1-config.json")
}

func TestOpenPanelTracker_Initialize_ConfigParseFailure(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	hostnameData := []byte("ff1-00023\n")
	configData := []byte(`{"version": "0.8.1"}`)

	ts.mockOS.EXPECT().
		ReadFile(constants.HOSTNAME_FILE).
		Return(hostnameData, nil)

	ts.mockOS.EXPECT().
		ReadFile(constants.FF1_CONFIG_FILE).
		Return(configData, nil)

	ts.mockJSON.EXPECT().
		Unmarshal(configData, gomock.Any()).
		Return(errors.New("invalid JSON"))

	err := ts.tracker.Initialize()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to parse ff1-config.json")
}

// Test TrackPlaylistView
func TestOpenPanelTracker_TrackPlaylistView_Success(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Initialize first
	hostnameData := []byte("ff1-00023\n")
	configData := []byte(`{"version": "0.8.1"}`)

	ts.mockOS.EXPECT().
		ReadFile(constants.HOSTNAME_FILE).
		Return(hostnameData, nil)

	ts.mockOS.EXPECT().
		ReadFile(constants.FF1_CONFIG_FILE).
		Return(configData, nil)

	ts.mockJSON.EXPECT().
		Unmarshal(configData, gomock.Any()).
		Return(nil)

	err := ts.tracker.Initialize()
	assert.NoError(t, err)

	// Create test playlist
	playlist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID: "ff-pl-1234",
			Items: []dp1playlist.PlaylistItem{
				{
					ID:       "item1",
					Title:    stringPtr("Test Item"),
					Source:   "https://example.com/video.mp4",
					Duration: 300,
					License:  "open",
				},
			},
		},
	}

	playlistURL := "https://feed.feralfile.com/api/v1/playlists/ff-pl-1234"

	// Expect JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			return []byte(`{"type":"track","payload":{"name":"playlist_view"}}`), nil
		})

	// Expect HTTP request creation and execution (async, so might not be called immediately)
	ts.mockHTTPClient.EXPECT().
		NewRequest("POST", metric.OPENPANEL_API_URL, gomock.Any()).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			req, _ := http.NewRequest(method, url, body)
			return req, nil
		}).
		AnyTimes()

	ts.mockHTTPClient.EXPECT().
		Do(gomock.Any()).
		Return(createMockResponse(200, `{"ok":true}`), nil).
		AnyTimes()

	err = ts.tracker.TrackPlaylistView(ts.ctx, playlist, playlistURL)
	assert.NoError(t, err)
}

func TestOpenPanelTracker_TrackPlaylistView_DisabledConfig(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	logger := zaptest.NewLogger(t, zaptest.Level(zap.FatalLevel))
	ctx := context.Background()

	mockOS := mocks.NewMockOS(ctrl)
	mockHTTPClient := mocks.NewMockHTTPClient(ctrl)
	mockJSON := mocks.NewMockJSON(ctrl)

	// Create disabled config
	config := &metric.OpenPanelConfig{
		ClientID:     "", // Empty means disabled
		ClientSecret: "",
	}

	tracker := metric.NewOpenPanelTracker(config, mockOS, mockHTTPClient, mockJSON, logger)

	playlist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID: "ff-pl-1234",
		},
	}

	// Should return without making any calls
	err := tracker.TrackPlaylistView(ctx, playlist, "https://example.com/playlist")
	assert.NoError(t, err)
}

func TestOpenPanelTracker_TrackPlaylistView_NilPlaylist(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Initialize first
	hostnameData := []byte("ff1-00023\n")
	configData := []byte(`{"version": "0.8.1"}`)

	ts.mockOS.EXPECT().
		ReadFile(constants.HOSTNAME_FILE).
		Return(hostnameData, nil)

	ts.mockOS.EXPECT().
		ReadFile(constants.FF1_CONFIG_FILE).
		Return(configData, nil)

	ts.mockJSON.EXPECT().
		Unmarshal(configData, gomock.Any()).
		Return(nil)

	err := ts.tracker.Initialize()
	assert.NoError(t, err)

	err = ts.tracker.TrackPlaylistView(ts.ctx, nil, "")
	assert.NoError(t, err)
}

func TestOpenPanelTracker_TrackPlaylistView_EmptyURL(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Initialize first
	hostnameData := []byte("ff1-00023\n")
	configData := []byte(`{"version": "0.8.1"}`)

	ts.mockOS.EXPECT().
		ReadFile(constants.HOSTNAME_FILE).
		Return(hostnameData, nil)

	ts.mockOS.EXPECT().
		ReadFile(constants.FF1_CONFIG_FILE).
		Return(configData, nil)

	ts.mockJSON.EXPECT().
		Unmarshal(configData, gomock.Any()).
		Return(nil)

	err := ts.tracker.Initialize()
	assert.NoError(t, err)

	playlist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID: "ff-pl-1234",
		},
	}

	// Expect JSON marshaling
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		DoAndReturn(func(v interface{}) ([]byte, error) {
			return []byte(`{"type":"track","payload":{"name":"playlist_view"}}`), nil
		})

	// Expect HTTP request (async)
	ts.mockHTTPClient.EXPECT().
		NewRequest("POST", metric.OPENPANEL_API_URL, gomock.Any()).
		DoAndReturn(func(method, url string, body io.Reader) (*http.Request, error) {
			req, _ := http.NewRequest(method, url, body)
			return req, nil
		}).
		AnyTimes()

	ts.mockHTTPClient.EXPECT().
		Do(gomock.Any()).
		Return(createMockResponse(200, `{"ok":true}`), nil).
		AnyTimes()

	// Empty URL should work fine (no host will be extracted)
	err = ts.tracker.TrackPlaylistView(ts.ctx, playlist, "")
	assert.NoError(t, err)
}

func TestOpenPanelTracker_TrackPlaylistView_MarshalError(t *testing.T) {
	ts := setup(t)
	defer ts.teardown()

	// Initialize first
	hostnameData := []byte("ff1-00023\n")
	configData := []byte(`{"version": "0.8.1"}`)

	ts.mockOS.EXPECT().
		ReadFile(constants.HOSTNAME_FILE).
		Return(hostnameData, nil)

	ts.mockOS.EXPECT().
		ReadFile(constants.FF1_CONFIG_FILE).
		Return(configData, nil)

	ts.mockJSON.EXPECT().
		Unmarshal(configData, gomock.Any()).
		Return(nil)

	err := ts.tracker.Initialize()
	assert.NoError(t, err)

	playlist := &dp1.Playlist{
		Playlist: dp1playlist.Playlist{
			ID: "ff-pl-1234",
		},
	}

	// Expect JSON marshaling to fail
	ts.mockJSON.EXPECT().
		Marshal(gomock.Any()).
		Return(nil, errors.New("marshal error"))

	err = ts.tracker.TrackPlaylistView(ts.ctx, playlist, "https://example.com")
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "failed to marshal payload")
}
