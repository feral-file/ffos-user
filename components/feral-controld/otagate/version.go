package otagate

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/feral-file/ffos-user/components/feral-controld/wrapper"
)

const (
	// localConfigPath is the running build descriptor written into the FF1 image.
	// Ported from feral-setupd constant::UPDATER_LOCAL_CONFIG_PATH.
	localConfigPath = "/home/feralfile/ff1-config.json"

	// upstreamURLSuffix is appended to the distributor endpoint, followed by the
	// build branch, to form the version-check URL:
	//   {endpoint}/api/latest/{branch}
	// Ported from feral-setupd constant::UPDATER_UPSTREAM_CONFIG_URL_SUFFIX.
	upstreamURLSuffix = "/api/latest/"

	// versionCheckRetries and versionCheckRetryDelay are the full retry budget for
	// a forced version refresh, ported from feral-setupd
	// constant::UPDATER_VERSION_CHECK_RETRIES / _RETRY_DELAY.
	//
	// feral-setupd also had a Single (one-attempt) budget used ONLY to honor the
	// BLE response deadline (RefreshRetries::Single). That variant is deliberately
	// NOT ported: controld's gate has no BLE notification held open, so every
	// caller uses the full budget.
	versionCheckRetries   = 3
	versionCheckRetryWait = 2 * time.Second

	// versionCheckTimeout bounds a single version-check HTTP attempt so an unstable
	// connection fails fast with a classified network error instead of hanging.
	// Ported from feral-setupd constant::UPDATER_VERSION_CHECK_REQUEST_TIMEOUT.
	versionCheckTimeout = 10 * time.Second
)

// ConfigProvider yields the running build's branch, semver version, and
// distributor endpoint. Injectable so tests need not touch the FF1 config file.
type ConfigProvider interface {
	LocalBuild(ctx context.Context) (branch, version, endpoint string, err error)
}

// versionFetchKind classifies a version-check failure. Ported from feral-setupd
// updater.rs VersionFetchFailureKind. The gate uses it only for structured
// logging (the per-kind TV copy stays with the future setupui surface).
type versionFetchKind int

const (
	fetchUnknown versionFetchKind = iota
	fetchNetwork
	fetchServer
	fetchClient
	fetchParse
)

func (k versionFetchKind) String() string {
	switch k {
	case fetchNetwork:
		return "network"
	case fetchServer:
		return "server"
	case fetchClient:
		return "client"
	case fetchParse:
		return "parse"
	default:
		return "unknown"
	}
}

type versionFetchError struct {
	kind versionFetchKind
	err  error
}

func (e *versionFetchError) Error() string { return fmt.Sprintf("%s: %v", e.kind, e.err) }
func (e *versionFetchError) Unwrap() error { return e.err }

// upstreamInfo is the distributor's version manifest.
type upstreamInfo struct {
	MinRuntimeVersion     string  `json:"min_runtime_version"`
	MinUpgradeableVersion *string `json:"min_upgradeable_version"`
	FlashingGuide         *string `json:"flashing_guide"`
	LatestVersion         string  `json:"latest_version"`
}

// remoteVersions is the parsed manifest with semver values.
type remoteVersions struct {
	minRuntime     semver
	minUpgradeable *semver
	flashingGuide  string
	latest         semver
}

// fetchRemoteVersion performs a forced version refresh over the full retry
// budget. It never falls back to stale cached metadata: a live failure surfaces
// as a classified versionFetchError so the caller can decide (matching
// feral-setupd's forced refresh_remote_version semantics).
func fetchRemoteVersion(ctx context.Context, deps Deps) (remoteVersions, error) {
	branch, _, endpoint, err := deps.Config.LocalBuild(ctx)
	if err != nil {
		return remoteVersions{}, &versionFetchError{kind: fetchUnknown, err: err}
	}
	url := endpoint + upstreamURLSuffix + branch

	return runVersionFetchWithRetries(ctx, deps, versionCheckRetries, versionCheckRetryWait,
		func() (remoteVersions, error) { return fetchRemoteVersionOnce(ctx, deps, url) })
}

// runVersionFetchWithRetries runs attemptFn up to maxAttempts times, sleeping
// retryWait between tries. Extracted (as in feral-setupd run_with_retries) so the
// retry budget is exercisable with an injected failing seam and a fake clock.
func runVersionFetchWithRetries(
	ctx context.Context,
	deps Deps,
	maxAttempts int,
	retryWait time.Duration,
	attemptFn func() (remoteVersions, error),
) (remoteVersions, error) {
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		v, err := attemptFn()
		if err == nil {
			return v, nil
		}
		lastErr = err
		if deps.Logger != nil {
			deps.Logger.Warn("OTA version check attempt failed",
				zap.Int("attempt", attempt), zap.Int("max", maxAttempts), zap.Error(err))
		}
		if attempt < maxAttempts {
			if serr := deps.Clock.SleepContext(ctx, retryWait); serr != nil {
				return remoteVersions{}, serr
			}
		}
	}
	if lastErr == nil {
		lastErr = &versionFetchError{kind: fetchUnknown,
			err: fmt.Errorf("version check failed after %d attempts", maxAttempts)}
	}
	return remoteVersions{}, lastErr
}

