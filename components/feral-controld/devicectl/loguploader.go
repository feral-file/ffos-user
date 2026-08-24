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

// maxLogArchiveInputBytes bounds the total bytes zipLogs reads into memory.
// The in-memory port holds both the raw file map and the zip buffer at once,
// so peak memory is roughly input + compressed ≤ 2× this budget. The bound
// exists because nothing else enforces one: log rotation keeps a healthy
// device far below it, but controld is now the sole provisioning/recovery
// daemon and an unrotated or runaway log must degrade to a partial archive
// (skipped files are logged at Warn), never OOM the process. Files are sized
// via Stat/DirEntry.Info BEFORE reading so an oversized file is never pulled
// into memory at all.
const maxLogArchiveInputBytes = 128 << 20

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
	// maxInputBytes is the zipLogs input budget (maxLogArchiveInputBytes in
	// production); a struct field so tests can exercise the cap without
	// gigabyte fixtures.
	maxInputBytes int64
	// netlogDir is the EFFECTIVE flight-recorder ring directory (netlog.dir
	// may relocate it outside logsDir, e.g. somewhere OTA-durable — plan
	// branch B2). Empty means the default <logsDir>/netlog. zipLogs archives
	// it under the netlog/ entry prefix either way: a configured ring must
	// never silently drop out of the bundle that exists to carry it.
	netlogDir string
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
		http:          httpClient,
		os:            osw,
		json:          jsonw,
		logsDir:       defaultLogsDir,
		extraLogs:     defaultExtraLogs,
		apiURL:        logUploadAPIURL,
		maxInputBytes: maxLogArchiveInputBytes,
		logger:        logger,
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
	// remaining is the input budget shared by the log dir walk and the extra
	// logs. Direct struct construction (tests) may leave maxInputBytes zero;
	// fall back to the production bound rather than collecting nothing.
	remaining := u.maxInputBytes
	if remaining <= 0 {
		remaining = maxLogArchiveInputBytes
	}

	files := map[string][]byte{}
	// The netlog ring is the outage artifact this bundle exists to carry
	// (docs/wan-outage-observability.md stage 2a), and it needs its budget
	// exactly when the rest of ~/.logs is at its worst: a flapping device
	// balloons controld.log and its rotated backups, and "backup" sorts
	// before "netlog" in the walk, so a shared budget consumed in directory
	// order could evict the ring from the very bundle raised to diagnose the
	// outage. Collect the ring FIRST — its own 8 MiB cap bounds the claim —
	// then walk the rest with the ring skipped so it is neither re-read nor
	// double-charged.
	ringDir := u.netlogDir
	if ringDir == "" {
		ringDir = filepath.Join(u.logsDir, netlogSubdir)
	}
	if err := u.collectDir(ringDir, netlogSubdir, files, &remaining, nil); err != nil {
		return nil, err
	}
	// The main walk skips <logsDir>/netlog unconditionally: when the ring is
	// in its default place this avoids double-charging it, and when netlog.dir
	// relocated it, a stale leftover default dir must not collide with (and
	// overwrite) the live ring's netlog/ archive entries. A ring relocated to
	// a DIFFERENT subdirectory of logsDir is skipped by its own relative path
	// for the same double-charge reason.
	skipRels := map[string]bool{netlogSubdir: true}
	if rel, err := filepath.Rel(u.logsDir, ringDir); err == nil && rel != "." && rel != ".." &&
		!strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		skipRels[filepath.ToSlash(rel)] = true
	}
	skipNetlog := func(rel string) bool { return skipRels[rel] }
	if err := u.collectDir(u.logsDir, "", files, &remaining, skipNetlog); err != nil {
		return nil, err
	}
	for _, path := range u.extraLogs {
		info, err := u.os.Stat(path)
		if err != nil {
			if !u.os.IsNotExist(err) {
				u.logger.Debug("Skipping unreadable extra log", zap.String("path", path), zap.Error(err))
			}
			continue
		}
		if info.Size() > remaining {
			u.logger.Warn("Log archive budget exhausted; skipping extra log",
				zap.String("path", path), zap.Int64("size", info.Size()), zap.Int64("remaining", remaining))
			continue
		}
		data, err := u.os.ReadFile(path) //nolint:gosec // fixed in-image log paths, never user input
		if err != nil {
			if !u.os.IsNotExist(err) {
				u.logger.Debug("Skipping unreadable extra log", zap.String("path", path), zap.Error(err))
			}
			continue
		}
		remaining -= int64(len(data))
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
// error — a device may not have produced logs yet. remaining is the shared
// input budget: files are sized via DirEntry.Info BEFORE reading so an
// oversized file never lands in memory, and every skip is logged at Warn so a
// truncated support archive is diagnosable (a file that grows between Info and
// ReadFile can overshoot by the growth only — acceptable slack for a hard cap
// without opening file handles through the wrapper seam).
// netlogSubdir is the flight-recorder ring directory under the logs dir; it
// gets budget priority in zipLogs (see the call site for why).
const netlogSubdir = "netlog"

// collectDir walks dir recursively. skip, when non-nil, drops entries whose
// archive-relative path matches (used to avoid re-walking the netlog ring
// after its priority collection).
func (u *logUploader) collectDir(dir, prefix string, files map[string][]byte, remaining *int64, skip func(rel string) bool) error {
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
		if skip != nil && skip(rel) {
			continue
		}
		if entry.IsDir() {
			if err := u.collectDir(full, rel, files, remaining, skip); err != nil {
				return err
			}
			continue
		}
		info, err := entry.Info()
		if err != nil {
			u.logger.Debug("Skipping unstat-able log file", zap.String("path", full), zap.Error(err))
			continue
		}
		if info.Size() > *remaining {
			u.logger.Warn("Log archive budget exhausted; skipping log file",
				zap.String("path", full), zap.Int64("size", info.Size()), zap.Int64("remaining", *remaining))
			continue
		}
		data, err := u.os.ReadFile(full) //nolint:gosec // walking the fixed in-image log dir
		if err != nil {
			u.logger.Debug("Skipping unreadable log file", zap.String("path", full), zap.Error(err))
			continue
		}
		*remaining -= int64(len(data))
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
