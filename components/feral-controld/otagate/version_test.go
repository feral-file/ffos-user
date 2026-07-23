package otagate

import (
	"context"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// TestFetchRemoteVersion_Success verifies the URL is built as {endpoint}/api/latest/{branch}
// and the manifest parses into semver values.
func TestFetchRemoteVersion_Success(t *testing.T) {
	var gotURL string
	http := &fakeHTTP{do: func(req *http.Request) (*http.Response, error) {
		gotURL = req.URL.String()
		return jsonResponse(200, okManifest("1.2.0", "1.0.0", "1.5.0")), nil
	}}
	deps := Deps{
		HTTP:   http,
		Clock:  newFakeClock(),
		Config: fakeConfig{branch: "stable", version: "1.4.0", endpoint: "https://dist.example"},
	}

	v, err := fetchRemoteVersion(context.Background(), deps)
	if err != nil {
		t.Fatalf("fetchRemoteVersion: %v", err)
	}
	if gotURL != "https://dist.example/api/latest/stable" {
		t.Fatalf("URL = %q, want https://dist.example/api/latest/stable", gotURL)
	}
	if v.minRuntime.compare(semver{major: 1, minor: 2, patch: 0}) != 0 {
		t.Errorf("minRuntime = %+v", v.minRuntime)
	}
	if v.minUpgradeable == nil || v.minUpgradeable.compare(semver{major: 1, patch: 0}) != 0 {
		t.Errorf("minUpgradeable = %+v", v.minUpgradeable)
	}
	if v.latest.compare(semver{major: 1, minor: 5}) != 0 {
		t.Errorf("latest = %+v", v.latest)
	}
}

// TestFetchRemoteVersion_RetryBudget mirrors updater.rs full_retries_failing_fetch:
// a Blocking-equivalent caller exhausts the configured retry budget (3 attempts)
// on a persistently failing fetch, sleeping between (but not after) attempts.
func TestFetchRemoteVersion_RetryBudget(t *testing.T) {
	var calls int32
	clock := newFakeClock()
	http := &fakeHTTP{do: func(*http.Request) (*http.Response, error) {
		atomic.AddInt32(&calls, 1)
		return nil, errors.New("dial tcp: connection refused")
	}}
	deps := Deps{
		HTTP:   http,
		Clock:  clock,
		Config: fakeConfig{branch: "b", version: "1.0.0", endpoint: "https://x"},
	}

	_, err := fetchRemoteVersion(context.Background(), deps)
	if err == nil {
		t.Fatal("expected error from persistently failing fetch")
	}
	if got := atomic.LoadInt32(&calls); got != versionCheckRetries {
		t.Fatalf("attempts = %d, want %d (full retry budget)", got, versionCheckRetries)
	}
	// versionCheckRetries attempts => versionCheckRetries-1 backoff sleeps.
	sleeps := clock.recordedSleeps()
	if len(sleeps) != versionCheckRetries-1 {
		t.Fatalf("backoff sleeps = %d, want %d", len(sleeps), versionCheckRetries-1)
	}
	for i, s := range sleeps {
		if s != versionCheckRetryWait {
			t.Errorf("sleep[%d] = %v, want %v", i, s, versionCheckRetryWait)
		}
	}
}

// TestFetchRemoteVersion_SucceedsAfterRetry: a transient failure then success
// returns the parsed manifest without exhausting the budget.
func TestFetchRemoteVersion_SucceedsAfterRetry(t *testing.T) {
	var calls int32
	http := &fakeHTTP{do: func(*http.Request) (*http.Response, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return nil, errors.New("timeout")
		}
		return jsonResponse(200, okManifest("1.0.0", "0.9.0", "1.0.0")), nil
	}}
	deps := Deps{
		HTTP:   http,
		Clock:  newFakeClock(),
		Config: fakeConfig{branch: "b", version: "1.0.0", endpoint: "https://x"},
	}
	if _, err := fetchRemoteVersion(context.Background(), deps); err != nil {
		t.Fatalf("expected success after retry, got %v", err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("attempts = %d, want 2", got)
	}
}

// TestClassifyVersionFetch mirrors updater.rs classify_version_fetch_error_tests:
// HTTP 5xx -> server, 4xx -> client, malformed JSON -> parse, transport error
// (no status) -> network.
func TestClassifyVersionFetch(t *testing.T) {
	cases := []struct {
		name string
		do   func(*http.Request) (*http.Response, error)
		want versionFetchKind
	}{
		{"5xx is server (http_status_line_5xx_is_server)",
			func(*http.Request) (*http.Response, error) { return jsonResponse(503, "unavailable"), nil }, fetchServer},
		{"4xx is client (http_status_line_4xx_is_client)",
			func(*http.Request) (*http.Response, error) { return jsonResponse(404, "not found"), nil }, fetchClient},
		{"bad json is parse (decoding_distributor_json_context_is_parse)",
			func(*http.Request) (*http.Response, error) { return jsonResponse(200, "{not json"), nil }, fetchParse},
		{"transport error is network (transport_error_without_status_is_network)",
			func(*http.Request) (*http.Response, error) { return nil, errors.New("dial tcp: connection refused") }, fetchNetwork},
		{"bad semver is parse (parsing_upstream_semver_context_is_parse)",
			func(*http.Request) (*http.Response, error) {
				return jsonResponse(200, okManifest("not-a-semver", "1.0.0", "1.0.0")), nil
			}, fetchParse},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			deps := Deps{
				HTTP:   &fakeHTTP{do: tc.do},
				Clock:  &fakeClock{now: time.Now(), sleepErr: nil},
				Config: fakeConfig{branch: "b", version: "1.0.0", endpoint: "https://x"},
			}
			// Single attempt so a failing seam does not sleep the (real) budget.
			_, err := runVersionFetchWithRetries(context.Background(), deps, 1, 0,
				func() (remoteVersions, error) {
					return fetchRemoteVersionOnce(context.Background(), deps, "https://x/api/latest/b")
				})
			var vfe *versionFetchError
			if !errors.As(err, &vfe) {
				t.Fatalf("expected *versionFetchError, got %v", err)
			}
			if vfe.kind != tc.want {
				t.Fatalf("kind = %v, want %v", vfe.kind, tc.want)
			}
		})
	}
}

