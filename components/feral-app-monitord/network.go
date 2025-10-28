package main

import (
	"bytes"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/zap"
)

// CheckConnectivity pings a reliable host to verify internet access.
func CheckConnectivity() bool {
	conn, err := net.DialTimeout("tcp", "8.8.8.8:53", 2*time.Second)
	if err != nil {
		return false
	}
	if err := conn.Close(); err != nil {
		log.Warn("Failed to close conn", zap.Error(err))
	}
	return true
}

// SendPayload sends the given JSON payload to the specified URL.
func SendPayload(payload []byte) error {
	req, err := http.NewRequest("POST", config.FF1Config.HeartbeatEndpoint, bytes.NewBuffer(payload))
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("failed to send request: %w", err)
	}

	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Error("Error closing resp.Body", zap.Error(err))
		}
	}()

	if resp.StatusCode >= 300 {
		return fmt.Errorf("server responded with non-success status: %s", resp.Status)
	}

	return nil
}
