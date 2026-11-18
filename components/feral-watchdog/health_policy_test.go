package main

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"go.uber.org/zap"
)

// Existing recovery-policy fixtures use an HTTP server as a controllable probe.
// Production always probes the native command socket; it never calls CDP.
func newHTTPPolicyTestMonitor(endpoint string, logger *zap.Logger, handler *CommandHandler) *ChromiumMonitor {
	m := NewChromiumMonitor(endpoint, logger, handler)
	m.nativeProtocol = false
	m.probe = func(ctx context.Context) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, m.cdpEndpoint+"/json/version", nil)
		if err != nil {
			return err
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return err
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("probe status %d", resp.StatusCode)
		}
		_, err = io.Copy(io.Discard, resp.Body)
		return err
	}
	return m
}
