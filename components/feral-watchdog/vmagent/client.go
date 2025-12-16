package vmagent

import (
	"bytes"
	"context"
	"fmt"

	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-watchdog/wrapper"
)

const (
	VMAGENT_REQUEST_TIMEOUT = 5 * time.Second
	VMAGENT_DEFAULT_URL     = "http://0.0.0.0:9431/api/v1/import/prometheus"
)

type Client interface {
	// SendCrashRebootMetric sends the crash_reboot metric to vmagent
	SendCrashRebootMetric(ctx context.Context, reason CrashReason) error
}

type client struct {
	// Dependencies
	io         wrapper.IO
	httpClient wrapper.HTTPClient
	logger     *zap.Logger

	// State
	url string
}

type CrashReason string

const (
	CrashReasonChromiumCrash CrashReason = "chromium_crash"
	CrashReasonGPUHang       CrashReason = "gpu_hang"
	CrashReasonDiskFull      CrashReason = "disk_full"
	CrashReasonRamCritical   CrashReason = "ram_critical"
)

// NewClient creates a new vmagent client instance
func NewClient(url string, logger *zap.Logger, httpClient wrapper.HTTPClient, io wrapper.IO) Client {
	if url == "" {
		url = VMAGENT_DEFAULT_URL
	}

	return &client{
		logger:     logger,
		url:        url,
		httpClient: httpClient,
		io:         io,
	}
}

func (c *client) SendCrashRebootMetric(ctx context.Context, reason CrashReason) error {
	c.logger.Info("Sending crash_reboot metric to vmagent",
		zap.String("reason", string(reason)),
		zap.String("url", c.url))
	metric := fmt.Sprintf("ff_crash_reboot{reason=\"%s\"} 1", reason)
	return c.send(ctx, metric)
}

func (c *client) send(ctx context.Context, metric string) error {
	resp, err := c.httpClient.PostWithContext(ctx, c.url, "text/plain", bytes.NewBufferString(metric))
	if err != nil {
		return fmt.Errorf("failed to send metric to vmagent: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("unexpected status code: %d for metric: %s", resp.StatusCode, metric)
	}

	return nil
}
