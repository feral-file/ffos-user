package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/zap"
)

func TestVmagentClient_SendMetric(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	tests := []struct {
		name           string
		metric         string
		serverResponse int
		wantErr        bool
	}{
		{
			name:           "successful metric send",
			metric:         "ff_crash_reboot{reason=\"test\"} 1",
			serverResponse: http.StatusNoContent,
			wantErr:        false,
		},
		{
			name:           "server error",
			metric:         "ff_crash_reboot{reason=\"test\"} 1",
			serverResponse: http.StatusInternalServerError,
			wantErr:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// GET requests are for reachability check, POST requests are for sending metrics
				if r.Method == http.MethodGet {
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.Method != http.MethodPost {
					t.Errorf("Expected POST request for metric send, got %s", r.Method)
				}
				w.WriteHeader(tt.serverResponse)
			}))
			defer server.Close()

			client := NewVmagentClient(server.URL, logger)
			ctx := context.Background()

			err := client.SendMetric(ctx, tt.metric)

			if (err != nil) != tt.wantErr {
				t.Errorf("SendMetric() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestVmagentClient_SendMetric_Timeout(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	// Create a slow server that delays response
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(10 * time.Second) // Longer than VMAGENT_REQUEST_TIMEOUT
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := NewVmagentClient(server.URL, logger)
	ctx := context.Background()

	err := client.SendMetric(ctx, "ff_crash_reboot{reason=\"test\"} 1")

	if err == nil {
		t.Error("Expected timeout error, got nil")
	}
}

func TestVmagentClient_SendMetric_Unreachable(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	// Use a URL that doesn't exist
	client := NewVmagentClient("http://localhost:19999", logger)
	ctx := context.Background()

	err := client.SendMetric(ctx, "ff_crash_reboot{reason=\"test\"} 1")

	if err == nil {
		t.Error("Expected error for unreachable server, got nil")
	}
}

func TestVmagentClient_SendCrashRebootMetric(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	tests := []struct {
		name   string
		reason string
	}{
		{
			name:   "chromium crash",
			reason: "chromium_crash",
		},
		{
			name:   "gpu hang",
			reason: "gpu_hang",
		},
		{
			name:   "disk full",
			reason: "disk_full",
		},
		{
			name:   "ram critical",
			reason: "ram_critical",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Create a test server
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusNoContent)
			}))
			defer server.Close()

			client := NewVmagentClient(server.URL, logger)
			ctx := context.Background()

			// This should not panic or hang
			client.SendCrashRebootMetric(ctx, tt.reason)
		})
	}
}

func TestVmagentClient_DefaultURL(t *testing.T) {
	logger, _ := zap.NewDevelopment()

	client := NewVmagentClient("", logger)

	if client.url != VMAGENT_DEFAULT_URL {
		t.Errorf("Expected default URL %s, got %s", VMAGENT_DEFAULT_URL, client.url)
	}
}

func TestVmagentClient_CustomURL(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	customURL := "http://custom:8080/api/v1/import/prometheus"

	client := NewVmagentClient(customURL, logger)

	if client.url != customURL {
		t.Errorf("Expected custom URL %s, got %s", customURL, client.url)
	}
}
