package devicectl

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"sort"
	"strings"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

// Log-upload port from feral-setupd log_uploader.rs. The wire contract is
// reproduced faithfully: a JSON pre-sign request to the v2 log-submissions API
// returns a pre-signed S3 URL, then the zipped logs are PUT to that URL. The
// only intentional divergence from the Rust is that the zip is assembled in
// memory rather than streamed through a temp file — controld's log set is
// bounded, and the request shape the server sees (endpoints, headers, JSON
// fields, application/zip body) is byte-for-byte the same.
const (
	// logUploadAPIURL is the v2 pre-sign endpoint. Ported from
	// feral-setupd constant::LOG_UPLOAD_API.
	logUploadAPIURL = "https://support-logs.feralfile.com/v2/ff1/log-submissions"

	// defaultLogsDir is the on-device log directory zipped for upload. Ported
	// from feral-setupd constant::LOG_FILEDIR.
	defaultLogsDir = "/home/feralfile/.logs"

	// logUploadSource tags the submission's origin. The Rust D-Bus callback
	// always passes "dbus" (the BLE callback passes "ble"); the controld
	// relayer command is the D-Bus-equivalent ingress, so it inherits "dbus".
	logUploadSource = "dbus"
)

// defaultExtraLogs are copied into the archive root alongside the log directory,
// mirroring feral-setupd log_uploader.rs which folds the updater logs in under
// their base names.
var defaultExtraLogs = []string{"/var/log/updaterd.log", "/var/log/auto-updaterd.log"}

// logUploadBuildInfo carries the device identity/build metadata the pre-sign
// request reports. In feral-setupd these come from AppState (device_id from
// /etc/hostname, branch/version from the FF1 build descriptor); controld gathers
// the same values from its own seams before calling Upload.
type logUploadBuildInfo struct {
	DeviceID string
	Branch   string
	Version  string
}

// logUploader zips the device logs and uploads them via the v2 pre-sign API. The
// HTTP client and filesystem are injected so the request shape can be exercised
// against an httptest server without touching the real endpoints or /home.
type logUploader struct {
	http      wrapper.HTTPClient
	os        wrapper.OS
	json      wrapper.JSON
	logsDir   string
	extraLogs []string
	apiURL    string
	logger    *zap.Logger
}

// logUploaderIface is the seam the executor drives; overridable in tests so the
// owner-mode routing can be checked without a real network transfer.
type logUploaderIface interface {
	Upload(ctx context.Context, apiKey, source string, info logUploadBuildInfo, supportBundleID string) error
}

func newLogUploader(httpClient wrapper.HTTPClient, osw wrapper.OS, jsonw wrapper.JSON, logger *zap.Logger) *logUploader {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &logUploader{
		http:      httpClient,
		os:        osw,
		json:      jsonw,
		logsDir:   defaultLogsDir,
		extraLogs: defaultExtraLogs,
		apiURL:    logUploadAPIURL,
		logger:    logger,
	}
}

// logSubmissionRequest is the pre-sign request body. Ported from
// feral-setupd log_uploader.rs::LogSubmissionRequest. title is always None in
// the port (the v2 API ignores it), so the field is omitted entirely;
// support_bundle_id is omitted when blank, matching the Rust
// skip_serializing_if.
type logSubmissionRequest struct {
	DeviceID        string   `json:"device_id"`
	SupportBundleID string   `json:"support_bundle_id,omitempty"`
	Source          string   `json:"source"`
	Tags            []string `json:"tags"`
	Branch          string   `json:"branch"`
	Version         string   `json:"version"`
}

// logSubmissionResponse mirrors the v2 pre-sign response; only upload.url is
// used. Ported from feral-setupd log_uploader.rs::LogSubmissionResponse.
type logSubmissionResponse struct {
	ObjectKey string `json:"object_key"`
	Upload    struct {
		URL string `json:"url"`
	} `json:"upload"`
	ExpiresInSeconds int64 `json:"expires_in_seconds"`
}

// Upload zips the device logs and uploads them: request a pre-signed URL from
// the v2 API, then PUT the archive to S3. Ported from
// feral-setupd log_uploader.rs::submit_logs.
func (u *logUploader) Upload(ctx context.Context, apiKey, source string, info logUploadBuildInfo, supportBundleID string) error {
	archive, err := u.zipLogs()
	if err != nil {
		return fmt.Errorf("failed to zip logs: %w", err)
	}

	uploadURL, err := u.getPresignedURL(ctx, apiKey, source, info, supportBundleID)
	if err != nil {
		return err
	}

	if err := u.uploadToS3(ctx, uploadURL, archive); err != nil {
		return err
	}

	u.logger.Info("Log submission completed successfully",
		zap.String("source", source), zap.Int("bytes", len(archive)))
	return nil
}