func fetchRemoteVersionOnce(ctx context.Context, deps Deps, url string) (remoteVersions, error) {
	reqCtx, cancel := context.WithTimeout(ctx, versionCheckTimeout)
	defer cancel()

	req, err := deps.HTTP.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return remoteVersions{}, &versionFetchError{kind: fetchUnknown, err: err}
	}
	req = req.WithContext(reqCtx)

	resp, err := deps.HTTP.Do(req)
	if err != nil {
		// No HTTP status means a transport-layer failure (DNS, TLS, connect,
		// timeout). Classify as network so the most common onboarding failure —
		// flaky Wi-Fi — is distinguishable from a server/parse fault.
		return remoteVersions{}, &versionFetchError{kind: fetchNetwork,
			err: fmt.Errorf("fetching %s: %w", url, err)}
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<12))
		return remoteVersions{}, &versionFetchError{
			kind: classifyHTTPStatus(resp.StatusCode),
			err:  fmt.Errorf("HTTP %d from distributor at %s: %s", resp.StatusCode, url, strings.TrimSpace(string(body))),
		}
	}

	var info upstreamInfo
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		return remoteVersions{}, &versionFetchError{kind: fetchParse,
			err: fmt.Errorf("decoding distributor JSON: %w", err)}
	}

	return parseUpstreamInfo(info)
}

func parseUpstreamInfo(info upstreamInfo) (remoteVersions, error) {
	minRuntime, err := parseSemver(info.MinRuntimeVersion)
	if err != nil {
		return remoteVersions{}, &versionFetchError{kind: fetchParse,
			err: fmt.Errorf("parsing upstream semver: %w", err)}
	}
	latest, err := parseSemver(info.LatestVersion)
	if err != nil {
		return remoteVersions{}, &versionFetchError{kind: fetchParse,
			err: fmt.Errorf("parsing upstream semver: %w", err)}
	}

	out := remoteVersions{minRuntime: minRuntime, latest: latest}
	if info.MinUpgradeableVersion != nil {
		mu, err := parseSemver(*info.MinUpgradeableVersion)
		if err != nil {
			return remoteVersions{}, &versionFetchError{kind: fetchParse,
				err: fmt.Errorf("parsing min_upgradeable_version semver: %w", err)}
		}
		out.minUpgradeable = &mu
	}
	if info.FlashingGuide != nil {
		out.flashingGuide = *info.FlashingGuide
	}
	return out, nil
}

// classifyHTTPStatus maps an HTTP status class to a fetch kind, mirroring the
// "HTTP {status}" arm of feral-setupd classify_version_fetch_error.
func classifyHTTPStatus(status int) versionFetchKind {
	switch {
	case status >= 500:
		return fetchServer
	case status >= 400:
		return fetchClient
	default:
		return fetchUnknown
	}
}

// isTooOldToUpgrade reports whether the running build is below the distributor's
// minimum upgradeable version (device must be reflashed). Ported from
// updater.rs::is_too_old_to_upgrade.
func isTooOldToUpgrade(current semver, versions remoteVersions) bool {
	if versions.minUpgradeable == nil {
		return false
	}
	return current.less(*versions.minUpgradeable)
}

// isUpdateRequired reports whether the running build is below the minimum
// supported (runtime) version. Ported from updater.rs::is_update_required.
func isUpdateRequired(current semver, versions remoteVersions) bool {
	return current.less(versions.minRuntime)
}

// isUpdateAvailable reports whether a newer build is available. Ported from
// updater.rs::is_update_available.
func isUpdateAvailable(current semver, versions remoteVersions) bool {
	return current.less(versions.latest)
}

// fileConfigProvider reads the FF1 local build descriptor once and caches it.
type fileConfigProvider struct {
	os   wrapper.OS
	json wrapper.JSON

	once     sync.Once
	branch   string
	version  string
	endpoint string
	loadErr  error
}

// NewFileConfigProvider returns the production ConfigProvider that reads
// /home/feralfile/ff1-config.json.
func NewFileConfigProvider(osw wrapper.OS, jsonw wrapper.JSON) ConfigProvider {
	return &fileConfigProvider{os: osw, json: jsonw}
}

func (p *fileConfigProvider) LocalBuild(_ context.Context) (string, string, string, error) {
	p.once.Do(func() {
		data, err := p.os.ReadFile(localConfigPath)
		if err != nil {
			p.loadErr = fmt.Errorf("reading local config: %w", err)
			return
		}
		var cfg struct {
			Branch   string `json:"branch"`
			Version  string `json:"version"`
			Endpoint string `json:"endpoint"`
		}
		if err := p.json.Unmarshal(data, &cfg); err != nil {
			p.loadErr = fmt.Errorf("parsing local config JSON: %w", err)
			return
		}
		p.branch, p.version, p.endpoint = cfg.Branch, cfg.Version, cfg.Endpoint
	})
	return p.branch, p.version, p.endpoint, p.loadErr
}
