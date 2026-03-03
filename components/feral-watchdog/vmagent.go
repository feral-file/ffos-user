package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	"go.uber.org/zap"
)

const (
	VMAGENT_REQUEST_TIMEOUT = 5 * time.Second
	VMAGENT_DEFAULT_URL     = "http://0.0.0.0:9431/api/v1/import/prometheus"
)

// VmagentClient handles sending metrics to vmagent
type VmagentClient struct {
	client *http.Client
	logger *zap.Logger
	url    string
}

type CrashReason string

const (
	CrashReasonChromiumCrash CrashReason = "chromium_crash"
	CrashReasonGPUHang       CrashReason = "gpu_hang"
	CrashReasonDiskFull      CrashReason = "disk_full"
	CrashReasonRamCritical   CrashReason = "ram_critical"
)

// NewVmagentClient creates a new vmagent client instance
func NewVmagentClient(url string, logger *zap.Logger) *VmagentClient {
	if url == "" {
		url = VMAGENT_DEFAULT_URL
	}

	return &VmagentClient{
		client: &http.Client{
			Timeout: VMAGENT_REQUEST_TIMEOUT,
		},
		logger: logger,
		url:    url,
	}
}

// SendMetric sends a metric to vmagent
// metric should be in Prometheus format, e.g.: metric_name{label="value"} 1
func (v *VmagentClient) SendMetric(ctx context.Context, metric string) error {
	// Create context with timeout
	timeoutCtx, cancel := context.WithTimeout(ctx, VMAGENT_REQUEST_TIMEOUT)
	defer cancel()

	// Check if vmagent is reachable first
	if !v.isReachable(timeoutCtx) {
		return fmt.Errorf("vmagent not reachable at %s", v.url)
	}

	// Send the metric
	req, err := http.NewRequestWithContext(timeoutCtx, http.MethodPost, v.url, bytes.NewBufferString(metric))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send metric: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Read and discard response body
	_, _ = io.Copy(io.Discard, resp.Body)

	// Check if the request was successful (204 No Content is the expected response)
	if resp.StatusCode != http.StatusNoContent {
		return fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	return nil
}

// isReachable checks if vmagent is reachable
func (v *VmagentClient) isReachable(ctx context.Context) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, v.url, nil)
	if err != nil {
		return false
	}

	resp, err := v.client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	// Read and discard response body
	_, _ = io.Copy(io.Discard, resp.Body)

	return true
}

// SendCrashRebootMetric sends the crash_reboot metric to vmagent
func (v *VmagentClient) SendCrashRebootMetric(ctx context.Context, reason CrashReason) {
	metric := fmt.Sprintf("ff_crash_reboot{reason=\"%s\"} 1", reason)

	v.logger.Info("Sending crash_reboot metric to vmagent",
		zap.String("metric", metric),
		zap.String("url", v.url))

	if err := v.SendMetric(ctx, metric); err != nil {
		v.logger.Error("Failed to send crash_reboot metric to vmagent",
			zap.Error(err),
			zap.String("url", v.url))
	} else {
		v.logger.Info("Successfully sent crash_reboot metric to vmagent",
			zap.String("url", v.url))
	}
}

// SendServiceFailedMetric sends the service_failed metric to vmagent
func (v *VmagentClient) SendServiceFailedMetric(ctx context.Context, service string) {
	metric := fmt.Sprintf("ff_service_failed{service=\"%s\"} 1", service)

	v.logger.Info("Sending service failed metric to vmagent",
		zap.String("metric", metric),
		zap.String("url", v.url))

	if err := v.SendMetric(ctx, metric); err != nil {
		v.logger.Error("Failed to send service failed metric to vmagent",
			zap.Error(err),
			zap.String("url", v.url))
	} else {
		v.logger.Info("Successfully sent service failed metric to vmagent",
			zap.String("url", v.url))
	}
}

// SendServiceFailedIncidentMetric sends an incident-level service failed metric to vmagent.
func (v *VmagentClient) SendServiceFailedIncidentMetric(ctx context.Context) {
	metric := "service_failed_incident 1"

	v.logger.Info("Sending service failed incident metric to vmagent",
		zap.String("metric", metric),
		zap.String("url", v.url))

	if err := v.SendMetric(ctx, metric); err != nil {
		v.logger.Error("Failed to send service failed incident metric to vmagent",
			zap.Error(err),
			zap.String("url", v.url))
	} else {
		v.logger.Info("Successfully sent service failed incident metric to vmagent",
			zap.String("url", v.url))
	}
}
