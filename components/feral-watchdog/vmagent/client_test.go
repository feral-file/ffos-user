package vmagent_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/golang/mock/gomock"
	"github.com/stretchr/testify/assert"
	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/mocks"
	"github.com/feral-file/ffos-user/components/feral-watchdog/vmagent"
)

func TestClient_SendCrashRebootMetric_Success(t *testing.T) {
	tests := []struct {
		name           string
		reason         vmagent.CrashReason
		expectedMetric string
	}{
		{
			name:           "chromium crash",
			reason:         vmagent.CrashReasonChromiumCrash,
			expectedMetric: "ff_crash_reboot{reason=\"chromium_crash\"} 1",
		},
		{
			name:           "GPU hang",
			reason:         vmagent.CrashReasonGPUHang,
			expectedMetric: "ff_crash_reboot{reason=\"gpu_hang\"} 1",
		},
		{
			name:           "disk full",
			reason:         vmagent.CrashReasonDiskFull,
			expectedMetric: "ff_crash_reboot{reason=\"disk_full\"} 1",
		},
		{
			name:           "RAM critical",
			reason:         vmagent.CrashReasonRamCritical,
			expectedMetric: "ff_crash_reboot{reason=\"ram_critical\"} 1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockHTTP := mocks.NewMockHTTPClient(ctrl)
			mockIO := mocks.NewMockIO(ctrl)
			logger := zap.NewNop()

			// Create a mock response
			mockResp := &http.Response{
				StatusCode: 200,
				Body:       io.NopCloser(strings.NewReader("OK")),
			}

			// Expect PostWithContext to be called with the correct parameters
			mockHTTP.EXPECT().
				PostWithContext(
					gomock.Any(),
					vmagent.VMAGENT_DEFAULT_URL,
					"text/plain",
					gomock.Any(),
				).
				Return(mockResp, nil).
				Times(1)

			client := vmagent.NewClient("", logger, mockHTTP, mockIO)
			ctx := context.Background()

			// Should not panic or error
			err := client.SendCrashRebootMetric(ctx, tt.reason)
			assert.NoError(t, err)
		})
	}
}

func TestClient_SendCrashRebootMetric_HTTPError(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	logger := zap.NewNop()

	// Simulate HTTP error
	expectedError := errors.New("connection refused")
	mockHTTP.EXPECT().
		PostWithContext(
			gomock.Any(),
			vmagent.VMAGENT_DEFAULT_URL,
			"text/plain",
			gomock.Any(),
		).
		Return(nil, expectedError).
		Times(1)

	client := vmagent.NewClient("", logger, mockHTTP, mockIO)
	ctx := context.Background()

	// Should handle error gracefully (logs error but doesn't panic)
	err := client.SendCrashRebootMetric(ctx, vmagent.CrashReasonChromiumCrash)
	assert.Error(t, err)
	assert.ErrorContains(t, err, expectedError.Error())
}

func TestClient_SendCrashRebootMetric_NonSuccessStatusCode(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{
			name:       "400 Bad Request",
			statusCode: 400,
		},
		{
			name:       "500 Internal Server Error",
			statusCode: 500,
		},
		{
			name:       "503 Service Unavailable",
			statusCode: 503,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockHTTP := mocks.NewMockHTTPClient(ctrl)
			mockIO := mocks.NewMockIO(ctrl)
			logger := zap.NewNop()

			// Create a mock response with non-success status code
			mockResp := &http.Response{
				StatusCode: tt.statusCode,
				Body:       io.NopCloser(strings.NewReader("Error")),
			}

			mockHTTP.EXPECT().
				PostWithContext(
					gomock.Any(),
					vmagent.VMAGENT_DEFAULT_URL,
					"text/plain",
					gomock.Any(),
				).
				Return(mockResp, nil).
				Times(1)

			client := vmagent.NewClient("", logger, mockHTTP, mockIO)
			ctx := context.Background()

			// Should handle error gracefully (logs error but doesn't panic)
			err := client.SendCrashRebootMetric(ctx, vmagent.CrashReasonGPUHang)
			assert.Error(t, err)
			assert.ErrorContains(t, err, fmt.Sprintf("unexpected status code: %d for metric: ff_crash_reboot{reason=\"gpu_hang\"} 1", tt.statusCode))
		})
	}
}

func TestClient_SendCrashRebootMetric_ContextCancellation(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	logger := zap.NewNop()

	// Create a canceled context
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	// Expect the HTTP call to be made with canceled context
	mockHTTP.EXPECT().
		PostWithContext(
			gomock.Any(),
			vmagent.VMAGENT_DEFAULT_URL,
			"text/plain",
			gomock.Any(),
		).
		Return(nil, context.Canceled).
		Times(1)

	client := vmagent.NewClient("", logger, mockHTTP, mockIO)

	// Should handle context cancellation gracefully
	err := client.SendCrashRebootMetric(ctx, vmagent.CrashReasonDiskFull)
	assert.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
}

func TestClient_SendCrashRebootMetric_CustomURL(t *testing.T) {
	ctrl := gomock.NewController(t)
	defer ctrl.Finish()

	mockHTTP := mocks.NewMockHTTPClient(ctrl)
	mockIO := mocks.NewMockIO(ctrl)
	logger := zap.NewNop()

	customURL := "http://custom-vmagent:9999/metrics"

	mockResp := &http.Response{
		StatusCode: 200,
		Body:       io.NopCloser(strings.NewReader("OK")),
	}

	// Expect the custom URL to be used
	mockHTTP.EXPECT().
		PostWithContext(
			gomock.Any(),
			customURL,
			"text/plain",
			gomock.Any(),
		).
		Return(mockResp, nil).
		Times(1)

	client := vmagent.NewClient(customURL, logger, mockHTTP, mockIO)
	ctx := context.Background()

	err := client.SendCrashRebootMetric(ctx, vmagent.CrashReasonRamCritical)
	assert.NoError(t, err)
}

func TestClient_SendCrashRebootMetric_SuccessStatusCodes(t *testing.T) {
	tests := []struct {
		name       string
		statusCode int
	}{
		{
			name:       "200 OK",
			statusCode: 200,
		},
		{
			name:       "201 Created",
			statusCode: 201,
		},
		{
			name:       "204 No Content",
			statusCode: 204,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctrl := gomock.NewController(t)
			defer ctrl.Finish()

			mockHTTP := mocks.NewMockHTTPClient(ctrl)
			mockIO := mocks.NewMockIO(ctrl)
			logger := zap.NewNop()

			mockResp := &http.Response{
				StatusCode: tt.statusCode,
				Body:       io.NopCloser(strings.NewReader("OK")),
			}

			mockHTTP.EXPECT().
				PostWithContext(
					gomock.Any(),
					vmagent.VMAGENT_DEFAULT_URL,
					"text/plain",
					gomock.Any(),
				).
				Return(mockResp, nil).
				Times(1)

			client := vmagent.NewClient("", logger, mockHTTP, mockIO)
			ctx := context.Background()

			// Should succeed for all 2xx status codes
			err := client.SendCrashRebootMetric(ctx, vmagent.CrashReasonChromiumCrash)
			assert.NoError(t, err)
		})
	}
}
