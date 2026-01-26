package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"go.uber.org/zap"
)

const (
	VERSION_REFRESH_INTERVAL  = 1 * time.Hour
	VERSION_CHECK_RETRIES     = 3
	VERSION_CHECK_RETRY_DELAY = 2 * time.Second
	VERSION_API_TIMEOUT       = 10 * time.Second
	VERSION_API_URL_SUFFIX    = "/api/latest/"
)

// VersionInfo holds the cached version information from the API
type VersionInfo struct {
	LatestVersion         string `json:"latest_version"`
	MinRuntimeVersion     string `json:"min_runtime_version"`
	MinUpgradeableVersion string `json:"min_upgradeable_version,omitempty"`
	FlashingGuide         string `json:"flashing_guide,omitempty"`
}

// VersionChecker handles fetching and caching version information
type VersionChecker struct {
	ctx        context.Context
	cancel     context.CancelFunc
	logger     *zap.Logger
	cache      *VersionInfo
	cacheMutex sync.RWMutex
	wg         sync.WaitGroup
}

// NewVersionChecker creates a new VersionChecker instance
func NewVersionChecker(ctx context.Context, logger *zap.Logger) *VersionChecker {
	childCtx, cancel := context.WithCancel(ctx)
	return &VersionChecker{
		ctx:    childCtx,
		cancel: cancel,
		logger: logger,
	}
}

// Start begins the background version refresh goroutine
func (v *VersionChecker) Start() {
	v.wg.Add(1)
	go v.refreshLoop()
}

// Stop cancels the background refresh and waits for it to finish
func (v *VersionChecker) Stop() {
	v.cancel()
	v.wg.Wait()
}

// refreshLoop periodically refreshes the cached version info
func (v *VersionChecker) refreshLoop() {
	defer v.wg.Done()

	// Do an initial fetch
	v.logger.Info("VersionChecker: Performing initial version fetch")
	_, _ = v.FetchVersion(true)

	ticker := time.NewTicker(VERSION_REFRESH_INTERVAL)
	defer ticker.Stop()

	for {
		select {
		case <-v.ctx.Done():
			v.logger.Info("VersionChecker: Stopping refresh loop")
			return
		case <-ticker.C:
			v.logger.Info("VersionChecker: Periodic version refresh triggered")
			_, _ = v.FetchVersion(true)
		}
	}
}

// FetchVersion retrieves version info, optionally forcing a refresh
func (v *VersionChecker) FetchVersion(forceRefresh bool) (*VersionInfo, error) {
	// If not forcing refresh, try to return cached value
	if !forceRefresh {
		v.cacheMutex.RLock()
		if v.cache != nil {
			cached := *v.cache
			v.cacheMutex.RUnlock()
			return &cached, nil
		}
		v.cacheMutex.RUnlock()
	}

	// Fetch with retries
	var info *VersionInfo
	var lastErr error

	for attempt := 1; attempt <= VERSION_CHECK_RETRIES; attempt++ {
		v.logger.Info("VersionChecker: Fetching version info",
			zap.Int("attempt", attempt),
			zap.Int("maxRetries", VERSION_CHECK_RETRIES))

		var err error
		info, err = v.fetchVersionOnce()
		if err == nil {
			// Success - update cache
			v.cacheMutex.Lock()
			v.cache = info
			v.cacheMutex.Unlock()
			v.logger.Info("VersionChecker: Successfully fetched version info",
				zap.String("latestVersion", info.LatestVersion))
			return info, nil
		}

		lastErr = err
		v.logger.Warn("VersionChecker: Version check attempt failed",
			zap.Int("attempt", attempt),
			zap.Int("maxRetries", VERSION_CHECK_RETRIES),
			zap.Error(err))

		// Don't sleep after the last attempt
		if attempt < VERSION_CHECK_RETRIES {
			select {
			case <-v.ctx.Done():
				return nil, v.ctx.Err()
			case <-time.After(VERSION_CHECK_RETRY_DELAY):
			}
		}
	}

	return nil, fmt.Errorf("version check failed after %d attempts: %w", VERSION_CHECK_RETRIES, lastErr)
}

// fetchVersionOnce performs a single API request to fetch version info
func (v *VersionChecker) fetchVersionOnce() (*VersionInfo, error) {
	if config.FF1Config.Endpoint == "" || config.FF1Config.Branch == "" {
		v.logger.Warn("VersionChecker: Missing endpoint or branch in config")
		return nil, fmt.Errorf("missing endpoint or branch in config")
	}

	apiURL := fmt.Sprintf("%s%s%s", config.FF1Config.Endpoint, VERSION_API_URL_SUFFIX, config.FF1Config.Branch)

	client := &http.Client{
		Timeout: VERSION_API_TIMEOUT,
	}

	req, err := http.NewRequestWithContext(v.ctx, "GET", apiURL, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch version: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API returned status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read response body: %w", err)
	}

	var info VersionInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return nil, fmt.Errorf("failed to parse response: %w", err)
	}

	return &info, nil
}

// GetCachedVersion returns the cached version without making an API call
// Returns nil if no cached version is available
func (v *VersionChecker) GetCachedVersion() *VersionInfo {
	v.cacheMutex.RLock()
	defer v.cacheMutex.RUnlock()

	if v.cache == nil {
		return nil
	}

	cached := *v.cache
	return &cached
}