// zipLogs builds an in-memory zip of the log directory plus the extra logs.
// Directory files keep their path relative to logsDir; extra logs are added at
// the archive root under their base name. Ported from
// feral-setupd log_uploader.rs::create_logs_zip (streaming temp file collapsed
// to an in-memory buffer). An empty archive is an error, matching the Rust
// "No log files found".
func (u *logUploader) zipLogs() ([]byte, error) {
	files := map[string][]byte{}
	if err := u.collectDir(u.logsDir, "", files); err != nil {
		return nil, err
	}
	for _, path := range u.extraLogs {
		data, err := u.os.ReadFile(path) //nolint:gosec // fixed in-image log paths, never user input
		if err != nil {
			if !u.os.IsNotExist(err) {
				u.logger.Debug("Skipping unreadable extra log", zap.String("path", path), zap.Error(err))
			}
			continue
		}
		files[filepath.Base(path)] = data
	}

	if len(files) == 0 {
		return nil, fmt.Errorf("no log files found")
	}

	// Deterministic entry order keeps the archive reproducible for tests; the
	// server does not care about ordering.
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)

	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, name := range names {
		w, err := zw.Create(name)
		if err != nil {
			return nil, fmt.Errorf("failed to create zip entry %q: %w", name, err)
		}
		if _, err := w.Write(files[name]); err != nil {
			return nil, fmt.Errorf("failed to write zip entry %q: %w", name, err)
		}
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("failed to finalize zip: %w", err)
	}
	return buf.Bytes(), nil
}

// collectDir recursively reads dir into files, keyed by path relative to the
// original root (prefix accumulates the sub-path). A missing directory is not an
// error — a device may not have produced logs yet.
func (u *logUploader) collectDir(dir, prefix string, files map[string][]byte) error {
	entries, err := u.os.ReadDir(dir)
	if err != nil {
		if u.os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("failed to read log dir %q: %w", dir, err)
	}
	for _, entry := range entries {
		full := filepath.Join(dir, entry.Name())
		rel := entry.Name()
		if prefix != "" {
			rel = prefix + "/" + entry.Name()
		}
		if entry.IsDir() {
			if err := u.collectDir(full, rel, files); err != nil {
				return err
			}
			continue
		}
		data, err := u.os.ReadFile(full) //nolint:gosec // walking the fixed in-image log dir
		if err != nil {
			u.logger.Debug("Skipping unreadable log file", zap.String("path", full), zap.Error(err))
			continue
		}
		files[rel] = data
	}
	return nil
}

// getPresignedURL performs the v2 pre-sign POST and returns the S3 upload URL.
// Ported from feral-setupd log_uploader.rs::get_presigned_url.
func (u *logUploader) getPresignedURL(ctx context.Context, apiKey, source string, info logUploadBuildInfo, supportBundleID string) (string, error) {
	reqBody := logSubmissionRequest{
		DeviceID:        info.DeviceID,
		SupportBundleID: strings.TrimSpace(supportBundleID),
		Source:          source,
		Tags:            []string{"device-logs"},
		Branch:          info.Branch,
		Version:         info.Version,
	}
	body, err := u.json.Marshal(reqBody)
	if err != nil {
		return "", fmt.Errorf("failed to marshal log submission request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.apiURL, bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("failed to build pre-sign request: %w", err)
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := u.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("pre-sign request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("pre-sign API returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed logSubmissionResponse
	respBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("failed to read pre-sign response: %w", err)
	}
	if err := u.json.Unmarshal(respBytes, &parsed); err != nil {
		return "", fmt.Errorf("failed to parse pre-sign response: %w", err)
	}
	if parsed.Upload.URL == "" {
		return "", fmt.Errorf("pre-sign response missing upload url")
	}
	return parsed.Upload.URL, nil
}

// uploadToS3 PUTs the archive to the pre-signed URL. Ported from
// feral-setupd log_uploader.rs::upload_zip_to_s3.
func (u *logUploader) uploadToS3(ctx context.Context, uploadURL string, archive []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(archive))
	if err != nil {
		return fmt.Errorf("failed to build S3 upload request: %w", err)
	}
	req.Header.Set("Content-Type", "application/zip")
	req.ContentLength = int64(len(archive))

	resp, err := u.http.Do(req)
	if err != nil {
		return fmt.Errorf("S3 upload failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 upload returned HTTP %d: %s", resp.StatusCode, string(respBody))
	}
	return nil
}