// TestUpgradeDecisions mirrors updater.rs is_too_old_to_upgrade / is_update_required /
// is_update_available.
func TestUpgradeDecisions(t *testing.T) {
	versions := remoteVersions{
		minRuntime: mustSemver(t, "1.2.0"),
		latest:     mustSemver(t, "1.5.0"),
	}
	mu := mustSemver(t, "1.0.0")
	versions.minUpgradeable = &mu

	cases := []struct {
		name      string
		current   string
		tooOld    bool
		required  bool
		available bool
	}{
		{"below-min-upgradeable", "0.9.0", true, true, true},
		{"below-min-runtime", "1.1.0", false, true, true},
		{"between-runtime-and-latest", "1.3.0", false, false, true},
		{"at-latest", "1.5.0", false, false, false},
		{"above-latest", "1.6.0", false, false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cur := mustSemver(t, tc.current)
			if got := isTooOldToUpgrade(cur, versions); got != tc.tooOld {
				t.Errorf("isTooOldToUpgrade = %v, want %v", got, tc.tooOld)
			}
			if got := isUpdateRequired(cur, versions); got != tc.required {
				t.Errorf("isUpdateRequired = %v, want %v", got, tc.required)
			}
			if got := isUpdateAvailable(cur, versions); got != tc.available {
				t.Errorf("isUpdateAvailable = %v, want %v", got, tc.available)
			}
		})
	}

	// Absent min_upgradeable => never too old (mirrors the None arm).
	noMU := remoteVersions{minRuntime: mustSemver(t, "1.0.0"), latest: mustSemver(t, "1.0.0")}
	if isTooOldToUpgrade(mustSemver(t, "0.1.0"), noMU) {
		t.Error("absent min_upgradeable must never be too old")
	}
}

func mustSemver(t *testing.T, s string) semver {
	t.Helper()
	v, err := parseSemver(s)
	if err != nil {
		t.Fatalf("parseSemver(%q): %v", s, err)
	}
	return v
}
